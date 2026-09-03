package httpserver_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"robloxkit/internal/audit"
	"robloxkit/internal/device"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/httpserver"
	"robloxkit/internal/mysqlstore"
	"robloxkit/internal/robloxauth"
	"robloxkit/internal/session"
)

const routerPepper = "router-test-pepper"

type mutableClock struct{ now time.Time }

func (c *mutableClock) Now() time.Time { return c.now }

func routerTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	rawDSN := os.Getenv("MYSQL_TEST_DSN")
	if rawDSN == "" {
		t.Skip("MYSQL_TEST_DSN is not configured")
	}
	base, err := mysql.ParseDSN(rawDSN)
	if err != nil {
		t.Fatalf("parse MYSQL_TEST_DSN: %v", err)
	}
	adminConfig := *base
	adminConfig.DBName = ""
	admin, err := sql.Open("mysql", adminConfig.FormatDSN())
	if err != nil {
		t.Fatalf("open admin database: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin database: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping MYSQL_TEST_DSN: %v", err)
	}
	dbName := fmt.Sprintf("robloxkit_router_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+dbName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create temporary database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+dbName+"`"); err != nil {
			t.Errorf("drop temporary database: %v", err)
		}
	})
	target := *base
	target.DBName = dbName
	target.ParseTime = true
	target.Loc = time.UTC
	db, err := sql.Open("mysql", target.FormatDSN())
	if err != nil {
		t.Fatalf("open temporary database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close temporary database: %v", err)
		}
	})
	if _, err := mysqlstore.Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

type routerStack struct {
	db          *sql.DB
	router      http.Handler
	sessions    *session.Service
	identities  *mysqlstore.IdentityStore
	enrollment  *device.Enrollment
	deviceStore *mysqlstore.DeviceStore
	allowedURL  string
}

func newRouterStack(t *testing.T) *routerStack {
	t.Helper()
	db := routerTestDatabase(t)
	clock := &mutableClock{now: time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC)}

	sessions := session.NewService(mysqlstore.NewSessionStore(db), []byte(routerPepper), time.Hour)
	identities := mysqlstore.NewIdentityStore(db)
	auditSvc := audit.NewService(mysqlstore.NewAuditStore(db))
	entitlements := entitlement.NewService(mysqlstore.NewEntitlementStore(db, clock, auditSvc), clock)
	deviceStore := mysqlstore.NewDeviceStore(db)
	enrollment, err := device.NewEnrollment(deviceStore, entitlements, []byte(routerPepper), clock.Now)
	if err != nil {
		t.Fatalf("construct enrollment: %v", err)
	}
	enrollment.VerificationBaseURL = "https://app.example.test"

	artifactPath := filepath.Join(t.TempDir(), "RobloxBridge.exe")
	if err := os.WriteFile(artifactPath, []byte("bridge-artifact-bytes"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	artifact := device.Artifact{Version: "1.4.2", Filename: "RobloxBridge.exe", Path: artifactPath}
	download, err := device.NewDownloadHandler(sessions, artifact)
	if err != nil {
		t.Fatalf("construct download handler: %v", err)
	}
	downloadMetadata, err := device.NewDownloadMetadataHandler(sessions, artifact)
	if err != nil {
		t.Fatalf("construct metadata handler: %v", err)
	}

	allowed := "https://app.example.test"
	router, err := httpserver.NewRouter(httpserver.Config{
		Sessions:         sessions,
		RobloxAuth:       &robloxauth.Handler{SuccessRedirect: "/"},
		IdentityReader:   deviceStore,
		Entitlements:     entitlements,
		Download:         download,
		DownloadMetadata: downloadMetadata,
		Enrollment:       enrollment,
		AllowedOrigin:    mustParseURL(t, allowed),
	})
	if err != nil {
		t.Fatalf("construct router: %v", err)
	}
	return &routerStack{
		db: db, router: router, sessions: sessions, identities: identities,
		enrollment: enrollment, deviceStore: deviceStore, allowedURL: allowed,
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}

// login provisions a Roblox identity and a live web session for it.
func (s *routerStack) login(t *testing.T, subject string) (robloxauth.User, *http.Cookie) {
	t.Helper()
	user, err := s.identities.UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{
		Subject: subject, Username: "builder", DisplayName: "Builder " + subject,
	})
	if err != nil {
		t.Fatalf("upsert identity: %v", err)
	}
	plain, _, err := s.sessions.Create(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return user, &http.Cookie{Name: session.CookieName, Value: plain}
}

func (s *routerStack) countTrials(t *testing.T) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM trial_entitlements").Scan(&n); err != nil {
		t.Fatalf("count trials: %v", err)
	}
	return n
}

func (s *routerStack) do(t *testing.T, method, path string, cookies []*http.Cookie, header http.Header, body string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	recorder := httptest.NewRecorder()
	s.router.ServeHTTP(recorder, req)
	return recorder.Result()
}

func decodeJSON(t *testing.T, res *http.Response, target any) {
	t.Helper()
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

// csrfFor fetches a CSRF token plus cookie using the given session.
func (s *routerStack) csrfFor(t *testing.T, session *http.Cookie) (*http.Cookie, string) {
	t.Helper()
	res := s.do(t, http.MethodGet, "/api/v1/csrf", []*http.Cookie{session}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("csrf issue status = %d", res.StatusCode)
	}
	var payload struct {
		CSRFToken string `json:"csrf_token"`
	}
	decodeJSON(t, res, &payload)
	var csrfCookie *http.Cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == "__Host-robloxkit_csrf" {
			csrfCookie = cookie
		}
	}
	if csrfCookie == nil || payload.CSRFToken == "" {
		t.Fatalf("csrf issuance missing token or cookie: %#v %q", csrfCookie, payload.CSRFToken)
	}
	return csrfCookie, payload.CSRFToken
}

func beginEnrollmentHTTP(t *testing.T, s *routerStack) string {
	t.Helper()
	res := s.do(t, http.MethodPost, "/api/v1/device/enrollment/begin", nil, http.Header{"Content-Type": []string{"application/json"}},
		`{"device_id":"device-http-1","hostname":"DESKTOP-HTTP","platform":"windows","bridge_version":"1.4.2"}`)
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("begin status = %d body=%q", res.StatusCode, body)
	}
	var payload struct {
		UserCode string `json:"user_code"`
	}
	decodeJSON(t, res, &payload)
	if payload.UserCode == "" {
		t.Fatal("begin response missing user code")
	}
	return payload.UserCode
}

func TestRouterMeRequiresSession(t *testing.T) {
	stack := newRouterStack(t)

	res := stack.do(t, http.MethodGet, "/api/v1/me", nil, nil, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me without session status = %d, want %d", res.StatusCode, http.StatusUnauthorized)
	}
}

func TestRouterMeReturnsIdentityAndTrialState(t *testing.T) {
	stack := newRouterStack(t)
	_, session := stack.login(t, "1516563360")

	res := stack.do(t, http.MethodGet, "/api/v1/me", []*http.Cookie{session}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("me status = %d", res.StatusCode)
	}
	var me struct {
		UserID      string `json:"user_id"`
		DisplayName string `json:"display_name"`
		Trial       *struct {
			Active    bool   `json:"active"`
			EndedAt   string `json:"ends_at"`
			StartedAt string `json:"started_at"`
		} `json:"trial"`
	}
	decodeJSON(t, res, &me)
	if me.DisplayName != "Builder 1516563360" {
		t.Fatalf("display name = %q", me.DisplayName)
	}
	if me.Trial != nil {
		t.Fatalf("fresh user trial = %+v, want null", me.Trial)
	}
}

func TestRouterDownloadRequiresSessionAndServesHeaders(t *testing.T) {
	stack := newRouterStack(t)
	_, session := stack.login(t, "1516563360")

	res := stack.do(t, http.MethodGet, "/api/v1/bridge/download", nil, nil, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("download without session status = %d", res.StatusCode)
	}

	res = stack.do(t, http.MethodGet, "/api/v1/bridge/download", []*http.Cookie{session}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	res.Body.Close()
	if string(body) != "bridge-artifact-bytes" {
		t.Fatalf("download body = %q", body)
	}
	if res.Header.Get("X-Checksum-Sha256") == "" || len(res.Header.Get("X-Checksum-Sha256")) != 64 {
		t.Fatalf("checksum header = %q", res.Header.Get("X-Checksum-Sha256"))
	}
	if res.Header.Get("X-Bridge-Version") != "1.4.2" {
		t.Fatalf("version header = %q", res.Header.Get("X-Bridge-Version"))
	}

	metadata := stack.do(t, http.MethodGet, "/api/v1/bridge/download/metadata", []*http.Cookie{session}, nil, "")
	if metadata.StatusCode != http.StatusOK {
		t.Fatalf("metadata status = %d", metadata.StatusCode)
	}
	var meta struct {
		Version   string `json:"version"`
		Filename  string `json:"filename"`
		SHA256    string `json:"sha256"`
		SizeBytes int64  `json:"size_bytes"`
	}
	decodeJSON(t, metadata, &meta)
	if meta.Version != "1.4.2" || meta.Filename != "RobloxBridge.exe" || meta.SizeBytes != int64(len("bridge-artifact-bytes")) || len(meta.SHA256) != 64 {
		t.Fatalf("metadata = %+v", meta)
	}
}

func TestRouterLoginAndDownloadNeverStartTrial(t *testing.T) {
	stack := newRouterStack(t)
	_, session := stack.login(t, "1516563360")

	if res := stack.do(t, http.MethodGet, "/api/v1/me", []*http.Cookie{session}, nil, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("me status = %d", res.StatusCode)
	}
	if res := stack.do(t, http.MethodGet, "/api/v1/bridge/download", []*http.Cookie{session}, nil, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d", res.StatusCode)
	}
	if res := stack.do(t, http.MethodGet, "/api/v1/bridge/download/metadata", []*http.Cookie{session}, nil, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("metadata status = %d", res.StatusCode)
	}
	if got := stack.countTrials(t); got != 0 {
		t.Fatalf("login/download created %d trial rows, want 0", got)
	}
}

func TestRouterAppliesExactCORSOriginOnly(t *testing.T) {
	stack := newRouterStack(t)

	evil := stack.do(t, http.MethodGet, "/api/v1/me", nil, http.Header{"Origin": []string{"https://evil.example"}}, "")
	if code := evil.Header.Get("Access-Control-Allow-Origin"); code != "" {
		t.Fatalf("evil origin received ACAO %q", code)
	}

	allowed := stack.do(t, http.MethodGet, "/api/v1/me", nil, http.Header{"Origin": []string{stack.allowedURL}}, "")
	if got := allowed.Header.Get("Access-Control-Allow-Origin"); got != stack.allowedURL {
		t.Fatalf("ACAO = %q, want exact %q", got, stack.allowedURL)
	}
	if got := allowed.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("ACAC = %q", got)
	}
	if got := allowed.Header.Get("Vary"); !strings.Contains(got, "Origin") {
		t.Fatalf("Vary = %q, want Origin", got)
	}

	preflight := stack.do(t, http.MethodOptions, "/api/v1/enrollments/approve", nil, http.Header{
		"Origin":                        []string{stack.allowedURL},
		"Access-Control-Request-Method": []string{"POST"},
	}, "")
	if preflight.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d", preflight.StatusCode)
	}
	if got := preflight.Header.Get("Access-Control-Allow-Origin"); got != stack.allowedURL {
		t.Fatalf("preflight ACAO = %q", got)
	}
	if got := preflight.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Fatalf("preflight AAM = %q", got)
	}
	if got := preflight.Header.Get("Access-Control-Allow-Headers"); !strings.Contains(got, "X-CSRF-Token") {
		t.Fatalf("preflight ACAH = %q", got)
	}

	evilPreflight := stack.do(t, http.MethodOptions, "/api/v1/enrollments/approve", nil, http.Header{
		"Origin":                        []string{"https://evil.example"},
		"Access-Control-Request-Method": []string{"POST"},
	}, "")
	if got := evilPreflight.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("evil preflight received ACAO %q", got)
	}
}

func TestRouterEnrollmentApproveRequiresSessionThenCSRF(t *testing.T) {
	stack := newRouterStack(t)
	_, session := stack.login(t, "1516563360")
	userCode := beginEnrollmentHTTP(t, stack)

	// Without a session the approve handler answers 401.
	res := stack.do(t, http.MethodPost, "/api/v1/enrollments/approve", nil,
		http.Header{"Content-Type": []string{"application/json"}},
		`{"user_code":"`+userCode+`"}`)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("approve without session status = %d, want %d", res.StatusCode, http.StatusUnauthorized)
	}

	// With a session but without the CSRF header, approval is forbidden and
	// nothing is consumed.
	res = stack.do(t, http.MethodPost, "/api/v1/enrollments/approve", []*http.Cookie{session},
		http.Header{"Content-Type": []string{"application/json"}},
		`{"user_code":"`+userCode+`"}`)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("approve without CSRF status = %d, want %d", res.StatusCode, http.StatusForbidden)
	}

	// A wrong CSRF token is equally rejected.
	wrongCookie, wrongToken := stack.csrfFor(t, session)
	wrongCookie.Value = "forged"
	res = stack.do(t, http.MethodPost, "/api/v1/enrollments/approve", []*http.Cookie{session, wrongCookie},
		http.Header{"Content-Type": []string{"application/json"}, "X-CSRF-Token": []string{wrongToken}},
		`{"user_code":"`+userCode+`"}`)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("approve with forged CSRF status = %d, want %d", res.StatusCode, http.StatusForbidden)
	}

	// The matching double-submit pair is accepted and the code is still live.
	csrfCookie, csrfToken := stack.csrfFor(t, session)
	res = stack.do(t, http.MethodPost, "/api/v1/enrollments/approve", []*http.Cookie{session, csrfCookie},
		http.Header{"Content-Type": []string{"application/json"}, "X-CSRF-Token": []string{csrfToken}},
		`{"user_code":"`+userCode+`"}`)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("approve status = %d, want %d", res.StatusCode, http.StatusNoContent)
	}
}

func TestRouterEndToEndEnrollmentStartsTrial(t *testing.T) {
	stack := newRouterStack(t)
	user, session := stack.login(t, "1516563360")

	// The Bridge begins enrollment without any credentials.
	res := stack.do(t, http.MethodPost, "/api/v1/device/enrollment/begin", nil,
		http.Header{"Content-Type": []string{"application/json"}},
		`{"device_id":"device-e2e","hostname":"DESKTOP-E2E","platform":"windows","bridge_version":"1.4.2"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("begin status = %d", res.StatusCode)
	}
	var beginPayload struct {
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		ExpiresIn       int    `json:"expires_in"`
	}
	decodeJSON(t, res, &beginPayload)
	if !strings.HasPrefix(beginPayload.UserCode, "rkuc_") || beginPayload.VerificationURL == "" || beginPayload.ExpiresIn <= 0 {
		t.Fatalf("begin payload = %+v", beginPayload)
	}

	// The browser displays the pending device before approving.
	lookup := stack.do(t, http.MethodGet, "/api/v1/enrollments/claim?code="+url.QueryEscape(beginPayload.UserCode), []*http.Cookie{session}, nil, "")
	if lookup.StatusCode != http.StatusOK {
		t.Fatalf("lookup status = %d", lookup.StatusCode)
	}
	var claim device.PendingEnrollment
	decodeJSON(t, lookup, &claim)
	if claim.Hostname != "DESKTOP-E2E" || claim.DeviceID != "device-e2e" || claim.Platform != "windows" {
		t.Fatalf("claim = %+v", claim)
	}
	if got := stack.countTrials(t); got != 0 {
		t.Fatalf("lookup created %d trials, want 0", got)
	}

	// Unauthenticated lookups are rejected.
	if res := stack.do(t, http.MethodGet, "/api/v1/enrollments/claim?code=x", nil, nil, ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("lookup without session status = %d", res.StatusCode)
	}

	// The session user approves with CSRF protection.
	csrfCookie, csrfToken := stack.csrfFor(t, session)
	res = stack.do(t, http.MethodPost, "/api/v1/enrollments/approve", []*http.Cookie{session, csrfCookie},
		http.Header{"Content-Type": []string{"application/json"}, "X-CSRF-Token": []string{csrfToken}},
		`{"user_code":"`+beginPayload.UserCode+`"}`)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("approve status = %d", res.StatusCode)
	}
	if got := stack.countTrials(t); got != 0 {
		t.Fatalf("approve created %d trials, want 0", got)
	}

	res = stack.do(t, http.MethodPost, "/api/v1/device/enrollment/exchange", nil,
		http.Header{"Content-Type": []string{"application/json"}},
		`{"device_code":"`+beginPayload.UserCode+`"}`)
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		t.Fatalf("exchange status = %d body=%q", res.StatusCode, body)
	}
	var exchanged struct {
		DeviceCredential string `json:"device_credential"`
		DeviceID         string `json:"device_id"`
	}
	decodeJSON(t, res, &exchanged)
	if !strings.HasPrefix(exchanged.DeviceCredential, "rkd_") || exchanged.DeviceID != "device-e2e" {
		t.Fatalf("exchange payload = %+v", exchanged)
	}

	if got := stack.countTrials(t); got != 1 {
		t.Fatalf("trials after exchange = %d, want 1", got)
	}

	// The browser-visible trial state flips active for the owner only.
	me := stack.do(t, http.MethodGet, "/api/v1/me", []*http.Cookie{session}, nil, "")
	var profile struct {
		Trial *struct {
			Active bool `json:"active"`
		} `json:"trial"`
	}
	decodeJSON(t, me, &profile)
	if profile.Trial == nil || !profile.Trial.Active {
		t.Fatalf("trial after exchange = %+v, want active", profile.Trial)
	}
	_ = user

	// The enrollment code is spent: replays fail and mint nothing extra.
	replay := stack.do(t, http.MethodPost, "/api/v1/device/enrollment/exchange", nil,
		http.Header{"Content-Type": []string{"application/json"}},
		`{"device_code":"`+beginPayload.UserCode+`"}`)
	if replay.StatusCode == http.StatusOK {
		t.Fatal("exchange replay succeeded")
	}
	if got := stack.countTrials(t); got != 1 {
		t.Fatalf("trials after replay = %d, want 1", got)
	}
}

func TestRouterCsrfIssuanceSetsHardenedCookie(t *testing.T) {
	stack := newRouterStack(t)
	_, session := stack.login(t, "1516563360")

	res := stack.do(t, http.MethodGet, "/api/v1/csrf", []*http.Cookie{session}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("csrf status = %d", res.StatusCode)
	}
	var cookie *http.Cookie
	for _, candidate := range res.Cookies() {
		if candidate.Name == "__Host-robloxkit_csrf" {
			cookie = candidate
		}
	}
	if cookie == nil {
		t.Fatal("csrf response missing cookie")
	}
	if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("csrf cookie flags = %#v", cookie)
	}
}

func TestRouterLogoutRevokesSessions(t *testing.T) {
	stack := newRouterStack(t)
	_, webSession := stack.login(t, "1516563360")

	if res := stack.do(t, http.MethodGet, "/api/v1/me", []*http.Cookie{webSession}, nil, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("pre-logout me status = %d", res.StatusCode)
	}

	logoutRes := stack.do(t, http.MethodPost, "/api/v1/auth/logout", []*http.Cookie{webSession}, nil, "")
	if logoutRes.StatusCode != http.StatusForbidden {
		t.Fatalf("logout without CSRF status = %d, want %d", logoutRes.StatusCode, http.StatusForbidden)
	}
	csrfCookie, csrfToken := stack.csrfFor(t, webSession)
	logoutRes = stack.do(t, http.MethodPost, "/api/v1/auth/logout", []*http.Cookie{webSession, csrfCookie},
		http.Header{"X-CSRF-Token": []string{csrfToken}}, "")
	if logoutRes.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", logoutRes.StatusCode)
	}
	var cleared bool
	for _, cookie := range logoutRes.Cookies() {
		if cookie.Name == session.CookieName && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("logout did not clear the session cookie: %#v", logoutRes.Cookies())
	}

	if res := stack.do(t, http.MethodGet, "/api/v1/me", []*http.Cookie{webSession}, nil, ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout me status = %d, want %d", res.StatusCode, http.StatusUnauthorized)
	}
}

func TestRouterRejectsMisconfiguredStacks(t *testing.T) {
	if _, err := httpserver.NewRouter(httpserver.Config{}); err == nil {
		t.Fatal("router accepted an empty config")
	}
	_, err := httpserver.NewRouter(httpserver.Config{AllowedOrigin: mustParseURL(t, "https://app.example.test")})
	if err == nil || !errors.Is(err, httpserver.ErrInvalidConfig) {
		t.Fatalf("partial config error = %v, want ErrInvalidConfig", err)
	}
}
