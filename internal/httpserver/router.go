// Package httpserver composes the public gateway routes: Roblox login and
// logout, the authenticated Bridge download, the device enrollment approval
// flow, the session-owned dashboard API, the liveness and readiness probes,
// and the OAuth discovery documents — under an exact-origin CORS policy and
// the shared security middleware.
//
// The bearer-authenticated /mcp endpoint and the device-authenticated
// /bridge endpoint are mounted, when configured, outside the session and
// CSRF middleware: browser cookie policy never gates them.
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"robloxkit/internal/dashboard"
	"robloxkit/internal/device"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/health"
	"robloxkit/internal/mcpoauth"
	"robloxkit/internal/robloxauth"
	"robloxkit/internal/session"
)

// ErrInvalidConfig indicates the router was assembled from incomplete parts.
var ErrInvalidConfig = errors.New("httpserver: invalid router configuration")

// timeFormat serializes timestamps for browser clients.
const timeFormat = "2006-01-02T15:04:05Z07:00"

// IdentityReader reads the browser-visible Roblox identity of a user.
type IdentityReader interface {
	RobloxIdentity(ctx context.Context, userID string) (device.RobloxIdentity, error)
}

// Entitlements evaluates the entitlement window of a subject.
type Entitlements interface {
	Authorize(ctx context.Context, subject entitlement.Subject) (entitlement.Decision, error)
}

// Config wires the router to the assembled application parts.
type Config struct {
	Sessions         *session.Service
	RobloxAuth       *robloxauth.Handler
	IdentityReader   IdentityReader
	Entitlements     Entitlements
	Download         *device.DownloadHandler
	DownloadMetadata *device.DownloadMetadataHandler
	Enrollment       *device.Enrollment

	// Dashboard backs the devices, studios, connectors, license, and
	// diagnostics reads and the self-service mutations.
	Dashboard dashboard.Store

	// Registry exposes live Bridge presence; nil reports every device
	// offline and skips the disconnect on device revocation.
	Registry BridgeRegistry

	// Health serves /healthz and /readyz.
	Health *health.Handler

	// Metadata publishes the OAuth discovery documents. It must come from
	// mcpoauth.NewMetadata; the served well-known paths are derived from
	// it so the /mcp challenge, the metadata location, and the issuer
	// always agree.
	Metadata *mcpoauth.Metadata

	// MCP optionally mounts the bearer-authenticated MCP Streamable HTTP
	// endpoint. It stays outside the session and CSRF middleware.
	MCP http.Handler

	// OAuth optionally mounts the connector authorization-server endpoints
	// (authorize, token, revocation) — typically mcpoauth.Provider.Handler.
	// It stays outside the session and CSRF middleware.
	OAuth http.Handler

	// Bridge optionally mounts the device-authenticated Bridge WebSocket
	// endpoint. It also stays outside the session and CSRF middleware.
	Bridge http.Handler

	// MaxBodyBytes bounds /api/ request bodies; zero selects
	// DefaultMaxBodyBytes. The /mcp and /bridge stacks bound their own
	// bodies and are never wrapped by this limit.
	MaxBodyBytes int64

	// Admin optionally mounts the privileged administration surface: the
	// audited device transfer, identity recovery, and trial extension.
	// The endpoints live inside the session and CSRF subtree; every
	// request is additionally gated on the configured admin user ids.
	Admin *AdminConfig

	// Limits optionally enforces per-class endpoint rate limits over the
	// mounted login, OAuth, enrollment, Bridge dial, admin execute, and
	// MCP endpoints. Health probes, the discovery documents, and static
	// assets are never limited. A nil limiter leaves every endpoint
	// unlimited; production composition should always supply one.
	Limits *Limiter

	AllowedOrigin *url.URL
	StaticDir     string
}

type userIDKeyType struct{}

var userIDKey userIDKeyType

// NewRouter validates the configuration and composes every public route.
func NewRouter(cfg Config) (http.Handler, error) {
	invalid := []string{}
	if cfg.Sessions == nil {
		invalid = append(invalid, "sessions service is required")
	}
	if cfg.RobloxAuth == nil {
		invalid = append(invalid, "roblox auth handler is required")
	}
	if cfg.IdentityReader == nil {
		invalid = append(invalid, "identity reader is required")
	}
	if cfg.Entitlements == nil {
		invalid = append(invalid, "entitlements service is required")
	}
	if cfg.Download == nil {
		invalid = append(invalid, "download handler is required")
	}
	if cfg.DownloadMetadata == nil {
		invalid = append(invalid, "download metadata handler is required")
	}
	if cfg.Enrollment == nil {
		invalid = append(invalid, "enrollment service is required")
	}
	if cfg.Dashboard == nil {
		invalid = append(invalid, "dashboard store is required")
	}
	if cfg.Health == nil {
		invalid = append(invalid, "health handler is required")
	}
	if cfg.Metadata == nil {
		invalid = append(invalid, "oauth metadata is required")
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalidConfig, strings.Join(invalid, "; "))
	}
	// The administration surface composes the frozen entitlement service and
	// the connector OAuth store; a half-configured AdminConfig is invalid
	// rather than silently unaudited.
	var admin *adminAPI
	if cfg.Admin != nil {
		if cfg.Admin.Entitlements == nil {
			invalid = append(invalid, "admin entitlement service is required")
		}
		if cfg.Admin.OAuth == nil {
			invalid = append(invalid, "admin oauth store is required")
		}
		if len(invalid) > 0 {
			return nil, fmt.Errorf("%w: %s", ErrInvalidConfig, strings.Join(invalid, "; "))
		}
		admin = newAdminAPI(cfg)
	}

	// Endpoint rate limits: each class wraps only its own endpoints, keyed
	// by the session user where one is already established and by the
	// remote host for unauthenticated endpoints. A nil limiter is a
	// pass-through at every mount.
	limit := func(class Class, keyOf func(*http.Request) string, next http.Handler) http.Handler {
		return cfg.Limits.Middleware(class, keyOf)(next)
	}
	csrf := NewCSRF()
	lookup := &device.EnrollmentLookupHandler{Sessions: cfg.Sessions, Enrollment: cfg.Enrollment}
	approve := &device.EnrollmentApproveHandler{Sessions: cfg.Sessions, Enrollment: cfg.Enrollment}
	begin := &device.EnrollmentBeginHandler{Enrollment: cfg.Enrollment}
	exchange := &device.EnrollmentExchangeHandler{Enrollment: cfg.Enrollment}
	logout := requireSession(cfg.Sessions, &logoutHandler{sessions: cfg.Sessions})
	me := requireSession(cfg.Sessions, &meHandler{identities: cfg.IdentityReader, entitlement: cfg.Entitlements})
	dashboard := &dashboardAPI{
		store:        cfg.Dashboard,
		sessions:     cfg.Sessions,
		identities:   cfg.IdentityReader,
		entitlements: cfg.Entitlements,
		registry:     cfg.Registry,
	}

	// Browser API subtree. The CSRF double-submit guard wraps the whole
	api := http.NewServeMux()
	api.Handle("/api/v1/auth/roblox/login", limit(ClassLogin, RemotePrincipal, http.HandlerFunc(cfg.RobloxAuth.Begin)))
	api.Handle("/api/v1/auth/roblox/callback", limit(ClassLogin, RemotePrincipal, http.HandlerFunc(cfg.RobloxAuth.Callback)))
	api.Handle("/api/v1/auth/logout", logout)
	api.Handle("/api/v1/me", me)
	api.Handle("/api/v1/csrf", requireSession(cfg.Sessions, http.HandlerFunc(csrf.Issue)))
	api.Handle("/api/v1/bridge/download/metadata", cfg.DownloadMetadata)
	api.Handle("/api/v1/bridge/download", cfg.Download)
	api.Handle("/api/v1/enrollments/claim", limit(ClassEnroll, RemotePrincipal, lookup))
	api.Handle("/api/v1/enrollments/approve", limit(ClassEnroll, RemotePrincipal, approve))
	api.Handle("/api/v1/device/enrollment/begin", limit(ClassEnroll, RemotePrincipal, begin))
	api.Handle("/api/v1/device/enrollment/exchange", limit(ClassEnroll, RemotePrincipal, exchange))
	// Dashboard routes are session-bound: the session middleware validates
	// the cookie and injects the user id every handler reads.
	sessionBound := func(handler http.HandlerFunc) http.Handler {
		return requireSession(cfg.Sessions, handler)
	}
	api.Handle("GET /api/v1/devices", sessionBound(dashboard.devices))
	api.Handle("POST /api/v1/devices/{device_id}/rename", sessionBound(dashboard.renameDevice))
	api.Handle("POST /api/v1/devices/{device_id}/revoke", sessionBound(dashboard.revokeDevice))
	api.Handle("POST /api/v1/devices/{device_id}/rotate-credential", sessionBound(dashboard.rotateDeviceCredential))
	api.Handle("GET /api/v1/studios", sessionBound(dashboard.studios))
	api.Handle("GET /api/v1/connectors", sessionBound(dashboard.connectors))
	api.Handle("POST /api/v1/connectors/{grant_id}/target", sessionBound(dashboard.setConnectorTarget))
	api.Handle("POST /api/v1/connectors/{grant_id}/revoke", sessionBound(dashboard.revokeConnector))
	api.Handle("GET /api/v1/license", sessionBound(dashboard.license))
	api.Handle("GET /api/v1/diagnostics", sessionBound(dashboard.diagnostics))
	api.Handle("POST /api/v1/sessions/revoke-all", sessionBound(dashboard.revokeAllSessions))
	// Administration endpoints: session-bound, CSRF-protected, and gated on
	// the configured administrator set. Reads preview the committed state
	// and mint version tokens; mutations verify them before executing.
	if admin != nil {
		adminBound := func(handler http.HandlerFunc) http.Handler {
			// The execute endpoints sit behind the session and the
			// administrator gate, so the class key is the authenticated
			// admin's user id: one admin's exhaustion never throttles
			// another's.
			limited := limit(ClassAdmin, SessionPrincipal, http.HandlerFunc(handler))
			return requireSession(cfg.Sessions, admin.authorized(limited.ServeHTTP))
		}
		api.Handle("GET /api/v1/admin/users/{user_id}/transfer-preview", adminBound(admin.transferPreview))
		api.Handle("GET /api/v1/admin/users/{user_id}/recovery-preview", adminBound(admin.recoveryPreview))
		api.Handle("GET /api/v1/admin/users/{user_id}/trial-preview", adminBound(admin.trialPreview))
		api.Handle("POST /api/v1/admin/transfers", adminBound(admin.transfer))
		api.Handle("POST /api/v1/admin/recoveries", adminBound(admin.recover))
		api.Handle("POST /api/v1/admin/trial-extensions", adminBound(admin.extend))
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", limitBody(bodyLimit(cfg))(csrf.Require(api)))
	if cfg.MCP != nil {
		// The gateway enforces its own per-grant and per-user limits
		// inside; this outer bound is the coarse per-peer brake.
		mux.Handle("/mcp", limit(ClassMCP, RemotePrincipal, cfg.MCP))
	}
	if cfg.Bridge != nil {
		// The dial is authenticated by the credential header after the
		// limiter, so the key is the remote host.
		mux.Handle("/bridge", limit(ClassWSS, RemotePrincipal, cfg.Bridge))
	}
	if cfg.OAuth != nil {
		mux.Handle("/oauth/", limit(ClassOAuth, RemotePrincipal, cfg.OAuth))
	}
	mux.HandleFunc("GET /healthz", cfg.Health.Live)
	mux.HandleFunc("GET /readyz", cfg.Health.Ready)
	if err := mountMetadata(mux, *cfg.Metadata); err != nil {
		return nil, err
	}
	if cfg.StaticDir != "" {
		mux.Handle("/", spaHandler(cfg.StaticDir))
	}

	// From the outside in: the exact-origin CORS policy first so allowed
	// preflights and CORS headers apply even to sanitized panic responses;
	// then panic recovery, request id assignment and echo, and the fixed
	// security headers around every route.
	return exactOrigin(cfg.AllowedOrigin)(RecoverPanics(requestID(secureHeaders(mux)))), nil
}

// bodyLimit resolves the configured /api/ body bound.
func bodyLimit(cfg Config) int64 {
	if cfg.MaxBodyBytes > 0 {
		return cfg.MaxBodyBytes
	}
	return DefaultMaxBodyBytes
}

// mountMetadata registers the two OAuth discovery documents at the well-known
// locations derived from the published issuer and resource, guaranteeing the
// /mcp challenge, the document location, and the issuer always agree.
func mountMetadata(mux *http.ServeMux, meta mcpoauth.Metadata) error {
	resource, err := url.Parse(meta.ProtectedResource().Resource)
	if err != nil {
		return fmt.Errorf("%w: parse protected resource: %v", ErrInvalidConfig, err)
	}
	protectedURL, err := mcpoauth.ProtectedResourceMetadataURL(resource)
	if err != nil {
		return fmt.Errorf("%w: derive protected-resource location: %v", ErrInvalidConfig, err)
	}
	protected, err := url.Parse(protectedURL)
	if err != nil {
		return fmt.Errorf("%w: parse protected-resource location: %v", ErrInvalidConfig, err)
	}
	issuer, err := url.Parse(meta.AuthorizationServer().Issuer)
	if err != nil {
		return fmt.Errorf("%w: parse issuer: %v", ErrInvalidConfig, err)
	}
	authorizationServerURL, err := mcpoauth.AuthorizationServerMetadataURL(issuer)
	if err != nil {
		return fmt.Errorf("%w: derive authorization-server location: %v", ErrInvalidConfig, err)
	}
	authorizationServer, err := url.Parse(authorizationServerURL)
	if err != nil {
		return fmt.Errorf("%w: parse authorization-server location: %v", ErrInvalidConfig, err)
	}
	mux.HandleFunc("GET "+protected.Path, serveProtectedResourceMetadata(meta))
	mux.HandleFunc("GET "+authorizationServer.Path, serveAuthorizationServerMetadata(meta))
	return nil
}

// requireSession validates the web session cookie and injects the user id.
func requireSession(sessions *session.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(session.CookieName)
		if err != nil || cookie.Value == "" {
			writeAPIError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		webSession, err := sessions.Validate(r.Context(), cookie.Value)
		if err != nil {
			writeAPIError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userIDKey, webSession.UserID)))
	})
}

func sessionUserID(r *http.Request) (string, error) {
	userID, ok := r.Context().Value(userIDKey).(string)
	if !ok || userID == "" {
		return "", errors.New("httpserver: missing session user")
	}
	return userID, nil
}

// meHandler serves the session user's identity and trial state. It never
// returns tokens or provider credentials.
type meHandler struct {
	identities  IdentityReader
	entitlement Entitlements
}

func (h *meHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	identity, err := h.identities.RobloxIdentity(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "identity unavailable")
		return
	}
	decision, err := h.entitlement.Authorize(r.Context(), entitlement.Subject{
		UserID: userID, Provider: "roblox", ProviderSubject: identity.Subject,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "entitlement unavailable")
		return
	}
	var trial any
	if decision.Entitlement.ID != "" {
		trial = map[string]any{
			"active":     decision.Active,
			"started_at": decision.Entitlement.StartedAt.UTC().Format(timeFormat),
			"ends_at":    decision.Entitlement.EndsAt.UTC().Format(timeFormat),
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"user_id":      userID,
		"display_name": identity.DisplayName,
		"trial":        trial,
	})
}

// logoutHandler revokes every web session of the signed-in user and clears
// the session cookie.
type logoutHandler struct {
	sessions *session.Service
}

func (h *logoutHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := h.sessions.RevokeAll(r.Context(), userID); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "logout unavailable")
		return
	}
	http.SetCookie(w, session.Cookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

// exactOrigin applies the single-allowed-origin CORS policy: only the exact
// configured origin receives CORS headers, every other origin gets none.
func exactOrigin(allowed *url.URL) func(http.Handler) http.Handler {
	allowedOrigin := allowed.String()
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" {
				w.Header().Add("Vary", "Origin")
				if origin == allowedOrigin {
					w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
					if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
						w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
						w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-CSRF-Token")
						w.Header().Set("Access-Control-Max-Age", "600")
						w.WriteHeader(http.StatusNoContent)
						return
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// spaHandler serves the built Vite bundle with an index.html fallback for
// client-side routes.
func spaHandler(dir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		clean := path.Clean(r.URL.Path)
		if strings.HasPrefix(clean, "/api/") || clean == "/api" {
			http.NotFound(w, r)
			return
		}
		target := filepath.Join(dir, filepath.FromSlash(clean))
		if stat, err := os.Stat(target); err == nil && !stat.IsDir() {
			http.ServeFile(w, r, target)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
