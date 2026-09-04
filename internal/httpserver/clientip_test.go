package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newClientAddressTestHandler(t *testing.T, trusted []string, principal *string) http.Handler {
	t.Helper()
	middleware, err := NewTrustedClientAddressMiddleware(trusted)
	if err != nil {
		t.Fatalf("NewTrustedClientAddressMiddleware: %v", err)
	}
	return middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*principal = RemotePrincipal(r)
		w.WriteHeader(http.StatusNoContent)
	}))
}

func TestTrustedClientAddressIgnoresUntrustedPeerSpoof(t *testing.T) {
	var principal string
	handler := newClientAddressTestHandler(t, []string{"10.0.0.0/8"}, &principal)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.8:4242"
	req.Header.Set("X-Forwarded-For", "198.51.100.17")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if principal != "203.0.113.8" {
		t.Fatalf("RemotePrincipal = %q, want direct untrusted peer", principal)
	}
}

func TestTrustedClientAddressUsesSingleTrustedProxy(t *testing.T) {
	var principal string
	handler := newClientAddressTestHandler(t, []string{"10.0.0.0/8"}, &principal)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.20.30.40:443"
	req.Header.Set("X-Forwarded-For", "198.51.100.17")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if principal != "198.51.100.17" {
		t.Fatalf("RemotePrincipal = %q, want forwarded client", principal)
	}
}

func TestTrustedClientAddressWalksTrustedChainFromRight(t *testing.T) {
	var principal string
	handler := newClientAddressTestHandler(t, []string{"10.0.0.0/8", "192.168.0.0/16"}, &principal)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.20.30.40:443"
	// The leftmost value is attacker-controlled data received by the first
	// untrusted hop. It must not displace that hop as the client principal.
	req.Header.Set("X-Forwarded-For", "192.0.2.99, 198.51.100.23, 192.168.1.7")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if principal != "198.51.100.23" {
		t.Fatalf("RemotePrincipal = %q, want first untrusted address from right", principal)
	}
}

func TestTrustedClientAddressRejectsMalformedForwardedChain(t *testing.T) {
	tests := []struct {
		name   string
		values []string
	}{
		{name: "empty", values: []string{""}},
		{name: "empty member", values: []string{"198.51.100.1,, 10.0.0.2"}},
		{name: "invalid address", values: []string{"not-an-address"}},
		{name: "address with port", values: []string{"198.51.100.1:1234"}},
		{name: "multiple header lines", values: []string{"198.51.100.1", "198.51.100.2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			middleware, err := NewTrustedClientAddressMiddleware([]string{"10.0.0.0/8"})
			if err != nil {
				t.Fatalf("NewTrustedClientAddressMiddleware: %v", err)
			}
			handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "10.0.0.4:443"
			for _, value := range tt.values {
				req.Header.Add("X-Forwarded-For", value)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if called {
				t.Fatal("malformed forwarded chain reached wrapped handler")
			}
			if body := rec.Body.String(); strings.Contains(body, strings.Join(tt.values, ",")) && strings.Join(tt.values, ",") != "" {
				t.Fatalf("400 response leaks rejected header: %q", body)
			}
		})
	}
}

func TestTrustedClientAddressBoundsForwardedChain(t *testing.T) {
	middleware, err := NewTrustedClientAddressMiddleware([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("NewTrustedClientAddressMiddleware: %v", err)
	}
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("oversized forwarded chain reached wrapped handler")
	}))

	tests := []struct {
		name  string
		value string
	}{
		{name: "bytes", value: strings.Repeat("1", maxForwardedForBytes+1)},
		{name: "hops", value: strings.Repeat("198.51.100.1,", maxForwardedForHops) + "198.51.100.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = "10.0.0.4:443"
			req.Header.Set("X-Forwarded-For", tt.value)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestTrustedClientAddressPreservesIPv6(t *testing.T) {
	var principal string
	handler := newClientAddressTestHandler(t, []string{"2001:db8:1::/48"}, &principal)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "[2001:db8:1::10]:443"
	req.Header.Set("X-Forwarded-For", "2001:0db8:abcd:0000:0000:0000:0000:0025")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if principal != "2001:db8:abcd::25" {
		t.Fatalf("RemotePrincipal = %q, want canonical IPv6 client", principal)
	}
}

func TestRemotePrincipalBucketsCanonicalVerifiedAddress(t *testing.T) {
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{
		Requests:    1,
		Window:      time.Minute,
		MaxInFlight: 1,
	})
	middleware, err := NewTrustedClientAddressMiddleware([]string{"2001:db8:1::/48"})
	if err != nil {
		t.Fatalf("NewTrustedClientAddressMiddleware: %v", err)
	}
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Allow(RemotePrincipal(r)) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	request := func(forwarded string) int {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "[2001:db8:1::10]:443"
		req.Header.Set("X-Forwarded-For", forwarded)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := request("2001:0db8:abcd:0000:0000:0000:0000:0025"); got != http.StatusNoContent {
		t.Fatalf("first equivalent client status = %d, want 204", got)
	}
	if got := request("2001:db8:abcd::25"); got != http.StatusTooManyRequests {
		t.Fatalf("second equivalent client status = %d, want shared bucket 429", got)
	}
	if got := request("2001:db8:abcd::26"); got != http.StatusNoContent {
		t.Fatalf("different client status = %d, want independent bucket 204", got)
	}
}

func TestTrustedClientAddressConstructorRejectsMalformedCIDR(t *testing.T) {
	if _, err := NewTrustedClientAddressMiddleware([]string{"10.0.0.0/8", "proxy.internal"}); err == nil {
		t.Fatal("NewTrustedClientAddressMiddleware error = nil, want malformed CIDR rejection")
	}
}
