package httpserver_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"robloxkit/internal/httpserver"
	"robloxkit/internal/session"
)

// csrfNextHandler records whether the wrapped handler ran.
type csrfNextHandler struct{ reached bool }

func (h *csrfNextHandler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	h.reached = true
	w.WriteHeader(http.StatusNoContent)
}

func csrfRequest(method, path string, cookies ...string) *http.Request {
	var reader io.Reader
	if method == http.MethodPost {
		reader = strings.NewReader(`{}`)
	}
	req := httptest.NewRequest(method, path, reader)
	for _, value := range cookies {
		name, val, _ := strings.Cut(value, "=")
		req.AddCookie(&http.Cookie{Name: name, Value: val})
	}
	return req
}

func csrfRoundTrip(t *testing.T, handler http.Handler, req *http.Request) (*http.Response, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	res := recorder.Result()
	t.Cleanup(func() { res.Body.Close() })
	return res, recorder
}

func TestCSRFRequirePassesSafeMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		next := &csrfNextHandler{}
		_, recorder := csrfRoundTrip(t, httpserver.NewCSRF().Require(next), csrfRequest(method, "/api/v1/me"))
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("%s without cookies status = %d, want %d", method, recorder.Code, http.StatusNoContent)
		}
	}
}

func TestCSRFRequirePassesSessionlessMutations(t *testing.T) {
	// Bridge-side endpoints POST without any session cookie; those requests
	// must fall through so the wrapped handler answers.
	next := &csrfNextHandler{}
	_, recorder := csrfRoundTrip(t, httpserver.NewCSRF().Require(next), csrfRequest(http.MethodPost, "/api/v1/device/enrollment/begin"))
	if !next.reached || recorder.Code != http.StatusNoContent {
		t.Fatalf("session-less mutation blocked: reached=%v status=%d", next.reached, recorder.Code)
	}
}

func TestCSRFRequireRejectsSessionMutationsWithoutToken(t *testing.T) {
	next := &csrfNextHandler{}
	res, _ := csrfRoundTrip(t, httpserver.NewCSRF().Require(next),
		csrfRequest(http.MethodPost, "/api/v1/devices/x/rename", session.CookieName+"=rks_session_value"))
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("mutation without CSRF status = %d, want %d", res.StatusCode, http.StatusForbidden)
	}
	if next.reached {
		t.Fatal("mutation without CSRF reached the handler")
	}
}

func TestCSRFRequireRejectsMismatchedDoubleSubmitPair(t *testing.T) {
	next := &csrfNextHandler{}
	req := csrfRequest(http.MethodPost, "/api/v1/devices/x/rename",
		session.CookieName+"=rks_session_value",
		httpserver.CSRFCookieName+"=cookie-token")
	req.Header.Set(httpserver.CSRFHeader, "header-token")
	res, _ := csrfRoundTrip(t, httpserver.NewCSRF().Require(next), req)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("mismatched pair status = %d, want %d", res.StatusCode, http.StatusForbidden)
	}
	if next.reached {
		t.Fatal("mismatched pair reached the handler")
	}
}

func TestCSRFRequireAcceptsMatchingDoubleSubmitPair(t *testing.T) {
	next := &csrfNextHandler{}
	req := csrfRequest(http.MethodPost, "/api/v1/devices/x/rename",
		session.CookieName+"=rks_session_value",
		httpserver.CSRFCookieName+"=matching-token")
	req.Header.Set(httpserver.CSRFHeader, "matching-token")
	res, _ := csrfRoundTrip(t, httpserver.NewCSRF().Require(next), req)
	if res.StatusCode != http.StatusNoContent || !next.reached {
		t.Fatalf("matching pair status = %d reached=%v", res.StatusCode, next.reached)
	}
}

func TestCSRFIssueReturnsTokenAndHardenedCookie(t *testing.T) {
	recorder := httptest.NewRecorder()
	httpserver.NewCSRF().Issue(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/csrf", nil))
	res := recorder.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("issue status = %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), `"csrf_token"`) {
		t.Fatalf("issue body = %q", body)
	}
	var cookie *http.Cookie
	for _, candidate := range res.Cookies() {
		if candidate.Name == httpserver.CSRFCookieName {
			cookie = candidate
		}
	}
	if cookie == nil || cookie.Value == "" {
		t.Fatalf("issue missing cookie: %#v", cookie)
	}
	if !strings.Contains(string(body), cookie.Value) {
		t.Fatalf("issued token does not match the cookie: body=%q cookie=%q", body, cookie.Value)
	}
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("cookie hardening flags = %#v", cookie)
	}
}
