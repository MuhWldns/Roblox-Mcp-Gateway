package mcpoauth_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"robloxkit/internal/audit"
	"robloxkit/internal/credential"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/mcpoauth"
	"robloxkit/internal/mysqlstore"
	"robloxkit/internal/session"
)

// PKCE example pair from RFC 7636 appendix B.
const (
	mcpTestVerifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	mcpTestChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	mcpTestRedirect  = "https://connector.example/callback"
	mcpTestResource  = "https://gateway.example.com/mcp"
	mcpTestLogin     = "/login"
	mcpGrantAudit    = "connector.grant.approve"
)

var mcpTestPepper = []byte("mcpoauth-provider-test-pepper")

// mcpClock feeds the entitlement service with real time. Note: -race is
// unavailable in this environment (CGO_ENABLED=0); concurrency safety is
// exercised logically through the committed transactional store instead.
type mcpClock struct{}

func (mcpClock) Now() time.Time { return time.Now().UTC() }

func mcpUUID(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func mcpTestNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// mcpTestDatabase creates a uniquely named, fully migrated temporary MySQL
// database per test, mirroring the mysqlstore test pattern. The original
// helper lives in another package's test files and cannot be imported.
func mcpTestDatabase(t *testing.T) *sql.DB {
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
	dbName := fmt.Sprintf("robloxkit_mcp_test_%d", time.Now().UnixNano())
	if !mcpSafeIdentifier(dbName) {
		t.Fatal("generated unsafe temporary database name")
	}
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

// mcpSafeIdentifier mirrors the mysqlstore test guard for generated names.
func mcpSafeIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// activeEntitlements is a policy stub for configuration-only tests.
type activeEntitlements struct{}

func (activeEntitlements) Authorize(context.Context, entitlement.Subject) (entitlement.Decision, error) {
	return entitlement.Decision{Active: true}, nil
}

type mcpFixtureSpec struct {
	trialStart time.Time
	trialEnd   time.Time
	audits     audit.Store // nil: the committed mysqlstore audit store
}

type mcpFixture struct {
	t          *testing.T
	db         *sql.DB
	store      mcpoauth.Store
	server     *httptest.Server
	httpClient *http.Client

	sessions *session.Service
	pepper   []byte

	userID        string
	deviceID      string
	secondDevID   string
	studioID      string
	otherStudioID string

	secondUserID   string
	otherUserDevID string
	otherUserStuID string

	connector mcpoauth.Client
}

func newMcpFixture(t *testing.T, mutate func(*mcpFixtureSpec)) *mcpFixture {
	t.Helper()
	spec := mcpFixtureSpec{
		trialStart: time.Now().UTC().Add(-time.Hour),
		trialEnd:   time.Now().UTC().Add(13 * 24 * time.Hour),
	}
	if mutate != nil {
		mutate(&spec)
	}

	db := mcpTestDatabase(t)
	store := mysqlstore.NewOAuthStore(db)
	registered, err := store.RegisterClient(t.Context(), mcpoauth.Client{
		ClientID:     "https://connector.example",
		ClientName:   "Test Connector",
		RedirectURIs: []string{mcpTestRedirect},
	})
	if err != nil {
		t.Fatalf("register connector client: %v", err)
	}

	fx := &mcpFixture{
		t:         t,
		db:        db,
		store:     store,
		pepper:    mcpTestPepper,
		connector: registered,
	}

	fx.userID = mcpUUID(t)
	fx.deviceID = mcpUUID(t)
	fx.secondDevID = mcpUUID(t)
	fx.studioID = mcpUUID(t)
	fx.otherStudioID = mcpUUID(t)
	fx.secondUserID = mcpUUID(t)
	fx.otherUserDevID = mcpUUID(t)
	fx.otherUserStuID = mcpUUID(t)

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatalf("fixture insert failed: %v\nquery: %s", err, query)
		}
	}
	exec(`INSERT INTO users (id) VALUES (?)`, fx.userID)
	exec(`INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, ?, 'active')`, fx.deviceID, fx.userID, "Primary Workstation")
	exec(`INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, ?, 'active')`, fx.secondDevID, fx.userID, "Secondary Workstation")
	exec(`INSERT INTO studio_sessions (id, user_id, device_id, studio_id, status, started_at) VALUES (?, ?, ?, ?, 'active', ?)`,
		fx.studioID, fx.userID, fx.deviceID, "studio-main", mcpTestNow())
	exec(`INSERT INTO studio_sessions (id, user_id, device_id, studio_id, status, started_at) VALUES (?, ?, ?, ?, 'active', ?)`,
		fx.otherStudioID, fx.userID, fx.secondDevID, "studio-secondary", mcpTestNow())
	exec(`INSERT INTO users (id) VALUES (?)`, fx.secondUserID)
	exec(`INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, ?, 'active')`, fx.otherUserDevID, fx.secondUserID, "Foreign Workstation")
	exec(`INSERT INTO studio_sessions (id, user_id, device_id, studio_id, status, started_at) VALUES (?, ?, ?, ?, 'active', ?)`,
		fx.otherUserStuID, fx.secondUserID, fx.otherUserDevID, "studio-foreign", mcpTestNow())
	exec(`INSERT INTO trial_entitlements (id, user_id, started_at, ends_at) VALUES (?, ?, ?, ?)`,
		mcpUUID(t), fx.userID, spec.trialStart.UTC(), spec.trialEnd.UTC())

	auditStore := spec.audits
	if auditStore == nil {
		auditStore = mysqlstore.NewAuditStore(db)
	}
	auditService := audit.NewService(auditStore)
	clock := mcpClock{}
	resource, err := url.Parse(mcpTestResource)
	if err != nil {
		t.Fatalf("parse resource URL: %v", err)
	}
	fx.sessions = session.NewService(mysqlstore.NewSessionStore(db), mcpTestPepper, 24*time.Hour)
	provider, err := mcpoauth.NewProvider(mcpoauth.Config{
		Resource:     resource,
		Store:        store,
		DB:           db,
		Audits:       auditService,
		Entitlements: entitlement.NewService(mysqlstore.NewEntitlementStore(db, clock, auditService), clock),
		Sessions:     fx.sessions,
		Pepper:       mcpTestPepper,
		LoginPath:    mcpTestLogin,
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	fx.server = httptest.NewServer(provider.Handler())
	t.Cleanup(fx.server.Close)
	fx.httpClient = &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return fx
}

func (fx *mcpFixture) sessionCookie(t *testing.T, userID string) string {
	t.Helper()
	plain, _, err := fx.sessions.Create(t.Context(), userID)
	if err != nil {
		t.Fatalf("create web session: %v", err)
	}
	return plain
}

// authorizeQuery builds a complete, valid authorize request.
func (fx *mcpFixture) authorizeQuery(mutate func(url.Values)) url.Values {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {fx.connector.ClientID},
		"redirect_uri":          {mcpTestRedirect},
		"scope":                 {"mcp:connect studio:read studio:edit"},
		"state":                 {"state-0123456789abcdef"},
		"code_challenge":        {mcpTestChallenge},
		"code_challenge_method": {"S256"},
		"resource":              {mcpTestResource},
	}
	if mutate != nil {
		mutate(q)
	}
	return q
}

func (fx *mcpFixture) doRequest(t *testing.T, method, path string, form url.Values, cookie string) *http.Response {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(t.Context(), method, fx.server.URL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: session.CookieName, Value: cookie})
	}
	resp, err := fx.httpClient.Do(req)
	if err != nil {
		t.Fatalf("http %s %s: %v", method, path, err)
	}
	return resp
}

func (fx *mcpFixture) authorizeGet(t *testing.T, q url.Values, cookie string) *http.Response {
	t.Helper()
	return fx.doRequest(t, http.MethodGet, mcpoauth.AuthorizePath+"?"+q.Encode(), nil, cookie)
}

// consentForm copies an authorize query into a consent POST body.
func consentForm(q url.Values) url.Values {
	form := url.Values{}
	for key, values := range q {
		form[key] = append([]string(nil), values...)
	}
	return form
}

func (fx *mcpFixture) approveConsent(t *testing.T, q url.Values, mutate func(*url.Values)) *http.Response {
	t.Helper()
	cookie := fx.sessionCookie(t, fx.userID)
	form := consentForm(q)
	form.Set("action", "approve")
	form.Set("device_id", fx.deviceID)
	form["grant"] = []string{"mcp:connect"}
	if mutate != nil {
		mutate(&form)
	}
	return fx.doRequest(t, http.MethodPost, mcpoauth.AuthorizePath, form, cookie)
}

func (fx *mcpFixture) denyConsent(t *testing.T, q url.Values) *http.Response {
	t.Helper()
	cookie := fx.sessionCookie(t, fx.userID)
	form := consentForm(q)
	form.Set("action", "deny")
	form.Set("device_id", fx.deviceID)
	return fx.doRequest(t, http.MethodPost, mcpoauth.AuthorizePath, form, cookie)
}

// redirectParams asserts a 303 consent outcome and returns the redirect query.
func redirectParams(t *testing.T, resp *http.Response) url.Values {
	t.Helper()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body: %s", resp.StatusCode, body)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("response missing Location header")
	}
	loc, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse redirect location %q: %v", location, err)
	}
	return loc.Query()
}

// consentCode runs a full approval and returns the issued code and state.
func (fx *mcpFixture) consentCode(t *testing.T, q url.Values, mutate func(*url.Values)) (code, state string) {
	t.Helper()
	params := redirectParams(t, fx.approveConsent(t, q, mutate))
	if params.Get("error") != "" {
		t.Fatalf("consent approve redirected with error %q: %q", params.Get("error"), params.Get("error_description"))
	}
	code = params.Get("code")
	state = params.Get("state")
	if code == "" {
		t.Fatal("consent redirect missing code parameter")
	}
	return code, state
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

type errorResponse struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// postToken posts one token endpoint request and decodes both shapes.
func (fx *mcpFixture) postToken(t *testing.T, form url.Values) (int, tokenResponse, errorResponse) {
	t.Helper()
	resp := fx.doRequest(t, http.MethodPost, mcpoauth.TokenPath, form, "")
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read token response: %v", err)
	}
	var tokens tokenResponse
	var errResp errorResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &tokens); err != nil {
			t.Fatalf("decode token response %q: %v", string(raw), err)
		}
		_ = json.Unmarshal(raw, &errResp)
	}
	return resp.StatusCode, tokens, errResp
}

func (fx *mcpFixture) exchangeToken(t *testing.T, code string, mutate func(*url.Values)) (int, tokenResponse, errorResponse) {
	t.Helper()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {mcpTestRedirect},
		"client_id":     {fx.connector.ClientID},
		"code_verifier": {mcpTestVerifier},
		"resource":      {mcpTestResource},
	}
	if mutate != nil {
		mutate(&form)
	}
	return fx.postToken(t, form)
}

func (fx *mcpFixture) refreshToken(t *testing.T, refresh string, mutate func(*url.Values)) (int, tokenResponse, errorResponse) {
	t.Helper()
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"client_id":     {fx.connector.ClientID},
	}
	if mutate != nil {
		mutate(&form)
	}
	return fx.postToken(t, form)
}

func (fx *mcpFixture) revokeToken(t *testing.T, token, hint string, mutate func(*url.Values)) int {
	t.Helper()
	form := url.Values{
		"token":           {token},
		"client_id":       {fx.connector.ClientID},
		"token_type_hint": {hint},
	}
	if mutate != nil {
		mutate(&form)
	}
	resp := fx.doRequest(t, http.MethodPost, mcpoauth.RevocationPath, form, "")
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read revocation response: %v", err)
	}
	return resp.StatusCode
}

func (fx *mcpFixture) queryInt(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := fx.db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func (fx *mcpFixture) accessInfo(t *testing.T, plain string) (mcpoauth.AccessTokenInfo, error) {
	t.Helper()
	return fx.store.AccessTokenByDigest(t.Context(), credential.Digest(plain, fx.pepper), time.Now().UTC())
}

// assertScopeSet compares two space-delimited scope strings as sets.
func assertScopeSet(t *testing.T, got, want string) {
	t.Helper()
	counts := func(raw string) map[string]int {
		out := map[string]int{}
		for _, scope := range strings.Fields(raw) {
			out[scope]++
		}
		return out
	}
	gotSet, wantSet := counts(got), counts(want)
	if len(gotSet) != len(wantSet) {
		t.Fatalf("scope sets differ: got %q, want %q", got, want)
	}
	for scope, count := range wantSet {
		if gotSet[scope] != count {
			t.Fatalf("scope sets differ: got %q, want %q", got, want)
		}
	}
}

func TestProviderRejectsIncompleteConfig(t *testing.T) {
	db := mcpTestDatabase(t)
	resource, err := url.Parse(mcpTestResource)
	if err != nil {
		t.Fatalf("parse resource: %v", err)
	}
	store := mysqlstore.NewOAuthStore(db)
	base := mcpoauth.Config{
		Resource:     resource,
		Store:        store,
		DB:           db,
		Audits:       audit.NewService(mysqlstore.NewAuditStore(db)),
		Entitlements: activeEntitlements{},
		Sessions:     session.NewService(mysqlstore.NewSessionStore(db), mcpTestPepper, time.Hour),
		Pepper:       mcpTestPepper,
		LoginPath:    mcpTestLogin,
	}
	cases := map[string]func(cfg *mcpoauth.Config){
		"missing resource":     func(cfg *mcpoauth.Config) { cfg.Resource = nil },
		"missing store":        func(cfg *mcpoauth.Config) { cfg.Store = nil },
		"missing database":     func(cfg *mcpoauth.Config) { cfg.DB = nil },
		"missing audit":        func(cfg *mcpoauth.Config) { cfg.Audits = nil },
		"missing sessions":     func(cfg *mcpoauth.Config) { cfg.Sessions = nil },
		"missing entitlements": func(cfg *mcpoauth.Config) { cfg.Entitlements = nil },
		"missing pepper":       func(cfg *mcpoauth.Config) { cfg.Pepper = nil },
		"missing login path":   func(cfg *mcpoauth.Config) { cfg.LoginPath = "" },
		"insecure resource": func(cfg *mcpoauth.Config) {
			cfg.Resource = &url.URL{Scheme: "http", Host: "gateway.example.com", Path: "/mcp"}
		},
		"negative lifespan": func(cfg *mcpoauth.Config) { cfg.AccessTokenLifespan = -time.Second },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if _, err := mcpoauth.NewProvider(cfg); err == nil {
				t.Fatalf("expected configuration error for %s", name)
			}
		})
	}
	if _, err := mcpoauth.NewProvider(base); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
}

func TestAuthorizeRedirectsUnauthenticatedToLogin(t *testing.T) {
	fx := newMcpFixture(t, nil)
	q := fx.authorizeQuery(func(v url.Values) { v.Set("state", "secret-state-987654321") })
	resp := fx.authorizeGet(t, q, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, mcpTestLogin+"?") {
		t.Fatalf("redirect %q does not target the login page", location)
	}
	loc, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parse login redirect: %v", err)
	}
	next := loc.Query().Get("next")
	if next == "" {
		t.Fatal("login redirect missing next parameter")
	}
	for _, want := range []string{
		mcpoauth.AuthorizePath,
		"state=secret-state-987654321",
		"client_id=" + url.QueryEscape(fx.connector.ClientID),
		"code_challenge=" + mcpTestChallenge,
	} {
		if !strings.Contains(next, want) {
			t.Fatalf("login redirect next %q missing %q", next, want)
		}
	}
}

func TestAuthorizeRejectsUnregisteredRedirectURI(t *testing.T) {
	fx := newMcpFixture(t, nil)
	cookie := fx.sessionCookie(t, fx.userID)
	q := fx.authorizeQuery(func(v url.Values) { v.Set("redirect_uri", "https://attacker.example/callback") })
	resp := fx.authorizeGet(t, q, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if resp.Header.Get("Location") != "" {
		t.Fatalf("mismatched redirect_uri must never redirect, got Location %q", resp.Header.Get("Location"))
	}
	var errResp errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error != "invalid_request" {
		t.Fatalf("error = %q, want invalid_request", errResp.Error)
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_authorization_codes"); n != 0 {
		t.Fatal("no authorization code may be persisted for a rejected redirect")
	}
}

func TestAuthorizeRejectsUnknownClient(t *testing.T) {
	fx := newMcpFixture(t, nil)
	cookie := fx.sessionCookie(t, fx.userID)
	q := fx.authorizeQuery(func(v url.Values) { v.Set("client_id", "https://unknown.example") })
	resp := fx.authorizeGet(t, q, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (invalid_client)", resp.StatusCode)
	}
	var errResp errorResponse
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error != "invalid_client" {
		t.Fatalf("error = %q, want invalid_client", errResp.Error)
	}
	if resp.Header.Get("Location") != "" {
		t.Fatalf("unknown client must never redirect, got Location %q", resp.Header.Get("Location"))
	}
}

func TestAuthorizeRequiresMatchingResource(t *testing.T) {
	fx := newMcpFixture(t, nil)
	cookie := fx.sessionCookie(t, fx.userID)
	for _, resource := range []string{"https://elsewhere.example/mcp", mcpTestRedirect} {
		q := fx.authorizeQuery(func(v url.Values) { v.Set("resource", resource) })
		resp := fx.authorizeGet(t, q, cookie)
		body := readAll(t, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("resource %q: status = %d, want 400", resource, resp.StatusCode)
		}
		if resp.Header.Get("Location") != "" {
			t.Fatalf("resource %q must never redirect, got Location %q", resource, resp.Header.Get("Location"))
		}
		if !strings.Contains(body, "invalid_request") {
			t.Fatalf("resource %q: body %q missing invalid_request", resource, body)
		}
	}
	q := fx.authorizeQuery(func(v url.Values) { v.Del("resource") })
	resp := fx.authorizeGet(t, q, cookie)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing resource: status = %d, want 400", resp.StatusCode)
	}
}

func TestAuthorizeRequiresPKCES256(t *testing.T) {
	fx := newMcpFixture(t, nil)
	cookie := fx.sessionCookie(t, fx.userID)

	cases := map[string]func(v url.Values){
		"missing challenge":   func(v url.Values) { v.Del("code_challenge") },
		"plain method":        func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"missing method":      func(v url.Values) { v.Del("code_challenge_method") },
		"unsupported method":  func(v url.Values) { v.Set("code_challenge_method", "S512") },
		"oversized challenge": func(v url.Values) { v.Set("code_challenge", strings.Repeat("a", 129)) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			q := fx.authorizeQuery(mutate)
			resp := fx.authorizeGet(t, q, cookie)
			body := readAll(t, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", resp.StatusCode, body)
			}
			if resp.Header.Get("Location") != "" {
				t.Fatalf("must never redirect, got Location %q", resp.Header.Get("Location"))
			}
			if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_authorization_codes"); n != 0 {
				t.Fatal("no authorization code may be persisted for a rejected request")
			}
		})
	}
}

func TestAuthorizeRedirectsProtocolErrors(t *testing.T) {
	fx := newMcpFixture(t, nil)
	cookie := fx.sessionCookie(t, fx.userID)

	cases := map[string]func(v url.Values){
		"short state":          func(v url.Values) { v.Set("state", "short") },
		"unsupported scope":    func(v url.Values) { v.Set("scope", "studio:admin") },
		"unsupported response": func(v url.Values) { v.Set("response_type", "token") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			q := fx.authorizeQuery(mutate)
			params := redirectParams(t, fx.authorizeGet(t, q, cookie))
			if params.Get("code") != "" {
				t.Fatal("protocol errors must not issue a code")
			}
			if params.Get("state") == "" {
				t.Fatal("error redirects must echo the state")
			}
			switch name {
			case "short state":
				if params.Get("error") != "invalid_state" {
					t.Fatalf("error = %q, want invalid_state", params.Get("error"))
				}
			case "unsupported scope":
				if params.Get("error") != "invalid_scope" {
					t.Fatalf("error = %q, want invalid_scope", params.Get("error"))
				}
			case "unsupported response":
				if params.Get("error") != "unsupported_response_type" {
					t.Fatalf("error = %q, want unsupported_response_type", params.Get("error"))
				}
			}
			if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_authorization_codes"); n != 0 {
				t.Fatal("no authorization code may be persisted for a rejected request")
			}
		})
	}
}

func TestAuthorizeConsentIssuesCodeAndState(t *testing.T) {
	fx := newMcpFixture(t, nil)
	q := fx.authorizeQuery(func(v url.Values) { v.Set("state", "issued-state-123456789") })
	code, state := fx.consentCode(t, q, func(f *url.Values) {
		(*f)["grant"] = []string{"mcp:connect", "studio:read"}
	})
	if state != "issued-state-123456789" {
		t.Fatalf("state = %q, want exact echo", state)
	}
	if !strings.HasPrefix(code, "mcc_") {
		t.Fatalf("code %q does not use the authorization code prefix", code)
	}

	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_grants"); n != 1 {
		t.Fatalf("grant rows = %d, want 1", n)
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_authorization_codes"); n != 1 {
		t.Fatalf("code rows = %d, want 1", n)
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM audit_logs WHERE action = ?", mcpGrantAudit); n != 1 {
		t.Fatalf("audit rows = %d, want 1", n)
	}

	// The stored code is a keyed digest, never the plaintext secret.
	var stored []byte
	var rawScopes []byte
	if err := fx.db.QueryRowContext(t.Context(),
		`SELECT code_digest, scopes FROM oauth_authorization_codes`).Scan(&stored, &rawScopes); err != nil {
		t.Fatalf("select stored code: %v", err)
	}
	want := credential.Digest(code, fx.pepper)
	if string(stored) != string(want[:]) {
		t.Fatal("stored code digest does not match the keyed digest of the issued code")
	}
	if string(stored) == code {
		t.Fatal("plaintext authorization code must never reach storage")
	}
	var scopes []string
	if err := json.Unmarshal(rawScopes, &scopes); err != nil {
		t.Fatalf("decode code scopes: %v", err)
	}
	assertScopeSet(t, strings.Join(scopes, " "), "mcp:connect studio:read")

	// The audit event carries identifiers only — never code or verifier.
	var metadata []byte
	if err := fx.db.QueryRowContext(t.Context(),
		`SELECT metadata FROM audit_logs WHERE action = ?`, mcpGrantAudit).Scan(&metadata); err != nil {
		t.Fatalf("select audit metadata: %v", err)
	}
	meta := string(metadata)
	if strings.Contains(meta, code) || strings.Contains(meta, mcpTestVerifier) {
		t.Fatal("audit metadata must never contain the authorization code or verifier")
	}
}

func TestTokenExchangesCodeForTokens(t *testing.T) {
	fx := newMcpFixture(t, nil)
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), func(f *url.Values) {
		(*f)["grant"] = []string{"mcp:connect", "studio:read"}
	})

	status, tokens, errResp := fx.exchangeToken(t, code, nil)
	if status != http.StatusOK {
		t.Fatalf("token exchange status = %d (%q %q), want 200", status, errResp.Error, errResp.Description)
	}
	if tokens.AccessToken == "" || tokens.TokenType != "bearer" {
		t.Fatalf("access token missing or wrong type: %+v", tokens)
	}
	if !strings.HasPrefix(tokens.AccessToken, "mca_") {
		t.Fatalf("access token %q does not use the access prefix", tokens.AccessToken)
	}
	if !strings.HasPrefix(tokens.RefreshToken, "mcr_") {
		t.Fatalf("refresh token %q does not use the refresh prefix", tokens.RefreshToken)
	}
	if tokens.ExpiresIn <= 0 {
		t.Fatalf("expires_in = %d, want positive", tokens.ExpiresIn)
	}
	assertScopeSet(t, tokens.Scope, "mcp:connect studio:read")

	// The committed validation path resolves grant, scopes, and resource.
	info, err := fx.accessInfo(t, tokens.AccessToken)
	if err != nil {
		t.Fatalf("access token must validate through the store: %v", err)
	}
	assertScopeSet(t, strings.Join(info.Grant.Scopes, " "), "mcp:connect studio:read")
	if info.Grant.Resource != mcpTestResource {
		t.Fatalf("grant resource = %q, want %q", info.Grant.Resource, mcpTestResource)
	}
	if info.Grant.DeviceID != fx.deviceID {
		t.Fatalf("grant device = %q, want %q", info.Grant.DeviceID, fx.deviceID)
	}

	// Token digests are keyed, never plaintext.
	var accessDigest []byte
	if err := fx.db.QueryRowContext(t.Context(), `SELECT token_digest FROM oauth_access_tokens`).Scan(&accessDigest); err != nil {
		t.Fatalf("select access digest: %v", err)
	}
	wantDigest := credential.Digest(tokens.AccessToken, fx.pepper)
	if string(accessDigest) != string(wantDigest[:]) {
		t.Fatal("stored access digest does not match the keyed digest of the issued token")
	}
	if string(accessDigest) == tokens.AccessToken {
		t.Fatal("plaintext access token must never reach storage")
	}
}

func TestTokenRejectsWrongVerifier(t *testing.T) {
	fx := newMcpFixture(t, nil)
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), nil)

	status, _, errResp := fx.exchangeToken(t, code, func(f *url.Values) {
		f.Set("code_verifier", "0123456789012345678901234567890123456789012")
	})
	if status != http.StatusBadRequest || errResp.Error != "invalid_grant" {
		t.Fatalf("status = %d error = %q, want 400 invalid_grant", status, errResp.Error)
	}

	// A failed verifier must not consume the code.
	status, _, errResp = fx.exchangeToken(t, code, nil)
	if status != http.StatusOK {
		t.Fatalf("retry with correct verifier failed: status = %d error = %q (%s)", status, errResp.Error, errResp.Description)
	}
}

func TestTokenRejectsWrongResource(t *testing.T) {
	fx := newMcpFixture(t, nil)
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), nil)

	status, _, errResp := fx.exchangeToken(t, code, func(f *url.Values) {
		f.Set("resource", "https://elsewhere.example/mcp")
	})
	if status != http.StatusBadRequest || errResp.Error != "invalid_grant" {
		t.Fatalf("wrong resource: status = %d error = %q, want 400 invalid_grant", status, errResp.Error)
	}

	// The binding mismatch must not consume the code.
	status, _, errResp = fx.exchangeToken(t, code, nil)
	if status != http.StatusOK {
		t.Fatalf("retry with correct resource failed: status = %d error = %q (%s)", status, errResp.Error, errResp.Description)
	}
}

func TestTokenRejectsWrongClient(t *testing.T) {
	fx := newMcpFixture(t, nil)
	second, err := fx.store.RegisterClient(t.Context(), mcpoauth.Client{
		ClientID:     "https://other-connector.example",
		ClientName:   "Other Connector",
		RedirectURIs: []string{"https://other-connector.example/callback"},
	})
	if err != nil {
		t.Fatalf("register second client: %v", err)
	}
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), nil)

	status, _, errResp := fx.exchangeToken(t, code, func(f *url.Values) {
		f.Set("client_id", second.ClientID)
	})
	if status != http.StatusBadRequest || errResp.Error != "invalid_grant" {
		t.Fatalf("wrong client: status = %d error = %q, want 400 invalid_grant", status, errResp.Error)
	}
}

func TestTokenRejectsCodeReuse(t *testing.T) {
	fx := newMcpFixture(t, nil)
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), nil)

	status, tokens, errResp := fx.exchangeToken(t, code, nil)
	if status != http.StatusOK {
		t.Fatalf("first exchange failed: status = %d error = %q (%s)", status, errResp.Error, errResp.Description)
	}

	status, _, errResp = fx.exchangeToken(t, code, nil)
	if status != http.StatusBadRequest || errResp.Error != "invalid_grant" {
		t.Fatalf("code reuse: status = %d error = %q, want 400 invalid_grant", status, errResp.Error)
	}

	// Replaying the code revokes the tokens it issued.
	if _, err := fx.accessInfo(t, tokens.AccessToken); !errors.Is(err, mcpoauth.ErrTokenRevoked) && !errors.Is(err, mcpoauth.ErrGrantRevoked) {
		t.Fatalf("access token after code replay: err = %v, want revoked", err)
	}
	status, _, errResp = fx.refreshToken(t, tokens.RefreshToken, nil)
	if status != http.StatusBadRequest || errResp.Error != "invalid_grant" {
		t.Fatalf("refresh after code replay: status = %d error = %q, want 400 invalid_grant", status, errResp.Error)
	}
}

func TestTokenRefreshRotatesTokens(t *testing.T) {
	fx := newMcpFixture(t, nil)
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), func(f *url.Values) {
		(*f)["grant"] = []string{"mcp:connect", "studio:read"}
	})
	_, first, _ := fx.exchangeToken(t, code, nil)

	status, second, errResp := fx.refreshToken(t, first.RefreshToken, nil)
	if status != http.StatusOK {
		t.Fatalf("refresh failed: status = %d error = %q (%s)", status, errResp.Error, errResp.Description)
	}
	if second.AccessToken == first.AccessToken {
		t.Fatal("refresh must rotate the access token")
	}
	if second.RefreshToken == first.RefreshToken {
		t.Fatal("refresh must rotate the refresh token")
	}
	assertScopeSet(t, second.Scope, "mcp:connect studio:read")

	// Both refresh tokens stay inside one family.
	var familySize int
	if err := fx.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM oauth_refresh_tokens WHERE family_id = (SELECT family_id FROM oauth_refresh_tokens ORDER BY created_at LIMIT 1)`).
		Scan(&familySize); err != nil {
		t.Fatalf("count refresh family: %v", err)
	}
	if familySize != 2 {
		t.Fatalf("refresh family size = %d, want 2", familySize)
	}

	var used int
	refreshDigest := credential.Digest(first.RefreshToken, fx.pepper)
	if err := fx.db.QueryRowContext(t.Context(),
		`SELECT used_at IS NOT NULL FROM oauth_refresh_tokens WHERE token_digest = ?`,
		refreshDigest[:]).Scan(&used); err != nil {
		t.Fatalf("read original refresh usage: %v", err)
	}
	if used != 1 {
		t.Fatal("the consumed refresh token must be marked used")
	}

	if _, err := fx.accessInfo(t, second.AccessToken); err != nil {
		t.Fatalf("rotated access token must validate: %v", err)
	}
}

func TestTokenRefreshReuseRevokesFamily(t *testing.T) {
	fx := newMcpFixture(t, nil)
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), nil)
	_, first, _ := fx.exchangeToken(t, code, nil)

	status, second, _ := fx.refreshToken(t, first.RefreshToken, nil)
	if status != http.StatusOK {
		t.Fatalf("first refresh failed: status = %d", status)
	}

	// Replaying the rotated refresh token revokes the whole family.
	status, _, errResp := fx.refreshToken(t, first.RefreshToken, nil)
	if status != http.StatusBadRequest || errResp.Error != "invalid_grant" {
		t.Fatalf("refresh reuse: status = %d error = %q, want 400 invalid_grant", status, errResp.Error)
	}
	if _, err := fx.accessInfo(t, second.AccessToken); !errors.Is(err, mcpoauth.ErrTokenRevoked) && !errors.Is(err, mcpoauth.ErrGrantRevoked) {
		t.Fatalf("rotated access token after reuse: err = %v, want revoked", err)
	}
	if _, err := fx.accessInfo(t, first.AccessToken); !errors.Is(err, mcpoauth.ErrTokenRevoked) && !errors.Is(err, mcpoauth.ErrGrantRevoked) {
		t.Fatalf("original access token after reuse: err = %v, want revoked", err)
	}
	status, _, errResp = fx.refreshToken(t, second.RefreshToken, nil)
	if status != http.StatusBadRequest || errResp.Error != "invalid_grant" {
		t.Fatalf("child refresh after reuse: status = %d error = %q, want 400 invalid_grant", status, errResp.Error)
	}
}

func TestTokenRefreshRequiresMatchingResource(t *testing.T) {
	fx := newMcpFixture(t, nil)
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), nil)
	_, first, _ := fx.exchangeToken(t, code, nil)

	status, _, errResp := fx.refreshToken(t, first.RefreshToken, func(f *url.Values) {
		f.Set("resource", "https://elsewhere.example/mcp")
	})
	if status != http.StatusBadRequest || errResp.Error != "invalid_grant" {
		t.Fatalf("refresh with wrong resource: status = %d error = %q, want 400 invalid_grant", status, errResp.Error)
	}

	status, _, errResp = fx.refreshToken(t, first.RefreshToken, nil)
	if status != http.StatusOK {
		t.Fatalf("refresh with omitted resource failed: status = %d error = %q (%s)", status, errResp.Error, errResp.Description)
	}
}

func TestTokenRejectsDisabledGrantTypes(t *testing.T) {
	fx := newMcpFixture(t, nil)
	cases := map[string]url.Values{
		"password": {
			"grant_type": {"password"},
			"client_id":  {fx.connector.ClientID},
			"username":   {"user"},
			"password":   {"secret"},
		},
		"client_credentials": {
			"grant_type": {"client_credentials"},
			"client_id":  {fx.connector.ClientID},
		},
		"implicit": {
			"grant_type": {"implicit"},
			"client_id":  {fx.connector.ClientID},
		},
	}
	for name, form := range cases {
		t.Run(name, func(t *testing.T) {
			status, _, errResp := fx.postToken(t, form)
			if status != http.StatusBadRequest {
				t.Fatalf("grant %s: status = %d, want 400 (disabled grant)", name, status)
			}
			if errResp.Error != "invalid_request" {
				t.Fatalf("grant %s: error = %q, want invalid_request", name, errResp.Error)
			}
		})
	}
}

func TestRevokeAccessTokenRevokesOnlyThatToken(t *testing.T) {
	fx := newMcpFixture(t, nil)
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), nil)
	_, tokens, _ := fx.exchangeToken(t, code, nil)

	if status := fx.revokeToken(t, tokens.AccessToken, "access_token", nil); status != http.StatusOK {
		t.Fatalf("revocation status = %d, want 200", status)
	}
	if _, err := fx.accessInfo(t, tokens.AccessToken); !errors.Is(err, mcpoauth.ErrTokenRevoked) {
		t.Fatalf("revoked access token: err = %v, want ErrTokenRevoked", err)
	}

	// The refresh sibling stays usable: RFC 7009 lets access revocation
	// leave the refresh family intact.
	status, rotated, errResp := fx.refreshToken(t, tokens.RefreshToken, nil)
	if status != http.StatusOK {
		t.Fatalf("refresh after access revocation: status = %d error = %q (%s)", status, errResp.Error, errResp.Description)
	}
	if rotated.AccessToken == "" {
		t.Fatal("rotation must issue a replacement access token")
	}
}

func TestRevokeRefreshTokenRevokesFamily(t *testing.T) {
	fx := newMcpFixture(t, nil)
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), nil)
	_, tokens, _ := fx.exchangeToken(t, code, nil)
	status, rotated, _ := fx.refreshToken(t, tokens.RefreshToken, nil)
	if status != http.StatusOK {
		t.Fatalf("first refresh failed: status = %d", status)
	}

	if status := fx.revokeToken(t, tokens.RefreshToken, "refresh_token", nil); status != http.StatusOK {
		t.Fatalf("revocation status = %d, want 200", status)
	}
	if _, err := fx.accessInfo(t, rotated.AccessToken); !errors.Is(err, mcpoauth.ErrTokenRevoked) && !errors.Is(err, mcpoauth.ErrGrantRevoked) {
		t.Fatalf("access token after family revocation: err = %v, want revoked", err)
	}
	status, _, errResp := fx.refreshToken(t, rotated.RefreshToken, nil)
	if status != http.StatusBadRequest || errResp.Error != "invalid_grant" {
		t.Fatalf("child refresh after family revocation: status = %d error = %q, want 400 invalid_grant", status, errResp.Error)
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_grants WHERE revoked_at IS NOT NULL"); n != 1 {
		t.Fatalf("revoked grants = %d, want 1", n)
	}
}

func TestRevokeUnknownTokenSucceedsSilently(t *testing.T) {
	fx := newMcpFixture(t, nil)
	if status := fx.revokeToken(t, "mca_unknown-token-value", "access_token", nil); status != http.StatusOK {
		t.Fatalf("unknown token revocation status = %d, want 200", status)
	}
	if status := fx.revokeToken(t, "mcr_unknown-token-value", "refresh_token", nil); status != http.StatusOK {
		t.Fatalf("unknown refresh revocation status = %d, want 200", status)
	}
}

func TestRevokeWrongClientLeavesTokenIntact(t *testing.T) {
	fx := newMcpFixture(t, nil)
	second, err := fx.store.RegisterClient(t.Context(), mcpoauth.Client{
		ClientID:     "https://other-connector.example",
		ClientName:   "Other Connector",
		RedirectURIs: []string{"https://other-connector.example/callback"},
	})
	if err != nil {
		t.Fatalf("register second client: %v", err)
	}
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), nil)
	_, tokens, _ := fx.exchangeToken(t, code, nil)

	status := fx.revokeToken(t, tokens.AccessToken, "access_token", func(f *url.Values) {
		f.Set("client_id", second.ClientID)
	})
	if status != http.StatusOK {
		t.Fatalf("foreign revocation status = %d, want 200 (no state change)", status)
	}
	if _, err := fx.accessInfo(t, tokens.AccessToken); err != nil {
		t.Fatalf("token must survive a foreign revocation attempt: %v", err)
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_access_tokens WHERE revoked_at IS NOT NULL"); n != 0 {
		t.Fatalf("revoked access tokens = %d, want 0", n)
	}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(raw)
}
