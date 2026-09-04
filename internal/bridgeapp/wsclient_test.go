package bridgeapp

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-sql-driver/mysql"

	"robloxkit/internal/audit"
	"robloxkit/internal/bridgehub"
	"robloxkit/internal/credential"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/mcpprocess"
	"robloxkit/internal/mysqlstore"
	"robloxkit/internal/statusui"
	"robloxkit/pkg/bridgeproto"
)

// The WSS client tests run the real, committed bridgehub hub against a real
// migrated MySQL database over TLS — the same surface the Phase 2 smoke gate
// exercises — so every assertion observes genuine wire behavior. The database
// helper mirrors the committed bridgehub fixture helper; it lives here because
// another package's test files cannot be imported.

type remoteClock struct{}

func (remoteClock) Now() time.Time { return time.Now().UTC() }

func remoteUUID(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

func remoteTestDatabase(t *testing.T) *sql.DB {
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
	dbName := fmt.Sprintf("robloxkit_remote_test_%d", time.Now().UnixNano())
	if !remoteSafeIdentifier(dbName) {
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

func remoteSafeIdentifier(s string) bool {
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

// remoteFixture provisions one fully licensed, bound device and serves the
// committed hub over TLS. inbound records every post-hello envelope the hub
// accepted from the Bridge.
type remoteFixture struct {
	t            *testing.T
	db           *sql.DB
	pepper       []byte
	hub          *bridgehub.Hub
	registry     *bridgehub.Registry
	server       *httptest.Server
	limits       bridgeproto.Limits
	userID       string
	identityID   string
	deviceID     string
	subject      string
	credentialID string
	credential   string

	inbound inboundLog
}

func newRemoteFixture(t *testing.T) *remoteFixture {
	t.Helper()
	db := remoteTestDatabase(t)
	fx := &remoteFixture{
		t:            t,
		db:           db,
		pepper:       []byte("bridgeapp-remote-test-pepper"),
		deviceID:     remoteUUID(t),
		userID:       remoteUUID(t),
		identityID:   remoteUUID(t),
		subject:      fmt.Sprintf("bridgeapp_subject_%d", time.Now().UnixNano()),
		credentialID: remoteUUID(t),
	}
	licenseID := remoteUUID(t)
	bindingID := remoteUUID(t)
	trialID := remoteUUID(t)
	trialIdentityID := remoteUUID(t)

	plain, digest, err := credential.Generate("brdg_", 32, fx.pepper)
	if err != nil {
		t.Fatalf("generate device credential: %v", err)
	}
	fx.credential = plain

	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatalf("fixture insert failed: %v\nquery: %s", err, query)
		}
	}
	exec(`INSERT INTO users (id) VALUES (?)`, fx.userID)
	exec(`INSERT INTO user_identities (id, user_id, provider, provider_subject, display_name, status) VALUES (?, ?, ?, ?, ?, 'active')`,
		fx.identityID, fx.userID, "roblox", fx.subject, "Remote Fixture")
	exec(`INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, ?, 'active')`, fx.deviceID, fx.userID, "Test Device")
	exec(`INSERT INTO licenses (id, user_id, roblox_identity_id, status, device_slots) VALUES (?, ?, ?, 'active', 1)`,
		licenseID, fx.userID, fx.identityID)
	exec(`INSERT INTO license_device_bindings (id, user_id, license_id, device_id, slot_ordinal, status) VALUES (?, ?, ?, ?, 1, 'active')`,
		bindingID, fx.userID, licenseID, fx.deviceID)
	exec(`INSERT INTO trial_entitlements (id, user_id, started_at, ends_at) VALUES (?, ?, ?, ?)`,
		trialID, fx.userID, time.Now().Add(-time.Hour).UTC(), time.Now().Add(13*24*time.Hour).UTC())
	exec(`INSERT INTO trial_entitlement_identities (id, trial_entitlement_id, user_id, provider, provider_subject) VALUES (?, ?, ?, ?, ?)`,
		trialIdentityID, trialID, fx.userID, "roblox", fx.subject)
	exec(`INSERT INTO device_credentials (id, user_id, device_id, credential_digest, expires_at, revoked_at) VALUES (?, ?, ?, ?, NULL, NULL)`,
		fx.credentialID, fx.userID, fx.deviceID, digest[:])

	fx.limits = bridgeproto.Limits{MaxPayloadBytes: 256 * 1024}
	auditService := audit.NewService(mysqlstore.NewAuditStore(db))
	clock := remoteClock{}
	cfg := bridgehub.Config{
		Store:             bridgehub.NewSQLStore(db),
		Entitlements:      entitlement.NewService(mysqlstore.NewEntitlementStore(db, clock, auditService), clock),
		Pepper:            fx.pepper,
		HelloTimeout:      500 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
		HeartbeatTimeout:  900 * time.Millisecond,
		ReauthInterval:    150 * time.Millisecond,
		MaxEnvelopeBytes:  fx.limits.MaxPayloadBytes,
		QueueDepth:        8,
		WriteTimeout:      5 * time.Second,
		OnEnvelope: func(_ context.Context, _ bridgehub.Device, env bridgeproto.Envelope) {
			fx.inbound.add(env)
		},
	}
	hub, err := bridgehub.NewHub(cfg)
	if err != nil {
		t.Fatalf("new hub: %v", err)
	}
	fx.hub = hub
	fx.registry = hub.Registry()
	t.Cleanup(hub.Shutdown)
	fx.server = httptest.NewTLSServer(hub)
	t.Cleanup(fx.server.Close)
	return fx
}

func (fx *remoteFixture) bridgeURL() string {
	return "wss" + strings.TrimPrefix(fx.server.URL, "https") + "/bridge"
}

func (fx *remoteFixture) tlsClient() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

func (fx *remoteFixture) revokeCredential() {
	fx.t.Helper()
	if _, err := fx.db.ExecContext(context.Background(), `UPDATE device_credentials SET revoked_at = ? WHERE id = ?`,
		time.Now().UTC(), fx.credentialID); err != nil {
		fx.t.Fatalf("revoke credential: %v", err)
	}
}

func (fx *remoteFixture) sendRequest(gatewayID, payload string) {
	fx.t.Helper()
	env := bridgeproto.Envelope{
		Version:          bridgeproto.Version,
		Type:             bridgeproto.TypeRequest,
		GatewayRequestID: gatewayID,
		DeviceID:         fx.deviceID,
		Payload:          json.RawMessage(payload),
	}
	if err := fx.registry.Send(context.Background(), fx.deviceID, env); err != nil {
		fx.t.Fatalf("send request envelope: %v", err)
	}
}

func (fx *remoteFixture) sendCancel(gatewayID string) {
	fx.t.Helper()
	env := bridgeproto.Envelope{
		Version:          bridgeproto.Version,
		Type:             bridgeproto.TypeCancel,
		GatewayRequestID: gatewayID,
		DeviceID:         fx.deviceID,
	}
	if err := fx.registry.Send(context.Background(), fx.deviceID, env); err != nil {
		fx.t.Fatalf("send cancel envelope: %v", err)
	}
}

func (fx *remoteFixture) remoteDeps(newProc func() mcpprocess.Process) RemoteDeps {
	return RemoteDeps{
		Machine:     statusui.NewMachine(),
		NewProcess:  newProc,
		Credential:  &fakeCredentialStore{credential: fx.credential},
		GatewayURL:  fx.bridgeURL(),
		DeviceID:    fx.deviceID,
		DeviceName:  "Test Bridge",
		HTTPClient:  fx.tlsClient(),
		StudioReady: func(context.Context) (int, error) { return 1, nil },
		Output:      io.Discard,
		Backoff: Backoff{
			Base:   10 * time.Millisecond,
			Max:    60 * time.Millisecond,
			Jitter: 5 * time.Millisecond,
		},
		Random:          newPatternReader(),
		ConnectTimeout:  2 * time.Second,
		ResponseTimeout: 2 * time.Second,
		WriteTimeout:    2 * time.Second,
		QueueLimit:      16,
		MaxMessageBytes: 256 * 1024,
		BridgeVersion:   "test-bridge",
	}
}

// inboundLog records accepted hub-side envelopes without consuming them.
type inboundLog struct {
	mu   sync.Mutex
	envs []bridgeproto.Envelope
}

func (l *inboundLog) add(env bridgeproto.Envelope) {
	l.mu.Lock()
	l.envs = append(l.envs, env)
	l.mu.Unlock()
}

func (l *inboundLog) snapshot() []bridgeproto.Envelope {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]bridgeproto.Envelope(nil), l.envs...)
}

func (l *inboundLog) await(t *testing.T, timeout time.Duration, what string, pred func(bridgeproto.Envelope) bool) bridgeproto.Envelope {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, env := range l.snapshot() {
			if pred(env) {
				return env
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
	return bridgeproto.Envelope{}
}

func (l *inboundLog) count(pred func(bridgeproto.Envelope) bool) int {
	total := 0
	for _, env := range l.snapshot() {
		if pred(env) {
			total++
		}
	}
	return total
}

// eventLog records terminal status events emitted by RunRemote.
type eventLog struct {
	mu     sync.Mutex
	events []statusui.Event
}

func (l *eventLog) sink(e statusui.Event) error {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
	return nil
}

func (l *eventLog) snapshot() []statusui.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]statusui.Event(nil), l.events...)
}

func (l *eventLog) count(state statusui.State) int {
	total := 0
	for _, event := range l.snapshot() {
		if event.State == state {
			total++
		}
	}
	return total
}

func (l *eventLog) nth(t *testing.T, state statusui.State, n int, timeout time.Duration) statusui.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		seen := 0
		for _, event := range l.snapshot() {
			if event.State == state {
				seen++
				if seen == n {
					return event
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %d occurrence(s) of state %q", timeout, n, state)
	return statusui.Event{}
}

func (l *eventLog) requireNone(t *testing.T, state statusui.State) {
	t.Helper()
	for _, event := range l.snapshot() {
		if event.State == state {
			t.Fatalf("unexpected %q event in %v", state, l.states())
		}
	}
}

func (l *eventLog) states() []statusui.State {
	var states []statusui.State
	for _, event := range l.snapshot() {
		states = append(states, event.State)
	}
	return states
}

func startRemote(t *testing.T, deps RemoteDeps) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan error, 1)
	go func() { result <- RunRemote(ctx, deps) }()
	return result, cancel
}

func awaitResult(t *testing.T, result <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for RunRemote to return", timeout)
		return nil
	}
}

// stopRemote ends a RunRemote loop and asserts it exits cleanly. Every test
// that starts a loop must finish with this invariant, so a wedged reconnect
// or leaked cycle fails the suite instead of lingering.
func stopRemote(t *testing.T, result <-chan error, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	if err := awaitResult(t, result, 3*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunRemote returned %v, want context.Canceled", err)
	}
}

type patternReader struct {
	pattern []byte
	offset  int
}

func newPatternReader() *patternReader {
	return &patternReader{pattern: []byte{0x27, 0x5b, 0x91, 0xc3, 0x6a, 0xf0, 0x11, 0x04}}
}

func (r *patternReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.pattern[r.offset%len(r.pattern)]
		r.offset++
	}
	return len(p), nil
}

// awaitCount waits until at least n recorded envelopes match pred. The plain
// await matches the first occurrence in history, so "the second X" waits must
// use this instead.
func (l *inboundLog) awaitCount(t *testing.T, timeout time.Duration, n int, what string, pred func(bridgeproto.Envelope) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if l.count(pred) >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %d %s", timeout, n, what)
}

type fakeCredentialStore struct {
	credential string
	err        error
}

func (s *fakeCredentialStore) Load() ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []byte(s.credential), nil
}

func (s *fakeCredentialStore) Save([]byte) error { return nil }

func (s *fakeCredentialStore) Delete() error { return nil }

// remoteProcRecorder aggregates activity across every MCP child instance the
// Bridge starts (a fresh child is created per connection cycle).
type remoteProcRecorder struct {
	mu      sync.Mutex
	starts  int
	stops   int
	all     []json.RawMessage
	cancels []json.RawMessage
}

type sentCall struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name      string `json:"name"`
		Arguments struct {
			Text string `json:"text"`
		} `json:"arguments"`
	} `json:"params"`
}

func (rec *remoteProcRecorder) framesFor(marker string) []json.RawMessage {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []json.RawMessage
	for _, frame := range rec.all {
		var call sentCall
		if json.Unmarshal(frame, &call) != nil {
			continue
		}
		if call.Method == "tools/call" && call.Params.Name == "echo" && call.Params.Arguments.Text == marker {
			out = append(out, append(json.RawMessage(nil), frame...))
		}
	}
	return out
}

func (rec *remoteProcRecorder) cancelFrames() []json.RawMessage {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]json.RawMessage(nil), rec.cancels...)
}

func (rec *remoteProcRecorder) counts() (starts int, stops int) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.starts, rec.stops
}

func (rec *remoteProcRecorder) awaitFramesFor(t *testing.T, marker string, n int, timeout time.Duration) []json.RawMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if frames := rec.framesFor(marker); len(frames) >= n {
			return frames
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %d %q tool call(s) to reach the MCP child", timeout, n, marker)
	return nil
}

// processTracker lets tests reach the child instances created by the RunRemote
// factory, which runs on the RunRemote goroutine.
type processTracker struct {
	mu        sync.Mutex
	processes []*remoteFakeProcess
}

func (tr *processTracker) add(p *remoteFakeProcess) {
	tr.mu.Lock()
	tr.processes = append(tr.processes, p)
	tr.mu.Unlock()
}

func (tr *processTracker) releaseAll() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, p := range tr.processes {
		p.release()
	}
}

// remoteFakeProcess is a restartable MCP child. tools/call frames whose
// arguments.text equals holdMarker never receive a response until release is
// called, which lets tests place a tool call in flight and then kill the
// connection before its response exists.
type remoteFakeProcess struct {
	rec         *remoteProcRecorder
	responses   chan json.RawMessage
	diag        chan mcpprocess.SafeProcessError
	holdMarker  string
	releaseCh   chan struct{}
	stopCh      chan struct{}
	stopOnce    sync.Once
	releaseOnce sync.Once
}

func newRemoteFakeProcess(rec *remoteProcRecorder, holdMarker string) *remoteFakeProcess {
	return &remoteFakeProcess{
		rec:        rec,
		responses:  make(chan json.RawMessage, 32),
		diag:       make(chan mcpprocess.SafeProcessError, 4),
		holdMarker: holdMarker,
		releaseCh:  make(chan struct{}),
		stopCh:     make(chan struct{}),
	}
}

func (p *remoteFakeProcess) release() {
	p.releaseOnce.Do(func() { close(p.releaseCh) })
}

func (p *remoteFakeProcess) Start(context.Context) error {
	p.rec.mu.Lock()
	p.rec.starts++
	p.rec.mu.Unlock()
	return nil
}

func (p *remoteFakeProcess) Send(_ context.Context, frame json.RawMessage) error {
	p.rec.mu.Lock()
	p.rec.all = append(p.rec.all, append(json.RawMessage(nil), frame...))
	p.rec.mu.Unlock()

	var call sentCall
	if json.Unmarshal(frame, &call) != nil {
		return errors.New("invalid JSON-RPC frame")
	}
	respond := func(result string) {
		select {
		case p.responses <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, call.ID, result)):
		default:
		}
	}
	switch call.Method {
	case "initialize":
		respond(`{"protocolVersion":"2025-06-18"}`)
	case "tools/list":
		respond(`{"tools":[{"name":"echo","annotations":{"readOnlyHint":true},"inputSchema":{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}}}]}`)
	case "tools/call":
		if p.holdMarker != "" && call.Params.Arguments.Text == p.holdMarker {
			go func() {
				// Withheld until release. A release that arrives after the
				// Bridge already moved on must still produce the frame: the
				// dead relay then drops it, proving no replay.
				<-p.releaseCh
				select {
				case p.responses <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`, call.ID)):
				default:
				}
			}()
			return nil
		}
		respond(`{"content":[{"type":"text","text":"ok"}]}`)
	case "notifications/cancelled":
		p.rec.mu.Lock()
		p.rec.cancels = append(p.rec.cancels, append(json.RawMessage(nil), frame...))
		p.rec.mu.Unlock()
	}
	return nil
}

func (p *remoteFakeProcess) Responses() <-chan json.RawMessage               { return p.responses }
func (p *remoteFakeProcess) Diagnostics() <-chan mcpprocess.SafeProcessError { return p.diag }

func (p *remoteFakeProcess) Stop(context.Context) error {
	p.stopOnce.Do(func() { close(p.stopCh) })
	p.rec.mu.Lock()
	p.rec.stops++
	p.rec.mu.Unlock()
	return nil
}

func (p *remoteFakeProcess) Wait() error {
	<-p.stopCh
	return nil
}

func (p *remoteFakeProcess) CommitReadiness(commit func() error) error {
	return commit()
}

// assertFullStatusSnapshot checks that a status envelope carries the complete
// device snapshot the gateway needs after every (re)authentication.
func assertFullStatusSnapshot(t *testing.T, env bridgeproto.Envelope) {
	t.Helper()
	if env.Type != bridgeproto.TypeStatus {
		t.Fatalf("snapshot envelope type = %q, want status", env.Type)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("snapshot payload is not an object: %v\npayload: %s", err, env.Payload)
	}
	required := []string{"state", "device_name", "studio_count", "mcp_ready", "gateway_connected", "bridge_version", "capabilities"}
	for _, key := range required {
		if _, ok := payload[key]; !ok {
			t.Fatalf("status snapshot missing %q; payload: %s", key, env.Payload)
		}
	}
	var state, deviceName, version string
	var studioCount int
	var mcpReady, gatewayConnected bool
	_ = json.Unmarshal(payload["state"], &state)
	_ = json.Unmarshal(payload["device_name"], &deviceName)
	_ = json.Unmarshal(payload["studio_count"], &studioCount)
	_ = json.Unmarshal(payload["mcp_ready"], &mcpReady)
	_ = json.Unmarshal(payload["gateway_connected"], &gatewayConnected)
	_ = json.Unmarshal(payload["bridge_version"], &version)
	if state != "connected" || deviceName != "Test Bridge" || studioCount != 1 || !mcpReady || !gatewayConnected || version != "test-bridge" {
		t.Fatalf("status snapshot values wrong: %s", env.Payload)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(payload["capabilities"])), "[") {
		t.Fatalf("capabilities must be a JSON array: %s", env.Payload)
	}
}

func TestRemoteRelaysRequestsAndSendsFullStatusSnapshot(t *testing.T) {
	fx := newRemoteFixture(t)
	rec := &remoteProcRecorder{}
	events := &eventLog{}
	deps := fx.remoteDeps(func() mcpprocess.Process { return newRemoteFakeProcess(rec, "") })
	deps.EventSink = events.sink
	result, cancel := startRemote(t, deps)
	defer stopRemote(t, result, cancel)

	connected := events.nth(t, statusui.Connected, 1, 5*time.Second)
	if connected.DeviceName != "Test Bridge" || connected.StudioCount != 1 {
		t.Fatalf("connected event = %+v", connected)
	}
	snapshot := fx.inbound.await(t, 5*time.Second, "full status snapshot", func(env bridgeproto.Envelope) bool {
		return env.Type == bridgeproto.TypeStatus
	})
	assertFullStatusSnapshot(t, snapshot)

	// A gateway tool call must reach the MCP child under a bridge-local id,
	// and the response payload must restore the gateway's original id.
	gatewayID := "gw_roundtrip"
	fx.sendRequest(gatewayID, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{"text":"roundtrip"}}}`)
	frames := rec.awaitFramesFor(t, "roundtrip", 1, 5*time.Second)
	var childCall sentCall
	if err := json.Unmarshal(frames[0], &childCall); err != nil {
		t.Fatalf("decode child frame: %v", err)
	}
	if string(childCall.ID) != "1" {
		t.Fatalf("child frame id = %s, want bridge-local id 1", childCall.ID)
	}
	response := fx.inbound.await(t, 5*time.Second, "relayed response", func(env bridgeproto.Envelope) bool {
		return env.Type == bridgeproto.TypeResponse && env.GatewayRequestID == gatewayID
	})
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode response payload: %v\npayload: %s", err, response.Payload)
	}
	if string(payload["id"]) != "7" {
		t.Fatalf("response payload id = %s, want the gateway's original id 7; payload: %s", payload["id"], response.Payload)
	}
	if _, ok := payload["result"]; !ok {
		t.Fatalf("response payload lost its result: %s", response.Payload)
	}
}

func TestRemoteConcurrentSameOriginalIDRoutesCorrectly(t *testing.T) {
	fx := newRemoteFixture(t)
	rec := &remoteProcRecorder{}
	events := &eventLog{}
	tracker := &processTracker{}
	deps := fx.remoteDeps(func() mcpprocess.Process {
		p := newRemoteFakeProcess(rec, "collision")
		tracker.add(p)
		return p
	})
	deps.EventSink = events.sink
	result, cancel := startRemote(t, deps)
	defer stopRemote(t, result, cancel)

	events.nth(t, statusui.Connected, 1, 5*time.Second)

	// Two concurrent gateway requests share the original JSON-RPC id 7; both
	// must be forwarded with distinct bridge-local ids and routed back to
	// their own gateway correlations.
	first := "gw_collision_first"
	second := "gw_collision_second"
	fx.sendRequest(first, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{"text":"collision"}}}`)
	fx.sendRequest(second, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"echo","arguments":{"text":"collision"}}}`)
	frames := rec.awaitFramesFor(t, "collision", 2, 5*time.Second)
	ids := map[string]bool{}
	for _, frame := range frames {
		var call sentCall
		if err := json.Unmarshal(frame, &call); err != nil {
			t.Fatalf("decode child frame: %v", err)
		}
		ids[string(call.ID)] = true
	}
	if len(ids) != 2 {
		t.Fatalf("child received local ids %v for concurrent same-original-id calls, want two distinct ids", ids)
	}

	// Release both withheld responses; each must reach its own correlation.
	tracker.releaseAll()

	for _, gatewayID := range []string{first, second} {
		response := fx.inbound.await(t, 5*time.Second, "response for "+gatewayID, func(env bridgeproto.Envelope) bool {
			return env.Type == bridgeproto.TypeResponse && env.GatewayRequestID == gatewayID
		})
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(response.Payload, &payload); err != nil {
			t.Fatalf("decode response payload: %v", err)
		}
		if string(payload["id"]) != "7" {
			t.Fatalf("response for %s restored id %s, want 7; payload: %s", gatewayID, payload["id"], response.Payload)
		}
	}
}

func TestRemoteCancelForwardsLocalRequestIDAndDropsCorrelation(t *testing.T) {
	fx := newRemoteFixture(t)
	rec := &remoteProcRecorder{}
	events := &eventLog{}
	tracker := &processTracker{}
	deps := fx.remoteDeps(func() mcpprocess.Process {
		p := newRemoteFakeProcess(rec, "cancel-probe")
		tracker.add(p)
		return p
	})
	deps.EventSink = events.sink
	result, cancel := startRemote(t, deps)
	defer stopRemote(t, result, cancel)

	events.nth(t, statusui.Connected, 1, 5*time.Second)

	gatewayID := "gw_cancel"
	fx.sendRequest(gatewayID, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"cancel-probe"}}}`)
	frames := rec.awaitFramesFor(t, "cancel-probe", 1, 5*time.Second)
	var childCall sentCall
	if err := json.Unmarshal(frames[0], &childCall); err != nil {
		t.Fatalf("decode child frame: %v", err)
	}

	fx.sendCancel(gatewayID)
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.cancelFrames()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancels := rec.cancelFrames()
	if len(cancels) != 1 {
		t.Fatalf("cancel notifications forwarded to child = %d, want 1", len(cancels))
	}
	var cancelFrame struct {
		Method string `json:"method"`
		Params struct {
			RequestID json.RawMessage `json:"requestId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(cancels[0], &cancelFrame); err != nil {
		t.Fatalf("decode cancel frame: %v", err)
	}
	if cancelFrame.Method != "notifications/cancelled" {
		t.Fatalf("cancel method = %s", cancelFrame.Method)
	}
	if string(cancelFrame.Params.RequestID) != string(childCall.ID) {
		t.Fatalf("cancel requestId = %s, want the bridge-local id %s", cancelFrame.Params.RequestID, childCall.ID)
	}

	// The cancelled correlation is gone: the child's late response must never
	// be forwarded to the gateway.
	tracker.releaseAll()

	time.Sleep(250 * time.Millisecond)
	if fx.inbound.count(func(env bridgeproto.Envelope) bool {
		return env.Type == bridgeproto.TypeResponse && env.GatewayRequestID == gatewayID
	}) != 0 {
		t.Fatal("response for a cancelled correlation reached the gateway")
	}
}

func TestNoReplayAfterDisconnectMidToolCall(t *testing.T) {
	fx := newRemoteFixture(t)
	rec := &remoteProcRecorder{}
	events := &eventLog{}
	tracker := &processTracker{}
	instance := 0
	deps := fx.remoteDeps(func() mcpprocess.Process {
		instance++
		if instance == 1 {
			p := newRemoteFakeProcess(rec, "replay-probe")
			tracker.add(p)
			return p
		}
		return newRemoteFakeProcess(rec, "")
	})
	deps.EventSink = events.sink
	result, cancel := startRemote(t, deps)
	defer stopRemote(t, result, cancel)

	events.nth(t, statusui.Connected, 1, 5*time.Second)

	// A tool call reaches the Bridge and the MCP child, but its response is
	// withheld while the gateway link dies.
	gatewayID := "gw_replay"
	fx.sendRequest(gatewayID, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{"text":"replay-probe"}}}`)
	rec.awaitFramesFor(t, "replay-probe", 1, 5*time.Second)

	fx.registry.Disconnect(fx.deviceID, "connection reset")

	// The Bridge reconnects and re-authenticates.
	events.nth(t, statusui.Connected, 2, 5*time.Second)
	fx.inbound.awaitCount(t, 5*time.Second, 2, "status snapshots (reconnect)",
		func(env bridgeproto.Envelope) bool { return env.Type == bridgeproto.TypeStatus })

	// The withheld response for the dead correlation surfaces now. It must be
	// discarded: no replay of the call, no late response to the gateway.
	tracker.releaseAll()

	time.Sleep(250 * time.Millisecond)

	if got := len(rec.framesFor("replay-probe")); got != 1 {
		t.Fatalf("tool call reached the MCP child %d times, want exactly 1 (no replay)", got)
	}
	if fx.inbound.count(func(env bridgeproto.Envelope) bool {
		return env.GatewayRequestID == gatewayID
	}) != 0 {
		t.Fatal("a response for the dropped correlation reached the gateway")
	}

	// The relay stays healthy after the reconnect: a fresh call round-trips.
	nextID := "gw_after_reconnect"
	fx.sendRequest(nextID, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{"text":"after-reconnect"}}}`)
	response := fx.inbound.await(t, 5*time.Second, "post-reconnect response", func(env bridgeproto.Envelope) bool {
		return env.Type == bridgeproto.TypeResponse && env.GatewayRequestID == nextID
	})
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(response.Payload, &payload); err != nil {
		t.Fatalf("decode response payload: %v", err)
	}
	if string(payload["id"]) != "5" {
		t.Fatalf("post-reconnect response restored id %s, want 5; payload: %s", payload["id"], response.Payload)
	}
}

func TestRemoteReconnectAcrossServerRestart(t *testing.T) {
	fx := newRemoteFixture(t)
	rec := &remoteProcRecorder{}
	events := &eventLog{}
	deps := fx.remoteDeps(func() mcpprocess.Process { return newRemoteFakeProcess(rec, "") })
	deps.EventSink = events.sink
	runtime.GC()
	baseline := runtime.NumGoroutine()
	result, cancel := startRemote(t, deps)

	events.nth(t, statusui.Connected, 1, 5*time.Second)
	fx.inbound.await(t, 5*time.Second, "first status snapshot", func(env bridgeproto.Envelope) bool {
		return env.Type == bridgeproto.TypeStatus
	})

	// A server restart tears every live connection down with a clean closure.
	fx.hub.Shutdown()

	reconnecting := events.nth(t, statusui.Reconnecting, 1, 5*time.Second)
	if reconnecting.RetryAfter <= 0 {
		t.Fatalf("reconnecting event has no RetryAfter: %+v", reconnecting)
	}
	second := events.nth(t, statusui.Connected, 2, 5*time.Second)
	if second.DeviceName != "Test Bridge" {
		t.Fatalf("second connected event = %+v", second)
	}
	fx.inbound.awaitCount(t, 5*time.Second, 2, "status snapshots (reconnect)",
		func(env bridgeproto.Envelope) bool { return env.Type == bridgeproto.TypeStatus })

	statusCount := 0
	for _, env := range fx.inbound.snapshot() {
		if env.Type == bridgeproto.TypeStatus {
			statusCount++
			assertFullStatusSnapshot(t, env)
		}
	}
	if statusCount != 2 {
		t.Fatalf("status snapshots received by hub = %d, want 2 (one per authentication)", statusCount)
	}

	// The terminal transition order must read connected → reconnecting →
	// connected across the restart.
	states := events.states()
	firstConnected, reconnectingIdx, secondConnected := -1, -1, -1
	for i, state := range states {
		if state == statusui.Connected && firstConnected == -1 {
			firstConnected = i
		}
		if state == statusui.Reconnecting && firstConnected != -1 && reconnectingIdx == -1 {
			reconnectingIdx = i
		}
		if state == statusui.Connected && reconnectingIdx != -1 && secondConnected == -1 {
			secondConnected = i
		}
	}
	if firstConnected == -1 || reconnectingIdx == -1 || secondConnected == -1 {
		t.Fatalf("states lack connected → reconnecting → connected: %v", states)
	}

	cancel()
	if err := awaitResult(t, result, 2*time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunRemote returned %v, want context.Canceled", err)
	}
	eventually(t, 3*time.Second, "goroutine count to stabilize", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baseline+4
	})
}

func TestRemoteTerminalAuthFailureOnRevokedCredential(t *testing.T) {
	fx := newRemoteFixture(t)
	rec := &remoteProcRecorder{}
	events := &eventLog{}
	deps := fx.remoteDeps(func() mcpprocess.Process { return newRemoteFakeProcess(rec, "") })
	deps.EventSink = events.sink
	result, _ := startRemote(t, deps)

	events.nth(t, statusui.Connected, 1, 5*time.Second)

	fx.revokeCredential()

	err := awaitResult(t, result, 5*time.Second)
	if err == nil {
		t.Fatal("RunRemote returned nil after credential revocation")
	}
	if !errors.Is(err, errTerminalAuth) {
		t.Fatalf("RunRemote returned %v, want a terminal auth failure", err)
	}
	last := events.snapshot()
	if len(last) == 0 || last[len(last)-1].State != statusui.Fatal {
		t.Fatalf("last event = %+v, want fatal", last)
	}
	if last[len(last)-1].Code != "DEVICE_CREDENTIAL_REVOKED" {
		t.Fatalf("fatal code = %q, want DEVICE_CREDENTIAL_REVOKED", last[len(last)-1].Code)
	}
	events.requireNone(t, statusui.Reconnecting)

	// Reconnection must have been abandoned permanently: no new dials and no
	// further status snapshots.
	time.Sleep(400 * time.Millisecond)
	if fx.registry.Len() != 0 {
		t.Fatalf("registry still holds %d connection(s) after terminal auth failure", fx.registry.Len())
	}
	snapshots := fx.inbound.count(func(env bridgeproto.Envelope) bool {
		return env.Type == bridgeproto.TypeStatus
	})
	if snapshots != 1 {
		t.Fatalf("status snapshots = %d, want exactly 1 (no re-authentication)", snapshots)
	}
}

func TestRemoteDialRejectedWithWrongCredentialIsTerminal(t *testing.T) {
	fx := newRemoteFixture(t)
	rec := &remoteProcRecorder{}
	events := &eventLog{}
	deps := fx.remoteDeps(func() mcpprocess.Process { return newRemoteFakeProcess(rec, "") })
	deps.Credential = &fakeCredentialStore{credential: "brdg_not_the_real_credential"}
	deps.EventSink = events.sink
	result, cancel := startRemote(t, deps)
	defer cancel()

	err := awaitResult(t, result, 5*time.Second)
	if err == nil {
		t.Fatal("RunRemote returned nil for a rejected credential")
	}
	if !errors.Is(err, errTerminalAuth) {
		t.Fatalf("RunRemote returned %v, want a terminal auth failure", err)
	}
	events.requireNone(t, statusui.Connected)
	events.requireNone(t, statusui.Reconnecting)
	snapshot := events.snapshot()
	if len(snapshot) == 0 || snapshot[len(snapshot)-1].State != statusui.Fatal {
		t.Fatalf("last event = %+v, want fatal", snapshot)
	}
	if snapshot[len(snapshot)-1].Code != "DEVICE_CREDENTIAL_REJECTED" {
		t.Fatalf("fatal code = %q, want DEVICE_CREDENTIAL_REJECTED", snapshot[len(snapshot)-1].Code)
	}
	time.Sleep(200 * time.Millisecond)
	if fx.registry.Len() != 0 {
		t.Fatal("a connection exists despite a rejected credential")
	}
	if fx.inbound.count(func(env bridgeproto.Envelope) bool { return true }) != 0 {
		t.Fatal("the hub accepted envelopes despite a rejected credential")
	}
}

func TestRemoteCleanShutdownCancelsImmediately(t *testing.T) {
	fx := newRemoteFixture(t)
	rec := &remoteProcRecorder{}
	events := &eventLog{}
	deps := fx.remoteDeps(func() mcpprocess.Process { return newRemoteFakeProcess(rec, "") })
	deps.EventSink = events.sink
	result, cancel := startRemote(t, deps)

	events.nth(t, statusui.Connected, 1, 5*time.Second)

	begin := time.Now()
	cancel()
	err := awaitResult(t, result, 2*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunRemote returned %v, want context.Canceled", err)
	}
	if elapsed := time.Since(begin); elapsed > 1500*time.Millisecond {
		t.Fatalf("clean shutdown took %s, want it to cancel immediately", elapsed)
	}

	_, stops := rec.counts()
	if stops == 0 {
		t.Fatal("MCP child was not stopped on clean shutdown")
	}
	events.requireNone(t, statusui.Fatal)
	events.requireNone(t, statusui.Reconnecting)

	eventually(t, 2*time.Second, "hub registry to drain", func() bool {
		return fx.registry.Len() == 0
	})
}

func TestRemoteEnrollmentRequiredWithoutCredential(t *testing.T) {
	rec := &remoteProcRecorder{}
	events := &eventLog{}
	deps := RemoteDeps{
		Machine:     statusui.NewMachine(),
		NewProcess:  func() mcpprocess.Process { return newRemoteFakeProcess(rec, "") },
		Credential:  &fakeCredentialStore{err: os.ErrNotExist},
		GatewayURL:  "wss://invalid.example.test/bridge",
		DeviceID:    "dev_enroll",
		DeviceName:  "Test Bridge",
		StudioReady: func(context.Context) (int, error) { return 1, nil },
		Output:      io.Discard,
		EventSink:   events.sink,
	}

	done := make(chan error, 1)
	go func() { done <- RunRemote(context.Background(), deps) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunRemote returned nil without an enrolled credential")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunRemote blocked without an enrolled credential")
	}

	states := events.states()
	want := []statusui.State{statusui.Initializing, statusui.EnrollmentRequired}
	if len(states) != len(want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("states = %v, want %v", states, want)
		}
	}
}

func TestRemoteBackoffRetriesWhileGatewayDown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release port: %v", err)
	}

	rec := &remoteProcRecorder{}
	events := &eventLog{}
	deps := RemoteDeps{
		Machine:     statusui.NewMachine(),
		NewProcess:  func() mcpprocess.Process { return newRemoteFakeProcess(rec, "") },
		Credential:  &fakeCredentialStore{credential: "brdg_transient"},
		GatewayURL:  "wss://" + address + "/bridge",
		DeviceID:    "dev_transient",
		DeviceName:  "Test Bridge",
		StudioReady: func(context.Context) (int, error) { return 1, nil },
		Output:      io.Discard,
		Backoff:     Backoff{Base: 5 * time.Millisecond, Max: 25 * time.Millisecond, Jitter: 0},
		EventSink:   events.sink,
	}
	result, cancel := startRemote(t, deps)

	// Transient failures must keep retrying with capped exponential backoff.
	var retries []statusui.Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		retries = nil
		for _, event := range events.snapshot() {
			if event.State == statusui.Reconnecting {
				retries = append(retries, event)
			}
		}
		if len(retries) >= 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(retries) < 4 {
		t.Fatalf("reconnect attempts = %d, want at least 4 while the gateway is down", len(retries))
	}
	want := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 25 * time.Millisecond}
	for i, expected := range want {
		if retries[i].RetryAfter != expected {
			t.Fatalf("reconnect %d RetryAfter = %s, want %s", i+1, retries[i].RetryAfter, expected)
		}
	}
	events.requireNone(t, statusui.Fatal)

	// Cancelling mid-backoff must end the loop immediately.
	begin := time.Now()
	cancel()
	err = awaitResult(t, result, 500*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunRemote returned %v, want context.Canceled", err)
	}
	if elapsed := time.Since(begin); elapsed > 400*time.Millisecond {
		t.Fatalf("cancellation took %s, want immediate", elapsed)
	}
}

func TestRemoteBearerHeaderDialing(t *testing.T) {
	const secret = "tok_bridge_secret"
	var mu sync.Mutex
	var recorded string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		recorded = r.Header.Get("Authorization")
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.Header().Set("WWW-Authenticate", `Bearer realm="bridge"`)
			http.Error(w, "invalid device credential", http.StatusUnauthorized)
			return
		}
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		readCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		ws.CloseRead(readCtx)
	}))
	t.Cleanup(server.Close)

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	base := dialConfig{
		URL:            "wss" + strings.TrimPrefix(server.URL, "https") + "/bridge",
		Credential:     secret,
		DeviceID:       "dev_header",
		HTTPClient:     client,
		ConnectTimeout: 2 * time.Second,
		WriteTimeout:   2 * time.Second,
		QueueDepth:     4,
		Limits:         bridgeproto.Limits{MaxPayloadBytes: 256 * 1024},
	}

	session, err := dialBridge(context.Background(), base)
	if err != nil {
		t.Fatalf("dialBridge with the valid credential failed: %v", err)
	}
	mu.Lock()
	got := recorded
	mu.Unlock()
	if got != "Bearer "+secret {
		t.Fatalf("Authorization header = %q, want %q", got, "Bearer "+secret)
	}
	if err := session.enqueue(bridgeproto.Envelope{
		Version:  bridgeproto.Version,
		Type:     bridgeproto.TypeHello,
		DeviceID: "dev_header",
		Payload:  json.RawMessage(`{"bridge_version":"test"}`),
	}); err != nil {
		t.Fatalf("enqueue hello: %v", err)
	}
	session.close(websocket.StatusNormalClosure, "done")

	// A wrong credential must be a terminal auth failure, never a retryable
	// dial error.
	bad := base
	bad.Credential = "tok_wrong"
	if _, err := dialBridge(context.Background(), bad); err == nil || !errors.Is(err, errTerminalAuth) {
		t.Fatalf("dialBridge with a wrong credential returned %v, want terminal auth failure", err)
	}
}

func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
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
