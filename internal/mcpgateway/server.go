// Package mcpgateway exposes the remote MCP gateway. This file composes the
// authenticated Streamable HTTP transport: origin and bearer admission,
// entitlement and rate enforcement ahead of the SDK handler, per-session
// servers bound to the authenticated principal, and the inbound device
// envelope path shared with the Bridge hub.
package mcpgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"robloxkit/internal/audit"
	"robloxkit/internal/bridgehub"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/httpserver"
	"robloxkit/internal/mcpoauth"
	"robloxkit/pkg/bridgeproto"
)

// Configuration defaults for the gateway.
const (
	defaultRequestTimeout  = 30 * time.Second
	defaultSessionTimeout  = time.Hour
	defaultMaxRequestBytes = 1 << 20
	defaultMaxPending      = 1024
)

// ErrInvalidConfig indicates the gateway was assembled from incomplete
// parts.
var ErrInvalidConfig = errors.New("mcpgateway: invalid gateway configuration")

// Config wires the gateway to the assembled application parts.
type Config struct {
	// OAuth resolves connector access tokens into their grants.
	OAuth mcpoauth.Store
	// Store reads identities, device ownership, and device bindings for
	// the per-call re-authorization pipeline.
	Store bridgehub.Store
	// Entitlements evaluates the frozen trial/license window.
	Entitlements *entitlement.Service
	// Audit records every synchronous denial event.
	Audit *audit.Service
	// Registry is the hub's live device connection registry.
	Registry *bridgehub.Registry
	// Pending correlates relayed requests with device responses; nil
	// selects a gateway-owned registry bounded by defaultMaxPending.
	Pending *Pending
	// Limiter bounds request rates and concurrent tool calls per grant
	// and per user before Bridge delivery.
	Limiter *httpserver.MCPLimiter
	// Pepper keys connector access-token digests; it must equal the
	// authorization server's pepper.
	Pepper []byte
	// Resource is the HTTPS resource URL this gateway serves
	// (origin + mcpoauth.ResourcePath).
	Resource string
	// AllowedOrigins authorizes browser origins; requests carrying any
	// other Origin are rejected. An empty list denies all browsers —
	// connectors, which are not browsers, are unaffected.
	AllowedOrigins []string
	// Implementation is the serverInfo advertised at initialize.
	Implementation mcp.Implementation
	// RequestTimeout bounds one relayed tool call.
	RequestTimeout time.Duration
	// SessionTimeout closes idle MCP sessions.
	SessionTimeout time.Duration
	// MaxRequestBytes bounds /mcp request bodies.
	MaxRequestBytes int64
	// Now supplies the authorization clock; nil defaults to time.Now.
	Now func() time.Time
}

// Gateway is the authenticated MCP Streamable HTTP endpoint.
type Gateway struct {
	cfg           Config
	authenticator *authenticator
	relay         *Relay
	policy        Policy
	origins       map[string]bool
	metadataURL   string
	sdk           *mcp.StreamableHTTPHandler
}

// NewGateway validates the configuration and composes the transport.
func NewGateway(cfg Config) (*Gateway, error) {
	var invalid []string
	if cfg.OAuth == nil {
		invalid = append(invalid, "oauth store is required")
	}
	if cfg.Store == nil {
		invalid = append(invalid, "device store is required")
	}
	if cfg.Entitlements == nil {
		invalid = append(invalid, "entitlement service is required")
	}
	if cfg.Audit == nil {
		invalid = append(invalid, "audit service is required")
	}
	if cfg.Registry == nil {
		invalid = append(invalid, "device registry is required")
	}
	if cfg.Limiter == nil {
		invalid = append(invalid, "rate limiter is required")
	}
	if len(cfg.Pepper) == 0 {
		invalid = append(invalid, "token pepper is required")
	}
	if err := mcpoauth.ValidateResourceURL(cfg.Resource); err != nil {
		invalid = append(invalid, "resource must be the gateway's https /mcp URL")
	} else if resourceURL, err := url.Parse(cfg.Resource); err != nil || resourceURL.Path != mcpoauth.ResourcePath {
		invalid = append(invalid, fmt.Sprintf("resource path must be %q", mcpoauth.ResourcePath))
	}
	if cfg.Implementation.Name == "" || cfg.Implementation.Version == "" {
		invalid = append(invalid, "implementation name and version are required")
	}
	if cfg.RequestTimeout < 0 {
		invalid = append(invalid, "request timeout must not be negative")
	}
	if cfg.SessionTimeout < 0 {
		invalid = append(invalid, "session timeout must not be negative")
	}
	if cfg.MaxRequestBytes < 0 {
		invalid = append(invalid, "max request bytes must not be negative")
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalidConfig, strings.Join(invalid, "; "))
	}

	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	if cfg.SessionTimeout == 0 {
		cfg.SessionTimeout = defaultSessionTimeout
	}
	if cfg.MaxRequestBytes == 0 {
		cfg.MaxRequestBytes = defaultMaxRequestBytes
	}
	if cfg.Pending == nil {
		cfg.Pending = NewPending(defaultMaxPending)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	origins := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, origin := range cfg.AllowedOrigins {
		parsed, err := url.Parse(origin)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
			(parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
			return nil, fmt.Errorf("%w: origin %q must be an absolute http(s) URL", ErrInvalidConfig, origin)
		}
		origins[origin] = true
	}

	resourceURL, err := url.Parse(cfg.Resource)
	if err != nil {
		return nil, fmt.Errorf("%w: parse resource: %v", ErrInvalidConfig, err)
	}
	metadataURL, err := mcpoauth.ProtectedResourceMetadataURL(resourceURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	relay, err := NewRelay(RelayConfig{
		Registry:         cfg.Registry,
		Pending:          cfg.Pending,
		Timeout:          cfg.RequestTimeout,
		MaxEnvelopeBytes: defaultMaxEnvelopeBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}

	gateway := &Gateway{
		cfg: cfg,
		authenticator: &authenticator{
			oauth:    cfg.OAuth,
			pepper:   cfg.Pepper,
			resource: cfg.Resource,
			audit:    cfg.Audit,
			now:      cfg.Now,
		},
		relay:       relay,
		policy:      Policy{},
		origins:     origins,
		metadataURL: metadataURL,
	}
	gateway.sdk = mcp.NewStreamableHTTPHandler(gateway.newSessionServer, &mcp.StreamableHTTPOptions{
		SessionTimeout:      cfg.SessionTimeout,
		MaxRequestBodyBytes: cfg.MaxRequestBytes,
	})
	return gateway, nil
}

// Handler composes the /mcp admission chain. From the outside in: the
// correlation id seeds audit trails; the origin gate rejects browsers from
// foreign origins; the bearer-presence check audits missing credentials;
// the SDK bearer middleware verifies tokens and pins sessions to their
// user; admission re-checks entitlement and enforces the grant and user
// rate and in-flight bounds; and the SDK handler runs the Streamable HTTP
// session state machine.
func (g *Gateway) Handler() http.Handler {
	bearerAuth := auth.RequireBearerToken(g.authenticator.verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: g.metadataURL,
	})
	authenticated := bearerAuth(g.admission(g.sdk))
	return g.withCorrelationHeader(g.originGate(g.bearerPresence(authenticated)))
}

// HandleEnvelope is the hub OnEnvelope hook: responses resolve their
// correlated relayed requests and status snapshots update device routing
// state.
func (g *Gateway) HandleEnvelope(ctx context.Context, device bridgehub.Device, env bridgeproto.Envelope) {
	g.relay.HandleEnvelope(ctx, device, env)
}

// CancelSession completes every pending request of an MCP session; the
// connector revocation path calls it to stop a session's in-flight work.
func (g *Gateway) CancelSession(sessionID string) {
	g.relay.CancelSession(sessionID)
}

// newSessionServer builds the per-session MCP server bound to the
// authenticated principal of the request that opens the session. Sessions
// retain only the token's keyed digest; every relayed call re-authorizes
// against it.
func (g *Gateway) newSessionServer(r *http.Request) *mcp.Server {
	tokenInfo := auth.TokenInfoFromContext(r.Context())
	if tokenInfo == nil {
		return nil
	}
	digestHex, _ := tokenInfo.Extra[extraTokenDigest].(string)
	digest, err := decodeDigest(digestHex)
	if err != nil {
		return nil
	}
	impl := g.cfg.Implementation
	server := mcp.NewServer(&impl, &mcp.ServerOptions{
		// The relayed tool catalog is dynamic; the gateway advertises the
		// tools capability without change notifications.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		InitializedHandler: func(_ context.Context, req *mcp.InitializedRequest) {
			go func() {
				// Session teardown retires the session's in-flight
				// correlations.
				_ = req.Session.Wait()
				g.relay.CancelSession(req.Session.ID())
			}()
		},
	})
	server.AddReceivingMiddleware(g.sessionMiddleware(digest))
	return server
}

// withCorrelationHeader seeds the audit correlation id for one request.
func (g *Gateway) withCorrelationHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(withCorrelation(r.Context())))
	})
}

// originGate enforces the browser-origin allowlist (MCP transport security
// guidance): non-browser clients send no Origin and pass; a foreign Origin
// is rejected and audited. Allowlisted origins receive CORS headers and
// preflight responses.
func (g *Gateway) originGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Origin")
		if !g.origins[origin] {
			recordDenial(r.Context(), g.cfg.Audit, auditReasonOrigin, "", "")
			writeDenied(w, http.StatusForbidden, "origin not allowed")
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers",
				"Authorization, Content-Type, Accept, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID, Mcp-Method, Mcp-Name")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerPresence audits the missing-credential rejection that the SDK
// bearer middleware would otherwise answer without an audit trail.
func (g *Gateway) bearerPresence(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hasBearerToken(r.Header.Get("Authorization")) {
			recordDenial(r.Context(), g.cfg.Audit, auditReasonMissingBearer, "", "")
			w.Header().Set("WWW-Authenticate", g.challengeHeader())
			writeDenied(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// challengeHeader is the RFC 9728 challenge pointing at the gateway's
// protected-resource metadata document, matching the SDK bearer
// middleware's format.
func (g *Gateway) challengeHeader() string {
	return fmt.Sprintf("Bearer resource_metadata=%q", g.metadataURL)
}

// admission re-checks the entitlement window on every request and enforces
// the limiter on both key namespaces — connector grant and user — before
// the request can reach the SDK and therefore before Bridge delivery.
// tools/call POSTs additionally reserve concurrent in-flight slots for the
// whole HTTP request lifecycle.
func (g *Gateway) admission(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		tokenInfo := auth.TokenInfoFromContext(ctx)
		if tokenInfo == nil {
			// The SDK bearer middleware guarantees a token on every
			// request that reaches here; treat its absence as tampering.
			w.Header().Set("WWW-Authenticate", g.challengeHeader())
			writeDenied(w, http.StatusUnauthorized, "authentication required")
			return
		}
		grantID, _ := tokenInfo.Extra[extraGrantID].(string)

		if err := g.entitle(ctx, tokenInfo.UserID); err != nil {
			recordDenial(ctx, g.cfg.Audit, auditReasonEntitlement, tokenInfo.UserID, grantID)
			writeDenied(w, http.StatusForbidden, "access denied")
			return
		}

		if !g.cfg.Limiter.Allow(grantRateKey(grantID)) || !g.cfg.Limiter.Allow(userRateKey(tokenInfo.UserID)) {
			recordDenial(ctx, g.cfg.Audit, auditReasonRateLimited, tokenInfo.UserID, grantID)
			writeRateLimited(w, g.cfg.Limiter.Window())
			return
		}

		if r.Method == http.MethodPost {
			release, proceed := g.reserveInFlight(w, r, grantID, tokenInfo.UserID)
			if !proceed {
				return
			}
			if release != nil {
				defer release()
			}
		}

		next.ServeHTTP(w, r)
	})
}

// reserveInFlight parses the POST body far enough to recognize a tools/call
// request, restores the body, and reserves that request's concurrent
// in-flight slots on both key namespaces. The peek is bounded by the
// configured request limit; oversized bodies are rejected here. It returns
// the release function (nil when no slot was needed) and whether the
// request may proceed.
func (g *Gateway) reserveInFlight(w http.ResponseWriter, r *http.Request, grantID, userID string) (func(), bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, g.cfg.MaxRequestBytes+1))
	if err != nil {
		// A transport-level read failure: hand the SDK the same failure
		// so it produces its own sanitized answer.
		r.Body = io.NopCloser(errBodyReader{err: err})
		return nil, true
	}
	if int64(len(body)) > g.cfg.MaxRequestBytes {
		writeDenied(w, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var peek struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &peek) // an unparseable body is the SDK's to answer
	if peek.Method != methodCallTool {
		return nil, true
	}

	grantRelease, ok := g.cfg.Limiter.Acquire(grantRateKey(grantID))
	if !ok {
		recordDenial(r.Context(), g.cfg.Audit, auditReasonRateLimited, userID, grantID)
		writeRateLimited(w, g.cfg.Limiter.Window())
		return nil, false
	}
	userRelease, ok := g.cfg.Limiter.Acquire(userRateKey(userID))
	if !ok {
		grantRelease()
		recordDenial(r.Context(), g.cfg.Audit, auditReasonRateLimited, userID, grantID)
		writeRateLimited(w, g.cfg.Limiter.Window())
		return nil, false
	}
	return func() {
		grantRelease()
		userRelease()
	}, true
}

// errBodyReader replays a failed body read to the SDK without panicking on
// a nil body.
type errBodyReader struct {
	err  error
	body io.ReadCloser
}

func (e errBodyReader) Read(p []byte) (int, error) { return 0, e.err }

func (e errBodyReader) Close() error {
	if e.body != nil {
		return e.body.Close()
	}
	return nil
}

// grantRateKey and userRateKey are the limiter's two key namespaces.
func grantRateKey(grantID string) string { return "grant:" + grantID }
func userRateKey(userID string) string   { return "user:" + userID }

// writeDenied answers with a sanitized JSON error body.
func writeDenied(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// writeRateLimited answers with the sanitized 429 and a Retry-After of one
// limiter window.
func writeRateLimited(w http.ResponseWriter, window time.Duration) {
	w.Header().Set("Content-Type", "application/json")
	if window > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int((window+time.Second-1)/time.Second)))
	}
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "rate limit exceeded, retry later"})
}
