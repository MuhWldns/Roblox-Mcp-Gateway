package audit

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
	"time"
)

// Injected secret fixtures. Every literal here is synthetic; the tests
// assert that none of them survive Redact or reach a Store through the
// Service.
const (
	secretAuthorization = "Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJyb2Jsb3hraXQifQ.c2lnbmF0dXJlLWZyYWdtZW50LW9ubHk"
	secretBearerToken   = "Bearer mca_0123456789abcdef0123456789abcdef01234567"
	secretCookie        = "Cookie: __Host-robloxkit_session=8f14e45fceea167a5a36dedd4bea2543; __Host-robloxkit_csrf=tok"
	secretSetCookie     = "Set-Cookie: __Host-robloxkit_session=8f14e45fceea167a5a36dedd4bea2543; HttpOnly"
	secretRobloxAccess  = "eyJraWQiOiJyb2Jsb3gtMjAyNiJ9.eyJzdWIiOiJyb2ItdXNlciJ9.ZnJhZ21lbnQtb2YtYW4tYWNjZXNzLXRva2Vu"
	secretRobloxRefresh = "eyJraWQiOiJyb2Jsb3gtMjAyNiJ9.eyJzdWIiOiJyZWZyZXNoIn9.bZnJhZ21lbnQtb2YtYV9yZWZyZXNo"
	secretRobloxID      = "eyJraWQiOiJyb2Jsb3gtMjAyNiJ9.eyJzdWIiOiJpZC10b2tlbiJ9.aWQtdG9rZW4tc2lnbmF0dXJlLWZyYWdtZW50"
	secretDeviceCred    = "rkd_9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	secretUserCode      = "rkuc_9f86d081ab2c"
	secretAccess        = "mca_abcdef0123456789abcdef0123456789abcdef01"
	secretRefresh       = "mcr_0123456789abcdef0123456789abcdef01234567"
	secretAuthCode      = "mcc_fedcba9876543210fedcba9876543210fedcba98"
	secretVerifier      = "code_verifier=dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	secretDSN           = "root:hunter2@tcp(127.0.0.1:3306)/robloxkit?parseTime=true"
)

// allSecrets joins every secret fixture so a single field poisons the whole
// pattern table at once.
func allSecrets() string {
	return strings.Join([]string{
		secretAuthorization, secretBearerToken, secretCookie, secretSetCookie,
		secretRobloxAccess, secretRobloxRefresh, secretRobloxID,
		secretDeviceCred, secretUserCode, secretAccess, secretRefresh,
		secretAuthCode, secretVerifier, secretDSN,
	}, " | ")
}

// secretFixtureList is the machine-checked list every assertion walks.
func secretFixtureList() []string {
	return []string{
		secretAuthorization, secretBearerToken, secretCookie, secretSetCookie,
		secretRobloxAccess, secretRobloxRefresh, secretRobloxID,
		secretDeviceCred, secretUserCode, secretAccess, secretRefresh,
		secretAuthCode, secretVerifier, secretDSN,
	}
}

// safeCorrelation is a well-formed correlation id that must survive redaction.
const safeCorrelation = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"

// assertNonePresent fails the test when any secret fixture appears anywhere
// inside observed.
func assertNonePresent(t *testing.T, observed []string) {
	t.Helper()
	for _, field := range observed {
		for _, secret := range secretFixtureList() {
			if strings.Contains(field, secret) {
				t.Fatalf("secret survived redaction in %q: %.80s", field, secret)
			}
		}
		if strings.Contains(field, "hunter2") || strings.Contains(field, "eyJ") {
			t.Fatalf("secret material survived redaction in %q", field)
		}
	}
}

// poisonedEvent carries every secret fixture in every free-form field, plus
// the safe identifiers that must survive.
func poisonedEvent() Event {
	return Event{
		Actor:         Actor{UserID: "11111111-2222-3333-4444-555555555555", Kind: ActorUser},
		Action:        "mcp.request_denied",
		CorrelationID: safeCorrelation + " " + allSecrets(),
		Reason:        "rate limited " + allSecrets(),
		UserID:        "11111111-2222-3333-4444-555555555555",
		TargetType:    "connector_grant " + allSecrets(),
		TargetID:      "77777777-8888-9999-aaaa-bbbbbbbbbbbb " + allSecrets(),
		Before: map[string]string{
			"name":   "Primary Laptop",
			"status": "active",
			"note":   allSecrets(),
		},
		After: map[string]string{
			"status": "revoked",
			"payload": `{"jsonrpc":"2.0","id":7,"method":"tools/call",` +
				`"params":{"name":"insert_block","arguments":{"credential":"` + secretDeviceCred + `"}}}`,
		},
		CreatedAt: time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC),
	}
}

func eventFields(event Event) []string {
	fields := []string{
		event.Actor.UserID, event.Action, event.CorrelationID, event.Reason,
		event.UserID, event.TargetType, event.TargetID,
	}
	for key, value := range event.Before {
		fields = append(fields, key+"="+value)
	}
	for key, value := range event.After {
		fields = append(fields, key+"="+value)
	}
	return fields
}

func TestRedactStripsSecretsFromEveryFreeFormField(t *testing.T) {
	redacted := Redact(poisonedEvent())
	assertNonePresent(t, eventFields(redacted))

	if redacted.Action != "mcp.request_denied" {
		t.Fatalf("action = %q, want unchanged", redacted.Action)
	}
	if !strings.HasPrefix(redacted.CorrelationID, safeCorrelation) {
		t.Fatalf("correlation = %q, want the safe identifier prefix preserved", redacted.CorrelationID)
	}
	if !strings.Contains(redacted.Reason, "rate limited") {
		t.Fatalf("reason = %q, want the safe prose preserved", redacted.Reason)
	}
	if redacted.UserID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("user id = %q, want the safe identifier preserved", redacted.UserID)
	}
	if redacted.Before["name"] != "Primary Laptop" || redacted.Before["status"] != "active" {
		t.Fatalf("before state = %+v, want safe values untouched", redacted.Before)
	}
	if redacted.After["status"] != "revoked" {
		t.Fatalf("after state = %+v, want safe values untouched", redacted.After)
	}
	if redacted.CreatedAt != poisonedEvent().CreatedAt {
		t.Fatalf("created_at = %v, want unchanged", redacted.CreatedAt)
	}
}

func TestRedactRemovesRawJSONPayloads(t *testing.T) {
	event := Event{
		Action:        "mcp.request_denied",
		CorrelationID: safeCorrelation,
		Reason: `relay failed for {"jsonrpc":"2.0","id":7,"method":"tools/call",` +
			`"params":{"name":"insert_block","arguments":{"position":{"x":1}}}}`,
	}
	redacted := Redact(event)
	for _, banned := range []string{"jsonrpc", "tools/call", "insert_block", "arguments"} {
		if strings.Contains(redacted.Reason, banned) {
			t.Fatalf("reason %q still carries raw payload marker %q", redacted.Reason, banned)
		}
	}
	if !strings.Contains(redacted.Reason, "relay failed for") {
		t.Fatalf("reason %q lost its safe prose prefix", redacted.Reason)
	}
}

func TestRedactRedactsSensitiveNamedMapKeys(t *testing.T) {
	event := Event{
		Action:        "device.rename",
		CorrelationID: safeCorrelation,
		After: map[string]string{
			"authorization":     secretBearerToken,
			"cookie":            secretCookie,
			"set-cookie":        secretSetCookie,
			"code_verifier":     "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			"verifier":          "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			"access_token":      secretAccess,
			"refresh_token":     secretRefresh,
			"id_token":          secretRobloxID,
			"device_credential": secretDeviceCred,
			"enrollment_code":   secretUserCode,
			"dsn":               secretDSN,
			"password":          "hunter2",
			"api_key":           "ak_live_51H8xQ",
		},
	}
	redacted := Redact(event)
	for key, value := range redacted.After {
		if value == "" {
			t.Fatalf("key %q was dropped; sensitive keys are redacted in place", key)
		}
	}
	for _, key := range []string{"authorization", "cookie", "set-cookie", "code_verifier", "verifier",
		"access_token", "refresh_token", "id_token", "device_credential", "enrollment_code", "dsn",
		"password", "api_key"} {
		if redacted.After[key] != "[redacted]" {
			t.Fatalf("sensitive key %q = %q, want [redacted]", key, redacted.After[key])
		}
	}
}

func TestRedactLeavesCleanEventUntouched(t *testing.T) {
	clean := Event{
		Actor:         Actor{UserID: "11111111-2222-3333-4444-555555555555", Kind: ActorAdmin},
		Action:        "license.transfer_device",
		CorrelationID: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0",
		Reason:        "requested by operator",
		UserID:        "11111111-2222-3333-4444-555555555555",
		TargetType:    "license",
		TargetID:      "aaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		Before:        map[string]string{"device_id": "device-old", "status": "active"},
		After:         map[string]string{"device_id": "device-new", "status": "active"},
		CreatedAt:     time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC),
	}
	redacted := Redact(clean)
	if redacted.Reason != clean.Reason || redacted.CorrelationID != clean.CorrelationID ||
		redacted.TargetType != clean.TargetType || redacted.TargetID != clean.TargetID {
		t.Fatalf("clean event mutated: %+v", redacted)
	}
	for key, value := range clean.Before {
		if redacted.Before[key] != value {
			t.Fatalf("before[%q] = %q, want %q", key, redacted.Before[key], value)
		}
	}
	for key, value := range clean.After {
		if redacted.After[key] != value {
			t.Fatalf("after[%q] = %q, want %q", key, redacted.After[key], value)
		}
	}
}

func TestRedactBoundsFieldLengths(t *testing.T) {
	event := Event{
		Action:        "audit.overflow",
		CorrelationID: strings.Repeat("c", 5000),
		Reason:        strings.Repeat("r", 5000),
		TargetType:    strings.Repeat("t", 5000),
		TargetID:      strings.Repeat("d", 5000),
		Before:        map[string]string{strings.Repeat("k", 5000): strings.Repeat("v", 5000)},
	}
	redacted := Redact(event)
	if len(redacted.Reason) > 1000 {
		t.Fatalf("reason length = %d, want <= 1000", len(redacted.Reason))
	}
	if len(redacted.CorrelationID) > 255 {
		t.Fatalf("correlation length = %d, want <= 255", len(redacted.CorrelationID))
	}
	if len(redacted.TargetType) > 128 {
		t.Fatalf("target type length = %d, want <= 128", len(redacted.TargetType))
	}
	if len(redacted.TargetID) > 255 {
		t.Fatalf("target id length = %d, want <= 255", len(redacted.TargetID))
	}
	for key, value := range redacted.Before {
		if len(key) > 128 {
			t.Fatalf("map key length = %d, want <= 128", len(key))
		}
		if len(value) > 512 {
			t.Fatalf("map value length = %d, want <= 512", len(value))
		}
	}
}

// spyStore captures every event the Service hands it, standing in for any
// persistence or logging sink.
type spyStore struct {
	events []Event
}

func (s *spyStore) Append(_ context.Context, event Event) error {
	s.events = append(s.events, event)
	return nil
}

func (s *spyStore) AppendInTx(_ context.Context, _ *sql.Tx, event Event) error {
	s.events = append(s.events, event)
	return nil
}

func TestServiceRecordRedactsBeforeStore(t *testing.T) {
	spy := &spyStore{}
	service := NewService(spy)
	if err := service.Record(context.Background(), poisonedEvent()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(spy.events) != 1 {
		t.Fatalf("stored events = %d, want 1", len(spy.events))
	}
	assertNonePresent(t, eventFields(spy.events[0]))
	// The persisted stream is exactly what any log sink would see.
	assertNonePresent(t, []string{spy.events[0].Reason, spy.events[0].CorrelationID})
}

func TestServiceRecordInTxRedactsBeforeStore(t *testing.T) {
	spy := &spyStore{}
	service := NewService(spy)
	if err := service.RecordInTx(context.Background(), testTransaction(t), poisonedEvent()); err != nil {
		t.Fatalf("RecordInTx: %v", err)
	}
	if len(spy.events) != 1 {
		t.Fatalf("stored events = %d, want 1", len(spy.events))
	}
	assertNonePresent(t, eventFields(spy.events[0]))
}

// testTransaction opens a real *sql.Tx over an in-memory stub driver, so
// RecordInTx runs with the non-nil transaction its contract requires.
func testTransaction(t *testing.T) *sql.Tx {
	t.Helper()
	db, err := sql.Open("audit-stub", "audit-stub")
	if err != nil {
		t.Fatalf("open stub driver: %v", err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin stub transaction: %v", err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback()
		_ = db.Close()
	})
	return tx
}

type stubDriver struct{}

func (stubDriver) Open(string) (driver.Conn, error) { return stubConn{}, nil }

type stubConn struct{}

func (stubConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (stubConn) Close() error                        { return nil }
func (stubConn) Begin() (driver.Tx, error)           { return stubTx{}, nil }

type stubTx struct{}

func (stubTx) Commit() error   { return nil }
func (stubTx) Rollback() error { return nil }

func init() { sql.Register("audit-stub", stubDriver{}) }

func TestServiceRecordPreservesSafeEventIntact(t *testing.T) {
	spy := &spyStore{}
	service := NewService(spy)
	clean := Event{
		Actor:         Actor{UserID: "11111111-2222-3333-4444-555555555555", Kind: ActorUser},
		Action:        "device.revoke",
		CorrelationID: safeCorrelation,
		Reason:        "owner requested revocation",
		UserID:        "11111111-2222-3333-4444-555555555555",
		TargetType:    "device",
		TargetID:      "device-a-1",
		After:         map[string]string{"status": "revoked"},
	}
	if err := service.Record(context.Background(), clean); err != nil {
		t.Fatalf("Record: %v", err)
	}
	stored := spy.events[0]
	if stored.Reason != clean.Reason || stored.TargetID != clean.TargetID ||
		stored.CorrelationID != clean.CorrelationID || stored.After["status"] != "revoked" {
		t.Fatalf("safe event was altered in flight: %+v", stored)
	}
}

// --- bounded success-audit queue ---

func TestAuditQueueFlushPersistsPendingEvents(t *testing.T) {
	spy := &spyStore{}
	queue, err := NewQueue(NewService(spy), 8)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	for i := 0; i < 3; i++ {
		queue.Record(Event{Action: "mcp.tool_call", CorrelationID: safeCorrelation})
	}
	if got := queue.Pending(); got != 3 {
		t.Fatalf("pending = %d, want 3", got)
	}
	queue.Flush(context.Background())
	if len(spy.events) != 3 {
		t.Fatalf("stored events = %d, want 3", len(spy.events))
	}
	if got := queue.Pending(); got != 0 {
		t.Fatalf("pending after flush = %d, want 0", got)
	}
	if got := queue.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0", got)
	}
}

func TestAuditQueueDropsWhenFullAndCountsDrops(t *testing.T) {
	spy := &spyStore{}
	queue, err := NewQueue(NewService(spy), 2)
	if err != nil {
		t.Fatalf("NewQueue: %v", err)
	}
	for i := 0; i < 5; i++ {
		queue.Record(Event{Action: "mcp.tool_call", CorrelationID: safeCorrelation})
	}
	if got := queue.Pending(); got != 2 {
		t.Fatalf("pending = %d, want the bound 2", got)
	}
	if got := queue.Dropped(); got != 3 {
		t.Fatalf("dropped = %d, want 3", got)
	}
	queue.Flush(context.Background())
	if len(spy.events) != 2 {
		t.Fatalf("stored events = %d, want 2", len(spy.events))
	}
	if got := queue.Dropped(); got != 3 {
		t.Fatalf("dropped after flush = %d, want 3 (drops are permanent)", got)
	}
}

func TestAuditQueueRejectsInvalidConfiguration(t *testing.T) {
	if _, err := NewQueue(NewService(&spyStore{}), 0); err == nil {
		t.Fatal("zero-capacity queue accepted")
	}
	if _, err := NewQueue(nil, 8); err == nil {
		t.Fatal("queue without a service accepted")
	}
}
