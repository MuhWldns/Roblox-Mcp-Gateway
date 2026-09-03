package mcpoauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Canonical endpoint paths published by the OAuth discovery documents. The
// protected resource is the MCP Streamable HTTP endpoint.
const (
	ResourcePath     = "/mcp"
	AuthorizePath    = "/oauth/authorize"
	TokenPath        = "/oauth/token"
	RevocationPath   = "/oauth/revoke"
	RegistrationPath = "/oauth/register"
)

// Protocol values advertised by the authorization server metadata.
const (
	ResponseTypeCode           = "code"
	GrantTypeAuthorizationCode = "authorization_code"
	GrantTypeRefreshToken      = "refresh_token"
	CodeChallengeMethodS256    = "S256"
	TokenEndpointAuthNone      = "none"
	BearerMethodHeader         = "header"
)

// ProtectedResourceMetadata is the RFC 9728 document served from
// /.well-known/oauth-protected-resource.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported,omitempty"`
}

// AuthorizationServerMetadata is the RFC 8414 document served from
// /.well-known/oauth-authorization-server.
type AuthorizationServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RevocationEndpoint                string   `json:"revocation_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	ScopesSupported                   []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
}

// Metadata builds the two OAuth discovery documents published by the gateway.
// The issuer is the public gateway origin and the resource is the MCP
// endpoint. Both documents share the issuer, so the protected-resource
// metadata always names exactly one authorization server that is consistent
// with the issuer claim. Construct via NewMetadata.
type Metadata struct {
	issuer   *url.URL
	resource *url.URL
	scopes   []string
}

// NewMetadata validates the discovery configuration and returns a Metadata
// builder. The issuer must be an absolute HTTPS URL without query, fragment,
// or credentials; the resource must satisfy the same rule; the scopes must
// be a non-empty list of distinct valid scope tokens.
func NewMetadata(issuer, resource *url.URL, scopes []string) (Metadata, error) {
	if issuer == nil {
		return Metadata{}, errors.New("mcpoauth: issuer is required")
	}
	if resource == nil {
		return Metadata{}, errors.New("mcpoauth: resource is required")
	}
	if err := validateDiscoveryURL(issuer, "issuer"); err != nil {
		return Metadata{}, err
	}
	if err := validateDiscoveryURL(resource, "resource"); err != nil {
		return Metadata{}, err
	}
	if err := ValidateScopes(scopes); err != nil {
		return Metadata{}, err
	}
	return Metadata{
		issuer:   cloneURL(issuer),
		resource: cloneURL(resource),
		scopes:   append([]string(nil), scopes...),
	}, nil
}

// cloneURL returns a deep copy of u so later mutation of the caller's URL
// cannot alter published metadata.
func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	copied := *u
	if u.User != nil {
		copied.User = new(url.Userinfo)
		*copied.User = *u.User
	}
	return &copied
}

func validateDiscoveryURL(u *url.URL, name string) error {
	if u.Scheme != "https" || !u.IsAbs() || u.Host == "" {
		return fmt.Errorf("mcpoauth: %s must be an absolute https URL, got %q", name, u.String())
	}
	if u.User != nil {
		return fmt.Errorf("mcpoauth: %s must not carry credentials", name)
	}
	if u.RawQuery != "" {
		return fmt.Errorf("mcpoauth: %s must not carry a query", name)
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return fmt.Errorf("mcpoauth: %s must not carry a fragment", name)
	}
	return nil
}

// ProtectedResource returns the RFC 9728 metadata for the /mcp endpoint.
func (m Metadata) ProtectedResource() ProtectedResourceMetadata {
	return ProtectedResourceMetadata{
		Resource:               m.resource.String(),
		AuthorizationServers:   []string{m.issuer.String()},
		ScopesSupported:        append([]string(nil), m.scopes...),
		BearerMethodsSupported: []string{BearerMethodHeader},
	}
}

// AuthorizationServer returns the RFC 8414 metadata for the connector
// authorization server: the OAuth 2.1 authorization-code and refresh flows
// with mandatory S256 PKCE, token revocation, and client registration, for
// public clients only.
func (m Metadata) AuthorizationServer() AuthorizationServerMetadata {
	return AuthorizationServerMetadata{
		Issuer:                            m.issuer.String(),
		AuthorizationEndpoint:             m.endpoint(AuthorizePath),
		TokenEndpoint:                     m.endpoint(TokenPath),
		RevocationEndpoint:                m.endpoint(RevocationPath),
		RegistrationEndpoint:              m.endpoint(RegistrationPath),
		ScopesSupported:                   append([]string(nil), m.scopes...),
		ResponseTypesSupported:            []string{ResponseTypeCode},
		GrantTypesSupported:               []string{GrantTypeAuthorizationCode, GrantTypeRefreshToken},
		CodeChallengeMethodsSupported:     []string{CodeChallengeMethodS256},
		TokenEndpointAuthMethodsSupported: []string{TokenEndpointAuthNone},
	}
}

// endpoint joins path onto the issuer without inheriting query or fragment.
func (m Metadata) endpoint(path string) string {
	resolved := cloneURL(m.issuer)
	resolved.Path = strings.TrimSuffix(resolved.Path, "/") + path
	resolved.RawQuery = ""
	resolved.Fragment = ""
	resolved.RawFragment = ""
	return resolved.String()
}

// ClientMetadataDocument is the RFC 7591 client metadata carried by an
// OAuth Client ID Metadata Document published at an https client_id URL.
type ClientMetadataDocument struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
	Scope        string   `json:"scope,omitempty"`
}

// Fetch policy defaults for Client ID Metadata Document fetches.
const (
	DefaultFetchTimeout     = 10 * time.Second
	DefaultMaxMetadataBytes = 64 << 10
	DefaultMaxRedirects     = 3
)

const (
	fetchUserAgent = "robloxkit-mcp-oauth"
	dialTimeout    = 10 * time.Second
)

// ipPolicy reports whether a resolved address may be dialed.
type ipPolicy func(net.IP) bool

// publicIPPolicy rejects loopback, private, link-local, unspecified, and
// multicast addresses in IPv4 and IPv6, including mapped forms.
func publicIPPolicy(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return !(ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast())
}

// ClientMetadataFetcher fetches Client ID Metadata Documents with strict
// SSRF controls: HTTPS only, public addresses only, controlled HTTPS-only
// redirects, strict application/json content type, and a hard size cap.
type ClientMetadataFetcher struct {
	client       *http.Client
	maxBytes     int64
	maxRedirects int
}

// NewClientMetadataFetcher returns the production fetcher. Non-positive
// timeout and size values fall back to the package defaults; a negative
// maxRedirects falls back to DefaultMaxRedirects while zero forbids
// redirects entirely.
func NewClientMetadataFetcher(timeout time.Duration, maxBytes int64, maxRedirects int) *ClientMetadataFetcher {
	return newClientMetadataFetcher(timeout, maxBytes, maxRedirects, publicIPPolicy, nil)
}

// newClientMetadataFetcher builds a fetcher for tests, which may relax the
// address policy and certificate verification for local loopback servers.
func newClientMetadataFetcher(timeout time.Duration, maxBytes int64, maxRedirects int, policy ipPolicy, tlsCfg *tls.Config) *ClientMetadataFetcher {
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMetadataBytes
	}
	if maxRedirects < 0 {
		maxRedirects = DefaultMaxRedirects
	}
	if policy == nil {
		policy = publicIPPolicy
	}
	fetcher := &ClientMetadataFetcher{maxBytes: maxBytes, maxRedirects: maxRedirects}
	fetcher.client = &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// Never consult proxy configuration: an HTTP proxy would bypass
			// the address guard and resolve the target itself.
			Proxy:             nil,
			DialContext:       guardedDialContext(policy),
			ForceAttemptHTTP2: true,
			TLSClientConfig:   tlsCfg,
		},
		CheckRedirect: fetcher.checkRedirect,
	}
	return fetcher
}

// checkRedirect enforces HTTPS-only redirect targets and the redirect budget.
// Private-address enforcement on every hop happens in the dial guard.
func (f *ClientMetadataFetcher) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > f.maxRedirects {
		return fmt.Errorf("%w: exceeded %d redirects", ErrUnsafeFetch, f.maxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("%w: redirect to non-https scheme %q", ErrUnsafeFetch, req.URL.Scheme)
	}
	return nil
}

// guardedDialContext resolves the host at connect time, rejects the dial when
// any resolved address is disallowed (closing the DNS-rebinding TOCTOU
// window), and connects only to the vetted addresses.
func guardedDialContext(policy ipPolicy) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("mcpoauth: split dial address %q: %w", addr, err)
		}
		if network != "tcp" {
			return nil, fmt.Errorf("mcpoauth: unsupported dial network %q", network)
		}
		resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("mcpoauth: resolve %q: %w", host, err)
		}
		if len(resolved) == 0 {
			return nil, fmt.Errorf("%w: %q resolves to no addresses", ErrUnsafeFetch, host)
		}
		addresses := make([]net.IP, 0, len(resolved))
		for _, entry := range resolved {
			if !policy(entry.IP) {
				return nil, fmt.Errorf("%w: %q resolves to disallowed address %s", ErrUnsafeFetch, host, entry.IP)
			}
			addresses = append(addresses, entry.IP)
		}
		dialer := &net.Dialer{Timeout: dialTimeout}
		var firstErr error
		for _, ip := range addresses {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if firstErr == nil {
				firstErr = err
			}
		}
		return nil, firstErr
	}
}

// FetchClientMetadataDocument fetches and validates the Client ID Metadata
// Document published at clientID, which must be an absolute HTTPS URL. The
// document's client_id must equal clientID and its redirect URIs must be
// absolute HTTPS URLs.
func (f *ClientMetadataFetcher) FetchClientMetadataDocument(ctx context.Context, clientID string) (ClientMetadataDocument, error) {
	if f == nil {
		return ClientMetadataDocument{}, errors.New("mcpoauth: nil client metadata fetcher")
	}
	if ctx == nil {
		return ClientMetadataDocument{}, errors.New("mcpoauth: nil context")
	}
	target, err := parseHTTPSClientID(clientID)
	if err != nil {
		return ClientMetadataDocument{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return ClientMetadataDocument{}, fmt.Errorf("mcpoauth: build client metadata request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", fetchUserAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return ClientMetadataDocument{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ClientMetadataDocument{}, fmt.Errorf("mcpoauth: client metadata fetch returned status %d", resp.StatusCode)
	}
	contentType := resp.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ClientMetadataDocument{}, fmt.Errorf("%w: malformed content type %q", ErrUnsafeFetch, contentType)
	}
	if mediaType != "application/json" {
		return ClientMetadataDocument{}, fmt.Errorf("%w: content type %q is not application/json", ErrUnsafeFetch, mediaType)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return ClientMetadataDocument{}, fmt.Errorf("mcpoauth: read client metadata: %w", err)
	}
	if int64(len(data)) > f.maxBytes {
		return ClientMetadataDocument{}, fmt.Errorf("%w: document exceeds %d bytes", ErrUnsafeFetch, f.maxBytes)
	}
	var doc ClientMetadataDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return ClientMetadataDocument{}, fmt.Errorf("%w: invalid json: %v", ErrInvalidMetadata, err)
	}
	if doc.ClientID != clientID {
		return ClientMetadataDocument{}, fmt.Errorf("%w: document client_id %q does not match %q", ErrInvalidMetadata, doc.ClientID, clientID)
	}
	if err := ValidateRedirectURIs(doc.RedirectURIs); err != nil {
		return ClientMetadataDocument{}, fmt.Errorf("%w: %v", ErrInvalidMetadata, err)
	}
	return doc, nil
}

// parseHTTPSClientID validates a client_id that doubles as the metadata
// document URL: an absolute HTTPS URL without credentials or fragment.
func parseHTTPSClientID(clientID string) (*url.URL, error) {
	parsed, err := url.Parse(clientID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidClientID, err)
	}
	if parsed.Scheme != "https" || !parsed.IsAbs() || parsed.Host == "" {
		return nil, fmt.Errorf("%w: %q is not an absolute https URL", ErrInvalidClientID, clientID)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: %q must not carry credentials", ErrInvalidClientID, clientID)
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return nil, fmt.Errorf("%w: %q must not carry a fragment", ErrInvalidClientID, clientID)
	}
	return parsed, nil
}
