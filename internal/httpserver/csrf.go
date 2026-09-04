package httpserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"robloxkit/internal/session"
)

const (
	// CSRFHeader echoes the double-submit token on mutations.
	CSRFHeader = "X-CSRF-Token"
	// CSRFCookieName is the hardened double-submit cookie.
	CSRFCookieName = "__Host-robloxkit_csrf"
)

const csrfTokenBytes = 32

// CSRF implements the double-submit cookie pattern for session-authenticated
// mutations. The cookie is HttpOnly and SameSite=Strict; the token reaches
// the SPA only through the JSON body of the issuance endpoint, so it lives
// in page memory and never in browser storage.
type CSRF struct {
	// MaxAge bounds the issued token lifetime.
	MaxAge time.Duration
}

// NewCSRF constructs the CSRF service with a one-hour token lifetime.
func NewCSRF() *CSRF {
	return &CSRF{MaxAge: time.Hour}
}

// Issue mints a fresh token, stores it in the hardened cookie, and returns
// it in the JSON body.
func (c *CSRF) Issue(w http.ResponseWriter, _ *http.Request) {
	raw := make([]byte, csrfTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "csrf unavailable")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	maxAge := c.MaxAge
	if maxAge <= 0 {
		maxAge = time.Hour
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(maxAge / time.Second),
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"csrf_token": token})
}

// Require enforces the double-submit pair on mutating requests that carry a
// session cookie. Requests without a session cookie fall through so the
// wrapped handler answers 401; unauthenticated traffic cannot mutate state.
func (c *CSRF) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if _, err := r.Cookie(session.CookieName); err != nil {
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := r.Cookie(CSRFCookieName)
		header := r.Header.Get(CSRFHeader)
		if err != nil || cookie.Value == "" || header == "" ||
			subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) != 1 {
			writeAPIError(w, http.StatusForbidden, "csrf token missing or invalid")
			return
		}
		next.ServeHTTP(w, r)
	})
}
