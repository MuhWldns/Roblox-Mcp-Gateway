package mcpoauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testIssuerURL   = "https://gateway.example.com"
	testResourceURL = "https://gateway.example.com/mcp"
)

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}

func mustAbsoluteHTTPS(t *testing.T, raw, what string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s %q: %v", what, raw, err)
	}
	if parsed.Scheme != "https" || !parsed.IsAbs() || parsed.Host == "" {
		t.Fatalf("%s %q must be an absolute HTTPS URL", what, raw)
	}
	return parsed
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertJSONKeys(t *testing.T, doc any, keys ...string) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	for _, key := range keys {
		if _, ok := parsed[key]; !ok {
			t.Fatalf("document %s lacks key %q", raw, key)
		}
	}
}

func testMetadata(t *testing.T) Metadata {
	t.Helper()
	metadata, err := NewMetadata(mustParseURL(t, testIssuerURL), mustParseURL(t, testResourceURL), SupportedScopes)
	if err != nil {
		t.Fatalf("NewMetadata: %v", err)
	}
	return metadata
}

func TestDiscoveryProtectedResourceMetadata(t *testing.T) {
	metadata := testMetadata(t)
	doc := metadata.ProtectedResource()

	resource := mustAbsoluteHTTPS(t, doc.Resource, "resource")
	if resource.Path != ResourcePath {
		t.Fatalf("resource path = %q, want %q", resource.Path, ResourcePath)
	}
	if doc.Resource != testResourceURL {
		t.Fatalf("resource = %q, want %q", doc.Resource, testResourceURL)
	}

	if len(doc.AuthorizationServers) != 1 {
		t.Fatalf("authorization_servers = %#v, want exactly one entry", doc.AuthorizationServers)
	}
	server := mustAbsoluteHTTPS(t, doc.AuthorizationServers[0], "authorization server")

	serverDoc := metadata.AuthorizationServer()
	if doc.AuthorizationServers[0] != serverDoc.Issuer {
		t.Fatalf("authorization server %q does not match issuer %q", doc.AuthorizationServers[0], serverDoc.Issuer)
	}
	if server.Host != mustParseURL(t, testIssuerURL).Host {
		t.Fatalf("authorization server host = %q, want %q", server.Host, testIssuerURL)
	}

	if !equalStringSlices(doc.ScopesSupported, SupportedScopes) {
		t.Fatalf("scopes_supported = %#v, want %#v", doc.ScopesSupported, SupportedScopes)
	}
	if !equalStringSlices(doc.BearerMethodsSupported, []string{BearerMethodHeader}) {
		t.Fatalf("bearer_methods_supported = %#v, want header only", doc.BearerMethodsSupported)
	}

	assertJSONKeys(t, doc, "resource", "authorization_servers", "scopes_supported")
}

func TestDiscoveryAuthorizationServerMetadata(t *testing.T) {
	metadata := testMetadata(t)
	doc := metadata.AuthorizationServer()

	issuer := mustAbsoluteHTTPS(t, doc.Issuer, "issuer")
	if doc.Issuer != testIssuerURL {
		t.Fatalf("issuer = %q, want %q", doc.Issuer, testIssuerURL)
	}

	endpoints := []struct {
		name string
		raw  string
		want string
	}{
		{"authorization_endpoint", doc.AuthorizationEndpoint, testIssuerURL + AuthorizePath},
		{"token_endpoint", doc.TokenEndpoint, testIssuerURL + TokenPath},
		{"revocation_endpoint", doc.RevocationEndpoint, testIssuerURL + RevocationPath},
		{"registration_endpoint", doc.RegistrationEndpoint, testIssuerURL + RegistrationPath},
	}
	for _, endpoint := range endpoints {
		parsed := mustAbsoluteHTTPS(t, endpoint.raw, endpoint.name)
		if endpoint.raw != endpoint.want {
			t.Fatalf("%s = %q, want %q", endpoint.name, endpoint.raw, endpoint.want)
		}
		if parsed.Host != issuer.Host {
			t.Fatalf("%s host = %q, want issuer host %q", endpoint.name, parsed.Host, issuer.Host)
		}
	}

	if !equalStringSlices(doc.CodeChallengeMethodsSupported, []string{CodeChallengeMethodS256}) {
		t.Fatalf("code_challenge_methods_supported = %#v, want S256 only", doc.CodeChallengeMethodsSupported)
	}
	for _, method := range doc.CodeChallengeMethodsSupported {
		if method == "plain" {
			t.Fatal("plain PKCE code challenge method must never be advertised")
		}
	}
	if !equalStringSlices(doc.ResponseTypesSupported, []string{ResponseTypeCode}) {
		t.Fatalf("response_types_supported = %#v, want [code]", doc.ResponseTypesSupported)
	}
	if !equalStringSlices(doc.GrantTypesSupported, []string{GrantTypeAuthorizationCode, GrantTypeRefreshToken}) {
		t.Fatalf("grant_types_supported = %#v, want authorization_code and refresh_token only", doc.GrantTypesSupported)
	}
	if !equalStringSlices(doc.TokenEndpointAuthMethodsSupported, []string{TokenEndpointAuthNone}) {
		t.Fatalf("token_endpoint_auth_methods_supported = %#v, want [none] for public connectors", doc.TokenEndpointAuthMethodsSupported)
	}
	if !equalStringSlices(doc.ScopesSupported, SupportedScopes) {
		t.Fatalf("scopes_supported = %#v, want %#v", doc.ScopesSupported, SupportedScopes)
	}

	assertJSONKeys(t, doc,
		"issuer",
		"authorization_endpoint",
		"token_endpoint",
		"revocation_endpoint",
		"registration_endpoint",
		"code_challenge_methods_supported",
	)
}

func TestDiscoveryMetadataRejectsUnsafeConfiguration(t *testing.T) {
	issuer := mustParseURL(t, testIssuerURL)
	resource := mustParseURL(t, testResourceURL)

	cases := []struct {
		name     string
		issuer   *url.URL
		resource *url.URL
		scopes   []string
	}{
		{"http issuer", mustParseURL(t, "http://gateway.example.com"), resource, SupportedScopes},
		{"schemeless issuer", mustParseURL(t, "gateway.example.com"), resource, SupportedScopes},
		{"issuer query", mustParseURL(t, "https://gateway.example.com/?next=x"), resource, SupportedScopes},
		{"issuer fragment", mustParseURL(t, "https://gateway.example.com/#fragment"), resource, SupportedScopes},
		{"issuer userinfo", mustParseURL(t, "https://admin:secret@gateway.example.com"), resource, SupportedScopes},
		{"http resource", issuer, mustParseURL(t, "http://gateway.example.com/mcp"), SupportedScopes},
		{"schemeless resource", issuer, mustParseURL(t, "gateway.example.com/mcp"), SupportedScopes},
		{"resource query", issuer, mustParseURL(t, "https://gateway.example.com/mcp?x=1"), SupportedScopes},
		{"resource fragment", issuer, mustParseURL(t, "https://gateway.example.com/mcp#f"), SupportedScopes},
		{"resource userinfo", issuer, mustParseURL(t, "https://admin@gateway.example.com/mcp"), SupportedScopes},
		{"no scopes", issuer, resource, nil},
		{"blank scope", issuer, resource, []string{ScopeConnect, " "}},
		{"empty scope entry", issuer, resource, []string{""}},
		{"duplicate scopes", issuer, resource, []string{ScopeConnect, ScopeConnect}},
	}
	for _, testCase := range cases {
		if _, err := NewMetadata(testCase.issuer, testCase.resource, testCase.scopes); err == nil {
			t.Fatalf("%s: NewMetadata accepted unsafe configuration", testCase.name)
		}
	}
	if _, err := NewMetadata(nil, resource, SupportedScopes); err == nil {
		t.Fatal("NewMetadata accepted a nil issuer")
	}
	if _, err := NewMetadata(issuer, nil, SupportedScopes); err == nil {
		t.Fatal("NewMetadata accepted a nil resource")
	}
}

func TestDiscoveryPublicIPPolicyRejectsPrivateAddresses(t *testing.T) {
	private := []string{
		"127.0.0.1", "::1", "0.0.0.0", "::",
		"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "fe80::1", "ff02::1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1",
	}
	for _, raw := range private {
		if publicIPPolicy(net.ParseIP(raw)) {
			t.Fatalf("address policy accepted private address %s", raw)
		}
	}
	public := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"}
	for _, raw := range public {
		if !publicIPPolicy(net.ParseIP(raw)) {
			t.Fatalf("address policy rejected public address %s", raw)
		}
	}
}

// allowAllIPs is a test-only address policy for local loopback servers.
func allowAllIPs(net.IP) bool { return true }

// testFetcherServer starts a local TLS server and returns a fetcher whose
// address policy and TLS verification are relaxed for the loopback test
// server. Production fetchers never disable certificate verification.
func testFetcherServer(t *testing.T, build func(*httptest.Server) http.HandlerFunc) (*ClientMetadataFetcher, *httptest.Server) {
	t.Helper()
	var handler http.HandlerFunc
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler == nil {
			http.Error(w, "handler not installed", http.StatusInternalServerError)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	handler = build(server)
	fetcher := newClientMetadataFetcher(5*time.Second, 4096, 3, allowAllIPs, &tls.Config{InsecureSkipVerify: true})
	return fetcher, server
}

func TestDiscoveryClientMetadataFetcherRequiresHTTPSClientID(t *testing.T) {
	fetcher := NewClientMetadataFetcher(time.Second, 4096, 3)
	clientIDs := []string{
		"",
		"chatgpt.com",
		"https://",
		"http://chatgpt.com/.well-known/oauth-client",
		"ftp://chatgpt.com/.well-known/oauth-client",
		"/relative/path",
		"https://chatgpt.com/.well-known/oauth-client#fragment",
		"https://user:pass@chatgpt.com/.well-known/oauth-client",
	}
	for _, clientID := range clientIDs {
		doc, err := fetcher.FetchClientMetadataDocument(context.Background(), clientID)
		if err == nil {
			t.Fatalf("client id %q was accepted: %+v", clientID, doc)
		}
		if !errors.Is(err, ErrInvalidClientID) {
			t.Fatalf("client id %q: want ErrInvalidClientID, got %v", clientID, err)
		}
	}
}

func TestDiscoveryClientMetadataFetcherRejectsPrivateTargets(t *testing.T) {
	fetcher := NewClientMetadataFetcher(10*time.Second, 4096, 3)
	private := []string{
		"https://127.0.0.1/.well-known/oauth-client",
		"https://127.0.0.1:8443/.well-known/oauth-client",
		"https://[::1]/.well-known/oauth-client",
		"https://10.20.30.40/.well-known/oauth-client",
		"https://172.16.1.2/.well-known/oauth-client",
		"https://192.168.100.100/.well-known/oauth-client",
		"https://169.254.169.254/.well-known/oauth-client",
		"https://[fe80::1]/.well-known/oauth-client",
		"https://[::ffff:127.0.0.1]/.well-known/oauth-client",
		"https://0.0.0.0/.well-known/oauth-client",
	}
	for _, clientID := range private {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		doc, err := fetcher.FetchClientMetadataDocument(ctx, clientID)
		cancel()
		if err == nil {
			t.Fatalf("private target %q was fetched: %+v", clientID, doc)
		}
		if !errors.Is(err, ErrUnsafeFetch) {
			t.Fatalf("private target %q: want ErrUnsafeFetch, got %v", clientID, err)
		}
	}

	// A hostname resolving to loopback is rejected by the same dial guard.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := fetcher.FetchClientMetadataDocument(ctx, "https://localhost/.well-known/oauth-client"); err == nil {
		t.Fatal("loopback hostname was fetched")
	}
}

func TestDiscoveryClientMetadataFetcherParsesDocument(t *testing.T) {
	fetcher, server := testFetcherServer(t, func(s *httptest.Server) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"client_id":%q,"client_name":"ChatGPT","redirect_uris":["https://chatgpt.com/aip/oauth/callback"],"scope":"mcp:connect studio:read"}`, s.URL+"/.well-known/oauth-client")
		}
	})
	clientID := server.URL + "/.well-known/oauth-client"

	doc, err := fetcher.FetchClientMetadataDocument(context.Background(), clientID)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if doc.ClientID != clientID {
		t.Fatalf("client_id = %q, want %q", doc.ClientID, clientID)
	}
	if doc.ClientName != "ChatGPT" {
		t.Fatalf("client_name = %q", doc.ClientName)
	}
	if !equalStringSlices(doc.RedirectURIs, []string{"https://chatgpt.com/aip/oauth/callback"}) {
		t.Fatalf("redirect_uris = %#v", doc.RedirectURIs)
	}
	if doc.Scope != "mcp:connect studio:read" {
		t.Fatalf("scope = %q", doc.Scope)
	}
}

func TestDiscoveryClientMetadataFetcherEnforcesResponsePolicy(t *testing.T) {
	cases := []struct {
		name      string
		handler   http.HandlerFunc
		wantError error
	}{
		{
			name: "wrong content type",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				_, _ = w.Write([]byte(`{"client_id":"x"}`))
			},
			wantError: ErrUnsafeFetch,
		},
		{
			name: "malformed content type",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", ";;;")
				_, _ = w.Write([]byte(`{"client_id":"x"}`))
			},
			wantError: ErrUnsafeFetch,
		},
		{
			name: "oversized document",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"padding":"` + strings.Repeat("x", 8192) + `"}`))
			},
			wantError: ErrUnsafeFetch,
		},
		{
			name: "http error status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "gone", http.StatusNotFound)
			},
			wantError: nil, // any error
		},
	}
	for _, testCase := range cases {
		fetcher, server := testFetcherServer(t, func(*httptest.Server) http.HandlerFunc {
			return testCase.handler
		})
		clientID := server.URL + "/.well-known/oauth-client"
		doc, err := fetcher.FetchClientMetadataDocument(context.Background(), clientID)
		if err == nil {
			t.Fatalf("%s: fetch succeeded: %+v", testCase.name, doc)
		}
		if testCase.wantError != nil && !errors.Is(err, testCase.wantError) {
			t.Fatalf("%s: want %v, got %v", testCase.name, testCase.wantError, err)
		}
	}
}

func TestDiscoveryClientMetadataFetcherControlsRedirects(t *testing.T) {
	t.Run("FollowsLimitedHTTPSRedirects", func(t *testing.T) {
		fetcher, server := testFetcherServer(t, func(s *httptest.Server) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/start" {
					http.Redirect(w, r, s.URL+"/doc", http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"client_id":%q,"redirect_uris":["https://chatgpt.com/aip/oauth/callback"]}`, s.URL+"/start")
			}
		})
		clientID := server.URL + "/start"
		doc, err := fetcher.FetchClientMetadataDocument(context.Background(), clientID)
		if err != nil {
			t.Fatalf("fetch through redirect: %v", err)
		}
		if doc.ClientID != clientID {
			t.Fatalf("client_id = %q, want %q", doc.ClientID, clientID)
		}
	})

	t.Run("RejectsSchemeDowngrade", func(t *testing.T) {
		fetcher, server := testFetcherServer(t, func(s *httptest.Server) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, strings.Replace(s.URL, "https://", "http://", 1)+"/down", http.StatusFound)
			}
		})
		_, err := fetcher.FetchClientMetadataDocument(context.Background(), server.URL+"/start")
		if !errors.Is(err, ErrUnsafeFetch) {
			t.Fatalf("want ErrUnsafeFetch for redirect downgrade, got %v", err)
		}
	})

	t.Run("RejectsExcessiveRedirects", func(t *testing.T) {
		fetcher, server := testFetcherServer(t, func(s *httptest.Server) http.HandlerFunc {
			return func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, s.URL+"/start", http.StatusFound)
			}
		})
		_, err := fetcher.FetchClientMetadataDocument(context.Background(), server.URL+"/start")
		if !errors.Is(err, ErrUnsafeFetch) {
			t.Fatalf("want ErrUnsafeFetch for redirect loop, got %v", err)
		}
	})
}

func TestDiscoveryClientMetadataFetcherValidatesDocument(t *testing.T) {
	// Every fetch in this test requests the same conventional client id, so
	// handlers can build the expected document body from the server URL.
	const clientIDPath = "/.well-known/oauth-client"
	cases := []struct {
		name    string
		body    func(clientID string) string
		wantErr error
	}{
		{
			name: "mismatched client id",
			body: func(string) string {
				return `{"client_id":"https://chatgpt.com/other","redirect_uris":["https://chatgpt.com/aip/oauth/callback"]}`
			},
			wantErr: ErrInvalidMetadata,
		},
		{
			name: "missing redirect uris",
			body: func(clientID string) string {
				return fmt.Sprintf(`{"client_id":%q}`, clientID)
			},
			wantErr: ErrInvalidMetadata,
		},
		{
			name: "insecure redirect uri",
			body: func(clientID string) string {
				return fmt.Sprintf(`{"client_id":%q,"redirect_uris":["http://chatgpt.com/cb"]}`, clientID)
			},
			wantErr: ErrInvalidMetadata,
		},
		{
			name:    "malformed json",
			body:    func(string) string { return `{"client_id":` },
			wantErr: ErrInvalidMetadata,
		},
	}
	for _, testCase := range cases {
		fetcher, server := testFetcherServer(t, func(s *httptest.Server) http.HandlerFunc {
			clientID := s.URL + clientIDPath
			return func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(testCase.body(clientID)))
			}
		})
		doc, err := fetcher.FetchClientMetadataDocument(context.Background(), server.URL+clientIDPath)
		if err == nil {
			t.Fatalf("%s: invalid document accepted: %+v", testCase.name, doc)
		}
		if !errors.Is(err, testCase.wantErr) {
			t.Fatalf("%s: want %v, got %v", testCase.name, testCase.wantErr, err)
		}
	}
}
