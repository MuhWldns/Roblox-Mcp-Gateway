// Package e2egate is the committed final E2E gate: it composes the real
// production packages — router, sessions, robloxauth, enrollment, download,
// dashboard, admin surface, bridge hub, mcpoauth provider, and the MCP
// gateway — on a real loopback TLS listener against a real migrated MySQL
// database, and proves the locally feasible production matrix live. It skips
// without MYSQL_TEST_DSN. scripts/e2e-matrix.ps1 aggregates these rows with
// the named package tests that cover the same contracts.
package e2egate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"robloxkit/internal/audit"
	"robloxkit/internal/bridgeapp"
	"robloxkit/internal/bridgehub"
	"robloxkit/internal/credential"
	"robloxkit/internal/device"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/health"
	"robloxkit/internal/httpserver"
	"robloxkit/internal/mcpgateway"
	"robloxkit/internal/mcpoauth"
	"robloxkit/internal/mcpprocess"
	"robloxkit/internal/mysqlstore"
	"robloxkit/internal/robloxauth"
	"robloxkit/internal/session"
	"robloxkit/internal/statusui"
	"robloxkit/pkg/bridgeproto"
)

// ---- clock, database, TLS ----

type gateClock struct{}

func (gateClock) Now() time.Time { return time.Now().UTC() }

func gateUUID(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func gateSafeIdentifier(s string) bool {
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

func gateDB(t *testing.T) (db *sql.DB, admin *sql.DB, dbName string, dbDSN string) {
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
	admin, err = sql.Open("mysql", adminConfig.FormatDSN())
	if err != nil {
		t.Fatalf("open admin database: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping MYSQL_TEST_DSN: %v", err)
	}
	dbName = fmt.Sprintf("robloxkit_e2egate_%d", time.Now().UnixNano())
	if !gateSafeIdentifier(dbName) {
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
	dbDSN = target.FormatDSN()
	db, err = sql.Open("mysql", dbDSN)
	if err != nil {
		t.Fatalf("open temporary database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := mysqlstore.Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db, admin, dbName, dbDSN
}

func selfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
}

func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// ---- fixture Roblox provider (login backend) ----

// fixtureProvider serves the OAuth surfaces the real Roblox login flow
// consumes: /oauth/v1/authorize, /oauth/v1/token, /oauth/v1/userinfo, plus
// the ES256 JWKS the gateway validates ID tokens against.
type fixtureProvider struct {
	t        *testing.T
	server   *http.Server
	listener net.Listener
	base     string
	issuer   string
	key      *ecdsa.PrivateKey
	kid      string
	clientID string
	secret   string

	mu     sync.Mutex
	codes  map[string]fixtureCode
	tokens map[string]string
}

type fixtureCode struct {
	subject   string
	challenge string
	nonce     string
}

func newFixtureProvider(t *testing.T) *fixtureProvider {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("provider key: %v", err)
	}
	p := &fixtureProvider{
		t:        t,
		key:      key,
		kid:      "e2egate-key",
		clientID: "e2e-roblox-client",
		secret:   "e2e-roblox-secret",
		codes:    map[string]fixtureCode{},
		tokens:   map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /oauth/v1/authorize", p.authorize)
	mux.HandleFunc("POST /oauth/v1/token", p.token)
	mux.HandleFunc("GET /oauth/v1/userinfo", p.userInfo)
	mux.HandleFunc("GET /.well-known/jwks.json", p.jwks)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("provider listen: %v", err)
	}
	p.listener = listener
	p.base = "http://" + listener.Addr().String()
	p.issuer = p.base
	p.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = p.server.Serve(listener) }()
	t.Cleanup(func() { _ = p.server.Close() })
	return p
}

func (p *fixtureProvider) authorize(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("client_id") != p.clientID || query.Get("response_type") != "code" {
		http.Error(w, "bad authorize request", http.StatusBadRequest)
		return
	}
	code := "prov_" + gateUUID(p.t)
	p.mu.Lock()
	p.codes[code] = fixtureCode{
		subject:   query.Get("sub"),
		challenge: query.Get("code_challenge"),
		nonce:     query.Get("nonce"),
	}
	p.mu.Unlock()
	redirect, err := url.Parse(query.Get("redirect_uri"))
	if err != nil || !redirect.IsAbs() {
		http.Error(w, "bad redirect", http.StatusBadRequest)
		return
	}
	target := *redirect
	q := target.Query()
	q.Set("code", code)
	q.Set("state", query.Get("state"))
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

func (p *fixtureProvider) token(w http.ResponseWriter, r *http.Request) {
	if user, pass, ok := r.BasicAuth(); !ok || user != p.clientID || pass != p.secret {
		http.Error(w, "bad client auth", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	code := r.Form.Get("code")
	verifier := r.Form.Get("code_verifier")
	p.mu.Lock()
	issued, ok := p.codes[code]
	delete(p.codes, code)
	p.mu.Unlock()
	if !ok {
		http.Error(w, "unknown code", http.StatusBadRequest)
		return
	}
	if base64.RawURLEncoding.EncodeToString(sha256Sum([]byte(verifier))) != issued.challenge {
		http.Error(w, "pkce mismatch", http.StatusBadRequest)
		return
	}
	access := "provat_" + gateUUID(p.t)
	p.mu.Lock()
	p.tokens[access] = issued.subject
	p.mu.Unlock()
	idToken, err := p.signIDToken(issued)
	if err != nil {
		http.Error(w, "sign failure", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

func (p *fixtureProvider) signIDToken(issued fixtureCode) (string, error) {
	now := time.Now().UTC()
	headerJSON := []byte(`{"alg":"ES256","typ":"JWT","kid":"` + p.kid + `"}`)
	claims := map[string]any{
		"iss": p.issuer, "sub": issued.subject, "aud": p.clientID,
		"nonce": issued.nonce, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	signing := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(claimsJSON)
	digest := sha256.Sum256([]byte(signing))
	rBytes, sBytes, err := ecdsa.Sign(rand.Reader, p.key, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64) // JOSE ES256: raw R||S, not ASN.1
	rBytes.FillBytes(sig[:32])
	sBytes.FillBytes(sig[32:])
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (p *fixtureProvider) userInfo(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	p.mu.Lock()
	subject, ok := p.tokens[token]
	p.mu.Unlock()
	if !ok || subject == "" {
		http.Error(w, "unknown token", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"sub": subject, "preferred_username": subject + "-name",
		"name": "E2E " + subject, "nickname": subject,
	})
}

func (p *fixtureProvider) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := p.key.PublicKey
	x := base64.RawURLEncoding.EncodeToString(pub.X.FillBytes(make([]byte, 32)))
	y := base64.RawURLEncoding.EncodeToString(pub.Y.FillBytes(make([]byte, 32)))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keys": []map[string]string{{
			"kty": "EC", "crv": "P-256", "x": x, "y": y,
			"kid": p.kid, "alg": "ES256", "use": "sig",
		}},
	})
}

// ---- fake MCP child ----

// fakeMCP implements mcpprocess.Process with the committed handshake
// semantics, request recording, a hold hook for the no-replay row, and a
// crash hook that kills the child independently of the bridge.
type fakeMCP struct {
	mu                sync.Mutex
	toolCallArguments []json.RawMessage
	responses         chan json.RawMessage
	holdRequests      bool
	held              []json.RawMessage
	started           bool
	stopped           bool
	stopCh            chan struct{}
	crashed           bool
	crashCh           chan struct{}
	observed          chan struct{}
}

func newFakeMCP() *fakeMCP {
	return &fakeMCP{
		responses: make(chan json.RawMessage, 16),
		stopCh:    make(chan struct{}),
		crashCh:   make(chan struct{}),
		observed:  make(chan struct{}, 1),
	}
}

func (p *fakeMCP) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
	return nil
}

func (p *fakeMCP) Send(_ context.Context, frame json.RawMessage) error {
	p.mu.Lock()
	if p.crashed {
		p.mu.Unlock()
		return errors.New("MCP process pipe is gone")
	}
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	_ = json.Unmarshal(frame, &req)
	if req.Method == "tools/call" {
		p.toolCallArguments = append(p.toolCallArguments, append(json.RawMessage(nil), req.Params.Arguments...))
	}
	hold := p.holdRequests
	p.mu.Unlock()
	select {
	case p.observed <- struct{}{}:
	default:
	}

	if len(req.ID) == 0 {
		return nil
	}
	result := json.RawMessage(`{}`)
	switch req.Method {
	case "initialize":
		result = json.RawMessage(`{"protocolVersion":"2025-06-18"}`)
	case "tools/list":
		result = json.RawMessage(`{"tools":[{"name":"get_instance_tree","description":"Read-only instance tree.","annotations":{"readOnlyHint":true},"inputSchema":{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}}}]}`)
	case "tools/call":
		result = json.RawMessage(`{"content":[{"type":"text","text":"studio ok"}]}`)
	}
	response := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result)
	if hold {
		// Held: the response is recorded but never delivered, so the relayed
		// call stays pending until the connection tears down.
		p.mu.Lock()
		p.held = append(p.held, json.RawMessage(response))
		p.mu.Unlock()
		return nil
	}
	p.responses <- json.RawMessage(response)
	return nil
}

func (p *fakeMCP) Responses() <-chan json.RawMessage { return p.responses }
func (p *fakeMCP) Diagnostics() <-chan mcpprocess.SafeProcessError {
	return make(chan mcpprocess.SafeProcessError, 1)
}

func (p *fakeMCP) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopped {
		p.stopped = true
		close(p.stopCh)
	}
	return nil
}

func (p *fakeMCP) Wait() error {
	select {
	case <-p.stopCh:
		return nil
	case <-p.crashCh:
		return errors.New("MCP process crashed unexpectedly")
	}
}

// crash kills the child on its own: Wait unblocks with a crash error and
// the pipe refuses writes. It is deliberately NOT Stop and never touches
// the bridge context — the supervisor must detect the death by itself.
func (p *fakeMCP) crash() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.crashed {
		return
	}
	p.crashed = true
	close(p.crashCh)
}

func (p *fakeMCP) setHoldRequests(hold bool) {
	p.mu.Lock()
	p.holdRequests = hold
	p.mu.Unlock()
}

// awaitToolCallArgument blocks on recorded child input, not elapsed time: it
// returns only after an exact arguments.text sentinel reaches this process.
func (p *fakeMCP) awaitToolCallArgument(t *testing.T, sentinel string, timeout time.Duration) {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if p.hasToolCallArgument(sentinel) {
			return
		}
		select {
		case <-p.observed:
		case <-timer.C:
			t.Fatalf("timed out waiting for child tools/call argument %q; saw %s", sentinel, p.toolCallArgumentsSnapshot())
		}
	}
}

func (p *fakeMCP) hasToolCallArgument(sentinel string) bool {
	for _, arguments := range p.toolCallArgumentsSnapshot() {
		var parsed struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(arguments, &parsed) == nil && parsed.Text == sentinel {
			return true
		}
	}
	return false
}

func (p *fakeMCP) toolCallArgumentsSnapshot() []json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]json.RawMessage, len(p.toolCallArguments))
	for i, arguments := range p.toolCallArguments {
		out[i] = append(json.RawMessage(nil), arguments...)
	}
	return out
}
func (p *fakeMCP) CommitReadiness(commit func() error) error { return commit() }

// ---- TLS kernel + live stack ----

type tlsKernel struct {
	server *http.Server
	ln     net.Listener
}

func (k *tlsKernel) ListenAndServe() error              { return k.server.Serve(k.ln) }
func (k *tlsKernel) Shutdown(ctx context.Context) error { return k.server.Shutdown(ctx) }

type inboundRecord struct {
	AuthenticatedDeviceID string
	Envelope              bridgeproto.Envelope
}

// liveStack composes the real production stack on one TLS loopback listener.
type liveStack struct {
	t *testing.T

	db       *sql.DB
	adminDB  *sql.DB
	dbName   string
	dbDSN    string
	provider *fixtureProvider

	devicePepper []byte
	oauthPepper  []byte
	adminUserID  string

	sessions    *session.Service
	entitlement *entitlement.Service
	auditSvc    *audit.Service
	oauthStore  mcpoauth.Store
	hub         *bridgehub.Hub
	registry    *bridgehub.Registry
	gateway     *mcpgateway.Gateway
	enrollment  *device.Enrollment
	artifact    string

	addr   string
	base   string
	server *httpserver.Server

	inboundMu sync.Mutex
	inbound   []inboundRecord
}

func newLiveStack(t *testing.T) *liveStack {
	t.Helper()
	db, adminDB, dbName, dbDSN := gateDB(t)
	st := &liveStack{
		t:            t,
		db:           db,
		adminDB:      adminDB,
		dbName:       dbName,
		dbDSN:        dbDSN,
		devicePepper: []byte("e2egate-device-pepper"),
		oauthPepper:  []byte("e2egate-oauth-pepper-32-bytes!"),
		adminUserID:  gateUUID(t),
		provider:     newFixtureProvider(t),
	}
	st.artifact = t.TempDir() + "/RobloxBridge-e2egate.exe"
	if err := os.WriteFile(st.artifact, []byte("e2egate bridge artifact bytes"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	st.addr = listener.Addr().String()
	st.base = "https://" + st.addr
	if err := listener.Close(); err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	st.compose()
	t.Cleanup(st.shutdown)
	return st
}

func (st *liveStack) resourceURL() *url.URL {
	parsed, err := url.Parse(st.base + "/mcp")
	if err != nil {
		st.t.Fatalf("parse resource url: %v", err)
	}
	return parsed
}

// useT re-binds the stack's test handle to the running subtest.
func (st *liveStack) useT(t *testing.T) { st.t = t }

// compose builds every production component and starts the TLS server; it is
// reusable for the graceful-restart row on the same port.
func (st *liveStack) compose() {
	t := st.t
	origin, err := url.Parse(st.base)
	if err != nil {
		t.Fatalf("parse origin: %v", err)
	}
	resource := st.resourceURL()
	issuer := &url.URL{Scheme: resource.Scheme, Host: resource.Host}

	st.auditSvc = audit.NewService(mysqlstore.NewAuditStore(st.db))
	st.sessions = session.NewService(mysqlstore.NewSessionStore(st.db), []byte("e2egate-session-pepper-32b"), time.Hour)
	identities := mysqlstore.NewIdentityStore(st.db)
	st.entitlement = entitlement.NewService(mysqlstore.NewEntitlementStore(st.db, gateClock{}, st.auditSvc), gateClock{})
	deviceStore := mysqlstore.NewDeviceStore(st.db)

	// The admin identity is pre-seeded so the admin login maps onto the
	// configured AdminUsers entry by subject (idempotent for recompose).
	st.exec("INSERT IGNORE INTO users (id) VALUES (?)", st.adminUserID)
	st.exec("INSERT IGNORE INTO user_identities (id, user_id, provider, provider_subject, display_name, status) VALUES (?, ?, 'roblox', 'subject-admin', 'E2E Admin', 'active')",
		gateUUID(t), st.adminUserID)

	enrollment, err := device.NewEnrollment(deviceStore, st.entitlement, st.devicePepper, time.Now)
	if err != nil {
		t.Fatalf("enrollment: %v", err)
	}
	enrollment.VerificationBaseURL = st.base
	st.enrollment = enrollment

	flow, err := robloxauth.NewFlow(robloxauth.Config{
		ClientID:        st.provider.clientID,
		ClientSecret:    st.provider.secret,
		RedirectURI:     st.base + "/api/v1/auth/roblox/callback",
		ProviderBaseURL: st.provider.base,
		Issuer:          st.provider.issuer,
		JWKSURI:         st.provider.base + "/.well-known/jwks.json",
	})
	if err != nil {
		t.Fatalf("roblox flow: %v", err)
	}
	robloxHandler := &robloxauth.Handler{
		Flow: flow, Identities: identities, Sessions: st.sessions,
		SuccessRedirect: "/download", SessionMaxAge: time.Hour,
	}

	artifact := device.Artifact{Version: "e2e-1.0.0", Filename: "RobloxBridge.exe", Path: st.artifact}
	download, err := device.NewDownloadHandler(st.sessions, artifact)
	if err != nil {
		t.Fatalf("download handler: %v", err)
	}
	downloadMeta, err := device.NewDownloadMetadataHandler(st.sessions, artifact)
	if err != nil {
		t.Fatalf("download metadata handler: %v", err)
	}

	metadata, err := mcpoauth.NewMetadata(issuer, resource, mcpoauth.SupportedScopes)
	if err != nil {
		t.Fatalf("oauth metadata: %v", err)
	}
	st.oauthStore = mysqlstore.NewOAuthStore(st.db)
	provider, err := mcpoauth.NewProvider(mcpoauth.Config{
		Resource:     resource,
		Store:        st.oauthStore,
		DB:           st.db,
		Audits:       st.auditSvc,
		Entitlements: st.entitlement,
		Sessions:     st.sessions,
		Pepper:       st.oauthPepper,
		LoginPath:    "/login",
	})
	if err != nil {
		t.Fatalf("oauth provider: %v", err)
	}

	gate := health.NewGate(st.db, nil)
	probes := health.NewHandler(gate, nil)

	// The gateway is built after the hub (it needs the registry), so the hub
	// fan-out goes through a slot filled once the gateway exists.
	var gatewayEnvelope func(context.Context, bridgehub.Device, bridgeproto.Envelope)
	hub, err := bridgehub.NewHub(bridgehub.Config{
		Store:             bridgehub.NewSQLStore(st.db),
		Entitlements:      st.entitlement,
		Pepper:            st.devicePepper,
		HelloTimeout:      3 * time.Second,
		HeartbeatInterval: 100 * time.Millisecond,
		HeartbeatTimeout:  2 * time.Second,
		ReauthInterval:    time.Hour,
		MaxEnvelopeBytes:  256 * 1024,
		QueueDepth:        16,
		WriteTimeout:      5 * time.Second,
		OnEnvelope: func(ctx context.Context, device bridgehub.Device, env bridgeproto.Envelope) {
			st.recordInbound(ctx, device, env)
			if gatewayEnvelope != nil {
				gatewayEnvelope(ctx, device, env)
			}
		},
	})
	if err != nil {
		t.Fatalf("hub: %v", err)
	}
	st.hub = hub
	st.registry = hub.Registry()

	limiter, err := httpserver.NewMCPLimiter(httpserver.MCPLimiterConfig{
		Requests: 240, Window: time.Minute, MaxInFlight: 8,
	})
	if err != nil {
		t.Fatalf("mcp limiter: %v", err)
	}
	gateway, err := mcpgateway.NewGateway(mcpgateway.Config{
		OAuth:          st.oauthStore,
		Store:          bridgehub.NewSQLStore(st.db),
		Entitlements:   st.entitlement,
		Audit:          st.auditSvc,
		Registry:       st.registry,
		Pending:        mcpgateway.NewPending(256),
		Limiter:        limiter,
		Pepper:         st.oauthPepper,
		Resource:       resource.String(),
		AllowedOrigins: []string{origin.String()},
		Implementation: mcp.Implementation{Name: "RobloxKit Remote Gateway", Version: "e2egate"},
		RequestTimeout: 15 * time.Second,
		SessionTimeout: time.Hour,
		Now:            time.Now,
	})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	st.gateway = gateway
	gatewayEnvelope = gateway.HandleEnvelope

	endpointLimits, err := httpserver.NewLimiter(httpserver.LimiterConfig{
		Budgets: map[httpserver.Class]httpserver.Budget{
			httpserver.ClassLogin:  {Burst: 60, Refill: 60, Interval: time.Minute},
			httpserver.ClassOAuth:  {Burst: 120, Refill: 120, Interval: time.Minute},
			httpserver.ClassEnroll: {Burst: 120, Refill: 120, Interval: time.Minute},
			httpserver.ClassWSS:    {Burst: 60, Refill: 60, Interval: time.Minute},
			httpserver.ClassAdmin:  {Burst: 60, Refill: 60, Interval: time.Minute},
			httpserver.ClassMCP:    {Burst: 600, Refill: 600, Interval: time.Minute, MaxInFlight: 8},
		},
	})
	if err != nil {
		t.Fatalf("limiter: %v", err)
	}

	work := httpserver.NewWorkGate()
	router, err := httpserver.NewRouter(httpserver.Config{
		Limits:           endpointLimits,
		Sessions:         st.sessions,
		RobloxAuth:       robloxHandler,
		IdentityReader:   deviceStore,
		Entitlements:     st.entitlement,
		Download:         download,
		DownloadMetadata: downloadMeta,
		Enrollment:       st.enrollment,
		Dashboard:        mysqlstore.NewDashboardStore(st.db, st.auditSvc, []byte("e2e-test-pepper")),
		Registry:         httpserver.NewBridgeRegistry(st.registry),
		Admin: &httpserver.AdminConfig{
			Entitlements: st.entitlement,
			OAuth:        st.oauthStore,
			AdminUsers:   []string{st.adminUserID},
		},
		Health:        probes,
		Metadata:      &metadata,
		MCP:           work.MCP(gateway.Handler()),
		OAuth:         provider.Handler(),
		Bridge:        work.WSS(hub),
		AllowedOrigin: origin,
	})
	if err != nil {
		t.Fatalf("router: %v", err)
	}

	cert, err := selfSignedCert()
	if err != nil {
		t.Fatalf("self-signed cert: %v", err)
	}
	listener, err := net.Listen("tcp", st.addr)
	if err != nil {
		t.Fatalf("re-listen %s: %v", st.addr, err)
	}
	kernel := &tlsKernel{
		server: &http.Server{Handler: router, ReadHeaderTimeout: 10 * time.Second},
		ln:     tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{cert}}),
	}
	server, err := httpserver.NewServer(httpserver.ServerConfig{
		Handler:     router,
		Gate:        gate,
		Work:        work,
		Hub:         hub,
		Pool:        nil, // the harness owns the pool lifecycle across rows
		DrainWindow: 5 * time.Second,
		Kernel:      kernel,
	})
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	st.server = server
	go func() { _ = server.ListenAndServe() }()
	st.awaitServing()
}

func (st *liveStack) recordInbound(_ context.Context, device bridgehub.Device, env bridgeproto.Envelope) {
	st.inboundMu.Lock()
	st.inbound = append(st.inbound, inboundRecord{AuthenticatedDeviceID: device.DeviceID, Envelope: env})
	st.inboundMu.Unlock()
}

func (st *liveStack) authenticatedEnvelopeCount(deviceID string) int {
	st.inboundMu.Lock()
	defer st.inboundMu.Unlock()
	total := 0
	for _, record := range st.inbound {
		if record.AuthenticatedDeviceID == deviceID {
			total++
		}
	}
	return total
}

func (st *liveStack) awaitServing() {
	st.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	for time.Now().Before(deadline) {
		resp, err := client.Get(st.base + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	st.t.Fatal("live stack never answered /healthz")
}

func (st *liveStack) shutdown() {
	if st.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		_ = st.server.Shutdown(ctx)
		cancel()
	}
}

func (st *liveStack) awaitRegistry(deviceID string, present bool, timeout time.Duration) time.Time {
	st.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, ok := st.registry.Get(deviceID)
		if ok == present {
			return time.Now()
		}
		time.Sleep(5 * time.Millisecond)
	}
	st.t.Fatalf("timed out waiting for registry presence=%v for %s", present, deviceID)
	return time.Time{}
}

// reopenDB replaces the stack's pool with a fresh connection to the same
// database (the outage row's restore path).
func (st *liveStack) reopenDB() {
	st.t.Helper()
	if st.db != nil {
		_ = st.db.Close()
	}
	db, err := sql.Open("mysql", st.dbDSN)
	if err != nil {
		st.t.Fatalf("reopen database: %v", err)
	}
	st.db = db
}

// ---- HTTP client helpers ----

type liveClient struct {
	t    *testing.T
	http *http.Client
	csrf string
}

func (st *liveStack) newClient() *liveClient {
	jar, err := cookiejar.New(nil)
	if err != nil {
		st.t.Fatalf("cookie jar: %v", err)
	}
	return &liveClient{
		t: st.t,
		http: &http.Client{
			Jar: jar,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (c *liveClient) do(method, path string, body io.Reader, headers map[string]string) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, path, body)
	if err != nil {
		c.t.Fatalf("build %s %s: %v", method, path, err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func (c *liveClient) body(resp *http.Response) []byte {
	c.t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	_ = resp.Body.Close()
	return raw
}

func (c *liveClient) getJSON(path string) (int, map[string]any) {
	c.t.Helper()
	resp := c.do(http.MethodGet, path, nil, nil)
	raw := c.body(resp)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("decode %s response %q: %v", path, raw, err)
		}
	}
	return resp.StatusCode, out
}

// postJSON sends a JSON body with the double-submit CSRF pair.
func (c *liveClient) postJSON(path string, payload any) (int, map[string]any) {
	c.t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		c.t.Fatalf("marshal payload: %v", err)
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if c.csrf != "" {
		headers["X-CSRF-Token"] = c.csrf
	}
	resp := c.do(http.MethodPost, path, strings.NewReader(string(raw)), headers)
	body := c.body(resp)
	var out map[string]any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &out)
	}
	return resp.StatusCode, out
}

// login drives the full real Roblox OAuth login flow through the fixture
// provider and the committed robloxauth handler, returning a session-bound
// client with a CSRF token.
func (st *liveStack) login(subject string) *liveClient {
	c := st.newClient()
	resp := c.do(http.MethodGet, st.base+"/api/v1/auth/roblox/login", nil, nil)
	if resp.StatusCode != http.StatusSeeOther {
		c.t.Fatalf("login begin status = %d", resp.StatusCode)
	}
	authorizeURL, err := resp.Location()
	if err != nil {
		c.t.Fatalf("login begin location: %v", err)
	}
	parsed, err := url.Parse(authorizeURL.String())
	if err != nil {
		c.t.Fatalf("parse authorize url: %v", err)
	}
	q := parsed.Query()
	q.Set("sub", subject) // the fixture account that logs in
	parsed.RawQuery = q.Encode()
	resp = c.do(http.MethodGet, parsed.String(), nil, nil)
	if resp.StatusCode != http.StatusSeeOther {
		c.t.Fatalf("provider authorize status = %d", resp.StatusCode)
	}
	callbackURL, err := resp.Location()
	if err != nil {
		c.t.Fatalf("provider redirect: %v", err)
	}
	resp = c.do(http.MethodGet, callbackURL.String(), nil, nil)
	if resp.StatusCode != http.StatusSeeOther {
		c.t.Fatalf("callback status = %d (body %s)", resp.StatusCode, c.body(resp))
	}
	status, csrf := c.getJSON(st.base + "/api/v1/csrf")
	if status != http.StatusOK {
		c.t.Fatalf("csrf issue status = %d", status)
	}
	token, _ := csrf["csrf_token"].(string)
	if token == "" {
		c.t.Fatal("csrf issuance returned no token")
	}
	c.csrf = token
	return c
}

// enroll runs the committed enrollment flow for the session user.
func (st *liveStack) enroll(session *liveClient, claim device.DeviceClaim) (credentialToken string, deviceID string) {
	st.t.Helper()
	raw, err := json.Marshal(claim)
	if err != nil {
		st.t.Fatalf("marshal claim: %v", err)
	}
	resp := st.newClient().do(http.MethodPost, st.base+"/api/v1/device/enrollment/begin",
		strings.NewReader(string(raw)), map[string]string{"Content-Type": "application/json"})
	beginBody := st.readBody(resp)
	var begin struct {
		UserCode string `json:"user_code"`
	}
	if err := json.Unmarshal(beginBody, &begin); err != nil {
		st.t.Fatalf("decode begin response %q: %v", beginBody, err)
	}
	status, _ := session.postJSON(st.base+"/api/v1/enrollments/approve", map[string]string{"user_code": begin.UserCode})
	if status != http.StatusNoContent {
		st.t.Fatalf("approve status = %d", status)
	}
	raw, err = json.Marshal(map[string]string{"device_code": begin.UserCode})
	if err != nil {
		st.t.Fatalf("marshal exchange: %v", err)
	}
	resp = st.newClient().do(http.MethodPost, st.base+"/api/v1/device/enrollment/exchange",
		strings.NewReader(string(raw)), map[string]string{"Content-Type": "application/json"})
	exchangeBody := st.readBody(resp)
	var cred struct {
		DeviceCredential string `json:"device_credential"`
		DeviceID         string `json:"device_id"`
	}
	if err := json.Unmarshal(exchangeBody, &cred); err != nil {
		st.t.Fatalf("decode exchange response %q: %v", exchangeBody, err)
	}
	if resp.StatusCode != http.StatusOK || cred.DeviceCredential == "" {
		st.t.Fatalf("exchange status = %d body %s", resp.StatusCode, exchangeBody)
	}
	return cred.DeviceCredential, cred.DeviceID
}

func (st *liveStack) readBody(resp *http.Response) []byte {
	st.t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		st.t.Fatalf("read response: %v", err)
	}
	_ = resp.Body.Close()
	return raw
}

// ---- bridge runner ----

// bridgeRunner drives one bridgeapp.RunRemote instance. The bridge hands
// itself a FRESH MCP child per connection cycle (the production contract),
// so the current child and the total child count are tracked here.
type bridgeRunner struct {
	events *bridgeEventLog
	cancel context.CancelFunc
	result <-chan error

	procMu  sync.Mutex
	proc    *fakeMCP // the current (latest) MCP child
	procSeq int      // how many children the bridge has started
}

func (r *bridgeRunner) currentProcess() *fakeMCP {
	r.procMu.Lock()
	defer r.procMu.Unlock()
	return r.proc
}

func (r *bridgeRunner) childCount() int {
	r.procMu.Lock()
	defer r.procMu.Unlock()
	return r.procSeq
}

// awaitChildCount waits until the bridge has started n children.
func (r *bridgeRunner) awaitChildCount(n int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if r.childCount() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	r.events.t.Fatalf("timed out waiting for child #%d; started %d", n, r.childCount())
}

func (st *liveStack) startBridge(credentialToken, deviceID, deviceName string, studioCount int) *bridgeRunner {
	t := st.t
	t.Helper()
	events := &bridgeEventLog{t: t, observed: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	runner := &bridgeRunner{events: events, cancel: cancel, result: nil}
	deps := bridgeapp.RemoteDeps{
		Machine: statusui.NewMachine(),
		// A fresh MCP child per call — the production contract — tracked so
		// the crash/restart row can observe the restart.
		NewProcess: func() mcpprocess.Process {
			proc := newFakeMCP()
			runner.procMu.Lock()
			runner.proc = proc
			runner.procSeq++
			runner.procMu.Unlock()
			return proc
		},
		Credential: &staticCredential{credential: credentialToken},
		GatewayURL: "wss" + strings.TrimPrefix(st.base, "https") + "/bridge",
		DeviceID:   deviceID,
		DeviceName: deviceName,
		HTTPClient: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}},
		StudioReady: func(context.Context) (int, error) { return studioCount, nil },
		EventSink:   events.sink,
		Backoff: bridgeapp.Backoff{
			Base: 50 * time.Millisecond, Max: 500 * time.Millisecond, Jitter: 20 * time.Millisecond,
		},
		Random:          &patternReader{},
		ConnectTimeout:  3 * time.Second,
		ResponseTimeout: 5 * time.Second,
		WriteTimeout:    5 * time.Second,
		QueueLimit:      16,
		MaxMessageBytes: 256 * 1024,
		BridgeVersion:   "e2egate",
	}
	result := make(chan error, 1)
	runner.result = result
	go func() { result <- bridgeapp.RunRemote(ctx, deps) }()
	t.Cleanup(cancel)
	return runner
}

func (r *bridgeRunner) awaitConnected(timeout time.Duration) {
	r.awaitState(statusui.Connected, timeout)
}

func (r *bridgeRunner) awaitState(state statusui.State, timeout time.Duration) {
	r.awaitStateAfter(state, 0, timeout)
}

// awaitStateAfter requires an event appended after baseline, so a historical
// Connected event cannot satisfy a reconnect wait.
func (r *bridgeRunner) awaitStateAfter(state statusui.State, baseline int, timeout time.Duration) {
	r.events.awaitStateAfter(state, baseline, timeout)
}

type bridgeEventLog struct {
	t        *testing.T
	mu       sync.Mutex
	events   []statusui.Event
	observed chan struct{}
}

func (l *bridgeEventLog) sink(event statusui.Event) error {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
	select {
	case l.observed <- struct{}{}:
	default:
	}
	return nil
}

func (l *bridgeEventLog) snapshot() []statusui.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]statusui.Event(nil), l.events...)
}

// cursor is the current append-only event count and can be captured before a
// teardown to distinguish subsequent state transitions from history.
func (l *bridgeEventLog) cursor() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

func (l *bridgeEventLog) awaitStateAfter(state statusui.State, baseline int, timeout time.Duration) {
	l.t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		events := l.snapshot()
		if baseline < 0 || baseline > len(events) {
			l.t.Fatalf("invalid event cursor %d for %d events", baseline, len(events))
		}
		for _, event := range events[baseline:] {
			if event.State == state {
				return
			}
		}
		select {
		case <-l.observed:
		case <-timer.C:
			l.t.Fatalf("timed out waiting for new %q after event %d; saw %v", state, baseline, l.states())
		}
	}
}

func (l *bridgeEventLog) count(state statusui.State) int {
	total := 0
	for _, event := range l.snapshot() {
		if event.State == state {
			total++
		}
	}
	return total
}

func (l *bridgeEventLog) states() []statusui.State {
	out := make([]statusui.State, 0, len(l.events))
	for _, event := range l.snapshot() {
		out = append(out, event.State)
	}
	return out
}

type staticCredential struct{ credential string }

func (s *staticCredential) Load() ([]byte, error) { return []byte(s.credential), nil }
func (s *staticCredential) Save([]byte) error     { return nil }
func (s *staticCredential) Delete() error         { return nil }

type patternReader struct{}

func (*patternReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0x5c
	}
	return len(p), nil
}

// ---- database helpers ----

func (st *liveStack) queryRows(query string, args ...any) []map[string]any {
	st.t.Helper()
	rows, err := st.db.Query(query, args...)
	if err != nil {
		st.t.Fatalf("query %s: %v", query, err)
	}
	defer rows.Close()
	var out []map[string]any
	cols, err := rows.Columns()
	if err != nil {
		st.t.Fatalf("columns: %v", err)
	}
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			st.t.Fatalf("scan: %v", err)
		}
		row := map[string]any{}
		for i, col := range cols {
			row[col] = values[i]
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		st.t.Fatalf("rows: %v", err)
	}
	return out
}

func (st *liveStack) queryInt(query string, args ...any) int {
	st.t.Helper()
	rows := st.queryRows(query, args...)
	if len(rows) == 0 {
		return 0
	}
	for _, cell := range rows[0] {
		if n, ok := cell.(int64); ok {
			return int(n)
		}
	}
	st.t.Fatalf("query %q returned no numeric cell", query)
	return 0
}

func (st *liveStack) queryString(query string, args ...any) string {
	st.t.Helper()
	rows := st.queryRows(query, args...)
	if len(rows) == 0 {
		return ""
	}
	for _, value := range rows[0] {
		if s, ok := value.([]byte); ok {
			return string(s)
		}
	}
	return ""
}

func (st *liveStack) exec(query string, args ...any) {
	st.t.Helper()
	if _, err := st.db.Exec(query, args...); err != nil {
		st.t.Fatalf("exec %s: %v", query, err)
	}
}

func (st *liveStack) trialFingerprint(userID string) []map[string]any {
	return st.queryRows("SELECT id, started_at, ends_at FROM trial_entitlements WHERE user_id = ?", userID)
}

func (st *liveStack) userIDBySubject(subject string) string {
	st.t.Helper()
	return st.queryString("SELECT user_id FROM user_identities WHERE provider = 'roblox' AND provider_subject = ? LIMIT 1", subject)
}

func (st *liveStack) latestUserID() string {
	st.t.Helper()
	return st.queryString("SELECT id FROM users ORDER BY created_at DESC, id DESC LIMIT 1")
}

func (st *liveStack) licenseID(userID string) string {
	st.t.Helper()
	return st.queryString("SELECT license_id FROM license_device_bindings WHERE user_id = ? AND status = 'active' LIMIT 1", userID)
}

func (st *liveStack) seedStudio(deviceID, userID, studioID string) {
	st.t.Helper()
	st.exec("INSERT INTO studio_sessions (id, user_id, device_id, studio_id, status, started_at) VALUES (?, ?, ?, ?, 'active', NOW(6))",
		gateUUID(st.t), userID, deviceID, studioID)
}

func (st *liveStack) studioSessionID(deviceID, studioID string) string {
	st.t.Helper()
	return st.queryString("SELECT id FROM studio_sessions WHERE device_id = ? AND studio_id = ? AND status = 'active' LIMIT 1", deviceID, studioID)
}

// seedLicenseAndBinding inserts an active paid license + device binding —
// the license-only entitlement source.
func (st *liveStack) seedLicenseAndBinding(userID, deviceID string) {
	st.t.Helper()
	identityID := st.queryString("SELECT id FROM user_identities WHERE user_id = ? AND provider = 'roblox' LIMIT 1", userID)
	licenseID := gateUUID(st.t)
	st.exec("INSERT INTO licenses (id, user_id, roblox_identity_id, status, device_slots) VALUES (?, ?, ?, 'active', 3)",
		licenseID, userID, identityID)
	st.exec("INSERT INTO license_device_bindings (id, user_id, license_id, device_id, slot_ordinal, status) VALUES (?, ?, ?, ?, 1, 'active')",
		gateUUID(st.t), userID, licenseID, deviceID)
}

// seedLicensedOnlyUser provisions one user whose trial window is already
// closed but who holds an active paid license + slot binding — the
// license-only entitlement source (trial rows are append-only, so the
// expired window is seeded, never updated).
func (st *liveStack) seedLicensedOnlyUser(subject string) (userID, deviceID, credential string) {
	st.t.Helper()
	userID = gateUUID(st.t)
	identityID := gateUUID(st.t)
	deviceID = gateUUID(st.t)
	st.exec("INSERT INTO users (id) VALUES (?)", userID)
	st.exec("INSERT INTO user_identities (id, user_id, provider, provider_subject, display_name, status) VALUES (?, ?, 'roblox', ?, 'Licensed Fixture', 'active')",
		identityID, userID, subject)
	st.exec("INSERT INTO trial_entitlements (id, user_id, started_at, ends_at) VALUES (?, ?, ?, ?)",
		gateUUID(st.t), userID, time.Now().UTC().Add(-15*24*time.Hour), time.Now().UTC().Add(-24*time.Hour))
	st.exec("INSERT INTO trial_entitlement_identities (id, trial_entitlement_id, user_id, provider, provider_subject) VALUES (?, ?, ?, 'roblox', ?)",
		gateUUID(st.t), st.queryString("SELECT id FROM trial_entitlements WHERE user_id = ? LIMIT 1", userID), userID, subject)
	credential = st.seedDeviceCredential(userID, deviceID)
	st.seedLicenseAndBinding(userID, deviceID)
	return userID, deviceID, credential
}

// seedDeviceCredential inserts one live credential for the device.
func (st *liveStack) seedDeviceCredential(userID, deviceID string) string {
	st.t.Helper()
	plain := "rkd_gate_" + gateUUID(st.t)
	digest := credential.Digest(plain, st.devicePepper)
	st.exec("INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, 'Gate Fixture Device', 'active')", deviceID, userID)
	st.exec("INSERT INTO device_credentials (id, user_id, device_id, credential_digest, expires_at, revoked_at) VALUES (?, ?, ?, ?, NULL, NULL)",
		gateUUID(st.t), userID, deviceID, digest[:])
	return plain
}

// seedGrantToken mints a connector grant + access token straight through the
// committed OAuth store.
func (st *liveStack) seedGrantToken(userID, deviceID string) string {
	st.t.Helper()
	now := time.Now().UTC()
	client, err := st.oauthStore.RegisterClient(st.t.Context(), mcpoauth.Client{
		ClientID:     "https://seeded.example/connector",
		ClientName:   "Seeded Connector",
		RedirectURIs: []string{"https://seeded.example/callback"},
	})
	if err != nil {
		st.t.Fatalf("register seeded client: %v", err)
	}
	grantID := gateUUID(st.t)
	if _, err := st.oauthStore.SaveGrant(st.t.Context(), mcpoauth.Grant{
		ID: grantID, UserID: userID, ClientID: client.ID,
		DeviceID: deviceID, Scopes: []string{"mcp:connect"}, Resource: st.base + "/mcp", CreatedAt: now,
	}); err != nil {
		st.t.Fatalf("save grant: %v", err)
	}
	plain := "mca_gate_" + gateUUID(st.t)
	tokenDigest := credential.Digest(plain, st.oauthPepper)
	if err := st.oauthStore.IssueTokens(st.t.Context(),
		mcpoauth.AccessToken{ID: gateUUID(st.t), UserID: userID, GrantID: grantID, ExpiresAt: now.Add(time.Hour), CreatedAt: now},
		tokenDigest,
		mcpoauth.RefreshToken{ID: gateUUID(st.t), UserID: userID, GrantID: grantID, FamilyID: gateUUID(st.t), ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now},
		tokenDigest); err != nil {
		st.t.Fatalf("issue tokens: %v", err)
	}
	return plain
}

// awaitAudit waits bounded for the admin-action ledger row.
func (st *liveStack) awaitAudit(action, actorUserID string, timeout time.Duration) int {
	st.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if n := st.queryInt("SELECT COUNT(*) FROM admin_actions WHERE action = ? AND actor_user_id = ?", action, actorUserID); n >= 1 {
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}
