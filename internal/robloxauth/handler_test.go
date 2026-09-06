package robloxauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"robloxkit/internal/session"
)

const testBindingCookie = "__Host-robloxkit_login"

func TestRobloxHandlerBeginRedirectsWithoutExposingTransactionSecrets(t *testing.T) {
	flow := &handlerFlow{
		authorize:   "https://apis.roblox.com/oauth/v1/authorize?client_id=safe&state=state-value",
		transaction: LoginTransaction{State: "state-value", Nonce: "nonce-secret", CodeVerifier: "verifier-secret", Binding: "browser-binding"},
	}
	handler := Handler{Flow: flow, Identities: &handlerIdentities{}, Sessions: &handlerSessions{}, SuccessRedirect: "/dashboard"}
	recorder := httptest.NewRecorder()
	handler.Begin(recorder, httptest.NewRequest(http.MethodGet, "/auth/roblox", nil))

	if recorder.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusSeeOther)
	}
	if location := recorder.Header().Get("Location"); location != string(flow.authorize) {
		t.Fatalf("location = %q", location)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != testBindingCookie || cookies[0].Value != flow.transaction.Binding || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("binding cookies = %#v", cookies)
	}
	response := recorder.Body.String()
	if strings.Contains(response, flow.transaction.CodeVerifier) || strings.Contains(response, flow.transaction.Nonce) {
		t.Fatalf("response disclosed transaction secrets: %q", response)
	}
}

func TestRobloxHandlerCallbackCreatesApplicationSessionAndLeaksNoProviderTokens(t *testing.T) {
	providerTokens := []string{"provider-access-secret", "provider-refresh-secret", "provider-id-secret"}
	flow := &handlerFlow{identity: RobloxIdentity{Subject: "1516563360", Username: "Builderman", DisplayName: "Builder Man"}}
	identities := &handlerIdentities{user: User{ID: "user-1", IdentityID: "identity-1", RobloxSubject: "1516563360", DisplayName: "Builder Man"}}
	sessions := &handlerSessions{plain: "rks_application-session", sess: session.Session{ID: "session-1", UserID: "user-1", ExpiresAt: time.Now().Add(time.Hour)}}
	var logs bytes.Buffer
	handler := Handler{
		Flow: flow, Identities: identities, Sessions: sessions,
		SuccessRedirect: "/dashboard", Logger: log.New(&logs, "", 0), SessionMaxAge: time.Hour,
	}
	request := httptest.NewRequest(http.MethodGet, "/auth/roblox/callback?code=provider-code&state=opaque-state", nil)
	request.AddCookie(&http.Cookie{Name: testBindingCookie, Value: "browser-binding"})
	recorder := httptest.NewRecorder()
	handler.Callback(recorder, request)

	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/dashboard" {
		t.Fatalf("callback response = %d location %q", recorder.Code, recorder.Header().Get("Location"))
	}
	if flow.callback != (Callback{Code: "provider-code", State: "opaque-state", Binding: "browser-binding"}) {
		t.Fatalf("callback input = %#v", flow.callback)
	}
	if identities.got.Subject != "1516563360" || sessions.userID != "user-1" {
		t.Fatalf("identity/session binding = %#v / %q", identities.got, sessions.userID)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) != 2 || cookies[0].Name != testBindingCookie || cookies[0].MaxAge != -1 || cookies[1].Name != session.CookieName || cookies[1].Value != sessions.plain || !cookies[1].Secure || !cookies[1].HttpOnly {
		t.Fatalf("session cookies = %#v", cookies)
	}
	visible := recorder.Body.String() + recorder.Header().Get("Location") + logs.String()
	for _, token := range providerTokens {
		if strings.Contains(visible, token) {
			t.Fatalf("handler output/log disclosed provider token %q", token)
		}
	}
	if strings.Contains(visible, "1516563360") {
		t.Fatalf("redirect response disclosed provider identity: %q", visible)
	}
}

func TestRobloxHandlerCallbackRejectsProviderErrorsWithoutCreatingSession(t *testing.T) {
	flow := &handlerFlow{completeErr: errors.New("provider-access-secret")}
	identities := &handlerIdentities{}
	sessions := &handlerSessions{}
	var logs bytes.Buffer
	handler := Handler{Flow: flow, Identities: identities, Sessions: sessions, SuccessRedirect: "/dashboard", Logger: log.New(&logs, "", 0)}
	request := httptest.NewRequest(http.MethodGet, "/auth/roblox/callback?code=bad&state=state", nil)
	request.AddCookie(&http.Cookie{Name: testBindingCookie, Value: "browser-binding"})
	recorder := httptest.NewRecorder()
	handler.Callback(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if identities.calls != 0 || sessions.calls != 0 {
		t.Fatalf("identity/session calls = %d/%d, want 0/0", identities.calls, sessions.calls)
	}
	visible := recorder.Body.String() + logs.String()
	if strings.Contains(visible, "provider-access-secret") {
		t.Fatalf("error output/log disclosed provider token: %q", visible)
	}
}

func TestRobloxHandlerCallbackLogsSafeFailureCategory(t *testing.T) {
	flow := &handlerFlow{completeErr: fmt.Errorf("%w: provider-access-secret", ErrTokenExchange)}
	var logs bytes.Buffer
	handler := Handler{
		Flow: flow, Identities: &handlerIdentities{}, Sessions: &handlerSessions{},
		Logger: log.New(&logs, "", 0),
	}
	request := httptest.NewRequest(http.MethodGet, "/auth/roblox/callback?code=provider-code&state=opaque-state", nil)
	request.AddCookie(&http.Cookie{Name: testBindingCookie, Value: "browser-binding"})
	recorder := httptest.NewRecorder()

	handler.Callback(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if got := logs.String(); !strings.Contains(got, "category=token_exchange") {
		t.Fatalf("callback log = %q, want safe token_exchange category", got)
	} else if strings.Contains(got, "provider-access-secret") || strings.Contains(got, "provider-code") || strings.Contains(got, "opaque-state") {
		t.Fatalf("callback log disclosed credential material: %q", got)
	}
}

type handlerFlow struct {
	authorize   AuthorizeURL
	transaction LoginTransaction
	identity    RobloxIdentity
	callback    Callback
	beginErr    error
	completeErr error
}

func (f *handlerFlow) Begin(context.Context) (AuthorizeURL, LoginTransaction, error) {
	return f.authorize, f.transaction, f.beginErr
}

func (f *handlerFlow) Complete(_ context.Context, callback Callback) (RobloxIdentity, error) {
	f.callback = callback
	return f.identity, f.completeErr
}

type handlerIdentities struct {
	got   RobloxIdentity
	user  User
	err   error
	calls int
}

func (s *handlerIdentities) UpsertRobloxIdentity(_ context.Context, identity RobloxIdentity) (User, error) {
	s.calls++
	s.got = identity
	return s.user, s.err
}

type handlerSessions struct {
	plain  string
	sess   session.Session
	err    error
	userID string
	calls  int
}

func (s *handlerSessions) Create(_ context.Context, userID string) (string, session.Session, error) {
	s.calls++
	s.userID = userID
	return s.plain, s.sess, s.err
}
