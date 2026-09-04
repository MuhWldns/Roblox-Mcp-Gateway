package mcpgateway

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-sql-driver/mysql"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"robloxkit/internal/audit"
	"robloxkit/internal/bridgehub"
	"robloxkit/internal/credential"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/httpserver"
	"robloxkit/internal/mcpoauth"
	"robloxkit/internal/mysqlstore"
	"robloxkit/pkg/bridgeproto"
)

// systemClock pins policy evaluation to wall time. Note: -race is
// unavailable in this environment (CGO_ENABLED=0); concurrency safety is
// exercised via logical stress tests instead.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func gwUUID(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

// gatewayTestDatabase creates a uniquely named, fully migrated temporary
// MySQL database per test, mirroring the bridgehub test helper.
func gatewayTestDatabase(t *testing.T) *sql.DB {
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
	if !gwSafeIdentifier(dbName) {
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
	t.Cleanup(func() { db.Close() })
	if _, err := mysqlstore.Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

func gwSafeIdentifier(s string) bool {
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

// gatewaySpec describes one test's fixture rows and limiter bounds.
type gatewaySpec struct {
	burst         int
	window        time.Duration
	maxInFlight   int
	grantScopes   []string
	resource      string        // the resource this gateway serves
	grantResource string        // the resource the grant was issued for
	licenseStatus string        // "" = no license row (expired-trial fixture)
	tokenExpiry   time.Duration // relative to now; negative = already expired
	tokenRevoked  bool
	trialEndsAt   time.Duration // relative to now; negative = expired trial
}

const (
	testResource       = "https://gateway.example.com/mcp"
	otherTestResource  = "https://other.example.com/mcp"
	testAllowedOrigin  = "https://dashboard.example.com"
	defaultRequestTO   = 3 * time.Second
	gatewayTestVersion = "16.0.0-test"
)

func defaultGatewaySpec() gatewaySpec {
	return gatewaySpec{
		burst:         100,
		window:        time.Minute,
		maxInFlight:   4,
		grantScopes:   []string{mcpoauth.ScopeConnect, mcpoauth.ScopeStudioRead},
		resource:      testResource,
		grantResource: testResource,
		licenseStatus: "active",
		tokenExpiry:   time.Hour,
		trialEndsAt:   13 * 24 * time.Hour,
	}
}

// gatewayFixture provisions a licensed device, a connector grant, an access
// token, a live hub, the gateway, a serving TLS endpoint, and a fake Bridge
// device against a temporary MySQL database.
type gatewayFixture struct {
	t  *testing.T
	db *sql.DB

	devicePepper     []byte
	oauthPepper      []byte
	clientID         string
	deviceCredential string

	hub        *bridgehub.Hub
	registry   *bridgehub.Registry
	gateway    *Gateway
	pending    *Pending
	server     *httptest.Server
	limits     bridgeproto.Limits
	auditStore *mysqlstore.AuditStore

	userID     string
	identityID string
	deviceID   string
	subject    string
	grantID    string
	token      string

	device *fakeDevice

	envelopeMu sync.Mutex
	envelopes  []func(context.Context, bridgehub.Device, bridgeproto.Envelope)
}

func newGatewayFixture(t *testing.T, mutate func(*gatewaySpec)) *gatewayFixture {
	t.Helper()
	spec := defaultGatewaySpec()
	if mutate != nil {
		mutate(&spec)
	}

	db := gatewayTestDatabase(t)
	fx := &gatewayFixture{
		t:            t,
		db:           db,
		devicePepper: []byte("gateway-test-device-pepper"),
		oauthPepper:  []byte("gateway-test-oauth-pepper"),
		limits:       bridgeproto.Limits{MaxPayloadBytes: 256 * 1024},
	}
	fx.auditStore = mysqlstore.NewAuditStore(db)

	now := time.Now().UTC()
	fx.userID = gwUUID(t)
	fx.identityID = gwUUID(t)
	fx.deviceID = gwUUID(t)
	fx.subject = fmt.Sprintf("gateway_subject_%d", time.Now().UnixNano())
	licenseID := gwUUID(t)
	bindingID := gwUUID(t)
	trialID := gwUUID(t)
	trialIdentityID := gwUUID(t)
	credentialID := gwUUID(t)

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatalf("fixture insert failed: %v\nquery: %s", err, query)
		}
	}
	exec(`INSERT INTO users (id) VALUES (?)`, fx.userID)
	exec(`INSERT INTO user_identities (id, user_id, provider, provider_subject, display_name, status) VALUES (?, ?, ?, ?, ?, 'active')`,
		fx.identityID, fx.userID, "roblox", fx.subject, "Gateway Fixture")
	exec(`INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, ?, 'active')`, fx.deviceID, fx.userID, "Test Device")
	if spec.licenseStatus != "" {
		exec(`INSERT INTO licenses (id, user_id, roblox_identity_id, status, device_slots) VALUES (?, ?, ?, ?, 1)`,
			licenseID, fx.userID, fx.identityID, spec.licenseStatus)
		exec(`INSERT INTO license_device_bindings (id, user_id, license_id, device_id, slot_ordinal, status) VALUES (?, ?, ?, ?, 1, 'active')`,
			bindingID, fx.userID, licenseID, fx.deviceID)
	}
	exec(`INSERT INTO trial_entitlements (id, user_id, started_at, ends_at) VALUES (?, ?, ?, ?)`,
		trialID, fx.userID, now.Add(-time.Hour), now.Add(spec.trialEndsAt))
	exec(`INSERT INTO trial_entitlement_identities (id, trial_entitlement_id, user_id, provider, provider_subject) VALUES (?, ?, ?, ?, ?)`,
		trialIdentityID, trialID, fx.userID, "roblox", fx.subject)

	// Device credential for the hub's WSS authentication.
	devicePlain, deviceDigest := mustCredential(t, fx.devicePepper)
	fx.deviceCredential = devicePlain
	exec(`INSERT INTO device_credentials (id, user_id, device_id, credential_digest, expires_at, revoked_at) VALUES (?, ?, ?, ?, ?, ?)`,
		credentialID, fx.userID, fx.deviceID, deviceDigest[:], nil, nil)

	// Connector client, grant, and access token straight through the
	// committed OAuth store contract.
	oauthStore := mysqlstore.NewOAuthStore(db)
	client, err := oauthStore.RegisterClient(t.Context(), mcpoauth.Client{
		ClientID:     "https://chatgpt.com/aip/connector",
		ClientName:   "ChatGPT",
		RedirectURIs: []string{"https://chatgpt.com/aip/oauth/callback"},
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	fx.clientID = client.ID
	fx.grantID = gwUUID(t)
	grant, err := oauthStore.SaveGrant(t.Context(), mcpoauth.Grant{
		ID:        fx.grantID,
		UserID:    fx.userID,
		ClientID:  fx.clientID,
		DeviceID:  fx.deviceID,
		Scopes:    append([]string(nil), spec.grantScopes...),
		Resource:  spec.grantResource,
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("save grant: %v", err)
	}
	plain, digest := mustCredential(t, fx.oauthPepper)
	access := mcpoauth.AccessToken{
		ID:        gwUUID(t),
		UserID:    fx.userID,
		GrantID:   grant.ID,
		ExpiresAt: now.Add(spec.tokenExpiry),
		CreatedAt: now,
	}
	_, refreshDigest := mustCredential(t, fx.oauthPepper)
	if err := oauthStore.IssueTokens(t.Context(), access, digest,
		mcpoauth.RefreshToken{
			ID: gwUUID(t), UserID: fx.userID, GrantID: grant.ID, FamilyID: gwUUID(t),
			ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now,
		}, refreshDigest); err != nil {
		t.Fatalf("issue tokens: %v", err)
	}
	fx.token = plain
	if spec.tokenRevoked {
		if err := oauthStore.RevokeAccessToken(t.Context(), digest, now); err != nil {
			t.Fatalf("revoke access token: %v", err)
		}
	}

	auditService := audit.NewService(fx.auditStore)
	clock := systemClock{}
	entitlements := entitlement.NewService(mysqlstore.NewEntitlementStore(db, clock, auditService), clock)
	fx.pending = NewPending(256)
	limiter, err := httpserver.NewMCPLimiter(httpserver.MCPLimiterConfig{
		Requests:    spec.burst,
		Window:      spec.window,
		MaxInFlight: spec.maxInFlight,
	})
	if err != nil {
		t.Fatalf("new limiter: %v", err)
	}

	// The hub owns the live connection registry and routes inbound device
	// envelopes through the fixture dispatcher, which fans them out to the
	// gateway and any test-built relays.
	hub, err := bridgehub.NewHub(bridgehub.Config{
		Store:             bridgehub.NewSQLStore(db),
		Entitlements:      entitlements,
		Pepper:            fx.devicePepper,
		HelloTimeout:      3 * time.Second,
		HeartbeatInterval: time.Hour,
		HeartbeatTimeout:  time.Hour,
		ReauthInterval:    time.Hour,
		MaxEnvelopeBytes:  fx.limits.MaxPayloadBytes,
		QueueDepth:        16,
		WriteTimeout:      5 * time.Second,
		OnEnvelope:        fx.dispatchEnvelope,
	})
	if err != nil {
		t.Fatalf("new hub: %v", err)
	}
	fx.hub = hub
	fx.registry = hub.Registry()
	t.Cleanup(hub.Shutdown)

	gateway, err := NewGateway(Config{
		OAuth:          oauthStore,
		Store:          bridgehub.NewSQLStore(db),
		Entitlements:   entitlements,
		Audit:          auditService,
		Registry:       fx.registry,
		Pending:        fx.pending,
		Limiter:        limiter,
		Pepper:         fx.oauthPepper,
		Resource:       spec.resource,
		AllowedOrigins: []string{testAllowedOrigin},
		Implementation: mcp.Implementation{Name: "RobloxKit Remote Gateway", Version: gatewayTestVersion},
		RequestTimeout: defaultRequestTO,
		SessionTimeout: time.Hour,
		Now:            time.Now,
	})
	if err != nil {
		t.Fatalf("new gateway: %v", err)
	}
	fx.gateway = gateway
	fx.registerEnvelopeHandler(gateway.HandleEnvelope)

	mux := http.NewServeMux()
	mux.Handle("/bridge", hub)
	mux.Handle("/mcp", gateway.Handler())
	fx.server = httptest.NewTLSServer(mux)
	t.Cleanup(fx.server.Close)
	return fx
}

func (fx *gatewayFixture) registerEnvelopeHandler(handler func(context.Context, bridgehub.Device, bridgeproto.Envelope)) {
	fx.envelopeMu.Lock()
	defer fx.envelopeMu.Unlock()
	fx.envelopes = append(fx.envelopes, handler)
}

func (fx *gatewayFixture) dispatchEnvelope(ctx context.Context, device bridgehub.Device, env bridgeproto.Envelope) {
	fx.envelopeMu.Lock()
	handlers := make([]func(context.Context, bridgehub.Device, bridgeproto.Envelope), len(fx.envelopes))
	copy(handlers, fx.envelopes)
	fx.envelopeMu.Unlock()
	for _, handler := range handlers {
		handler(ctx, device, env)
	}
}

func (fx *gatewayFixture) mcpURL() string {
	return fx.server.URL + "/mcp"
}

func (fx *gatewayFixture) bridgeURL() string {
	return "wss" + strings.TrimPrefix(fx.server.URL, "https") + "/bridge"
}

func (fx *gatewayFixture) httpClient() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

func mustCredential(t *testing.T, pepper []byte) (string, [32]byte) {
	t.Helper()
	plain, digest, err := credential.Generate("rk13_", 32, pepper)
	if err != nil {
		t.Fatalf("generate credential: %v", err)
	}
	return plain, digest
}

func hubDevice(fx *gatewayFixture) bridgehub.Device {
	return bridgehub.Device{UserID: fx.userID, DeviceID: fx.deviceID}
}

// auditRow is one persisted denial audit event.
type auditRow struct {
	Action      string
	Reason      string
	TargetType  string
	TargetID    sql.NullString
	UserID      sql.NullString
	Metadata    sql.NullString
	Correlation string
}

func (fx *gatewayFixture) auditRows(t *testing.T, action string) []auditRow {
	t.Helper()
	rows, err := fx.db.QueryContext(context.Background(),
		`SELECT action, reason, target_type, target_id, user_id, metadata, correlation_id
		 FROM audit_logs WHERE action = ? ORDER BY created_at`, action)
	if err != nil {
		t.Fatalf("query audit rows: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.Action, &row.Reason, &row.TargetType, &row.TargetID,
			&row.UserID, &row.Metadata, &row.Correlation); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit rows: %v", err)
	}
	return out
}

func (fx *gatewayFixture) requireAuditReason(t *testing.T, action, reason string) auditRow {
	t.Helper()
	for _, row := range fx.auditRows(t, action) {
		if row.Reason == reason {
			return row
		}
	}
	t.Fatalf("no %q audit row with reason %q; got %+v", action, reason, fx.auditRows(t, action))
	return auditRow{}
}

// ---- fake Bridge device ----

// fakeDevice is a scripted Bridge: it completes the hub handshake, reports
// the Task-12 status snapshot, and answers relayed requests from a canned
// Studio MCP catalog.
type fakeDevice struct {
	t  *testing.T
	fx *gatewayFixture
	ws *websocket.Conn

	mu      sync.Mutex
	holding bool
	records []bridgeproto.Envelope

	hold func(env bridgeproto.Envelope) bool // nil → answer everything
}

func (fx *gatewayFixture) connectDevice(t *testing.T, mutate func(*fakeDevice)) *fakeDevice {
	t.Helper()
	header := http.Header{}
	header.Set("Authorization", "Bearer "+fx.deviceCredential)
	dialCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(dialCtx, fx.bridgeURL(), &websocket.DialOptions{
		HTTPHeader: header,
		HTTPClient: fx.httpClient(),
	})
	if err != nil {
		t.Fatalf("device dial failed: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })

	device := &fakeDevice{t: t, fx: fx, ws: ws}
	if mutate != nil {
		mutate(device)
	}
	fx.device = device
	ws.SetReadLimit(1 << 20)

	// hello + full status snapshot per the Task-12 device contract.
	hello, err := bridgeproto.Encode(bridgeproto.Envelope{
		Version: bridgeproto.Version, Type: bridgeproto.TypeHello, DeviceID: fx.deviceID,
		Payload: json.RawMessage(`{"bridge_version":"test","platform":"windows","capabilities":[]}`),
	}, fx.limits)
	if err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer writeCancel()
	if err := ws.Write(writeCtx, websocket.MessageBinary, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	device.sendStatus(true, 1)

	go device.readLoop()
	return device
}

func (d *fakeDevice) sendStatus(ready bool, studioCount int) {
	payload, err := json.Marshal(map[string]any{
		"state": "connected", "device_name": "gw-test", "studio_count": studioCount,
		"mcp_ready": ready, "gateway_connected": true, "bridge_version": "test",
		"capabilities": []string{},
	})
	if err != nil {
		d.t.Fatalf("marshal status: %v", err)
	}
	env := bridgeproto.Envelope{
		Version: bridgeproto.Version, Type: bridgeproto.TypeStatus, DeviceID: d.fx.deviceID,
		Payload: payload,
	}
	if err := d.write(env); err != nil {
		d.t.Fatalf("write status: %v", err)
	}
}

func (d *fakeDevice) write(env bridgeproto.Envelope) error {
	data, err := bridgeproto.Encode(env, d.fx.limits)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return d.ws.Write(ctx, websocket.MessageBinary, data)
}

func (d *fakeDevice) setHold(hold func(env bridgeproto.Envelope) bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hold = hold
}

func (d *fakeDevice) record(env bridgeproto.Envelope) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.records = append(d.records, env)
}

// requests returns the TypeRequest envelopes received so far.
func (d *fakeDevice) requests() []bridgeproto.Envelope {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []bridgeproto.Envelope
	for _, env := range d.records {
		if env.Type == bridgeproto.TypeRequest {
			out = append(out, env)
		}
	}
	return out
}

// cancels returns the TypeCancel envelopes received so far.
func (d *fakeDevice) cancels() []bridgeproto.Envelope {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []bridgeproto.Envelope
	for _, env := range d.records {
		if env.Type == bridgeproto.TypeCancel {
			out = append(out, env)
		}
	}
	return out
}

// readLoop consumes hub envelopes, records them, and answers TypeRequest
// messages that are not withheld by the hold hook.
func (d *fakeDevice) readLoop() {
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		_, data, err := d.ws.Read(ctx)
		cancel()
		if err != nil {
			return
		}
		env, err := bridgeproto.Decode(data, bridgeproto.Limits{MaxPayloadBytes: 4 << 20})
		if err != nil {
			return
		}
		if env.Type != bridgeproto.TypeRequest {
			if env.Type == bridgeproto.TypeCancel {
				d.record(env)
			}
			continue
		}
		d.record(env)
		d.mu.Lock()
		hold := d.hold
		d.mu.Unlock()
		if hold != nil && hold(env) {
			continue
		}
		d.respond(env)
	}
}

// respond answers one relayed JSON-RPC request with the canned Studio MCP
// behavior: a catalog for tools/list and a text-content echo for tools/call.
func (d *fakeDevice) respond(env bridgeproto.Envelope) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(env.Payload, &request); err != nil {
		return
	}
	var result json.RawMessage
	switch request.Method {
	case "tools/list":
		result = json.RawMessage(`{"tools":[
			{"name":"get_instance_tree","description":"Returns the open place's instance tree.","inputSchema":{"type":"object","properties":{}}},
			{"name":"set_instance_properties","description":"Sets properties on instances.","inputSchema":{"type":"object"}},
			{"name":"run_playtest","description":"Starts a playtest session.","inputSchema":{"type":"object"}},
			{"name":"get_unreleased_plugin_pack","description":"Not an official tool.","inputSchema":{"type":"object"}}
		]}`)
	case "tools/call":
		result = json.RawMessage(`{"content":[{"type":"text","text":"studio ok"}],"isError":false}`)
	default:
		result = json.RawMessage(`{}`)
	}
	response, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(request.ID), "result": result,
	})
	if err != nil {
		d.t.Errorf("marshal device response: %v", err)
		return
	}
	if err := d.write(bridgeproto.Envelope{
		Version: bridgeproto.Version, Type: bridgeproto.TypeResponse,
		GatewayRequestID: env.GatewayRequestID, DeviceID: d.fx.deviceID,
		Payload: response,
	}); err != nil {
		d.t.Errorf("device response write: %v", err)
	}
}

func (d *fakeDevice) close() {
	_ = d.ws.CloseNow()
}

// ---- MCP HTTP client helpers ----

type mcpSession struct {
	t         *testing.T
	fx        *gatewayFixture
	sessionID string
	nextID    int
}

func (fx *gatewayFixture) post(t *testing.T, token, sessionID, body, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, fx.mcpURL(), strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := fx.httpClient().Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// openSession completes initialize + initialized and returns the session.
func (fx *gatewayFixture) openSession(t *testing.T) *mcpSession {
	return fx.openSessionWithToken(t, fx.token)
}

func (fx *gatewayFixture) openSessionWithToken(t *testing.T, token string) *mcpSession {
	t.Helper()
	session := &mcpSession{t: t, fx: fx, nextID: 1}
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"gateway-test","version":"1.0"}}}`
	resp := fx.post(t, token, "", body, "")
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize status = %d, want %d (body %s)", resp.StatusCode, http.StatusOK, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("initialize content type = %q, want text/event-stream", ct)
	}
	session.sessionID = resp.Header.Get("Mcp-Session-Id")
	if session.sessionID == "" {
		t.Fatal("initialize response missing Mcp-Session-Id")
	}
	messages := parseSSE(t, resp)
	if len(messages) == 0 {
		t.Fatal("initialize response carried no JSON-RPC message")
	}
	var initResp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(messages[0], &initResp); err != nil {
		t.Fatalf("decode initialize result %s: %v", messages[0], err)
	}
	if initResp.Result.ProtocolVersion == "" {
		t.Fatalf("initialize result missing protocolVersion: %s", messages[0])
	}
	if initResp.Result.ServerInfo.Name != "RobloxKit Remote Gateway" {
		t.Fatalf("initialize serverInfo name = %q", initResp.Result.ServerInfo.Name)
	}

	notif := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	notifResp := fx.post(t, token, session.sessionID, notif, "")
	if notifResp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(notifResp.Body)
		t.Fatalf("initialized status = %d, want %d (body %s)", notifResp.StatusCode, http.StatusAccepted, raw)
	}
	return session
}

// call posts one JSON-RPC request and returns the parsed response.
func (s *mcpSession) call(method string, params string) map[string]json.RawMessage {
	s.t.Helper()
	s.nextID++
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, s.nextID, method, params)
	resp := s.fx.post(s.t, s.fx.token, s.sessionID, body, "")
	return s.parseResponse(resp, method)
}

func (s *mcpSession) parseResponse(resp *http.Response, method string) map[string]json.RawMessage {
	s.t.Helper()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		s.t.Fatalf("%s status = %d, want %d (body %s)", method, resp.StatusCode, http.StatusOK, raw)
	}
	messages := parseSSE(s.t, resp)
	if len(messages) == 0 {
		s.t.Fatalf("%s response carried no JSON-RPC message", method)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(messages[len(messages)-1], &envelope); err != nil {
		s.t.Fatalf("decode %s response %s: %v", method, messages[len(messages)-1], err)
	}
	return envelope
}

// asyncCall posts a JSON-RPC request in the background and returns its
// response future; used for calls the device withholds.
func (s *mcpSession) asyncCall(method string, params string) <-chan map[string]json.RawMessage {
	s.t.Helper()
	s.nextID++
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, s.nextID, method, params)
	out := make(chan map[string]json.RawMessage, 1)
	go func() {
		resp := s.fx.post(s.t, s.fx.token, s.sessionID, body, "")
		var envelope map[string]json.RawMessage
		if resp.StatusCode == http.StatusOK {
			messages := parseSSE(s.t, resp)
			if len(messages) > 0 {
				if err := json.Unmarshal(messages[len(messages)-1], &envelope); err != nil {
					s.t.Errorf("decode async response: %v", err)
				}
			}
		} else {
			envelope = map[string]json.RawMessage{
				"http_status": json.RawMessage(fmt.Sprintf("%d", resp.StatusCode)),
			}
		}
		out <- envelope
	}()
	return out
}

// notify posts a JSON-RPC notification and asserts 202 Accepted.
func (s *mcpSession) notify(method string, params string) {
	s.t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","method":%q,"params":%s}`, method, params)
	resp := s.fx.post(s.t, s.fx.token, s.sessionID, body, "")
	if resp.StatusCode != http.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		s.t.Fatalf("%s notification status = %d, want %d (body %s)", method, resp.StatusCode, http.StatusAccepted, raw)
	}
}

// parseSSE extracts every data message from a completed event stream.
func parseSSE(t *testing.T, resp *http.Response) []json.RawMessage {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	var messages []json.RawMessage
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			messages = append(messages, json.RawMessage(data))
		}
	}
	return messages
}

func jsonrpcErrorCode(t *testing.T, envelope map[string]json.RawMessage) int {
	t.Helper()
	raw, ok := envelope["error"]
	if !ok {
		return 0
	}
	var wire struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode jsonrpc error %s: %v", raw, err)
	}
	return wire.Code
}

func toolNames(t *testing.T, envelope map[string]json.RawMessage) []string {
	t.Helper()
	raw, ok := envelope["result"]
	if !ok {
		t.Fatalf("tools/list response missing result: %v", envelope)
	}
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode tools result %s: %v", raw, err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func requireErrorEnvelope(t *testing.T, envelope map[string]json.RawMessage, wantCode int) {
	t.Helper()
	raw, ok := envelope["error"]
	if !ok {
		t.Fatalf("expected JSON-RPC error envelope, got %v", envelope)
	}
	var wire struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("decode jsonrpc error %s: %v", raw, err)
	}
	if wire.Code != wantCode {
		t.Fatalf("error code = %d (%s), want %d", wire.Code, wire.Message, wantCode)
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// ---- transport admission tests ----

func TestBearerMissingIsRejectedWithChallenge(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	resp := fx.post(t, "", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status without bearer = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", challenge)
	}
	if !strings.Contains(challenge, "resource_metadata=") {
		t.Fatalf("WWW-Authenticate = %q, want the protected-resource metadata pointer", challenge)
	}
	if !strings.Contains(challenge, "/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("WWW-Authenticate = %q, want the RFC 9728 well-known path", challenge)
	}
	fx.requireAuditReason(t, auditActionDenied, auditReasonMissingBearer)
}

func TestBearerInvalidIsRejectedWithChallenge(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	resp := fx.post(t, "rk13_totally_wrong_token", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status with unknown token = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", resp.Header.Get("WWW-Authenticate"))
	}
	fx.requireAuditReason(t, auditActionDenied, auditReasonInvalidToken)
}

func TestBearerRevokedIsRejected(t *testing.T) {
	fx := newGatewayFixture(t, func(spec *gatewaySpec) { spec.tokenRevoked = true })
	resp := fx.post(t, fx.token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status with revoked token = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	fx.requireAuditReason(t, auditActionDenied, auditReasonInvalidToken)
}

func TestBearerExpiredIsRejected(t *testing.T) {
	fx := newGatewayFixture(t, func(spec *gatewaySpec) { spec.tokenExpiry = -time.Minute })
	resp := fx.post(t, fx.token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status with expired token = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	fx.requireAuditReason(t, auditActionDenied, auditReasonInvalidToken)
}
func TestBearerWrongResourceIsRejected(t *testing.T) {
	fx := newGatewayFixture(t, func(spec *gatewaySpec) { spec.grantResource = otherTestResource })
	resp := fx.post(t, fx.token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status with wrong-resource token = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	fx.requireAuditReason(t, auditActionDenied, auditReasonWrongResource)
}

func TestExpiredTrialIsRejected(t *testing.T) {
	// A license-less user whose trial window has closed is denied /mcp;
	// an active license would legitimately keep the user authorized.
	fx := newGatewayFixture(t, func(spec *gatewaySpec) {
		spec.licenseStatus = ""
		spec.trialEndsAt = -time.Hour
	})
	resp := fx.post(t, fx.token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status with expired trial = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	fx.requireAuditReason(t, auditActionDenied, auditReasonEntitlement)
}

func TestOriginHeaderNotInAllowlistIsRejected(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	resp := fx.post(t, fx.token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "https://evil.example.com")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status with disallowed origin = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	fx.requireAuditReason(t, auditActionDenied, auditReasonOrigin)
}

func TestOriginHeaderInAllowlistPreflightSucceeds(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	req, err := http.NewRequest(http.MethodOptions, fx.mcpURL(), nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Origin", testAllowedOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	resp, err := fx.httpClient().Do(req)
	if err != nil {
		t.Fatalf("OPTIONS /mcp: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != testAllowedOrigin {
		t.Fatalf("Access-Control-Allow-Origin = %q, want %q", got, testAllowedOrigin)
	}
}

// ---- session and relayed tool tests ----

func TestMCPInitializeHandshakePreserved(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	fx.connectDevice(t, nil)
	session := fx.openSession(t)
	if session.sessionID == "" {
		t.Fatal("session was not established")
	}
	// The SDK session state must reject requests carrying an unknown session.
	resp := fx.post(t, fx.token, "session-that-does-not-exist",
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status with unknown session = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestMCPToolsListFilteredToGrantScopes(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	fx.connectDevice(t, nil)
	session := fx.openSession(t)

	envelope := session.call("tools/list", `{}`)
	if code := jsonrpcErrorCode(t, envelope); code != 0 {
		t.Fatalf("tools/list failed with code %d", code)
	}
	names := toolNames(t, envelope)
	want := []string{"get_instance_tree"} // read scope only
	if len(names) != len(want) || names[0] != want[0] {
		t.Fatalf("tools/list names = %v, want %v (edit, playtest, and unmapped tools must be filtered)", names, want)
	}
}

func TestMCPToolsListBroadScopesSeeMoreTools(t *testing.T) {
	fx := newGatewayFixture(t, func(spec *gatewaySpec) {
		spec.grantScopes = []string{
			mcpoauth.ScopeConnect, mcpoauth.ScopeStudioRead, mcpoauth.ScopeStudioEdit, mcpoauth.ScopeStudioPlay,
		}
	})
	fx.connectDevice(t, nil)
	session := fx.openSession(t)

	names := toolNames(t, session.call("tools/list", `{}`))
	for _, want := range []string{"get_instance_tree", "set_instance_properties", "run_playtest"} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("tools/list names = %v, missing %q", names, want)
		}
	}
	for _, forbidden := range []string{"get_unreleased_plugin_pack"} {
		for _, name := range names {
			if name == forbidden {
				t.Fatalf("tools/list names = %v, must not include unmapped tool %q", names, forbidden)
			}
		}
	}
}

func TestMCPToolsCallUnknownToolDeniedWithoutRelay(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	device := fx.connectDevice(t, nil)
	session := fx.openSession(t)

	envelope := session.call("tools/call", `{"name":"get_unreleased_plugin_pack","arguments":{}}`)
	requireErrorEnvelope(t, envelope, codeInvalidParams)

	// The unknown tool is denied locally: nothing may reach the device.
	time.Sleep(300 * time.Millisecond)
	if requests := device.requests(); len(requests) != 0 {
		t.Fatalf("unknown tool was relayed to the device: %v", requests)
	}
	fx.requireAuditReason(t, auditActionDenied, auditReasonUnknownTool)
}

func TestMCPToolsCallDeniedWithoutScope(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	device := fx.connectDevice(t, nil)
	session := fx.openSession(t)

	envelope := session.call("tools/call", `{"name":"set_instance_properties","arguments":{"instanceId":"x"}}`)
	requireErrorEnvelope(t, envelope, codeScopeDenied)
	if requests := device.requests(); len(requests) != 0 {
		t.Fatalf("denied tool was relayed to the device: %v", requests)
	}
	row := fx.requireAuditReason(t, auditActionDenied, auditReasonInsufficientScope)
	if !row.TargetID.Valid || row.TargetID.String != fx.grantID {
		t.Fatalf("insufficient-scope audit target = %+v, want grant %q", row.TargetID, fx.grantID)
	}
}

func TestMCPToolsCallAllowedRelayedWithCorrelation(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	device := fx.connectDevice(t, nil)
	session := fx.openSession(t)

	envelope := session.call("tools/call", `{"name":"get_instance_tree","arguments":{}}`)
	result, ok := envelope["result"]
	if !ok {
		t.Fatalf("allowed tools/call failed: %v", envelope)
	}
	var callResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &callResult); err != nil {
		t.Fatalf("decode call result %s: %v", result, err)
	}
	if len(callResult.Content) != 1 || callResult.Content[0].Text != "studio ok" {
		t.Fatalf("call result content = %+v, want the device echo", callResult.Content)
	}

	// The relayed envelope must carry a gateway correlation id and the
	// original JSON-RPC request, and the correlation must be retired.
	waitFor(t, 2*time.Second, "device request", func() bool { return len(device.requests()) == 1 })
	request := device.requests()[0]
	if request.GatewayRequestID == "" {
		t.Fatal("relayed envelope missing gateway_request_id")
	}
	var relayed struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(request.Payload, &relayed); err != nil {
		t.Fatalf("decode relayed payload %s: %v", request.Payload, err)
	}
	if relayed.Method != "tools/call" || relayed.Params.Name != "get_instance_tree" {
		t.Fatalf("relayed payload = %s", request.Payload)
	}
	waitFor(t, 2*time.Second, "pending correlation retirement", func() bool {
		fx.pending.mu.Lock()
		defer fx.pending.mu.Unlock()
		return len(fx.pending.entries) == 0
	})
}

func TestMCPToolsCallSendsCancelOnClientCancellation(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	device := fx.connectDevice(t, nil)
	session := fx.openSession(t)

	device.setHold(func(bridgeproto.Envelope) bool { return true })
	future := session.asyncCall("tools/call", `{"name":"get_instance_tree","arguments":{}}`)

	waitFor(t, 2*time.Second, "device request", func() bool { return len(device.requests()) == 1 })
	request := device.requests()[0]

	// The client cancels the in-flight request over a separate POST.
	session.notify("notifications/cancelled", fmt.Sprintf(`{"requestId":%d}`, session.nextID))

	waitFor(t, 2*time.Second, "cancel envelope on the device", func() bool {
		cancels := device.cancels()
		return len(cancels) == 1 && cancels[0].GatewayRequestID == request.GatewayRequestID
	})

	select {
	case envelope := <-future:
		requireErrorEnvelope(t, envelope, codeCancelled)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled call never completed")
	}
}

func TestMCPGrantBurstExhaustionReturns429(t *testing.T) {
	fx := newGatewayFixture(t, func(spec *gatewaySpec) {
		spec.burst = 2
		spec.window = time.Minute
	})
	fx.connectDevice(t, nil)
	session := fx.openSession(t) // initialize + initialized consume the burst

	resp := fx.post(t, fx.token, session.sessionID,
		`{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{}}`, "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("burst-exhausted status = %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 response missing Retry-After")
	}
	fx.requireAuditReason(t, auditActionDenied, auditReasonRateLimited)
}

func TestMCPConcurrentInFlightExhaustionReturns429(t *testing.T) {
	fx := newGatewayFixture(t, func(spec *gatewaySpec) {
		spec.maxInFlight = 1
		spec.burst = 100
	})
	device := fx.connectDevice(t, nil)
	session := fx.openSession(t)

	device.setHold(func(bridgeproto.Envelope) bool { return true })
	first := session.asyncCall("tools/call", `{"name":"get_instance_tree","arguments":{}}`)
	waitFor(t, 2*time.Second, "first in-flight call", func() bool { return len(device.requests()) == 1 })

	resp := fx.post(t, fx.token, session.sessionID,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"get_instance_tree","arguments":{}}}`, "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("in-flight-exhausted status = %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
	fx.requireAuditReason(t, auditActionDenied, auditReasonRateLimited)

	// Releasing the slot must make room again: answer the withheld request
	// so the first call completes and its slot frees.
	device.setHold(nil)
	device.respond(device.requests()[0])
	select {
	case envelope := <-first:
		if _, ok := envelope["result"]; !ok {
			t.Fatalf("first call failed: %v", envelope)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first call never completed")
	}
}

func TestMCPRateLimitEnforcedPerUserAcrossGrants(t *testing.T) {
	fx := newGatewayFixture(t, func(spec *gatewaySpec) {
		spec.burst = 2
		spec.window = time.Minute
	})
	fx.connectDevice(t, nil)
	_ = fx.openSession(t) // both the grant and user buckets now sit at the burst limit

	// A second grant for the same user still shares the user key.
	secondPlain, secondDigest := mustCredential(t, fx.oauthPepper)
	_, secondRefreshDigest := mustCredential(t, fx.oauthPepper)
	oauthStore := mysqlstore.NewOAuthStore(fx.db)
	secondGrantID := gwUUID(t)
	grant, err := oauthStore.SaveGrant(t.Context(), mcpoauth.Grant{
		ID:        secondGrantID,
		UserID:    fx.userID,
		ClientID:  fx.clientID,
		DeviceID:  fx.deviceID,
		Scopes:    []string{mcpoauth.ScopeConnect, mcpoauth.ScopeStudioRead},
		Resource:  testResource,
		CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("save second grant: %v", err)
	}
	now := time.Now().UTC()
	if err := oauthStore.IssueTokens(t.Context(), mcpoauth.AccessToken{
		ID: gwUUID(t), UserID: fx.userID, GrantID: grant.ID,
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}, secondDigest, mcpoauth.RefreshToken{
		ID: gwUUID(t), UserID: fx.userID, GrantID: grant.ID, FamilyID: gwUUID(t),
		ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now,
	}, secondRefreshDigest); err != nil {
		t.Fatalf("issue second grant tokens: %v", err)
	}

	resp := fx.post(t, secondPlain, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second grant status = %d, want %d (per-user exhaustion)", resp.StatusCode, http.StatusTooManyRequests)
	}
	row := fx.requireAuditReason(t, auditActionDenied, auditReasonRateLimited)
	if !row.UserID.Valid || row.UserID.String != fx.userID {
		t.Fatalf("rate-limit audit user = %+v, want %q", row.UserID, fx.userID)
	}
}

func TestMCPAuditDenialsAreSecretFree(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	fx.post(t, "", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "")
	fx.post(t, fx.token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "https://evil.example.com")
	time.Sleep(50 * time.Millisecond)

	rows, err := fx.db.QueryContext(context.Background(),
		`SELECT reason, metadata FROM audit_logs`)
	if err != nil {
		t.Fatalf("query audit rows: %v", err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var reason string
		var metadata sql.NullString
		if err := rows.Scan(&reason, &metadata); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		checked++
		for _, secret := range []string{fx.token, "Bearer", "rk13_", "brdg_"} {
			if strings.Contains(reason, secret) || strings.Contains(metadata.String, secret) {
				t.Fatalf("audit row leaks %q: reason=%q metadata=%q", secret, reason, metadata.String)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit rows: %v", err)
	}
	if checked == 0 {
		t.Fatal("expected audited denials")
	}
}
