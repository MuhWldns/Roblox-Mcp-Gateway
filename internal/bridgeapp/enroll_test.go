package bridgeapp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingEnrollStore captures the saved credential without printing it.
type recordingEnrollStore struct {
	mu     sync.Mutex
	saved  []byte
	saveFn func([]byte) error
}

func (s *recordingEnrollStore) Load() ([]byte, error) { return nil, errors.New("not supported") }
func (s *recordingEnrollStore) Save(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append([]byte(nil), data...)
	if s.saveFn != nil {
		return s.saveFn(data)
	}
	return nil
}
func (s *recordingEnrollStore) Delete() error { return nil }

// enrollServer serves a scripted begin/exchange pair and records requests.
type enrollServer struct {
	mu         sync.Mutex
	beginBody  []byte
	exchanges  int
	exchangeFn func(attempt int, w http.ResponseWriter)
	server     *httptest.Server
}

func newEnrollServer(t *testing.T, exchangeFn func(int, http.ResponseWriter)) *enrollServer {
	t.Helper()
	s := &enrollServer{exchangeFn: exchangeFn}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/device/enrollment/begin", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read begin body: %v", err)
		}
		s.mu.Lock()
		s.beginBody = body
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user_code":        "rkuc_e2etest",
			"verification_url": "https://gateway.example/enroll?code=rkuc_e2etest",
			"expires_in":       600,
		})
	})
	mux.HandleFunc("POST /api/v1/device/enrollment/exchange", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DeviceCode string `json:"device_code"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.DeviceCode != "rkuc_e2etest" {
			http.Error(w, "wrong code", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.exchanges++
		attempt := s.exchanges
		s.mu.Unlock()
		s.exchangeFn(attempt, w)
	})
	s.server = httptest.NewTLSServer(mux)
	t.Cleanup(s.server.Close)
	return s
}

func (s *enrollServer) exchangeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exchanges
}

func testEnrollConfig(base string, store *recordingEnrollStore, out io.Writer) EnrollConfig {
	return EnrollConfig{
		HTTPClient: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // local test server only
		}},
		APIBaseURL:      base,
		DeviceID:        "device-enroll-test",
		DeviceName:      "Enroll Test Device",
		Hostname:        "HOST-E2E",
		Platform:        "windows",
		FingerprintHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		BridgeVersion:   "test-bridge",
		Credential:      store,
		Output:          out,
		PollInterval:    5 * time.Millisecond,
	}
}

// TestRunEnrollPrintsCodeAndSavesCredential proves the happy path: the exact
// verification URL and user code reach the operator, the exchange polls
// through 202, the returned credential is saved through the credential store,
// and the device id is printed — but the plaintext credential never is.
func TestRunEnrollPrintsCodeAndSavesCredential(t *testing.T) {
	srv := newEnrollServer(t, func(attempt int, w http.ResponseWriter) {
		if attempt < 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"device_credential": "rkd_secret_test",
			"device_id":         "device-enroll-test",
		})
	})
	store := &recordingEnrollStore{}
	var out strings.Builder
	cfg := testEnrollConfig(srv.server.URL, store, &out)

	if err := RunEnroll(context.Background(), cfg); err != nil {
		t.Fatalf("RunEnroll: %v", err)
	}

	if got := string(store.saved); got != "rkd_secret_test" {
		t.Fatalf("saved credential = %q", got)
	}
	if n := srv.exchangeCount(); n < 3 {
		t.Fatalf("exchange attempts = %d, want polling through 202s (>=3)", n)
	}
	printed := out.String()
	if !strings.Contains(printed, "rkuc_e2etest") {
		t.Fatalf("output must print the user code: %q", printed)
	}
	if !strings.Contains(printed, "https://gateway.example/enroll?code=rkuc_e2etest") {
		t.Fatalf("output must print the exact verification URL: %q", printed)
	}
	if !strings.Contains(printed, "device-enroll-test") {
		t.Fatalf("output must print the device id: %q", printed)
	}
	if strings.Contains(printed, "rkd_secret_test") {
		t.Fatalf("output must never contain the plaintext credential: %q", printed)
	}
}

// TestRunEnrollSendsClaimPayload proves the begin request carries the exact
// device claim fields.
func TestRunEnrollSendsClaimPayload(t *testing.T) {
	srv := newEnrollServer(t, func(_ int, w http.ResponseWriter) {
		http.Error(w, "unused", http.StatusTeapot)
	})
	// The exchange handler never runs in this test: the assertion is on the
	// begin body, and RunEnroll fails fast on the teapot response.
	store := &recordingEnrollStore{}
	cfg := testEnrollConfig(srv.server.URL, store, io.Discard)
	_ = RunEnroll(context.Background(), cfg)

	srv.mu.Lock()
	body := srv.beginBody
	srv.mu.Unlock()
	var claim map[string]any
	if err := json.Unmarshal(body, &claim); err != nil {
		t.Fatalf("begin body is not JSON: %q", body)
	}
	for key, want := range map[string]string{
		"device_id": "device-enroll-test", "name": "Enroll Test Device",
		"hostname": "HOST-E2E", "platform": "windows", "bridge_version": "test-bridge",
		"fingerprint_hash": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		if got, _ := claim[key].(string); got != want {
			t.Fatalf("claim %q = %q, want %q (body %s)", key, got, want, body)
		}
	}
}

// TestRunEnrollRejectsPlainHTTPOrigin proves the API origin must be https —
// enrollment never relaxes TLS.
func TestRunEnrollRejectsPlainHTTPOrigin(t *testing.T) {
	store := &recordingEnrollStore{}
	cfg := testEnrollConfig("http://insecure.example", store, io.Discard)
	if err := RunEnroll(context.Background(), cfg); err == nil {
		t.Fatal("RunEnroll must refuse a plain-http API origin")
	}
	if store.saved != nil {
		t.Fatal("no credential may be saved for an insecure origin")
	}
}

// TestRunEnrollFailsOnBeginError proves non-2xx begin responses surface as an
// error without touching the credential store.
func TestRunEnrollFailsOnBeginError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/device/enrollment/begin", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "server unavailable", http.StatusInternalServerError)
	})
	server := httptest.NewTLSServer(mux)
	defer server.Close()
	store := &recordingEnrollStore{}
	cfg := testEnrollConfig(server.URL, store, io.Discard)
	err := RunEnroll(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("begin failure must surface, got %v", err)
	}
	if store.saved != nil {
		t.Fatal("no credential may be saved when begin fails")
	}
}

// TestRunEnrollExpiryIsError proves a gone (410) enrollment code is a terminal
// error, not a retry.
func TestRunEnrollExpiryIsError(t *testing.T) {
	srv := newEnrollServer(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "enrollment expired"})
	})
	store := &recordingEnrollStore{}
	cfg := testEnrollConfig(srv.server.URL, store, io.Discard)
	err := RunEnroll(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired enrollment must fail with expiry, got %v", err)
	}
	if n := srv.exchangeCount(); n != 1 {
		t.Fatalf("a 410 must not be retried, exchange attempts = %d", n)
	}
}

// TestRunEnrollRetriesRateLimitAfterRetryAfter proves a transient 429 does
// not terminate enrollment: the client honors Retry-After before exchanging
// the same device code again.
func TestRunEnrollRetriesRateLimitAfterRetryAfter(t *testing.T) {
	srv := newEnrollServer(t, func(attempt int, w http.ResponseWriter) {
		switch attempt {
		case 1:
			w.WriteHeader(http.StatusAccepted)
		case 2:
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"device_credential": "rkd_after_rate_limit",
				"device_id":         "device-enroll-test",
			})
		}
	})
	store := &recordingEnrollStore{}
	cfg := testEnrollConfig(srv.server.URL, store, io.Discard)
	started := time.Now()

	if err := RunEnroll(context.Background(), cfg); err != nil {
		t.Fatalf("RunEnroll after 429: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
		t.Fatalf("RunEnroll retried after %v, want Retry-After delay", elapsed)
	}
	if n := srv.exchangeCount(); n != 3 {
		t.Fatalf("exchange attempts = %d, want 202, 429, then 200", n)
	}
	if got := string(store.saved); got != "rkd_after_rate_limit" {
		t.Fatalf("saved credential = %q", got)
	}
}

// TestRunEnrollRateLimitWaitRespectsContext proves a long server-directed
// backoff remains interruptible and cannot save a credential after cancel.
func TestRunEnrollRateLimitWaitRespectsContext(t *testing.T) {
	srv := newEnrollServer(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	store := &recordingEnrollStore{}
	cfg := testEnrollConfig(srv.server.URL, store, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := RunEnroll(ctx, cfg)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RunEnroll during Retry-After = %v, want context deadline", err)
	}
	if n := srv.exchangeCount(); n != 1 {
		t.Fatalf("exchange attempts = %d, want one before cancellation", n)
	}
	if store.saved != nil {
		t.Fatal("no credential may be saved after cancellation")
	}
}

// TestRunEnrollRespectsContextTimeout proves pending-forever polling stops at
// the context deadline with a clear error.
func TestRunEnrollRespectsContextTimeout(t *testing.T) {
	srv := newEnrollServer(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "pending"})
	})
	store := &recordingEnrollStore{}
	cfg := testEnrollConfig(srv.server.URL, store, io.Discard)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := RunEnroll(ctx, cfg)
	if err == nil {
		t.Fatal("RunEnroll must fail at the context deadline")
	}
	if store.saved != nil {
		t.Fatal("no credential may be saved on timeout")
	}
}

// TestRunEnrollRequiresStore proves a missing credential store is refused.
func TestRunEnrollRequiresStore(t *testing.T) {
	cfg := testEnrollConfig("https://gateway.example", nil, io.Discard)
	if err := RunEnroll(context.Background(), cfg); err == nil {
		t.Fatal("RunEnroll without a credential store must fail")
	}
}

// TestRunEnrollRequiresValidFingerprintHash proves that empty or malformed
// fingerprint hashes are refused during config validation before network requests.
func TestRunEnrollRequiresValidFingerprintHash(t *testing.T) {
	store := &recordingEnrollStore{}
	cases := []struct {
		name string
		hash string
	}{
		{"empty", ""},
		{"too short", "0123456789abcdef"},
		{"too long", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef12"},
		{"non-hex", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdeg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testEnrollConfig("https://gateway.example", store, io.Discard)
			cfg.FingerprintHash = tc.hash
			err := RunEnroll(context.Background(), cfg)
			if err == nil {
				t.Fatalf("expected error for fingerprint %q, got nil", tc.hash)
			}
			if store.saved != nil {
				t.Fatal("no credential may be saved when fingerprint validation fails")
			}
		})
	}
}

// TestRunEnrollExchangeForbiddenLicenseRequired proves that on HTTP 403,
// the exact user-facing message is parsed, printed to Output, returned as a terminal error
// without retry, and zero credentials are saved.
func TestRunEnrollExchangeForbiddenLicenseRequired(t *testing.T) {
	const expectedCopy = "You don’t have a license. Please contact support to get a license."
	srv := newEnrollServer(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": expectedCopy,
		})
	})
	store := &recordingEnrollStore{}
	var out strings.Builder
	cfg := testEnrollConfig(srv.server.URL, store, &out)

	err := RunEnroll(context.Background(), cfg)
	if err == nil {
		t.Fatal("RunEnroll must fail on 403 Forbidden")
	}
	if err.Error() != expectedCopy {
		t.Fatalf("RunEnroll error = %q, want %q", err.Error(), expectedCopy)
	}
	if got := strings.Count(out.String(), expectedCopy); got != 1 {
		t.Fatalf("output must contain %q exactly once, got %d times in %q", expectedCopy, got, out.String())
	}
	// Assert internal details / prefixes / status codes are absent from the returned error.
	for _, forbidden := range []string{"403", "status", "forbidden", "fingerprint", "collision", "trial", "bridgeapp:"} {
		if strings.Contains(strings.ToLower(err.Error()), forbidden) {
			t.Fatalf("returned error leaked technical detail %q: %q", forbidden, err.Error())
		}
		if strings.Contains(strings.ToLower(out.String()), forbidden) {
			t.Fatalf("output leaked technical detail %q: %q", forbidden, out.String())
		}
	}
	if n := srv.exchangeCount(); n != 1 {
		t.Fatalf("403 must be terminal and never retried, exchange attempts = %d", n)
	}
	if store.saved != nil {
		t.Fatal("no credential may be saved when license is denied")
	}
}

// TestRunEnrollExchangeForbiddenSanitizesArbitraryHTML proves that arbitrary HTML
// or unstructured bodies on 403 are not blindly printed or returned.
func TestRunEnrollExchangeForbiddenSanitizesArbitraryHTML(t *testing.T) {
	const expectedCopy = "You don’t have a license. Please contact support to get a license."
	srv := newEnrollServer(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><body><h1>403 Forbidden - Internal Gateway Error</h1></body></html>"))
	})
	store := &recordingEnrollStore{}
	var out strings.Builder
	cfg := testEnrollConfig(srv.server.URL, store, &out)

	err := RunEnroll(context.Background(), cfg)
	if err == nil {
		t.Fatal("RunEnroll must fail on 403 Forbidden")
	}
	if err.Error() != expectedCopy {
		t.Fatalf("RunEnroll error on HTML 403 = %q, want fallback %q", err.Error(), expectedCopy)
	}
	if strings.Contains(out.String(), "<html>") {
		t.Fatalf("output must not print raw HTML: %q", out.String())
	}
	if !strings.Contains(out.String(), expectedCopy) {
		t.Fatalf("output must contain fallback copy %q, got %q", expectedCopy, out.String())
	}
	if n := srv.exchangeCount(); n != 1 {
		t.Fatalf("403 must be terminal and never retried, exchange attempts = %d", n)
	}
	if store.saved != nil {
		t.Fatal("no credential may be saved when license is denied")
	}
}
