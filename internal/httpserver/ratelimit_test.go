package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"robloxkit/internal/mcpoauth"
	"robloxkit/internal/mysqlstore"
	"robloxkit/internal/robloxauth"
	"robloxkit/internal/session"
)

// limiterClock is a mutable time source for deterministic window tests.
type limiterClock struct{ now time.Time }

func (c *limiterClock) Now() time.Time { return c.now }

func newTestLimiter(t *testing.T, cfg MCPLimiterConfig) (*MCPLimiter, *limiterClock) {
	t.Helper()
	clock := &limiterClock{now: time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)}
	cfg.Now = clock.Now
	limiter, err := NewMCPLimiter(cfg)
	if err != nil {
		t.Fatalf("NewMCPLimiter: %v", err)
	}
	return limiter, clock
}

func TestRateLimiterAllowsBurstThenRejects(t *testing.T) {
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 3, Window: time.Minute, MaxInFlight: 4})
	for i := range 3 {
		if !limiter.Allow("grant:a") {
			t.Fatalf("request %d within burst rejected", i+1)
		}
	}
	if limiter.Allow("grant:a") {
		t.Fatal("request beyond burst allowed")
	}
}

func TestRateLimiterWindowResetsBurst(t *testing.T) {
	limiter, clock := newTestLimiter(t, MCPLimiterConfig{Requests: 1, Window: time.Minute, MaxInFlight: 4})
	if !limiter.Allow("user:u") {
		t.Fatal("first request in window rejected")
	}
	if limiter.Allow("user:u") {
		t.Fatal("second request in same window allowed")
	}
	clock.now = clock.now.Add(2 * time.Minute)
	if !limiter.Allow("user:u") {
		t.Fatal("request in a fresh window rejected")
	}
}

func TestRateLimiterKeysAreIndependent(t *testing.T) {
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 1, Window: time.Minute, MaxInFlight: 4})
	if !limiter.Allow("grant:a") {
		t.Fatal("grant a first request rejected")
	}
	if !limiter.Allow("grant:b") {
		t.Fatal("grant b first request rejected by grant a's exhaustion")
	}
	if limiter.Allow("grant:a") {
		t.Fatal("grant a second request allowed")
	}
}

func TestRateLimiterConcurrentInFlightExhaustion(t *testing.T) {
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 100, Window: time.Minute, MaxInFlight: 2})
	first, ok := limiter.Acquire("grant:a")
	if !ok {
		t.Fatal("first in-flight slot rejected")
	}
	second, ok := limiter.Acquire("grant:a")
	if !ok {
		t.Fatal("second in-flight slot rejected")
	}
	if _, ok := limiter.Acquire("grant:a"); ok {
		t.Fatal("third concurrent in-flight slot allowed")
	}
	second()
	if release, ok := limiter.Acquire("grant:a"); !ok {
		t.Fatal("released slot not reusable")
	} else {
		release()
	}
	first()
	first() // release must be idempotent
	if release, ok := limiter.Acquire("grant:a"); !ok {
		t.Fatal("slot not freed after releases")
	} else {
		release()
	}
	if got := limiter.InFlight("grant:a"); got != 0 {
		t.Fatalf("InFlight after full release = %d, want 0", got)
	}
}

func TestRateLimiterInFlightKeysAreIndependent(t *testing.T) {
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 100, Window: time.Minute, MaxInFlight: 1})
	release, ok := limiter.Acquire("user:one")
	if !ok {
		t.Fatal("user one slot rejected")
	}
	if _, ok := limiter.Acquire("user:two"); !ok {
		t.Fatal("user two rejected because user one holds a slot")
	}
	release()
}

func TestRateLimiterRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  MCPLimiterConfig
	}{
		{"zero requests", MCPLimiterConfig{Window: time.Minute, MaxInFlight: 1}},
		{"negative requests", MCPLimiterConfig{Requests: -1, Window: time.Minute, MaxInFlight: 1}},
		{"zero window", MCPLimiterConfig{Requests: 1, MaxInFlight: 1}},
		{"zero in-flight", MCPLimiterConfig{Requests: 1, Window: time.Minute}},
	}
	for _, tt := range tests {
		if _, err := NewMCPLimiter(tt.cfg); err == nil {
			t.Errorf("%s: NewMCPLimiter unexpectedly succeeded", tt.name)
		}
	}
}

func TestRateLimiterBoundedKeyCountFailsClosed(t *testing.T) {
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 10, Window: time.Minute, MaxInFlight: 2, MaxKeys: 2})
	if !limiter.Allow("grant:a") || !limiter.Allow("grant:b") {
		t.Fatal("keys within the cap rejected")
	}
	if limiter.Allow("grant:c") {
		t.Fatal("key beyond the cap allowed; the limiter must fail closed")
	}
	if _, ok := limiter.Acquire("user:c"); ok {
		t.Fatal("in-flight key beyond the cap allowed; the limiter must fail closed")
	}
}

func TestRateLimiterPrunesStaleWindows(t *testing.T) {
	limiter, clock := newTestLimiter(t, MCPLimiterConfig{Requests: 1, Window: time.Minute, MaxInFlight: 2, MaxKeys: 2})
	if !limiter.Allow("grant:a") {
		t.Fatal("first key rejected")
	}
	clock.now = clock.now.Add(3 * time.Minute)
	// The stale grant:a bucket is swept on the next operation, freeing room
	// for a new key without exceeding the key cap.
	if !limiter.Allow("grant:b") {
		t.Fatal("new key rejected because stale buckets were not pruned")
	}
	if !limiter.Allow("grant:c") {
		t.Fatal("key after pruning rejected")
	}
}

func TestRateLimiterRejectsNegativeClockRegression(t *testing.T) {
	limiter, clock := newTestLimiter(t, MCPLimiterConfig{Requests: 1, Window: time.Minute, MaxInFlight: 1})
	if !limiter.Allow("grant:a") {
		t.Fatal("request rejected")
	}
	clock.now = clock.now.Add(-2 * time.Minute)
	if limiter.Allow("grant:a") {
		t.Fatal("clock regression resurrected a stale window allowance")
	}
}

func TestRateLimiterKeysAreWellFormed(t *testing.T) {
	// Callers construct the key namespace; the limiter itself must treat
	// keys opaquely, which this test pins by exercising a path-like key.
	limiter, _ := newTestLimiter(t, MCPLimiterConfig{Requests: 1, Window: time.Minute, MaxInFlight: 1})
	key := "grant:" + strings.Repeat("d", 36)
	if !limiter.Allow(key) {
		t.Fatal("opaque key rejected")
	}
}

// --- general keyed endpoint limiter (Task 21) ---

func newClassLimiter(t *testing.T, cfg LimiterConfig) *Limiter {
	t.Helper()
	limiter, err := NewLimiter(cfg)
	if err != nil {
		t.Fatalf("NewLimiter: %v", err)
	}
	return limiter
}

func testBudget(burst int) Budget {
	return Budget{Burst: burst, Refill: 1, Interval: time.Second}
}

func TestNewLimiterRejectsInvalidBudgets(t *testing.T) {
	tests := []struct {
		name string
		cfg  LimiterConfig
	}{
		{"no budgets", LimiterConfig{}},
		{"zero burst", LimiterConfig{Budgets: map[Class]Budget{ClassLogin: {Refill: 1, Interval: time.Second}}}},
		{"zero refill", LimiterConfig{Budgets: map[Class]Budget{ClassLogin: {Burst: 1, Interval: time.Second}}}},
		{"zero interval", LimiterConfig{Budgets: map[Class]Budget{ClassLogin: {Burst: 1, Refill: 1}}}},
		{"negative in-flight", LimiterConfig{Budgets: map[Class]Budget{ClassLogin: {Burst: 1, Refill: 1, Interval: time.Second, MaxInFlight: -1}}}},
	}
	for _, tt := range tests {
		if _, err := NewLimiter(tt.cfg); err == nil {
			t.Errorf("%s: NewLimiter unexpectedly succeeded", tt.name)
		}
	}
}

func TestRateLimitTokenBucketAllowsBurstThenRefills(t *testing.T) {
	limiter := newClassLimiter(t, LimiterConfig{
		Budgets: map[Class]Budget{ClassLogin: testBudget(2)},
	})
	start := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	key := Key{Class: ClassLogin, ID: "10.0.0.1"}

	if !limiter.Allow(start, key, 1).Allowed {
		t.Fatal("first burst token denied")
	}
	if !limiter.Allow(start, key, 1).Allowed {
		t.Fatal("second burst token denied")
	}
	denied := limiter.Allow(start, key, 1)
	if denied.Allowed {
		t.Fatal("third request allowed beyond burst")
	}
	if denied.RetryAfter <= 0 {
		t.Fatalf("denied decision carries RetryAfter %v, want positive", denied.RetryAfter)
	}

	// One second refills one token.
	if !limiter.Allow(start.Add(time.Second), key, 1).Allowed {
		t.Fatal("refilled token denied")
	}
	if limiter.Allow(start.Add(time.Second), key, 1).Allowed {
		t.Fatal("request beyond the refill allowed")
	}
	// Waiting a full burst window restores the whole burst.
	if !limiter.Allow(start.Add(2*time.Second), key, 1).Allowed {
		t.Fatal("refilled token after two intervals denied")
	}
}

func TestRateLimitClassesHaveDistinctBudgets(t *testing.T) {
	limiter := newClassLimiter(t, LimiterConfig{
		Budgets: map[Class]Budget{
			ClassLogin: testBudget(1),
			ClassOAuth: {Burst: 5, Refill: 1, Interval: time.Second},
		},
	})
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	login := Key{Class: ClassLogin, ID: "10.0.0.1"}
	oauth := Key{Class: ClassOAuth, ID: "10.0.0.1"}

	if !limiter.Allow(now, login, 1).Allowed {
		t.Fatal("login burst denied")
	}
	if limiter.Allow(now, login, 1).Allowed {
		t.Fatal("login beyond burst allowed")
	}
	// Exhausting the login budget must not touch the OAuth budget.
	for i := range 5 {
		if !limiter.Allow(now, oauth, 1).Allowed {
			t.Fatalf("oauth request %d denied by login exhaustion", i+1)
		}
	}
	if limiter.Allow(now, oauth, 1).Allowed {
		t.Fatal("oauth beyond its own burst allowed")
	}
}

func TestRateLimitKeysAreIndependent(t *testing.T) {
	limiter := newClassLimiter(t, LimiterConfig{
		Budgets: map[Class]Budget{ClassWSS: testBudget(1)},
	})
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	first := Key{Class: ClassWSS, ID: "10.0.0.1"}
	second := Key{Class: ClassWSS, ID: "10.0.0.2"}

	if !limiter.Allow(now, first, 1).Allowed {
		t.Fatal("first key denied")
	}
	if !limiter.Allow(now, second, 1).Allowed {
		t.Fatal("second key denied by the first key's exhaustion")
	}
	if limiter.Allow(now, first, 1).Allowed {
		t.Fatal("first key beyond burst allowed")
	}
}

func TestRateLimitCostConsumesMultipleTokens(t *testing.T) {
	limiter := newClassLimiter(t, LimiterConfig{
		Budgets: map[Class]Budget{ClassAdmin: testBudget(3)},
	})
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	key := Key{Class: ClassAdmin, ID: "11111111-2222-3333-4444-555555555555"}

	if !limiter.Allow(now, key, 2).Allowed {
		t.Fatal("two-token cost denied with a full burst")
	}
	if !limiter.Allow(now, key, 1).Allowed {
		t.Fatal("one-token cost denied with one token left")
	}
	if limiter.Allow(now, key, 1).Allowed {
		t.Fatal("request allowed after the burst was consumed")
	}
}

func TestRateLimitUnknownClassFailsClosed(t *testing.T) {
	limiter := newClassLimiter(t, LimiterConfig{
		Budgets: map[Class]Budget{ClassLogin: testBudget(10)},
	})
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	decision := limiter.Allow(now, Key{Class: Class("nope"), ID: "10.0.0.1"}, 1)
	if decision.Allowed {
		t.Fatal("unconfigured class allowed; the limiter must fail closed")
	}
	if decision.RetryAfter <= 0 {
		t.Fatalf("unconfigured class RetryAfter = %v, want positive", decision.RetryAfter)
	}
}

func TestRateLimitNonPositiveCostFailsClosed(t *testing.T) {
	limiter := newClassLimiter(t, LimiterConfig{
		Budgets: map[Class]Budget{ClassLogin: testBudget(10)},
	})
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	key := Key{Class: ClassLogin, ID: "10.0.0.1"}
	for _, cost := range []int{0, -1} {
		if limiter.Allow(now, key, cost).Allowed {
			t.Fatalf("cost %d allowed; the limiter must fail closed", cost)
		}
	}
}

func TestRateLimitClockRegressionFailsClosed(t *testing.T) {
	limiter := newClassLimiter(t, LimiterConfig{
		Budgets: map[Class]Budget{ClassLogin: testBudget(10)},
	})
	now := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	key := Key{Class: ClassLogin, ID: "10.0.0.1"}
	if !limiter.Allow(now, key, 1).Allowed {
		t.Fatal("request denied")
	}
	if limiter.Allow(now.Add(-time.Hour), key, 1).Allowed {
		t.Fatal("clock regression resurrected spent tokens")
	}
}

func TestRateLimitInFlightBoundsRequestScopedClass(t *testing.T) {
	limiter := newClassLimiter(t, LimiterConfig{
		Budgets: map[Class]Budget{
			ClassMCP: {Burst: 100, Refill: 1, Interval: time.Second, MaxInFlight: 2},
		},
	})
	key := Key{Class: ClassMCP, ID: "grant-a"}

	first, ok := limiter.Acquire(key)
	if !ok {
		t.Fatal("first in-flight slot rejected")
	}
	second, ok := limiter.Acquire(key)
	if !ok {
		t.Fatal("second in-flight slot rejected")
	}
	if _, ok := limiter.Acquire(key); ok {
		t.Fatal("third concurrent in-flight slot allowed")
	}
	if got := limiter.InFlight(key); got != 2 {
		t.Fatalf("InFlight = %d, want 2", got)
	}
	second()
	if got := limiter.InFlight(key); got != 1 {
		t.Fatalf("InFlight after release = %d, want 1", got)
	}
	release, ok := limiter.Acquire(key)
	if !ok {
		t.Fatal("released slot not reusable")
	}
	release()
	first()
	first() // release must be idempotent
	if got := limiter.InFlight(key); got != 0 {
		t.Fatalf("InFlight after full release = %d, want 0", got)
	}
	// Keys are independent in-flight namespaces too.
	other := Key{Class: ClassMCP, ID: "grant-b"}
	if _, ok := limiter.Acquire(other); !ok {
		t.Fatal("second key rejected because the first key held slots")
	}
}

func TestRateLimitInFlightRequiresInFlightBudget(t *testing.T) {
	limiter := newClassLimiter(t, LimiterConfig{
		Budgets: map[Class]Budget{ClassLogin: testBudget(10)},
	})
	if _, ok := limiter.Acquire(Key{Class: ClassLogin, ID: "10.0.0.1"}); ok {
		t.Fatal("in-flight slot granted for a class without an in-flight budget")
	}
}

func TestRateLimitMiddlewareWrites429WithRetryAfterAndSanitizedBody(t *testing.T) {
	limiter := newClassLimiter(t, LimiterConfig{
		Budgets: map[Class]Budget{ClassLogin: testBudget(1)},
	})
	handler := limiter.Middleware(ClassLogin, func(r *http.Request) string {
		return "192.0.2.1"
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/roblox/login", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec.Code)
	}
	if retry := rec.Header().Get("Retry-After"); retry == "" {
		t.Fatal("429 response missing Retry-After")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "rate limit exceeded") {
		t.Fatalf("429 body = %q, want the sanitized message", body)
	}
	if strings.Contains(body, "192.0.2.1") {
		t.Fatalf("429 body leaks the rate key: %q", body)
	}
}

func TestRateLimitMiddlewareNilLimiterPassesThrough(t *testing.T) {
	var limiter *Limiter
	handler := limiter.Middleware(ClassLogin, func(r *http.Request) string { return "x" })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("nil limiter status = %d, want the handler to run", rec.Code)
	}
}

func TestRemotePrincipalUsesHostPart(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.7:5678"
	if got := RemotePrincipal(req); got != "203.0.113.7" {
		t.Fatalf("RemotePrincipal = %q, want the host part", got)
	}
	req.RemoteAddr = "203.0.113.9"
	if got := RemotePrincipal(req); got != "203.0.113.9" {
		t.Fatalf("RemotePrincipal without port = %q, want the address", got)
	}
}

// --- router wiring (MySQL-backed; skips without MYSQL_TEST_DSN) ---

// hitHandler is a stub endpoint that records how many requests reached it.
type hitHandler struct {
	mu   sync.Mutex
	hits int
}

func (h *hitHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.hits++
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (h *hitHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hits
}

type limitStack struct {
	db           *sql.DB
	router       http.Handler
	sessions     *session.Service
	identities   *mysqlstore.IdentityStore
	entitlements *entitlement.Service
	enrollment   *device.Enrollment
	deviceStore  *mysqlstore.DeviceStore
	mcp          *hitHandler
	bridge       *hitHandler
	oauth        *hitHandler
	adminCookies []*http.Cookie
}

func limitTestDatabase(t *testing.T) *sql.DB {
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
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping MYSQL_TEST_DSN: %v", err)
	}
	dbName := fmt.Sprintf("robloxkit_limit_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+dbName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create temporary database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+dbName+"`")
	})
	target := *base
	target.DBName = dbName
	target.ParseTime = true
	target.Loc = time.UTC
	db, err := sql.Open("mysql", target.FormatDSN())
	if err != nil {
		t.Fatalf("open temporary database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := mysqlstore.Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

// newLimitStack composes the production router with endpoint rate limits
// configured. mutate may adjust the configuration before validation.
func newLimitStack(t *testing.T, budgets map[Class]Budget, mutate func(cfg *Config, stack *limitStack)) *limitStack {
	t.Helper()
	db := limitTestDatabase(t)
	limiter := newClassLimiter(t, LimiterConfig{Budgets: budgets})
	pepper := []byte("limit-test-pepper")
	sessions := session.NewService(mysqlstore.NewSessionStore(db), pepper, time.Hour)
	identities := mysqlstore.NewIdentityStore(db)
	auditSvc := audit.NewService(mysqlstore.NewAuditStore(db))
	clock := func() time.Time { return time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC) }
	entitlements := entitlement.NewService(mysqlstore.NewEntitlementStore(db, entClock{now: clock()}, auditSvc), entClock{now: clock()})
	deviceStore := mysqlstore.NewDeviceStore(db)
	dashboard := mysqlstore.NewDashboardStore(db, auditSvc, pepper)
	enrollment, err := device.NewEnrollment(deviceStore, entitlements, pepper, clock)
	if err != nil {
		t.Fatalf("construct enrollment: %v", err)
	}
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
		mustLimitURL(t, "https://gateway.example.test"),
		mustLimitURL(t, "https://gateway.example.test/mcp"),
		mcpoauth.SupportedScopes,
	)
	if err != nil {
		t.Fatalf("construct oauth metadata: %v", err)
	}
	staticDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("<html>ok</html>"), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	mcpStub, bridgeStub, oauthStub := &hitHandler{}, &hitHandler{}, &hitHandler{}
	stack := &limitStack{
		db: db, sessions: sessions, identities: identities, entitlements: entitlements,
		enrollment: enrollment, deviceStore: deviceStore,
		mcp: mcpStub, bridge: bridgeStub, oauth: oauthStub,
	}
	cfg := Config{
		Sessions:         sessions,
		RobloxAuth:       &robloxauth.Handler{SuccessRedirect: "/"},
		IdentityReader:   deviceStore,
		Entitlements:     entitlements,
		Download:         download,
		DownloadMetadata: downloadMetadata,
		Enrollment:       enrollment,
		Dashboard:        dashboard,
		Health:           health.NewHandler(db, nil),
		Metadata:         &metadata,
		AllowedOrigin:    mustLimitURL(t, "https://app.example.test"),
		Limits:           limiter,
		MCP:              mcpStub,
		Bridge:           bridgeStub,
		OAuth:            oauthStub,
		StaticDir:        staticDir,
	}
	if mutate != nil {
		mutate(&cfg, stack)
	}
	router, err := NewRouter(cfg)
	if err != nil {
		t.Fatalf("construct router: %v", err)
	}
	stack.router = router
	return stack
}

type entClock struct{ now time.Time }

func (c entClock) Now() time.Time { return c.now }

func mustLimitURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return parsed
}

func (s *limitStack) do(t *testing.T, method, path string, cookies []*http.Cookie, header http.Header, body string) *http.Response {
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

func (s *limitStack) login(t *testing.T, subject string) (*http.Cookie, string) {
	t.Helper()
	user, err := s.identities.UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{
		Subject: subject, Username: subject, DisplayName: "Builder " + subject,
	})
	if err != nil {
		t.Fatalf("upsert identity: %v", err)
	}
	plain, _, err := s.sessions.Create(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return &http.Cookie{Name: session.CookieName, Value: plain}, user.ID
}

func (s *limitStack) csrfPair(t *testing.T, sessionCookie *http.Cookie) ([]*http.Cookie, http.Header) {
	t.Helper()
	res := s.do(t, http.MethodGet, "/api/v1/csrf", []*http.Cookie{sessionCookie}, nil, "")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("csrf issue status = %d", res.StatusCode)
	}
	var payload struct {
		Token string `json:"csrf_token"`
	}
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decode csrf: %v", err)
	}
	var csrfCookie *http.Cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == CSRFCookieName {
			csrfCookie = cookie
		}
	}
	if csrfCookie == nil || payload.Token == "" {
		t.Fatal("csrf issuance missing token or cookie")
	}
	return []*http.Cookie{sessionCookie, csrfCookie}, http.Header{
		"Content-Type": []string{"application/json"},
		"X-CSRF-Token": []string{payload.Token},
	}
}

func assertRateLimited(t *testing.T, res *http.Response, where string) {
	t.Helper()
	defer res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("%s status = %d, want 429", where, res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Fatalf("%s 429 missing Retry-After", where)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "rate limit exceeded") {
		t.Fatalf("%s 429 body = %q, want the sanitized message", where, body)
	}
}

func TestRouterLimitsLoginBeginBurst(t *testing.T) {
	stack := newLimitStack(t, map[Class]Budget{ClassLogin: testBudget(2)}, nil)
	for i := range 2 {
		res := stack.do(t, http.MethodGet, "/api/v1/auth/roblox/login", nil, nil, "")
		res.Body.Close()
		if res.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("login request %d rate limited within burst", i+1)
		}
	}
	assertRateLimited(t, stack.do(t, http.MethodGet, "/api/v1/auth/roblox/login", nil, nil, ""), "third login")
}

func TestRouterLimitBudgetsArePerClass(t *testing.T) {
	stack := newLimitStack(t, map[Class]Budget{
		ClassLogin:  testBudget(1),
		ClassEnroll: testBudget(50),
	}, nil)
	// Exhaust the login budget.
	stack.do(t, http.MethodGet, "/api/v1/auth/roblox/login", nil, nil, "").Body.Close()
	assertRateLimited(t, stack.do(t, http.MethodGet, "/api/v1/auth/roblox/login", nil, nil, ""), "second login")
	// The enrollment budget is untouched.
	res := stack.do(t, http.MethodPost, "/api/v1/device/enrollment/begin", nil,
		http.Header{"Content-Type": []string{"application/json"}},
		`{"device_id":"device-limit-1","hostname":"LIMIT-TEST","platform":"windows","bridge_version":"1.4.2"}`)
	res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		t.Fatal("enrollment begin rate limited by login exhaustion")
	}
}

func TestRouterLimitsEnrollmentBeginBurst(t *testing.T) {
	stack := newLimitStack(t, map[Class]Budget{ClassEnroll: testBudget(2)}, nil)
	body := `{"device_id":"device-limit-2","hostname":"LIMIT-TEST","platform":"windows","bridge_version":"1.4.2"}`
	for i := range 2 {
		res := stack.do(t, http.MethodPost, "/api/v1/device/enrollment/begin", nil,
			http.Header{"Content-Type": []string{"application/json"}}, body)
		res.Body.Close()
		if res.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("enrollment begin %d rate limited within burst", i+1)
		}
	}
	assertRateLimited(t, stack.do(t, http.MethodPost, "/api/v1/device/enrollment/begin", nil,
		http.Header{"Content-Type": []string{"application/json"}}, body), "third enrollment begin")
}

func TestRouterLimitsOAuthMount(t *testing.T) {
	stack := newLimitStack(t, map[Class]Budget{ClassOAuth: testBudget(1)}, nil)
	res := stack.do(t, http.MethodGet, "/oauth/authorize", nil, nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first oauth request status = %d, want 200", res.StatusCode)
	}
	assertRateLimited(t, stack.do(t, http.MethodGet, "/oauth/authorize", nil, nil, ""), "second oauth")
	if stack.oauth.count() != 1 {
		t.Fatalf("oauth handler hits = %d, want 1 (denied requests never reach it)", stack.oauth.count())
	}
}

func TestRouterLimitsWSSDial(t *testing.T) {
	stack := newLimitStack(t, map[Class]Budget{ClassWSS: testBudget(1)}, nil)
	res := stack.do(t, http.MethodGet, "/bridge", nil, nil, "")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first dial status = %d, want 200", res.StatusCode)
	}
	assertRateLimited(t, stack.do(t, http.MethodGet, "/bridge", nil, nil, ""), "second dial")
}

func TestRouterLimitsMCPMount(t *testing.T) {
	stack := newLimitStack(t, map[Class]Budget{ClassMCP: testBudget(1)}, nil)
	res := stack.do(t, http.MethodPost, "/mcp", nil, http.Header{"Content-Type": []string{"application/json"}}, "{}")
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("first mcp status = %d, want 200", res.StatusCode)
	}
	assertRateLimited(t, stack.do(t, http.MethodPost, "/mcp", nil, http.Header{"Content-Type": []string{"application/json"}}, "{}"), "second mcp")
}

func TestRouterLimitsAdminExecutesPerUser(t *testing.T) {
	stack := newLimitStack(t, map[Class]Budget{ClassAdmin: testBudget(1)}, func(cfg *Config, stack *limitStack) {
		cookieA, userA := stack.login(t, "limit-admin-a")
		cookieB, userB := stack.login(t, "limit-admin-b")
		stack.adminCookies = []*http.Cookie{cookieA, cookieB}
		cfg.Admin = &AdminConfig{
			Entitlements: stack.entitlements,
			OAuth:        mysqlstore.NewOAuthStore(stack.db),
			AdminUsers:   []string{userA, userB},
		}
	})

	// Admin A's first execute consumes the per-user budget; the response is
	// the handler's own (invalid-body) answer, not a rate limit.
	cookiesA, headerA := stack.csrfPair(t, stack.adminCookies[0])
	res := stack.do(t, http.MethodPost, "/api/v1/admin/trial-extensions", cookiesA, headerA, `{}`)
	res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		t.Fatal("admin A's first execute rate limited within burst")
	}
	assertRateLimited(t, stack.do(t, http.MethodPost, "/api/v1/admin/trial-extensions", cookiesA, headerA, `{}`),
		"admin A's second execute")

	// Admin B's budget is independent: the first execute is not a 429.
	cookiesB, headerB := stack.csrfPair(t, stack.adminCookies[1])
	res = stack.do(t, http.MethodPost, "/api/v1/admin/trial-extensions", cookiesB, headerB, `{}`)
	res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		t.Fatal("admin B's first execute rate limited by admin A's exhaustion")
	}
}

func TestRouterUnlimitedEndpointsStayUnlimited(t *testing.T) {
	stack := newLimitStack(t, map[Class]Budget{
		ClassLogin: testBudget(1), ClassEnroll: testBudget(1), ClassOAuth: testBudget(1),
		ClassWSS: testBudget(1), ClassAdmin: testBudget(1), ClassMCP: testBudget(1),
	}, nil)
	paths := []string{"/healthz", "/readyz", "/", "/.well-known/oauth-protected-resource"}
	for _, path := range paths {
		for range 25 {
			res := stack.do(t, http.MethodGet, path, nil, nil, "")
			res.Body.Close()
			if res.StatusCode == http.StatusTooManyRequests {
				t.Fatalf("%s rate limited; the endpoint must stay unlimited", path)
			}
		}
	}
}
