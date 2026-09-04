package httpserver_test

import (
	"context"
	"crypto/sha256"
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
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"robloxkit/internal/audit"
	"robloxkit/internal/device"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/health"
	"robloxkit/internal/httpserver"
	"robloxkit/internal/mcpoauth"
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

// fakeRegistry is the test double for the live Bridge registry: it reports
// canned presence, records disconnects, and can be told to panic like a
// broken handler would.
type fakeRegistry struct {
	mu           sync.Mutex
	online       map[string]bool
	disconnected []string
	panicOnline  bool
}

func (f *fakeRegistry) Online(deviceID string) bool {
	if f.panicOnline {
		panic("boom: secret internal detail DSN=root@tcp(127.0.0.1:3306)/prod")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.online[deviceID]
}

func (f *fakeRegistry) Disconnect(deviceID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnected = append(f.disconnected, deviceID)
}

func (f *fakeRegistry) disconnects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.disconnected...)
}

func (f *fakeRegistry) setOnline(deviceID string, online bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.online == nil {
		f.online = map[string]bool{}
	}
	f.online[deviceID] = online
}

// leakingReadiness fails readiness with an error carrying data that must
// never reach a probe response.
type leakingReadiness struct{}

func (leakingReadiness) PingContext(context.Context) error {
	return errors.New("dial tcp 10.0.0.9:3306: connection refused (dsn root:hunter2@tcp(10.0.0.9:3306)/prod)")
}

type routerStack struct {
	db               *sql.DB
	router           http.Handler
	sessions         *session.Service
	identities       *mysqlstore.IdentityStore
	enrollment       *device.Enrollment
	deviceStore      *mysqlstore.DeviceStore
	dashboard        *mysqlstore.DashboardStore
	entitlements     *entitlement.Service
	download         *device.DownloadHandler
	downloadMetadata *device.DownloadMetadataHandler
	registry         *fakeRegistry
	readiness        health.Checker
	metadata         mcpoauth.Metadata
	allowedURL       string
}

func newRouterStack(t *testing.T) *routerStack {
	return newRouterStackWithReadiness(t, nil)
}

// newRouterStackWithReadiness assembles the full production composition. A
// non-nil ready checker replaces the database readiness probe.
func newRouterStackWithReadiness(t *testing.T, ready health.Checker) *routerStack {
	t.Helper()
	db := routerTestDatabase(t)
	clock := &mutableClock{now: time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC)}

	sessions := session.NewService(mysqlstore.NewSessionStore(db), []byte(routerPepper), time.Hour)
	identities := mysqlstore.NewIdentityStore(db)
	auditSvc := audit.NewService(mysqlstore.NewAuditStore(db))
	entitlements := entitlement.NewService(mysqlstore.NewEntitlementStore(db, clock, auditSvc), clock)
	deviceStore := mysqlstore.NewDeviceStore(db)
	dashboard := mysqlstore.NewDashboardStore(db, auditSvc)
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

	metadata, err := mcpoauth.NewMetadata(
		mustParseURL(t, "https://gateway.example.test"),
		mustParseURL(t, "https://gateway.example.test/mcp"),
		mcpoauth.SupportedScopes,
	)
	if err != nil {
		t.Fatalf("construct oauth metadata: %v", err)
	}
	if ready == nil {
		ready = db
	}

	stack := &routerStack{
		db:               db,
		sessions:         sessions,
		identities:       identities,
		enrollment:       enrollment,
		deviceStore:      deviceStore,
		dashboard:        dashboard,
		entitlements:     entitlements,
		download:         download,
		downloadMetadata: downloadMetadata,
		registry:         &fakeRegistry{},
		readiness:        ready,
		metadata:         metadata,
		allowedURL:       "https://app.example.test",
	}
	stack.buildRouter(t, nil)
	return stack
}

// buildRouter assembles the router from the stack's shared parts. mutate may
// adjust the configuration before validation, which tests use for body
// limits and optional mounts.
func (s *routerStack) buildRouter(t *testing.T, mutate func(*httpserver.Config)) {
	t.Helper()
	cfg := &httpserver.Config{
		Sessions:         s.sessions,
		RobloxAuth:       &robloxauth.Handler{SuccessRedirect: "/"},
		IdentityReader:   s.deviceStore,
		Entitlements:     s.entitlements,
		Download:         s.download,
		DownloadMetadata: s.downloadMetadata,
		Enrollment:       s.enrollment,
		Dashboard:        s.dashboard,
		Registry:         s.registry,
		Health:           health.NewHandler(s.readiness, nil),
		Metadata:         &s.metadata,
		AllowedOrigin:    mustParseURL(t, s.allowedURL),
	}
	if mutate != nil {
		mutate(cfg)
	}
	router, err := httpserver.NewRouter(*cfg)
	if err != nil {
		t.Fatalf("construct router: %v", err)
	}
	s.router = router
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

// exec runs one fixture statement.
func (s *routerStack) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("fixture %q: %v", query, err)
	}
}

func (s *routerStack) insertDevice(t *testing.T, userID, deviceID, name string) {
	s.exec(t, `INSERT INTO devices (id, user_id, name) VALUES (?, ?, ?)`, deviceID, userID, name)
}

func (s *routerStack) insertDeviceCredential(t *testing.T, userID, deviceID string) {
	digest := sha256.Sum256([]byte("router-credential:" + deviceID))
	s.exec(t, `INSERT INTO device_credentials (id, user_id, device_id, credential_digest) VALUES (?, ?, ?, ?)`,
		"cred-"+deviceID, userID, deviceID, digest[:])
}

func (s *routerStack) insertStudio(t *testing.T, studioID, userID, deviceID, status string) {
	var ended any
	if status != "active" {
		ended = time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC)
	}
	s.exec(t, `INSERT INTO studio_sessions (id, user_id, device_id, studio_id, status, started_at, ended_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		studioID, userID, deviceID, "studio-"+deviceID, status, time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC), ended)
}

func (s *routerStack) insertConnectorClient(t *testing.T) {
	s.exec(t, `INSERT INTO oauth_clients (id, client_id, client_name, redirect_uris) VALUES ('client-1', 'https://chatgpt.com/aip/mcp', 'ChatGPT', '["https://chatgpt.com/aip/oauth/callback"]')`)
}

func (s *routerStack) insertConnectorGrant(t *testing.T, grantID, userID, deviceID, studioSessionID string) {
	var device, studio any
	if deviceID != "" {
		device = deviceID
	}
	if studioSessionID != "" {
		studio = studioSessionID
	}
	s.exec(t, `INSERT INTO oauth_grants (id, user_id, client_id, device_id, studio_session_id, scopes, resource, created_at) VALUES (?, ?, 'client-1', ?, ?, ?, ?, ?)`,
		grantID, userID, device, studio, `["mcp:connect","studio:read"]`, "https://gateway.example.test/mcp",
		time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC))
}
func (s *routerStack) insertConnectorTokens(t *testing.T, userID, grantID string) {
	accessDigest := sha256.Sum256([]byte("router-access:" + grantID))
	refreshDigest := sha256.Sum256([]byte("router-refresh:" + grantID))
	s.exec(t, `INSERT INTO oauth_access_tokens (id, user_id, grant_id, token_digest, expires_at) VALUES (?, ?, ?, ?, ?)`,
		"access-"+grantID, userID, grantID, accessDigest[:], time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
	s.exec(t, `INSERT INTO oauth_refresh_tokens (id, user_id, grant_id, family_id, token_digest, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"refresh-"+grantID, userID, grantID, "refresh-"+grantID, refreshDigest[:], time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC))
}

func (s *routerStack) insertTrial(t *testing.T, userID string) {
	s.exec(t, `INSERT INTO trial_entitlements (id, user_id, started_at, ends_at) VALUES (?, ?, ?, ?)`,
		"trial-router-1", userID, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC))
}

func (s *routerStack) insertLicense(t *testing.T, licenseID, userID string, slots int) {
	s.exec(t, `INSERT INTO licenses (id, user_id, status, device_slots) VALUES (?, ?, 'active', ?)`, licenseID, userID, slots)
}

func (s *routerStack) insertBinding(t *testing.T, bindingID, licenseID, userID, deviceID string) {
	s.exec(t, `INSERT INTO license_device_bindings (id, user_id, license_id, device_id, slot_ordinal, status) VALUES (?, ?, ?, ?, 1, 'active')`,
		bindingID, userID, licenseID, deviceID)
}

// auditEvent is one audit_logs row of interest.
type auditEvent struct {
	CorrelationID string
	TargetID      string
	Metadata      string
}

func (s *routerStack) auditEvents(t *testing.T, action string) []auditEvent {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(),
		`SELECT correlation_id, target_id, metadata FROM audit_logs WHERE action = ? ORDER BY created_at`, action)
	if err != nil {
		t.Fatalf("query audit events: %v", err)
	}
	defer rows.Close()
	var events []auditEvent
	for rows.Next() {
		var event auditEvent
		var targetID, metadata sql.NullString
		if err := rows.Scan(&event.CorrelationID, &targetID, &metadata); err != nil {
			t.Fatalf("scan audit event: %v", err)
		}
		event.TargetID = targetID.String
		event.Metadata = metadata.String
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit events: %v", err)
	}
	return events
}

func (s *routerStack) deviceRow(t *testing.T, deviceID string) (name, status string) {
	t.Helper()
	if err := s.db.QueryRowContext(t.Context(), `SELECT name, status FROM devices WHERE id = ?`, deviceID).Scan(&name, &status); err != nil {
		t.Fatalf("read device row: %v", err)
	}
	return name, status
}

func (s *routerStack) grantRevokedAt(t *testing.T, grantID string) sql.NullTime {
	t.Helper()
	var revoked sql.NullTime
	if err := s.db.QueryRowContext(t.Context(), `SELECT revoked_at FROM oauth_grants WHERE id = ?`, grantID).Scan(&revoked); err != nil {
		t.Fatalf("read grant revoked_at: %v", err)
	}
	return revoked
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

// doUnknownLength issues a request whose body length is not declared up
// front, forcing the body-limit middleware through its read path.
func (s *routerStack) doUnknownLength(t *testing.T, method, path string, cookies []*http.Cookie, header http.Header, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, io.NopCloser(strings.NewReader(body)))
	req.ContentLength = -1
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

// --- Task 18: middleware behavior -----------------------------------------

func TestRouterAssignsAndEchoesRequestID(t *testing.T) {
	stack := newRouterStack(t)

	res := stack.do(t, http.MethodGet, "/api/v1/me", nil, nil, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me without session status = %d", res.StatusCode)
	}
	if got := res.Header.Get(httpserver.RequestIDHeader); !strings.HasPrefix(got, "req_") || len(got) < 8 {
		t.Fatalf("assigned request id = %q", got)
	}

	// A safe incoming correlation id is echoed verbatim.
	echoed := stack.do(t, http.MethodGet, "/api/v1/me", nil,
		http.Header{httpserver.RequestIDHeader: []string{"client-correlation-42"}}, "")
	if got := echoed.Header.Get(httpserver.RequestIDHeader); got != "client-correlation-42" {
		t.Fatalf("echoed request id = %q, want client-correlation-42", got)
	}

	// Unsafe incoming values (whitespace, control characters, absurd
	// length) are replaced by a server-generated id, never echoed.
	for _, unsafe := range []string{"has spaces", "line\nbreak", strings.Repeat("x", 300)} {
		res := stack.do(t, http.MethodGet, "/api/v1/me", nil,
			http.Header{httpserver.RequestIDHeader: []string{unsafe}}, "")
		got := res.Header.Get(httpserver.RequestIDHeader)
		if got == unsafe {
			t.Fatalf("unsafe request id %q was echoed verbatim", unsafe)
		}
		if !strings.HasPrefix(got, "req_") {
			t.Fatalf("replacement request id = %q", got)
		}
	}
}

func TestRouterPropagatesRequestIDIntoAuditCorrelation(t *testing.T) {
	stack := newRouterStack(t)
	userA, sessionA := stack.login(t, "1516563360")
	stack.insertDevice(t, userA.ID, "device-a-1", "Laptop A")
	csrfCookie, csrfToken := stack.csrfFor(t, sessionA)

	res := stack.do(t, http.MethodPost, "/api/v1/devices/device-a-1/rename", []*http.Cookie{sessionA, csrfCookie},
		http.Header{
			"Content-Type":             []string{"application/json"},
			"X-CSRF-Token":             []string{csrfToken},
			httpserver.RequestIDHeader: []string{"audit-correlation-42"},
		},
		`{"name":"Renamed Laptop"}`)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("rename status = %d", res.StatusCode)
	}

	events := stack.auditEvents(t, "device.rename")
	if len(events) != 1 {
		t.Fatalf("device.rename audit events = %d, want 1: %+v", len(events), events)
	}
	if events[0].CorrelationID != "audit-correlation-42" {
		t.Fatalf("audit correlation = %q, want the request id", events[0].CorrelationID)
	}
	if events[0].TargetID != "device-a-1" {
		t.Fatalf("audit target = %q", events[0].TargetID)
	}
}

func TestRouterPanicReturnsSanitized500(t *testing.T) {
	stack := newRouterStack(t)
	userA, sessionA := stack.login(t, "1516563360")
	stack.insertDevice(t, userA.ID, "device-a-1", "Laptop A")
	stack.registry.panicOnline = true

	res := stack.do(t, http.MethodGet, "/api/v1/devices", []*http.Cookie{sessionA}, nil, "")
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("panic status = %d, want %d", res.StatusCode, http.StatusInternalServerError)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.TrimSpace(string(body)) != `{"error":"internal server error"}` {
		t.Fatalf("panic body = %q", body)
	}
	for _, forbidden := range []string{"boom", "secret", "DSN", "3306", "goroutine", "stack"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("panic body leaked %q: %s", forbidden, body)
		}
	}
	// The outer middleware still labels the sanitized response.
	if got := res.Header.Get(httpserver.RequestIDHeader); !strings.HasPrefix(got, "req_") {
		t.Fatalf("panic response request id = %q", got)
	}
}

func TestRecoverPanicsUnit(t *testing.T) {
	// Direct unit proof that the middleware answers a sanitized 500 and
	// keeps a mid-write panic from producing a second status line.
	panicking := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		panic("secret internal detail: DSN=root@tcp(127.0.0.1:3306)/prod")
	})
	recorder := httptest.NewRecorder()
	httpserver.RecoverPanics(panicking).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil))
	res := recorder.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	if strings.TrimSpace(string(body)) != `{"error":"internal server error"}` {
		t.Fatalf("body = %q", body)
	}
	for _, forbidden := range []string{"secret", "DSN", "3306", "goroutine"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("body leaked %q: %s", forbidden, body)
		}
	}
}

func TestRouterAppliesSecureHeadersToEveryResponse(t *testing.T) {
	stack := newRouterStack(t)

	for _, path := range []string{"/healthz", "/readyz", "/api/v1/me"} {
		res := stack.do(t, http.MethodGet, path, nil, nil, "")
		if got := res.Header.Get("Strict-Transport-Security"); !strings.Contains(got, "max-age") {
			t.Fatalf("%s: HSTS = %q", path, got)
		}
		if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s: X-Content-Type-Options = %q", path, got)
		}
		if got := res.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Fatalf("%s: X-Frame-Options = %q", path, got)
		}
		if got := res.Header.Get("Referrer-Policy"); got != "no-referrer" {
			t.Fatalf("%s: Referrer-Policy = %q", path, got)
		}
	}
}

func TestRouterHealthLivenessAndReadiness(t *testing.T) {
	stack := newRouterStack(t)

	live := stack.do(t, http.MethodGet, "/healthz", nil, nil, "")
	if live.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", live.StatusCode)
	}
	if got := live.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("healthz content type = %q", got)
	}
	body, _ := io.ReadAll(live.Body)
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("healthz body = %q", body)
	}

	ready := stack.do(t, http.MethodGet, "/readyz", nil, nil, "")
	if ready.StatusCode != http.StatusOK {
		t.Fatalf("readyz status = %d", ready.StatusCode)
	}
	body, _ = io.ReadAll(ready.Body)
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("readyz body = %q", body)
	}

	if got := stack.do(t, http.MethodHead, "/healthz", nil, nil, "").StatusCode; got != http.StatusOK {
		t.Fatalf("HEAD healthz status = %d", got)
	}
	if res := stack.do(t, http.MethodPost, "/healthz", nil, nil, ""); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST healthz status = %d, want 405", res.StatusCode)
	}
}

func TestRouterReadinessFailsClosedWithoutDetailLeakage(t *testing.T) {
	stack := newRouterStackWithReadiness(t, leakingReadiness{})

	res := stack.do(t, http.MethodGet, "/readyz", nil, nil, "")
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want %d", res.StatusCode, http.StatusServiceUnavailable)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != `{"status":"unavailable"}` {
		t.Fatalf("readyz body = %q", body)
	}
	for _, forbidden := range []string{"3306", "hunter2", "10.0.0.9", "connection refused", "dsn"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("readyz body leaked %q: %s", forbidden, body)
		}
	}

	// Liveness stays independent of readiness.
	if got := stack.do(t, http.MethodGet, "/healthz", nil, nil, "").StatusCode; got != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", got)
	}
}

func TestRouterRejectsWrongMethodsWithAllow(t *testing.T) {
	stack := newRouterStack(t)

	res := stack.do(t, http.MethodPost, "/api/v1/devices", nil, nil, "")
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST devices status = %d, want 405", res.StatusCode)
	}
	if got := res.Header.Get("Allow"); !strings.Contains(got, "GET") {
		t.Fatalf("POST devices Allow = %q", got)
	}

	res = stack.do(t, http.MethodPut, "/api/v1/devices/device-a-1/rename", nil, nil, "")
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PUT rename status = %d, want 405", res.StatusCode)
	}
	if got := res.Header.Get("Allow"); !strings.Contains(got, "POST") {
		t.Fatalf("PUT rename Allow = %q", got)
	}

	res = stack.do(t, http.MethodDelete, "/api/v1/license", nil, nil, "")
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE license status = %d, want 405", res.StatusCode)
	}
	if got := res.Header.Get("Allow"); !strings.Contains(got, "GET") {
		t.Fatalf("DELETE license Allow = %q", got)
	}
}

func TestRouterRejectsBodiesBeyondConfiguredLimit(t *testing.T) {
	stack := newRouterStack(t)
	stack.buildRouter(t, func(cfg *httpserver.Config) { cfg.MaxBodyBytes = 64 })

	// Declared Content-Length beyond the limit is rejected before any read.
	res := stack.do(t, http.MethodPost, "/api/v1/sessions/revoke-all", nil, nil,
		`{"padding":"`+strings.Repeat("x", 200)+`"}`)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized declared body status = %d, want 413", res.StatusCode)
	}

	// An undeclared-length body fails during the read with the same status.
	_, session := stack.login(t, "1516563360")
	csrfCookie, csrfToken := stack.csrfFor(t, session)
	res = stack.doUnknownLength(t, http.MethodPost, "/api/v1/devices/device-a-1/rename",
		[]*http.Cookie{session, csrfCookie},
		http.Header{"Content-Type": []string{"application/json"}, "X-CSRF-Token": []string{csrfToken}},
		`{"name":"`+strings.Repeat("y", 200)+`"}`)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized streamed body status = %d, want 413", res.StatusCode)
	}

	// Bodies within the limit keep flowing to the handler.
	res = stack.do(t, http.MethodPost, "/api/v1/sessions/revoke-all", []*http.Cookie{session},
		http.Header{"X-CSRF-Token": []string{"irrelevant"}}, "")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("small body status = %d, want 403 (no CSRF pair)", res.StatusCode)
	}
}

func TestRouterMCPAndBridgeStayOutsideSessionAndCSRFMiddleware(t *testing.T) {
	stack := newRouterStack(t)
	_, webSession := stack.login(t, "1516563360")
	stack.buildRouter(t, func(cfg *httpserver.Config) {
		cfg.MCP = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Stub", "mcp")
			w.WriteHeader(http.StatusOK)
		})
		cfg.Bridge = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Stub", "bridge")
			w.WriteHeader(http.StatusOK)
		})
	})

	// No cookies: the endpoints answer through their own auth stacks.
	for path, marker := range map[string]string{"/mcp": "mcp", "/bridge": "bridge"} {
		res := stack.do(t, http.MethodPost, path, nil, nil, "")
		if res.StatusCode != http.StatusOK || res.Header.Get("X-Stub") != marker {
			t.Fatalf("%s without cookies: status=%d stub=%q", path, res.StatusCode, res.Header.Get("X-Stub"))
		}
	}

	// With a session cookie but no CSRF token, the cookie middleware must
	// not intercept: bearer/device auth stays the only gate.
	res := stack.do(t, http.MethodPost, "/mcp", []*http.Cookie{webSession}, nil, "")
	if res.StatusCode != http.StatusOK || res.Header.Get("X-Stub") != "mcp" {
		t.Fatalf("/mcp with session cookie: status=%d stub=%q", res.StatusCode, res.Header.Get("X-Stub"))
	}
	res = stack.do(t, http.MethodGet, "/bridge", []*http.Cookie{webSession}, nil, "")
	if res.StatusCode != http.StatusOK || res.Header.Get("X-Stub") != "bridge" {
		t.Fatalf("/bridge with session cookie: status=%d stub=%q", res.StatusCode, res.Header.Get("X-Stub"))
	}

	// A garbage session value must not be validated on these routes either.
	bogus := &http.Cookie{Name: session.CookieName, Value: "rks_forged"}
	res = stack.do(t, http.MethodPost, "/mcp", []*http.Cookie{bogus}, nil, "")
	if res.StatusCode != http.StatusOK || res.Header.Get("X-Stub") != "mcp" {
		t.Fatalf("/mcp with garbage session: status=%d stub=%q", res.StatusCode, res.Header.Get("X-Stub"))
	}
}

func TestRouterServesOAuthMetadataDocuments(t *testing.T) {
	stack := newRouterStack(t)

	protected := stack.do(t, http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil, nil, "")
	if protected.StatusCode != http.StatusOK {
		t.Fatalf("protected-resource metadata status = %d", protected.StatusCode)
	}
	var resourceDoc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
		ScopesSupported      []string `json:"scopes_supported"`
		BearerMethods        []string `json:"bearer_methods_supported"`
	}
	decodeJSON(t, protected, &resourceDoc)
	if resourceDoc.Resource != "https://gateway.example.test/mcp" {
		t.Fatalf("resource = %q", resourceDoc.Resource)
	}
	if len(resourceDoc.AuthorizationServers) != 1 || resourceDoc.AuthorizationServers[0] != "https://gateway.example.test" {
		t.Fatalf("authorization_servers = %q", resourceDoc.AuthorizationServers)
	}
	if len(resourceDoc.ScopesSupported) == 0 {
		t.Fatal("scopes_supported is empty")
	}

	authServer := stack.do(t, http.MethodGet, "/.well-known/oauth-authorization-server", nil, nil, "")
	if authServer.StatusCode != http.StatusOK {
		t.Fatalf("authorization-server metadata status = %d", authServer.StatusCode)
	}
	var serverDoc struct {
		Issuer                string   `json:"issuer"`
		AuthorizationEndpoint string   `json:"authorization_endpoint"`
		TokenEndpoint         string   `json:"token_endpoint"`
		RevocationEndpoint    string   `json:"revocation_endpoint"`
		GrantTypesSupported   []string `json:"grant_types_supported"`
	}
	decodeJSON(t, authServer, &serverDoc)
	if serverDoc.Issuer != "https://gateway.example.test" {
		t.Fatalf("issuer = %q", serverDoc.Issuer)
	}
	if serverDoc.AuthorizationEndpoint != "https://gateway.example.test/oauth/authorize" {
		t.Fatalf("authorization_endpoint = %q", serverDoc.AuthorizationEndpoint)
	}
	if serverDoc.TokenEndpoint != "https://gateway.example.test/oauth/token" {
		t.Fatalf("token_endpoint = %q", serverDoc.TokenEndpoint)
	}
	if serverDoc.RevocationEndpoint != "https://gateway.example.test/oauth/revoke" {
		t.Fatalf("revocation_endpoint = %q", serverDoc.RevocationEndpoint)
	}

	if res := stack.do(t, http.MethodPost, "/.well-known/oauth-authorization-server", nil, nil, ""); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST metadata status = %d, want 405", res.StatusCode)
	}
}

// --- Task 18: dashboard reads ---------------------------------------------

// dashboardFixture seeds two users with devices, Studio sessions, connector
// grants, a trial, and a license, and returns both sessions.
func dashboardFixture(t *testing.T, stack *routerStack) (userA robloxauth.User, sessionA *http.Cookie, userB robloxauth.User, sessionB *http.Cookie) {
	t.Helper()
	userA, sessionA = stack.login(t, "1516563360")
	userB, sessionB = stack.login(t, "4819574472")

	stack.insertDevice(t, userA.ID, "device-a-1", "Laptop A")
	stack.insertDevice(t, userA.ID, "device-a-2", "Desktop A")
	stack.insertDevice(t, userB.ID, "device-b-1", "Laptop B")

	stack.insertStudio(t, "studio-a-1", userA.ID, "device-a-1", "active")
	stack.insertStudio(t, "studio-a-2", userA.ID, "device-a-2", "ended")
	stack.insertStudio(t, "studio-b-1", userB.ID, "device-b-1", "active")

	stack.insertConnectorClient(t)
	stack.insertConnectorGrant(t, "grant-a-1", userA.ID, "device-a-1", "studio-a-1")
	stack.insertConnectorGrant(t, "grant-b-1", userB.ID, "device-b-1", "")
	stack.insertTrial(t, userA.ID)
	stack.insertLicense(t, "license-a-1", userA.ID, 2)
	stack.insertBinding(t, "binding-a-1", "license-a-1", userA.ID, "device-a-1")

	stack.registry.setOnline("device-a-1", true)
	return userA, sessionA, userB, sessionB
}

func TestRouterDashboardReadsRequireValidSession(t *testing.T) {
	stack := newRouterStack(t)

	paths := []string{
		"/api/v1/devices", "/api/v1/studios", "/api/v1/connectors",
		"/api/v1/license", "/api/v1/diagnostics",
	}
	for _, path := range paths {
		if res := stack.do(t, http.MethodGet, path, nil, nil, ""); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without session status = %d, want 401", path, res.StatusCode)
		}
		// A cookie holding a session-shaped but unknown token is rejected
		// by validation, not merely by presence.
		bogus := &http.Cookie{Name: session.CookieName, Value: "rks_forged_unknown_token"}
		if res := stack.do(t, http.MethodGet, path, []*http.Cookie{bogus}, nil, ""); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s with forged session status = %d, want 401", path, res.StatusCode)
		}
	}

	// Mutations require the session too, before CSRF details matter.
	mutations := []string{
		"/api/v1/devices/device-a-1/rename",
		"/api/v1/devices/device-a-1/revoke",
		"/api/v1/connectors/grant-a-1/target",
		"/api/v1/connectors/grant-a-1/revoke",
		"/api/v1/sessions/revoke-all",
	}
	for _, path := range mutations {
		if res := stack.do(t, http.MethodPost, path, nil, nil, ""); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without session status = %d, want 401", path, res.StatusCode)
		}
	}
}

func TestRouterDevicesListScopedToOwnerWithOnlineState(t *testing.T) {
	stack := newRouterStack(t)
	_, sessionA, _, sessionB := dashboardFixture(t, stack)

	res := stack.do(t, http.MethodGet, "/api/v1/devices", []*http.Cookie{sessionA}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("devices status = %d", res.StatusCode)
	}
	var payload struct {
		Devices []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Status    string `json:"status"`
			Online    bool   `json:"online"`
			CreatedAt string `json:"created_at"`
			UpdatedAt string `json:"updated_at"`
		} `json:"devices"`
	}
	decodeJSON(t, res, &payload)
	if len(payload.Devices) != 2 {
		t.Fatalf("user A devices = %+v, want 2", payload.Devices)
	}
	byID := map[string]bool{}
	for _, device := range payload.Devices {
		byID[device.ID] = true
		if device.Status != "active" || device.CreatedAt == "" || device.UpdatedAt == "" {
			t.Fatalf("device fields = %+v", device)
		}
	}
	if !byID["device-a-1"] || !byID["device-a-2"] {
		t.Fatalf("user A device ids = %+v", payload.Devices)
	}
	if payload.Devices[0].ID == "device-a-1" && !payload.Devices[0].Online {
		t.Fatalf("online flag for connected device-a-1 = %+v", payload.Devices[0])
	}

	// User B sees only the own device and nothing of user A.
	res = stack.do(t, http.MethodGet, "/api/v1/devices", []*http.Cookie{sessionB}, nil, "")
	decodeJSON(t, res, &payload)
	if len(payload.Devices) != 1 || payload.Devices[0].ID != "device-b-1" {
		t.Fatalf("user B devices = %+v, want only device-b-1", payload.Devices)
	}
	if payload.Devices[0].Online {
		t.Fatalf("device-b-1 reported online: %+v", payload.Devices[0])
	}
}

func TestRouterStudiosListScopedToOwner(t *testing.T) {
	stack := newRouterStack(t)
	_, sessionA, _, sessionB := dashboardFixture(t, stack)

	res := stack.do(t, http.MethodGet, "/api/v1/studios", []*http.Cookie{sessionA}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("studios status = %d", res.StatusCode)
	}
	var payload struct {
		Studios []struct {
			ID        string  `json:"id"`
			DeviceID  string  `json:"device_id"`
			StudioID  string  `json:"studio_id"`
			Status    string  `json:"status"`
			StartedAt string  `json:"started_at"`
			EndedAt   *string `json:"ended_at"`
		} `json:"studios"`
	}
	decodeJSON(t, res, &payload)
	if len(payload.Studios) != 2 {
		t.Fatalf("user A studios = %+v, want 2", payload.Studios)
	}
	for _, studio := range payload.Studios {
		if studio.DeviceID == "" || studio.StudioID == "" || studio.Status == "" || studio.StartedAt == "" {
			t.Fatalf("studio fields = %+v", studio)
		}
	}

	res = stack.do(t, http.MethodGet, "/api/v1/studios", []*http.Cookie{sessionB}, nil, "")
	decodeJSON(t, res, &payload)
	if len(payload.Studios) != 1 || payload.Studios[0].ID != "studio-b-1" {
		t.Fatalf("user B studios = %+v, want only studio-b-1", payload.Studios)
	}
}

func TestRouterConnectorsListScopedToOwner(t *testing.T) {
	stack := newRouterStack(t)
	_, sessionA, _, sessionB := dashboardFixture(t, stack)

	res := stack.do(t, http.MethodGet, "/api/v1/connectors", []*http.Cookie{sessionA}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("connectors status = %d", res.StatusCode)
	}
	var payload struct {
		Connectors []struct {
			ID              string   `json:"id"`
			ClientID        string   `json:"client_id"`
			ClientName      string   `json:"client_name"`
			Scopes          []string `json:"scopes"`
			Resource        string   `json:"resource"`
			DeviceID        string   `json:"device_id"`
			StudioSessionID *string  `json:"studio_session_id"`
			CreatedAt       string   `json:"created_at"`
			RevokedAt       *string  `json:"revoked_at"`
		} `json:"connectors"`
	}
	decodeJSON(t, res, &payload)
	if len(payload.Connectors) != 1 {
		t.Fatalf("user A connectors = %+v, want 1", payload.Connectors)
	}
	connector := payload.Connectors[0]
	if connector.ID != "grant-a-1" || connector.ClientName != "ChatGPT" ||
		connector.ClientID != "https://chatgpt.com/aip/mcp" ||
		connector.DeviceID != "device-a-1" ||
		connector.StudioSessionID == nil || *connector.StudioSessionID != "studio-a-1" ||
		connector.Resource != "https://gateway.example.test/mcp" ||
		connector.RevokedAt != nil || connector.CreatedAt == "" {
		t.Fatalf("connector = %+v", connector)
	}
	if len(connector.Scopes) == 0 {
		t.Fatalf("connector scopes = %+v", connector.Scopes)
	}

	res = stack.do(t, http.MethodGet, "/api/v1/connectors", []*http.Cookie{sessionB}, nil, "")
	decodeJSON(t, res, &payload)
	if len(payload.Connectors) != 1 || payload.Connectors[0].ID != "grant-b-1" {
		t.Fatalf("user B connectors = %+v, want only grant-b-1", payload.Connectors)
	}
	if payload.Connectors[0].StudioSessionID != nil {
		t.Fatalf("grant-b-1 studio target = %v, want null", *payload.Connectors[0].StudioSessionID)
	}
}

func TestRouterLicenseMirrorsTrialAndSlotState(t *testing.T) {
	stack := newRouterStack(t)
	_, sessionA, _, sessionB := dashboardFixture(t, stack)

	res := stack.do(t, http.MethodGet, "/api/v1/license", []*http.Cookie{sessionA}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("license status = %d", res.StatusCode)
	}
	var payload struct {
		Trial *struct {
			Active    bool   `json:"active"`
			StartedAt string `json:"started_at"`
			EndsAt    string `json:"ends_at"`
		} `json:"trial"`
		License *struct {
			Status         string `json:"status"`
			DeviceSlots    int    `json:"device_slots"`
			ActiveBindings int    `json:"active_bindings"`
		} `json:"license"`
	}
	decodeJSON(t, res, &payload)
	if payload.Trial == nil || !payload.Trial.Active {
		t.Fatalf("user A trial = %+v, want active", payload.Trial)
	}
	if !strings.HasPrefix(payload.Trial.StartedAt, "2026-09-01") || !strings.HasPrefix(payload.Trial.EndsAt, "2026-09-15") {
		t.Fatalf("trial window = %+v", payload.Trial)
	}
	if payload.License == nil || payload.License.Status != "active" ||
		payload.License.DeviceSlots != 2 || payload.License.ActiveBindings != 1 {
		t.Fatalf("user A license = %+v", payload.License)
	}

	// User B has neither trial nor license; both fields are null.
	res = stack.do(t, http.MethodGet, "/api/v1/license", []*http.Cookie{sessionB}, nil, "")
	decodeJSON(t, res, &payload)
	if payload.Trial != nil || payload.License != nil {
		t.Fatalf("user B license payload = trial:%+v license:%+v, want nulls", payload.Trial, payload.License)
	}
}

func TestRouterDiagnosticsSummary(t *testing.T) {
	stack := newRouterStack(t)
	_, sessionA, _, sessionB := dashboardFixture(t, stack)

	res := stack.do(t, http.MethodGet, "/api/v1/diagnostics", []*http.Cookie{sessionA}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("diagnostics status = %d", res.StatusCode)
	}
	var payload struct {
		Database          string `json:"database"`
		DevicesRegistered int    `json:"devices_registered"`
		DevicesOnline     int    `json:"devices_online"`
		StudioSessionsAct int    `json:"studio_sessions_active"`
	}
	decodeJSON(t, res, &payload)
	if payload.Database != "ok" {
		t.Fatalf("database = %q", payload.Database)
	}
	if payload.DevicesRegistered != 2 {
		t.Fatalf("devices_registered = %d, want 2", payload.DevicesRegistered)
	}
	if payload.DevicesOnline != 1 {
		t.Fatalf("devices_online = %d, want 1", payload.DevicesOnline)
	}
	if payload.StudioSessionsAct != 1 {
		t.Fatalf("studio_sessions_active = %d, want 1", payload.StudioSessionsAct)
	}

	// User B's diagnostics never count user A's rows.
	res = stack.do(t, http.MethodGet, "/api/v1/diagnostics", []*http.Cookie{sessionB}, nil, "")
	decodeJSON(t, res, &payload)
	if payload.DevicesRegistered != 1 || payload.DevicesOnline != 0 || payload.StudioSessionsAct != 1 {
		t.Fatalf("user B diagnostics = %+v", payload)
	}
}

// --- Task 18: dashboard mutations -----------------------------------------

func mutationCookies(t *testing.T, stack *routerStack, session *http.Cookie) ([]*http.Cookie, http.Header) {
	t.Helper()
	csrfCookie, csrfToken := stack.csrfFor(t, session)
	return []*http.Cookie{session, csrfCookie}, http.Header{"Content-Type": []string{"application/json"}, "X-CSRF-Token": []string{csrfToken}}
}

func TestRouterDeviceRenameRequiresCSRFAndOwnership(t *testing.T) {
	stack := newRouterStack(t)
	userA, sessionA, _, sessionB := dashboardFixture(t, stack)

	// Session without the CSRF pair is rejected before any write.
	res := stack.do(t, http.MethodPost, "/api/v1/devices/device-a-1/rename", []*http.Cookie{sessionA},
		http.Header{"Content-Type": []string{"application/json"}}, `{"name":"Nope"}`)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("rename without CSRF status = %d, want 403", res.StatusCode)
	}

	cookies, header := mutationCookies(t, stack, sessionA)

	// A user cannot rename another user's device: the row is invisible.
	res = stack.do(t, http.MethodPost, "/api/v1/devices/device-b-1/rename", cookies, header, `{"name":"Stolen"}`)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("rename foreign device status = %d, want 404", res.StatusCode)
	}
	if name, _ := stack.deviceRow(t, "device-b-1"); name != "Laptop B" {
		t.Fatalf("foreign device renamed to %q", name)
	}

	// Unknown device ids are equally rejected.
	if res := stack.do(t, http.MethodPost, "/api/v1/devices/missing/rename", cookies, header, `{"name":"X"}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("rename unknown device status = %d, want 404", res.StatusCode)
	}

	// Invalid names never reach the store.
	if res := stack.do(t, http.MethodPost, "/api/v1/devices/device-a-1/rename", cookies, header, `{"name":"   "}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("rename blank status = %d, want 400", res.StatusCode)
	}
	if res := stack.do(t, http.MethodPost, "/api/v1/devices/device-a-1/rename", cookies, header, `{"name":"`+strings.Repeat("n", 300)+`"}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("rename overlong status = %d, want 400", res.StatusCode)
	}

	// The owner renames the own device successfully and the change is
	// audited with before/after state.
	res = stack.do(t, http.MethodPost, "/api/v1/devices/device-a-1/rename", cookies, header, `{"name":"Primary Laptop"}`)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("rename status = %d", res.StatusCode)
	}
	if name, status := stack.deviceRow(t, "device-a-1"); name != "Primary Laptop" || status != "active" {
		t.Fatalf("device row = %q/%q", name, status)
	}
	events := stack.auditEvents(t, "device.rename")
	if len(events) != 1 || events[0].TargetID != "device-a-1" {
		t.Fatalf("device.rename events = %+v", events)
	}
	if !strings.Contains(events[0].Metadata, "Primary Laptop") || !strings.Contains(events[0].Metadata, "Laptop A") {
		t.Fatalf("rename audit metadata = %q", events[0].Metadata)
	}
	_ = userA
	_ = sessionB
}

func TestRouterDeviceRevokeRevokesCredentialsKeepsSlotAndDisconnects(t *testing.T) {
	stack := newRouterStack(t)
	userA, sessionA, _, sessionB := dashboardFixture(t, stack)
	stack.insertDeviceCredential(t, userA.ID, "device-a-2")

	cookies, header := mutationCookies(t, stack, sessionA)

	// Cross-user revoke is invisible.
	if res := stack.do(t, http.MethodPost, "/api/v1/devices/device-b-1/revoke", cookies, header, ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke foreign device status = %d, want 404", res.StatusCode)
	}
	if _, status := stack.deviceRow(t, "device-b-1"); status != "active" {
		t.Fatalf("foreign device status = %q", status)
	}

	res := stack.do(t, http.MethodPost, "/api/v1/devices/device-a-2/revoke", cookies, header, "")
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d", res.StatusCode)
	}
	if _, status := stack.deviceRow(t, "device-a-2"); status != "revoked" {
		t.Fatalf("device status = %q, want revoked", status)
	}

	// The device credential is revoked with it...
	var credentialRevoked sql.NullTime
	if err := stack.db.QueryRowContext(t.Context(),
		`SELECT revoked_at FROM device_credentials WHERE device_id = ?`, "device-a-2").Scan(&credentialRevoked); err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if !credentialRevoked.Valid {
		t.Fatal("device credential was not revoked")
	}

	// ...while the license slot stays occupied (revocation frees nothing).
	var bindingStatus string
	var bindingRevoked sql.NullTime
	if err := stack.db.QueryRowContext(t.Context(),
		`SELECT status, revoked_at FROM license_device_bindings WHERE id = 'binding-a-1'`).Scan(&bindingStatus, &bindingRevoked); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if bindingStatus != "active" || bindingRevoked.Valid {
		t.Fatalf("binding after revoke = %s/%v, want active/NULL", bindingStatus, bindingRevoked)
	}

	// The live connection is dropped immediately.
	if got := stack.registry.disconnects(); len(got) != 1 || got[0] != "device-a-2" {
		t.Fatalf("registry disconnects = %v, want [device-a-2]", got)
	}

	events := stack.auditEvents(t, "device.revoke")
	if len(events) != 1 || events[0].TargetID != "device-a-2" {
		t.Fatalf("device.revoke events = %+v", events)
	}

	// Revoking twice stays successful and idempotent.
	if res := stack.do(t, http.MethodPost, "/api/v1/devices/device-a-2/revoke", cookies, header, ""); res.StatusCode != http.StatusNoContent {
		t.Fatalf("second revoke status = %d", res.StatusCode)
	}
	if events := stack.auditEvents(t, "device.revoke"); len(events) != 1 {
		t.Fatalf("device.revoke audited twice: %+v", events)
	}
	_ = sessionB
}

func TestRouterConnectorTargetAndRevokeEnforceOwnership(t *testing.T) {
	stack := newRouterStack(t)
	userA, sessionA, _, sessionB := dashboardFixture(t, stack)
	stack.insertConnectorTokens(t, userA.ID, "grant-a-1")

	cookiesA, headerA := mutationCookies(t, stack, sessionA)
	cookiesB, headerB := mutationCookies(t, stack, sessionB)

	// Target changes require a device id.
	if res := stack.do(t, http.MethodPost, "/api/v1/connectors/grant-a-1/target", cookiesA, headerA, `{}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("target without device status = %d, want 400", res.StatusCode)
	}

	// Another user's grant is invisible.
	if res := stack.do(t, http.MethodPost, "/api/v1/connectors/grant-b-1/target", cookiesA, headerA, `{"device_id":"device-a-2"}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("target foreign grant status = %d, want 404", res.StatusCode)
	}

	// A foreign target device is equally rejected without leaking whether
	// the device exists.
	if res := stack.do(t, http.MethodPost, "/api/v1/connectors/grant-a-1/target", cookiesA, headerA, `{"device_id":"device-b-1"}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("target foreign device status = %d, want 404", res.StatusCode)
	}

	// A foreign Studio session is rejected too.
	if res := stack.do(t, http.MethodPost, "/api/v1/connectors/grant-a-1/target", cookiesA, headerA,
		`{"device_id":"device-a-2","studio_session_id":"studio-b-1"}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("target foreign studio status = %d, want 404", res.StatusCode)
	}

	// The owner retargets to the own device and Studio.
	res := stack.do(t, http.MethodPost, "/api/v1/connectors/grant-a-1/target", cookiesA, headerA,
		`{"device_id":"device-a-2","studio_session_id":"studio-a-2"}`)
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("target status = %d", res.StatusCode)
	}
	var deviceID, studioID sql.NullString
	if err := stack.db.QueryRowContext(t.Context(),
		`SELECT device_id, studio_session_id FROM oauth_grants WHERE id = 'grant-a-1'`).Scan(&deviceID, &studioID); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if deviceID.String != "device-a-2" || studioID.String != "studio-a-2" {
		t.Fatalf("grant target = %q/%q", deviceID.String, studioID.String)
	}
	events := stack.auditEvents(t, "connector.target")
	if len(events) != 1 || events[0].TargetID != "grant-a-1" {
		t.Fatalf("connector.target events = %+v", events)
	}

	// Clearing the Studio target with an empty value keeps the device.
	if res := stack.do(t, http.MethodPost, "/api/v1/connectors/grant-a-1/target", cookiesA, headerA,
		`{"device_id":"device-a-2","studio_session_id":""}`); res.StatusCode != http.StatusNoContent {
		t.Fatalf("target clear status = %d", res.StatusCode)
	}
	if err := stack.db.QueryRowContext(t.Context(),
		`SELECT device_id, studio_session_id FROM oauth_grants WHERE id = 'grant-a-1'`).Scan(&deviceID, &studioID); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if studioID.Valid {
		t.Fatalf("studio target after clear = %q, want NULL", studioID.String)
	}

	// Revoking another user's connector changes nothing.
	if res := stack.do(t, http.MethodPost, "/api/v1/connectors/grant-b-1/revoke", cookiesA, headerA, ""); res.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke foreign connector status = %d, want 404", res.StatusCode)
	}
	if stack.grantRevokedAt(t, "grant-b-1").Valid {
		t.Fatal("foreign connector grant was revoked")
	}

	// The owner revokes the own connector; grant and tokens go together.
	if res := stack.do(t, http.MethodPost, "/api/v1/connectors/grant-a-1/revoke", cookiesA, headerA, ""); res.StatusCode != http.StatusNoContent {
		t.Fatalf("connector revoke status = %d", res.StatusCode)
	}
	if !stack.grantRevokedAt(t, "grant-a-1").Valid {
		t.Fatal("connector grant was not revoked")
	}
	var accessRevoked, refreshRevoked sql.NullTime
	if err := stack.db.QueryRowContext(t.Context(),
		`SELECT revoked_at FROM oauth_access_tokens WHERE grant_id = 'grant-a-1'`).Scan(&accessRevoked); err != nil {
		t.Fatalf("read access token: %v", err)
	}
	if err := stack.db.QueryRowContext(t.Context(),
		`SELECT revoked_at FROM oauth_refresh_tokens WHERE grant_id = 'grant-a-1'`).Scan(&refreshRevoked); err != nil {
		t.Fatalf("read refresh token: %v", err)
	}
	if !accessRevoked.Valid || !refreshRevoked.Valid {
		t.Fatalf("tokens after grant revoke = access:%v refresh:%v", accessRevoked.Valid, refreshRevoked.Valid)
	}
	events = stack.auditEvents(t, "connector.revoke")
	if len(events) != 1 || events[0].TargetID != "grant-a-1" {
		t.Fatalf("connector.revoke events = %+v", events)
	}

	// A revoked connector no longer accepts target changes.
	if res := stack.do(t, http.MethodPost, "/api/v1/connectors/grant-a-1/target", cookiesA, headerA,
		`{"device_id":"device-a-1"}`); res.StatusCode != http.StatusNotFound {
		t.Fatalf("target revoked grant status = %d, want 404", res.StatusCode)
	}

	// Revoking twice stays successful without a second audit event.
	if res := stack.do(t, http.MethodPost, "/api/v1/connectors/grant-a-1/revoke", cookiesA, headerA, ""); res.StatusCode != http.StatusNoContent {
		t.Fatalf("second connector revoke status = %d", res.StatusCode)
	}
	if events := stack.auditEvents(t, "connector.revoke"); len(events) != 1 {
		t.Fatalf("connector.revoke audited twice: %+v", events)
	}
	_ = cookiesB
	_ = headerB
}

func TestRouterRevokeAllSessionsRevokesOnlyOwnSessions(t *testing.T) {
	stack := newRouterStack(t)
	_, sessionA, _, sessionB := dashboardFixture(t, stack)

	cookies, header := mutationCookies(t, stack, sessionA)
	res := stack.do(t, http.MethodPost, "/api/v1/sessions/revoke-all", cookies, header, "")
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke-all status = %d", res.StatusCode)
	}

	var cleared bool
	for _, cookie := range res.Cookies() {
		if cookie.Name == session.CookieName && cookie.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("revoke-all did not clear the session cookie: %#v", res.Cookies())
	}

	// The caller's sessions are gone...
	if res := stack.do(t, http.MethodGet, "/api/v1/me", []*http.Cookie{sessionA}, nil, ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-revoke me status = %d, want 401", res.StatusCode)
	}
	// ...while the other user's session survives untouched.
	if res := stack.do(t, http.MethodGet, "/api/v1/me", []*http.Cookie{sessionB}, nil, ""); res.StatusCode != http.StatusOK {
		t.Fatalf("other user me status = %d, want 200", res.StatusCode)
	}
}
