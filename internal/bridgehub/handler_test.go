package bridgehub

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-sql-driver/mysql"

	"robloxkit/internal/audit"
	"robloxkit/internal/credential"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/mysqlstore"
	"robloxkit/pkg/bridgeproto"
)

// systemClock feeds the entitlement service with real time. Note: -race is
// unavailable in this environment (CGO_ENABLED=0); concurrency safety is
// exercised via logical stress tests instead.

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func hubUUID(t *testing.T) string {
	t.Helper()
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		t.Fatalf("generate uuid: %v", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

// hubTestDatabase creates a uniquely named, fully migrated temporary MySQL
// database per test, mirroring the mysqlstore test helper. The hub test files
// cannot import that helper because it lives in another package's test files.
func hubTestDatabase(t *testing.T) *sql.DB {
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
		_ = admin.Close()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping MYSQL_TEST_DSN: %v", err)
	}
	dbName := fmt.Sprintf("robloxkit_hub_test_%d", time.Now().UnixNano())
	if !hubSafeIdentifier(dbName) {
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
	t.Cleanup(func() {
		_ = db.Close()
	})
	if _, err := mysqlstore.Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

// hubSafeIdentifier mirrors the mysqlstore test guard for generated names.
func hubSafeIdentifier(s string) bool {
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

// fixtureSpec describes one test's database rows.
type fixtureSpec struct {
	credentialExpiresAt time.Time
	credentialRevokedAt time.Time
	trialStartedAt      time.Time
	trialEndsAt         time.Time
	licenseStatus       string
	withBinding         bool
	withSecondDevice    bool
	deviceStatus        string
	queueDepth          int
}

func defaultSpec() fixtureSpec {
	return fixtureSpec{
		trialStartedAt: time.Now().Add(-time.Hour),
		trialEndsAt:    time.Now().Add(13 * 24 * time.Hour),
		licenseStatus:  "active",
		withBinding:    true,
		deviceStatus:   "active",
		queueDepth:     4,
	}
}

// bridgeFixture provisions a fully licensed device against a temporary MySQL
// database and serves the hub over TLS.
type bridgeFixture struct {
	t  *testing.T
	db *sql.DB

	pepper         []byte
	hub            *Hub
	registry       *Registry
	server         *httptest.Server
	limits         bridgeproto.Limits
	userID         string
	identityID     string
	deviceID       string
	secondDeviceID string
	subject        string
	credentialID   string
	credential     string
}

func newBridgeFixture(t *testing.T, mutate func(*fixtureSpec)) *bridgeFixture {
	t.Helper()
	spec := defaultSpec()
	if mutate != nil {
		mutate(&spec)
	}

	db := hubTestDatabase(t)
	fx := &bridgeFixture{t: t, db: db, pepper: []byte("bridgehub-test-pepper")}

	fx.userID = hubUUID(t)
	fx.identityID = hubUUID(t)
	fx.deviceID = hubUUID(t)
	fx.subject = fmt.Sprintf("bridgehub_subject_%d", time.Now().UnixNano())
	licenseID := hubUUID(t)
	bindingID := hubUUID(t)
	trialID := hubUUID(t)
	trialIdentityID := hubUUID(t)
	fx.credentialID = hubUUID(t)
	if spec.withSecondDevice {
		fx.secondDeviceID = hubUUID(t)
	}

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
		fx.identityID, fx.userID, "roblox", fx.subject, "Bridge Fixture")
	exec(`INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, ?, ?)`, fx.deviceID, fx.userID, "Test Device", spec.deviceStatus)
	if fx.secondDeviceID != "" {
		exec(`INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, ?, 'active')`, fx.secondDeviceID, fx.userID, "Second Device")
	}
	exec(`INSERT INTO licenses (id, user_id, roblox_identity_id, status, device_slots) VALUES (?, ?, ?, ?, 1)`,
		licenseID, fx.userID, fx.identityID, spec.licenseStatus)
	if spec.withBinding {
		exec(`INSERT INTO license_device_bindings (id, user_id, license_id, device_id, slot_ordinal, status) VALUES (?, ?, ?, ?, 1, 'active')`,
			bindingID, fx.userID, licenseID, fx.deviceID)
	}
	exec(`INSERT INTO trial_entitlements (id, user_id, started_at, ends_at) VALUES (?, ?, ?, ?)`,
		trialID, fx.userID, spec.trialStartedAt.UTC(), spec.trialEndsAt.UTC())
	exec(`INSERT INTO trial_entitlement_identities (id, trial_entitlement_id, user_id, provider, provider_subject) VALUES (?, ?, ?, ?, ?)`,
		trialIdentityID, trialID, fx.userID, "roblox", fx.subject)
	var expiresAt, revokedAt any
	if !spec.credentialExpiresAt.IsZero() {
		expiresAt = spec.credentialExpiresAt.UTC()
	}
	if !spec.credentialRevokedAt.IsZero() {
		revokedAt = spec.credentialRevokedAt.UTC()
	}
	exec(`INSERT INTO device_credentials (id, user_id, device_id, credential_digest, expires_at, revoked_at) VALUES (?, ?, ?, ?, ?, ?)`,
		fx.credentialID, fx.userID, fx.deviceID, digest[:], expiresAt, revokedAt)

	fx.limits = bridgeproto.Limits{MaxPayloadBytes: 256 * 1024}
	auditService := audit.NewService(mysqlstore.NewAuditStore(db))
	clock := systemClock{}
	cfg := Config{
		Store:             NewSQLStore(db),
		Entitlements:      entitlement.NewService(mysqlstore.NewEntitlementStore(db, clock, auditService), clock),
		Pepper:            fx.pepper,
		HelloTimeout:      300 * time.Millisecond,
		HeartbeatInterval: 100 * time.Millisecond,
		HeartbeatTimeout:  900 * time.Millisecond,
		ReauthInterval:    150 * time.Millisecond,
		MaxEnvelopeBytes:  fx.limits.MaxPayloadBytes,
		QueueDepth:        spec.queueDepth,
		WriteTimeout:      5 * time.Second,
	}
	hub, err := NewHub(cfg)
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

func (fx *bridgeFixture) bridgeURL() string {
	return "wss" + strings.TrimPrefix(fx.server.URL, "https") + "/bridge"
}

func (fx *bridgeFixture) bridgeHTTPURL() string {
	return fx.server.URL + "/bridge"
}

func (fx *bridgeFixture) tlsClient() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
}

func (fx *bridgeFixture) dialRaw(t *testing.T, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return websocket.Dial(ctx, fx.bridgeURL(), &websocket.DialOptions{HTTPHeader: header, HTTPClient: fx.tlsClient()})
}

// dialAuthenticated asserts the upgrade succeeds with the fixture credential.
func (fx *bridgeFixture) dialAuthenticated(t *testing.T) *websocket.Conn {
	t.Helper()
	ws, resp, err := fx.dialRaw(t, fx.credential)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("expected successful WSS upgrade, got error %v (http %d)", err, status)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

// dialRejected asserts the handshake is refused with wantStatus before upgrade.
func (fx *bridgeFixture) dialRejected(t *testing.T, token string, wantStatus int) {
	t.Helper()
	ws, resp, err := fx.dialRaw(t, token)
	if err == nil {
		_ = ws.CloseNow()
		t.Fatalf("dial with token %q unexpectedly succeeded", token)
	}
	if resp == nil {
		t.Fatalf("dial error without HTTP response: %v", err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("http status = %d, want %d (err %v)", resp.StatusCode, wantStatus, err)
	}
}

func (fx *bridgeFixture) sendHelloEnvelope(t *testing.T, ws *websocket.Conn, deviceID string) {
	t.Helper()
	data, err := bridgeproto.Encode(bridgeproto.Envelope{
		Version:  bridgeproto.Version,
		Type:     bridgeproto.TypeHello,
		DeviceID: deviceID,
		Payload:  json.RawMessage(`{"bridge_version":"test","platform":"windows","capabilities":[]}`),
	}, fx.limits)
	if err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, data); err != nil {
		t.Fatalf("write hello: %v", err)
	}
}

func (fx *bridgeFixture) statusEnvelope() bridgeproto.Envelope {
	return bridgeproto.Envelope{
		Version:  bridgeproto.Version,
		Type:     bridgeproto.TypeStatus,
		DeviceID: fx.deviceID,
		Payload:  json.RawMessage(`{"state":"connected"}`),
	}
}

func (fx *bridgeFixture) revokeCredential(t *testing.T) {
	t.Helper()
	if _, err := fx.db.ExecContext(context.Background(), `UPDATE device_credentials SET revoked_at = ? WHERE id = ?`,
		time.Now().UTC(), fx.credentialID); err != nil {
		t.Fatalf("revoke credential: %v", err)
	}
}

func (fx *bridgeFixture) awaitRegistryLen(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	eventually(t, timeout, fmt.Sprintf("registry length %d", want), func() bool {
		return fx.registry.Len() == want
	})
}

func (fx *bridgeFixture) awaitRegistryEmpty(t *testing.T, timeout time.Duration) {
	t.Helper()
	eventually(t, timeout, "registry to become empty", func() bool {
		return fx.registry.Len() == 0
	})
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

// bridgeClient is a test bridge that keeps reading (so it answers server pings)
// and reports received envelopes plus the terminal close error.
type bridgeClient struct {
	messages chan bridgeproto.Envelope
	closed   chan error
}

func newBridgeClientReader(ws *websocket.Conn) *bridgeClient {
	client := &bridgeClient{
		messages: make(chan bridgeproto.Envelope, 16),
		closed:   make(chan error, 1),
	}
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, data, err := ws.Read(ctx)
			cancel()
			if err != nil {
				client.closed <- err
				return
			}
			env, decodeErr := bridgeproto.Decode(data, bridgeproto.Limits{MaxPayloadBytes: 4 << 20})
			if decodeErr != nil {
				client.closed <- decodeErr
				return
			}
			select {
			case client.messages <- env:
			default:
			}
		}
	}()
	return client
}

func expectCloseStatus(t *testing.T, ws *websocket.Conn, want websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := ws.Read(ctx)
	if err == nil {
		t.Fatalf("expected close with %d, got a data frame", want)
	}
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected websocket.CloseError %d, got %v", want, err)
	}
	if closeErr.Code != want {
		t.Fatalf("close code = %d (%s), want %d", closeErr.Code, closeErr.Reason, want)
	}
}

func expectReaderClose(t *testing.T, client *bridgeClient, want websocket.StatusCode) {
	t.Helper()
	select {
	case err := <-client.closed:
		var closeErr websocket.CloseError
		if !errors.As(err, &closeErr) {
			t.Fatalf("expected websocket.CloseError %d, got %v", want, err)
		}
		if closeErr.Code != want {
			t.Fatalf("close code = %d (%s), want %d", closeErr.Code, closeErr.Reason, want)
		}
	case <-time.After(4 * time.Second):
		t.Fatalf("timed out waiting for close %d", want)
	}
}

func TestBridgeHubValidCredentialUpgrades(t *testing.T) {
	fx := newBridgeFixture(t, nil)
	ws := fx.dialAuthenticated(t)
	client := newBridgeClientReader(ws)
	fx.sendHelloEnvelope(t, ws, fx.deviceID)
	fx.awaitRegistryLen(t, 1, 2*time.Second)

	if err := fx.registry.Send(t.Context(), fx.deviceID, fx.statusEnvelope()); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case env := <-client.messages:
		if env.Type != bridgeproto.TypeStatus || env.DeviceID != fx.deviceID {
			t.Fatalf("unexpected delivered envelope %#v", env)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for delivered envelope")
	}

	fx.hub.Shutdown()
	expectReaderClose(t, client, websocket.StatusNormalClosure)
	fx.awaitRegistryEmpty(t, 2*time.Second)
}

func TestBridgeHubRejectsUnauthorizedBeforeUpgrade(t *testing.T) {
	fx := newBridgeFixture(t, nil)
	cases := []struct {
		name   string
		header string
	}{
		{"missing authorization", ""},
		{"wrong scheme", "Basic Zm9v"},
		{"empty bearer", "Bearer "},
		{"token with spaces", "Bearer abc def"},
		{"token too long", "Bearer " + strings.Repeat("a", 513)},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodGet, fx.bridgeHTTPURL(), nil)
		if err != nil {
			t.Fatalf("%s: build request: %v", tc.name, err)
		}
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		resp, err := fx.server.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: request: %v", tc.name, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("%s: read body: %v", tc.name, readErr)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", tc.name, resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
			t.Fatalf("%s: WWW-Authenticate = %q, want Bearer challenge", tc.name, got)
		}
		if string(body) != "invalid device credential\n" {
			t.Fatalf("%s: body = %q, must be the generic sanitized rejection", tc.name, body)
		}
	}

	// A well-formed credential whose digest is unknown is also rejected 401.
	unknown, _, err := credential.Generate("brdg_", 32, fx.pepper)
	if err != nil {
		t.Fatalf("generate unknown credential: %v", err)
	}
	fx.dialRejected(t, unknown, http.StatusUnauthorized)
}

func TestBridgeHubRejectsExpiredCredential(t *testing.T) {
	fx := newBridgeFixture(t, func(spec *fixtureSpec) {
		spec.credentialExpiresAt = time.Now().Add(-time.Hour)
	})
	fx.dialRejected(t, fx.credential, http.StatusUnauthorized)
}

func TestBridgeHubRejectsRevokedCredential(t *testing.T) {
	fx := newBridgeFixture(t, func(spec *fixtureSpec) {
		spec.credentialRevokedAt = time.Now().Add(-time.Minute)
	})
	fx.dialRejected(t, fx.credential, http.StatusUnauthorized)
}

func TestBridgeHubRejectsExpiredTrial(t *testing.T) {
	fx := newBridgeFixture(t, func(spec *fixtureSpec) {
		spec.trialStartedAt = time.Now().Add(-15 * 24 * time.Hour)
		spec.trialEndsAt = time.Now().Add(-time.Hour)
		// An expired trial without a paid license must deny WSS: the license
		// fallback in the entitlement decision stays inactive.
		spec.licenseStatus = "expired"
	})
	fx.dialRejected(t, fx.credential, http.StatusUnauthorized)
}

// An active trial covers the credential-owned active device without a paid
// slot binding: the first enrollment creates the trial, device, and
// credential — never a binding — so the WSS dial must upgrade and serve.
func TestBridgeHubActiveTrialWithoutPaidBindingUpgrades(t *testing.T) {
	fx := newBridgeFixture(t, func(spec *fixtureSpec) {
		spec.licenseStatus = "" // trial-only: no paid license path at all
		spec.withBinding = false
	})
	ws := fx.dialAuthenticated(t)
	defer ws.CloseNow()
	client := newBridgeClientReader(ws)
	fx.sendHelloEnvelope(t, ws, fx.deviceID)
	fx.awaitRegistryLen(t, 1, 2*time.Second)
	// The upgraded connection serves relayed envelopes.
	if err := fx.registry.Send(t.Context(), fx.deviceID, fx.statusEnvelope()); err != nil {
		t.Fatalf("send on trial-only connection: %v", err)
	}
	select {
	case env := <-client.messages:
		if env.DeviceID != fx.deviceID {
			t.Fatalf("unexpected envelope %#v", env)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for envelope on trial-only connection")
	}
}

// An expired trial with an active paid license keeps the device on WSS
// through its slot binding: the license fallback stays binding-enforced.
func TestBridgeHubExpiredTrialWithLicensedBindingUpgrades(t *testing.T) {
	fx := newBridgeFixture(t, func(spec *fixtureSpec) {
		spec.trialStartedAt = time.Now().Add(-15 * 24 * time.Hour)
		spec.trialEndsAt = time.Now().Add(-time.Hour)
		spec.licenseStatus = "active"
		spec.withBinding = true
	})
	ws := fx.dialAuthenticated(t)
	defer ws.CloseNow()
	fx.sendHelloEnvelope(t, ws, fx.deviceID)
	fx.awaitRegistryLen(t, 1, 2*time.Second)
}

// License-only access without a slot binding is refused: the paid path binds
// devices to license slots.
func TestBridgeHubLicenseOnlyWithoutBindingRejected(t *testing.T) {
	fx := newBridgeFixture(t, func(spec *fixtureSpec) {
		spec.trialStartedAt = time.Now().Add(-15 * 24 * time.Hour)
		spec.trialEndsAt = time.Now().Add(-time.Hour)
		spec.licenseStatus = "active"
		spec.withBinding = false
	})
	fx.dialRejected(t, fx.credential, http.StatusUnauthorized)
}

func TestBridgeHubRejectsInactiveDevice(t *testing.T) {
	fx := newBridgeFixture(t, func(spec *fixtureSpec) {
		spec.deviceStatus = "revoked"
	})
	fx.dialRejected(t, fx.credential, http.StatusUnauthorized)
}

func TestBridgeHubWrongOwnerClaimRejected(t *testing.T) {
	fx := newBridgeFixture(t, func(spec *fixtureSpec) {
		spec.withSecondDevice = true
	})
	ws := fx.dialAuthenticated(t)
	client := newBridgeClientReader(ws)
	// Valid credential of the fixture device, but the hello claims another
	// device owned by the same user: a credential cannot act for a device it
	// does not belong to.
	fx.sendHelloEnvelope(t, ws, fx.secondDeviceID)
	expectReaderClose(t, client, websocket.StatusPolicyViolation)
	fx.awaitRegistryEmpty(t, 2*time.Second)
}

func TestBridgeHubHelloTimeoutCloses(t *testing.T) {
	fx := newBridgeFixture(t, nil)
	ws := fx.dialAuthenticated(t)
	client := newBridgeClientReader(ws)
	// Never send hello.
	fx.awaitRegistryEmpty(t, 2*time.Second)
	expectReaderClose(t, client, websocket.StatusPolicyViolation)
	err := fx.registry.Send(context.Background(), fx.deviceID, fx.statusEnvelope())
	if !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("send after hello timeout err = %v, want ErrDeviceOffline", err)
	}
}

func TestBridgeHubDuplicateConnectionReplacesOld(t *testing.T) {
	fx := newBridgeFixture(t, nil)
	ws1 := fx.dialAuthenticated(t)
	client1 := newBridgeClientReader(ws1)
	fx.awaitRegistryLen(t, 1, 2*time.Second)

	ws2 := fx.dialAuthenticated(t)
	client2 := newBridgeClientReader(ws2)
	fx.sendHelloEnvelope(t, ws2, fx.deviceID)
	fx.awaitRegistryLen(t, 1, 2*time.Second)

	expectReaderClose(t, client1, websocket.StatusPolicyViolation)
	if n := fx.registry.Len(); n != 1 {
		t.Fatalf("registry len = %d, want 1 after replacement", n)
	}

	if err := fx.registry.Send(t.Context(), fx.deviceID, fx.statusEnvelope()); err != nil {
		t.Fatalf("send to replacement: %v", err)
	}
	select {
	case env := <-client2.messages:
		if env.DeviceID != fx.deviceID {
			t.Fatalf("unexpected envelope %#v", env)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for envelope on replacement connection")
	}
}

func TestBridgeHubReadLimitClosesWithMessageTooBig(t *testing.T) {
	fx := newBridgeFixture(t, nil)
	ws := fx.dialAuthenticated(t)
	client := newBridgeClientReader(ws)
	fx.sendHelloEnvelope(t, ws, fx.deviceID)
	fx.awaitRegistryLen(t, 1, 2*time.Second)

	oversized := make([]byte, fx.limits.MaxPayloadBytes+16*1024)
	for i := range oversized {
		oversized[i] = 'x'
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageBinary, oversized); err != nil {
		t.Fatalf("write oversized frame: %v", err)
	}
	expectReaderClose(t, client, websocket.StatusMessageTooBig)
	fx.awaitRegistryEmpty(t, 2*time.Second)
}

func TestBridgeHubHeartbeatTimeoutCloses(t *testing.T) {
	fx := newBridgeFixture(t, nil)
	ws := fx.dialAuthenticated(t)
	fx.sendHelloEnvelope(t, ws, fx.deviceID)
	fx.awaitRegistryLen(t, 1, 2*time.Second)
	// The client never reads, so server pings are never ponged.
	fx.awaitRegistryEmpty(t, 3*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := ws.Read(ctx)
	if err == nil {
		t.Fatal("expected close after heartbeat timeout, got data")
	}
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.StatusPolicyViolation {
		t.Fatalf("close after heartbeat timeout = %v, want 1008", err)
	}
}

func TestBridgeHubWriterQueueOverflowClosesSlowConsumer(t *testing.T) {
	fx := newBridgeFixture(t, func(spec *fixtureSpec) {
		spec.queueDepth = 1
	})
	ws := fx.dialAuthenticated(t)
	fx.sendHelloEnvelope(t, ws, fx.deviceID)
	fx.awaitRegistryLen(t, 1, 2*time.Second)
	// The client never reads: server-side writes wedge inside kernel buffers.

	payload := `"` + strings.Repeat("x", 200*1024) + `"`
	started := time.Now()
	slowConsumer := false
	for i := 0; i < 128; i++ {
		env := fx.statusEnvelope()
		env.Payload = json.RawMessage(payload)
		err := fx.registry.Send(context.Background(), fx.deviceID, env)
		switch {
		case err == nil:
		case errors.Is(err, ErrSlowConsumer):
			slowConsumer = true
		case errors.Is(err, ErrConnectionClosed), errors.Is(err, ErrDeviceOffline):
		default:
			t.Fatalf("unexpected send error: %v", err)
		}
	}
	elapsed := time.Since(started)
	if !slowConsumer {
		t.Fatal("expected ErrSlowConsumer from the bounded writer queue")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("send loop blocked the hub for %s; the hub must never block", elapsed)
	}
	fx.awaitRegistryEmpty(t, 8*time.Second)
}

func TestBridgeHubActiveRevokeDisconnects(t *testing.T) {
	fx := newBridgeFixture(t, nil)
	ws := fx.dialAuthenticated(t)
	client := newBridgeClientReader(ws)
	fx.sendHelloEnvelope(t, ws, fx.deviceID)
	fx.awaitRegistryLen(t, 1, 2*time.Second)

	fx.revokeCredential(t)
	expectReaderClose(t, client, websocket.StatusPolicyViolation)
	fx.awaitRegistryEmpty(t, 2*time.Second)
}

func TestBridgeHubGoroutineCountStableAcrossReconnects(t *testing.T) {
	fx := newBridgeFixture(t, nil)
	runtime.GC()
	baseline := runtime.NumGoroutine()

	for i := 0; i < 10; i++ {
		ws := fx.dialAuthenticated(t)
		client := newBridgeClientReader(ws)
		fx.sendHelloEnvelope(t, ws, fx.deviceID)
		fx.awaitRegistryLen(t, 1, 2*time.Second)
		if err := ws.CloseNow(); err != nil {
			t.Fatalf("disconnect %d: %v", i, err)
		}
		_ = client
		fx.awaitRegistryEmpty(t, 2*time.Second)
	}

	eventually(t, 5*time.Second, "goroutine count to stabilize", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baseline+5
	})
}
