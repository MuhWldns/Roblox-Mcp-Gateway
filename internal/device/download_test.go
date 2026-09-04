package device_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"robloxkit/internal/device"
	"robloxkit/internal/session"
)

type fakeSessionValidator struct {
	validToken string
	userID     string
	denied     bool
}

func (f fakeSessionValidator) Validate(_ context.Context, token string) (session.Session, error) {
	if f.denied || token != f.validToken || token == "" {
		return session.Session{}, errors.New("session: invalid")
	}
	return session.Session{ID: "session-1", UserID: f.userID}, nil
}

func testArtifact(t *testing.T) (device.Artifact, []byte) {
	t.Helper()
	body := []byte("roblox-bridge-artifact-payload\x00\x01\x02")
	path := filepath.Join(t.TempDir(), "RobloxBridge.exe")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return device.Artifact{Version: "1.4.2", Filename: "RobloxBridge.exe", Path: path}, body
}

func requestWithSessionCookie(method, target, token string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: token})
	}
	return req
}

func TestDownloadWithoutSessionIsRejected(t *testing.T) {
	artifact, _ := testArtifact(t)
	handler, err := device.NewDownloadHandler(fakeSessionValidator{validToken: "sess", userID: "u1"}, artifact)
	if err != nil {
		t.Fatalf("construct download handler: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, requestWithSessionCookie(http.MethodGet, "/api/v1/bridge/download", ""))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status without cookie = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, requestWithSessionCookie(http.MethodGet, "/api/v1/bridge/download", "wrong-token"))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status with invalid cookie = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}
}

func TestDownloadServesArtifactWithChecksumAndVersionHeaders(t *testing.T) {
	artifact, body := testArtifact(t)
	handler, err := device.NewDownloadHandler(fakeSessionValidator{validToken: "sess", userID: "u1"}, artifact)
	if err != nil {
		t.Fatalf("construct download handler: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, requestWithSessionCookie(http.MethodGet, "/api/v1/bridge/download", "sess"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := recorder.Body.Bytes(); string(got) != string(body) {
		t.Fatalf("body mismatch: got %q want %q", got, body)
	}
	if checksum := recorder.Header().Get("X-Checksum-Sha256"); len(checksum) != 64 || !isHex(checksum) {
		t.Fatalf("X-Checksum-Sha256 = %q, want 64-char hex sha256", checksum)
	}
	if version := recorder.Header().Get("X-Bridge-Version"); version != "1.4.2" {
		t.Fatalf("X-Bridge-Version = %q, want %q", version, "1.4.2")
	}
	disposition := recorder.Header().Get("Content-Disposition")
	if !strings.Contains(disposition, `attachment;`) || !strings.Contains(disposition, `RobloxBridge.exe`) {
		t.Fatalf("Content-Disposition = %q, want attachment with filename", disposition)
	}
	if recorder.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", recorder.Header().Get("Content-Type"))
	}
}

func TestDownloadRejectsNonGetMethods(t *testing.T) {
	artifact, _ := testArtifact(t)
	handler, err := device.NewDownloadHandler(fakeSessionValidator{validToken: "sess", userID: "u1"}, artifact)
	if err != nil {
		t.Fatalf("construct download handler: %v", err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, requestWithSessionCookie(http.MethodPost, "/api/v1/bridge/download", "sess"))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
	}
}

func TestDownloadMetadataRequiresSessionAndReturnsArtifactInfo(t *testing.T) {
	artifact, body := testArtifact(t)
	handler, err := device.NewDownloadMetadataHandler(fakeSessionValidator{validToken: "sess", userID: "u1"}, artifact)
	if err != nil {
		t.Fatalf("construct metadata handler: %v", err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, requestWithSessionCookie(http.MethodGet, "/api/v1/bridge/download/metadata", ""))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status without cookie = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, requestWithSessionCookie(http.MethodGet, "/api/v1/bridge/download/metadata", "sess"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%q", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	contentType := recorder.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("Content-Type = %q", contentType)
	}
	payload := recorder.Body.String()
	if !strings.Contains(payload, `"version":"1.4.2"`) || !strings.Contains(payload, `"filename":"RobloxBridge.exe"`) {
		t.Fatalf("metadata missing version/filename: %q", payload)
	}
	if !strings.Contains(payload, `"sha256"`) {
		t.Fatalf("metadata missing checksum: %q", payload)
	}
	if !strings.Contains(payload, `"size_bytes":`) {
		t.Fatalf("metadata missing size: %q", payload)
	}
	if strings.Contains(payload, string(body)) {
		t.Fatal("metadata must not embed the artifact payload")
	}
}

func TestDownloadHandlerRejectsMissingArtifact(t *testing.T) {
	artifact, _ := testArtifact(t)
	artifact.Path = filepath.Join(t.TempDir(), "missing.exe")
	if _, err := device.NewDownloadHandler(fakeSessionValidator{}, artifact); err == nil {
		t.Fatal("constructor accepted unreadable artifact")
	}
}

func isHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
