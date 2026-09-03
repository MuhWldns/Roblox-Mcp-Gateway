// Package httpserver composes the public gateway routes: Roblox login and
// logout, the authenticated Bridge download, the device enrollment approval
// flow, the session-owned /api/v1/me view, and CSRF issuance — all under an
// exact-origin CORS policy.
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

	"robloxkit/internal/device"
	"robloxkit/internal/entitlement"
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
	AllowedOrigin    *url.URL
	StaticDir        string
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
	if cfg.AllowedOrigin == nil {
		invalid = append(invalid, "allowed origin is required")
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalidConfig, strings.Join(invalid, "; "))
	}

	csrf := NewCSRF()
	lookup := &device.EnrollmentLookupHandler{Sessions: cfg.Sessions, Enrollment: cfg.Enrollment}
	approve := &device.EnrollmentApproveHandler{Sessions: cfg.Sessions, Enrollment: cfg.Enrollment}
	begin := &device.EnrollmentBeginHandler{Enrollment: cfg.Enrollment}
	exchange := &device.EnrollmentExchangeHandler{Enrollment: cfg.Enrollment}
	logout := csrf.Require(requireSession(cfg.Sessions, &logoutHandler{sessions: cfg.Sessions}))
	me := requireSession(cfg.Sessions, &meHandler{identities: cfg.IdentityReader, entitlement: cfg.Entitlements})

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/roblox/login", cfg.RobloxAuth.Begin)
	mux.HandleFunc("/api/v1/auth/roblox/callback", cfg.RobloxAuth.Callback)
	mux.Handle("/api/v1/auth/logout", logout)
	mux.Handle("/api/v1/me", me)
	mux.Handle("/api/v1/csrf", requireSession(cfg.Sessions, http.HandlerFunc(csrf.Issue)))
	mux.Handle("/api/v1/bridge/download/metadata", cfg.DownloadMetadata)
	mux.Handle("/api/v1/bridge/download", cfg.Download)
	mux.Handle("/api/v1/enrollments/claim", lookup)
	mux.Handle("/api/v1/enrollments/approve", csrf.Require(approve))
	mux.Handle("/api/v1/device/enrollment/begin", begin)
	mux.Handle("/api/v1/device/enrollment/exchange", exchange)
	if cfg.StaticDir != "" {
		mux.Handle("/", spaHandler(cfg.StaticDir))
	}

	return exactOrigin(cfg.AllowedOrigin)(mux), nil
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
