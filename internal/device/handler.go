package device

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"robloxkit/internal/entitlement"
	"robloxkit/internal/session"
)

const maxEnrollmentBodyBytes = 16 << 10

// EnrollmentBeginHandler serves the unauthenticated Bridge-side enrollment
// start. The response is the pairing code plus the URL the Bridge displays.
type EnrollmentBeginHandler struct {
	Enrollment *Enrollment
}

// ServeHTTP accepts a Bridge claim and returns the pairing code.
func (h *EnrollmentBeginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Enrollment == nil {
		writeError(w, http.StatusServiceUnavailable, "enrollment unavailable")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var claim DeviceClaim
	if !decodeJSONBody(w, r, &claim) {
		return
	}
	userCode, verificationURL, err := h.Enrollment.Begin(r.Context(), claim)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidClaim):
			writeError(w, http.StatusBadRequest, "invalid device claim")
		case errors.Is(err, ErrTooManyPending):
			writeError(w, http.StatusServiceUnavailable, "enrollment capacity reached")
		default:
			writeError(w, http.StatusInternalServerError, "enrollment unavailable")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"user_code":        string(userCode),
		"verification_url": string(verificationURL),
		"expires_in":       int(h.Enrollment.PendingTTL.Seconds()),
	})
}

// EnrollmentLookupHandler serves the session-owned review of a pending
// enrollment: hostname, platform, and Bridge version of the device claiming
// access.
type EnrollmentLookupHandler struct {
	Sessions   SessionValidator
	Enrollment *Enrollment
}

// ServeHTTP requires a web session and returns the pending device claim.
func (h *EnrollmentLookupHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.Enrollment == nil || h.Sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "enrollment unavailable")
		return
	}
	if _, err := requireSession(r, h.Sessions); err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		writeError(w, http.StatusBadRequest, "enrollment code is required")
		return
	}
	pending, err := h.Enrollment.Lookup(r.Context(), code)
	switch {
	case err == nil:
	case errors.Is(err, ErrEnrollmentExpired):
		writeError(w, http.StatusGone, "enrollment expired")
		return
	case errors.Is(err, ErrEnrollmentNotFound):
		writeError(w, http.StatusNotFound, "enrollment not found")
		return
	default:
		writeError(w, http.StatusInternalServerError, "enrollment unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(pending)
}

// EnrollmentApproveHandler serves the CSRF-protected, session-owned device
// approval. The approving session user becomes the device owner.
type EnrollmentApproveHandler struct {
	Sessions   SessionValidator
	Enrollment *Enrollment
}

// ServeHTTP requires a web session and approves the presented code.
func (h *EnrollmentApproveHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.Enrollment == nil || h.Sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "enrollment unavailable")
		return
	}
	webSession, err := requireSession(r, h.Sessions)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		UserCode string `json:"user_code"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if err := h.Enrollment.Approve(r.Context(), webSession.UserID, strings.TrimSpace(body.UserCode)); err != nil {
		switch {
		case errors.Is(err, ErrApprovalOwnerRequired):
			writeError(w, http.StatusUnauthorized, "authentication required")
		case errors.Is(err, ErrEnrollmentExpired):
			writeError(w, http.StatusGone, "enrollment expired")
		case errors.Is(err, ErrEnrollmentNotFound):
			writeError(w, http.StatusNotFound, "enrollment not found")
		default:
			writeError(w, http.StatusInternalServerError, "approval unavailable")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// EnrollmentExchangeHandler serves the unauthenticated Bridge-side code
// exchange for a device credential. A pending approval answers 202 so the
// Bridge polls; success mints the credential exactly once.
type EnrollmentExchangeHandler struct {
	Enrollment *Enrollment
}

// ServeHTTP exchanges an approved enrollment code for a device credential.
func (h *EnrollmentExchangeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Enrollment == nil {
		writeError(w, http.StatusServiceUnavailable, "enrollment unavailable")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	credential, err := h.Enrollment.Exchange(r.Context(), strings.TrimSpace(body.DeviceCode))
	if err != nil {
		switch {
		case errors.Is(err, ErrEnrollmentPending):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
		case errors.Is(err, ErrEnrollmentNotFound), errors.Is(err, ErrCodeNotFound), errors.Is(err, ErrCodeConsumed):
			writeError(w, http.StatusNotFound, "enrollment code not found")
		case errors.Is(err, ErrEnrollmentExpired), errors.Is(err, ErrCodeExpired):
			writeError(w, http.StatusGone, "enrollment expired")
		case errors.Is(err, entitlement.ErrTrialAlreadyUsed):
			writeError(w, http.StatusForbidden, "trial already used")
		case errors.Is(err, entitlement.ErrNoSlot):
			writeError(w, http.StatusForbidden, "no free device slot")
		default:
			writeError(w, http.StatusInternalServerError, "enrollment unavailable")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(credential)
}

func requireSession(r *http.Request, sessions SessionValidator) (session.Session, error) {
	cookie, err := r.Cookie(session.CookieName)
	if err != nil || cookie.Value == "" {
		return session.Session{}, errors.New("device: missing session cookie")
	}
	return sessions.Validate(r.Context(), cookie.Value)
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxEnrollmentBodyBytes)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
