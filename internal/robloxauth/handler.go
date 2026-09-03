package robloxauth

import (
	"context"
	"log"
	"net/http"
	"time"

	"robloxkit/internal/session"
)

const loginBindingCookieName = "__Host-robloxkit_login"

type FlowService interface {
	Begin(context.Context) (AuthorizeURL, LoginTransaction, error)
	Complete(context.Context, Callback) (RobloxIdentity, error)
}

type IdentityService interface {
	UpsertRobloxIdentity(context.Context, RobloxIdentity) (User, error)
}

type SessionService interface {
	Create(context.Context, string) (string, session.Session, error)
}

// Handler translates the provider callback into an internal browser session.
// It intentionally returns redirects and a session cookie only; provider
// credentials and provider identity details never enter an HTTP response.
type Handler struct {
	Flow            FlowService
	Identities      IdentityService
	Sessions        SessionService
	SuccessRedirect string
	Logger          *log.Logger
	SessionMaxAge   time.Duration
}

func (h *Handler) Begin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Flow == nil {
		h.internalError(w, "login unavailable")
		return
	}
	authorize, transaction, err := h.Flow.Begin(r.Context())
	if err != nil {
		h.internalError(w, "begin login failed")
		return
	}
	http.SetCookie(w, loginBindingCookie(transaction.Binding, 0))
	http.Redirect(w, r, string(authorize), http.StatusSeeOther)
}

func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.Flow == nil || h.Identities == nil || h.Sessions == nil {
		h.internalError(w, "login unavailable")
		return
	}
	binding, err := r.Cookie(loginBindingCookieName)
	if err != nil || binding.Value == "" {
		h.badCallback(w, "invalid login callback")
		return
	}
	http.SetCookie(w, loginBindingCookie("", -1))
	query := r.URL.Query()
	identity, err := h.Flow.Complete(r.Context(), Callback{Code: query.Get("code"), State: query.Get("state"), Binding: binding.Value, Error: query.Get("error")})
	if err != nil {
		h.badCallback(w, "invalid login callback")
		return
	}
	user, err := h.Identities.UpsertRobloxIdentity(r.Context(), identity)
	if err != nil {
		h.internalError(w, "identity binding failed")
		return
	}
	plain, _, err := h.Sessions.Create(r.Context(), user.ID)
	if err != nil {
		h.internalError(w, "session creation failed")
		return
	}
	maxAge := 0
	if h.SessionMaxAge > 0 {
		maxAge = int(h.SessionMaxAge / time.Second)
	}
	http.SetCookie(w, session.Cookie(plain, maxAge))
	redirect := h.SuccessRedirect
	if redirect == "" {
		redirect = "/"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}
func loginBindingCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     loginBindingCookieName,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

func (h *Handler) badCallback(w http.ResponseWriter, message string) {
	if h.Logger != nil {
		h.Logger.Print(message)
	}
	http.Error(w, "invalid login callback", http.StatusBadRequest)
}

func (h *Handler) internalError(w http.ResponseWriter, message string) {
	if h.Logger != nil {
		h.Logger.Print(message)
	}
	http.Error(w, "login unavailable", http.StatusInternalServerError)
}
