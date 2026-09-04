package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"robloxkit/internal/audit"
)

// Injected secret fixtures. The tests assert none of them reach the
// append-only tables even when a caller embeds them in an event.
const (
	auditSecretBearer   = "Authorization: Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJyb2Jsb3hraXQifQ.c2lnbmF0dXJlLWZyYWdtZW50"
	auditSecretDevice   = "rkd_9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	auditSecretUserCode = "rkuc_9f86d081ab2c"
	auditSecretAccess   = "mca_abcdef0123456789abcdef0123456789abcdef01"
	auditSecretVerifier = "code_verifier=dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	auditSecretDSN      = "root:hunter2@tcp(127.0.0.1:3306)/robloxkit?parseTime=true"
)

func auditSecretFixtures() []string {
	return []string{auditSecretBearer, auditSecretDevice, auditSecretUserCode,
		auditSecretAccess, auditSecretVerifier, auditSecretDSN}
}

func assertNoSecretsInRows(t *testing.T, rows []string) {
	t.Helper()
	for _, row := range rows {
		for _, secret := range auditSecretFixtures() {
			if strings.Contains(row, secret) {
				t.Fatalf("secret survived persistence in row %q: %.60s", row, secret)
			}
		}
		if strings.Contains(row, "hunter2") {
			t.Fatalf("DSN credential survived persistence in row %q", row)
		}
	}
}

const auditFixtureDeviceID = "11111111-1111-1111-1111-111111111111"

func seedAuditUser(t *testing.T, db *sql.DB, userID string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO users (id) VALUES (?)`, userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO devices (id, user_id, name) VALUES (?, ?, 'Audit Fixture')`,
		auditFixtureDeviceID, userID); err != nil {
		t.Fatalf("seed device: %v", err)
	}
}

func auditPoisonedEvent(userID, correlation string) audit.Event {
	return audit.Event{
		Actor:         audit.Actor{UserID: userID, Kind: audit.ActorUser},
		UserID:        userID,
		Action:        "device.rename",
		CorrelationID: correlation + " " + auditSecretBearer + " " + auditSecretDSN,
		Reason:        "requested " + auditSecretDevice + " " + auditSecretUserCode,
		TargetType:    "device",
		TargetID:      "device-1 " + auditSecretAccess,
		Before:        map[string]string{"name": "Primary Laptop", "leak": auditSecretVerifier},
		After:         map[string]string{"name": "Renamed", "leak": auditSecretDSN},
	}
}

func TestAuditStoreAppendRedactsSecretsBeforePersist(t *testing.T) {
	db := identityTestDatabase(t)
	userID := "22222222-2222-2222-2222-222222222222"
	seedAuditUser(t, db, userID)
	store := NewAuditStore(db)
	correlation := "6ba7b810-9dad-11d1-80b4-00c04fd430c8"

	if err := store.Append(t.Context(), auditPoisonedEvent(userID, correlation)); err != nil {
		t.Fatalf("Append: %v", err)
	}

	rows, err := db.QueryContext(t.Context(),
		`SELECT action, correlation_id, COALESCE(reason,''), COALESCE(target_id,''), COALESCE(CAST(metadata AS CHAR),'')
		 FROM audit_logs WHERE correlation_id LIKE ?`, correlation[:8]+"%")
	if err != nil {
		t.Fatalf("query audit rows: %v", err)
	}
	defer rows.Close()
	var collected []string
	var metadataSeen string
	for rows.Next() {
		var action, corr, reason, target, metadata string
		if err := rows.Scan(&action, &corr, &reason, &target, &metadata); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		collected = append(collected, strings.Join([]string{action, corr, reason, target, metadata}, "|"))
		metadataSeen = metadata
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit rows: %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("audit rows = %d, want 1 (%v)", len(collected), collected)
	}
	assertNoSecretsInRows(t, collected)
	if !strings.Contains(collected[0], "device.rename") {
		t.Fatalf("audit row lost its action: %q", collected[0])
	}
	if !strings.Contains(metadataSeen, "Primary Laptop") {
		t.Fatalf("safe metadata value lost: %q", metadataSeen)
	}
}

func TestAuditStoreAppendAdminEventRedactsState(t *testing.T) {
	db := identityTestDatabase(t)
	adminID := "33333333-3333-3333-3333-333333333333"
	seedAuditUser(t, db, adminID)
	store := NewAuditStore(db)

	event := auditPoisonedEvent(adminID, "admin-correlation-1")
	event.Actor.Kind = audit.ActorAdmin
	event.After = map[string]string{"status": "revoked", "leak": auditSecretBearer}
	if err := store.Append(t.Context(), event); err != nil {
		t.Fatalf("Append admin event: %v", err)
	}
	rows, err := db.QueryContext(t.Context(),
		`SELECT action, COALESCE(CAST(before_state AS CHAR),''), COALESCE(CAST(after_state AS CHAR),'')
		 FROM admin_actions WHERE correlation_id LIKE CONCAT(?, '%')`, "admin-correlation-1")
	if err != nil {
		t.Fatalf("query admin rows: %v", err)
	}
	defer rows.Close()
	var collected []string
	for rows.Next() {
		var action, before, after string
		if err := rows.Scan(&action, &before, &after); err != nil {
			t.Fatalf("scan admin row: %v", err)
		}
		collected = append(collected, strings.Join([]string{action, before, after}, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate admin rows: %v", err)
	}
	if len(collected) != 1 {
		t.Fatalf("admin rows = %d, want 1", len(collected))
	}
	assertNoSecretsInRows(t, collected)
	if !strings.Contains(collected[0], "revoked") {
		t.Fatalf("safe admin state lost: %q", collected[0])
	}
}

func TestAuditStoreAppendFKSafeNullUser(t *testing.T) {
	db := identityTestDatabase(t)
	store := NewAuditStore(db)
	err := store.Append(t.Context(), audit.Event{
		Actor:         audit.Actor{Kind: audit.ActorUser},
		Action:        "device.revoke",
		CorrelationID: "unknown-user-correlation",
		UserID:        "99999999-9999-9999-9999-999999999999",
		Reason:        "owner requested revocation",
	})
	if err != nil {
		t.Fatalf("Append with unknown user: %v", err)
	}
	var userID sql.NullString
	if err := db.QueryRowContext(t.Context(),
		`SELECT user_id FROM audit_logs WHERE correlation_id = ?`, "unknown-user-correlation",
	).Scan(&userID); err != nil {
		t.Fatalf("query audit row: %v", err)
	}
	if userID.Valid {
		t.Fatalf("user_id = %q, want NULL for a missing user", userID.String)
	}
}

func TestUsageIncrementAppendsUsageRecord(t *testing.T) {
	db := identityTestDatabase(t)
	userID := "44444444-4444-4444-4444-444444444444"
	seedAuditUser(t, db, userID)
	store := NewUsageStore(db)

	err := store.Increment(t.Context(), "gw-req-1", audit.Usage{
		UserID:    userID,
		DeviceID:  auditFixtureDeviceID,
		Operation: "tools/call",
		Outcome:   "success",
		Units:     1,
		Metadata:  map[string]string{"tool": "get_instance_tree"},
	})
	if err != nil {
		t.Fatalf("Increment: %v", err)
	}

	var (
		rowUser, rowDevice, rowOperation, rowOutcome, rowRequest string
		rowUnits                                                 int64
		rowStudio                                                sql.NullString
		rowMetadata                                              []byte
	)
	if err := db.QueryRowContext(t.Context(),
		`SELECT user_id, COALESCE(device_id,''), COALESCE(studio_session_id,''), operation, outcome,
		        COALESCE(request_id,''), units, COALESCE(CAST(metadata AS CHAR),'{}')
		 FROM usage_records WHERE request_id = ?`, "gw-req-1",
	).Scan(&rowUser, &rowDevice, &rowStudio, &rowOperation, &rowOutcome, &rowRequest, &rowUnits, &rowMetadata); err != nil {
		t.Fatalf("query usage record: %v", err)
	}
	if rowUser != userID || rowDevice != auditFixtureDeviceID {
		t.Fatalf("usage owner = user %q device %q", rowUser, rowDevice)
	}
	if rowOperation != "tools/call" || rowOutcome != "success" || rowUnits != 1 {
		t.Fatalf("usage row = op %q outcome %q units %d", rowOperation, rowOutcome, rowUnits)
	}
	if rowRequest != "gw-req-1" {
		t.Fatalf("request_id = %q, want gw-req-1", rowRequest)
	}
	var metadata map[string]string
	if err := json.Unmarshal(rowMetadata, &metadata); err != nil {
		t.Fatalf("decode metadata %s: %v", rowMetadata, err)
	}
	if metadata["tool"] != "get_instance_tree" {
		t.Fatalf("metadata = %+v, want the tool name", metadata)
	}
}

func TestUsageIncrementIsIdempotentPerGatewayRequestID(t *testing.T) {
	db := identityTestDatabase(t)
	userID := "55555555-5555-5555-5555-555555555555"
	seedAuditUser(t, db, userID)
	store := NewUsageStore(db)

	if err := store.Increment(t.Context(), "gw-req-dup", audit.Usage{
		UserID: userID, Operation: "tools/call", Outcome: "success", Units: 1,
	}); err != nil {
		t.Fatalf("first Increment: %v", err)
	}
	// The same gateway request id incremented again must not double-count,
	// even when the retried call reports different units.
	if err := store.Increment(t.Context(), "gw-req-dup", audit.Usage{
		UserID: userID, Operation: "tools/call", Outcome: "success", Units: 7,
	}); err != nil {
		t.Fatalf("second Increment: %v", err)
	}

	var count int
	var units int64
	if err := db.QueryRowContext(t.Context(),
		`SELECT COUNT(*), COALESCE(MAX(units),0) FROM usage_records WHERE request_id = ?`, "gw-req-dup",
	).Scan(&count, &units); err != nil {
		t.Fatalf("count usage rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("usage rows = %d, want 1 (double increment must not double-count)", count)
	}
	if units != 1 {
		t.Fatalf("units = %d, want the first record's 1", units)
	}
}

func TestUsageIncrementDistinctRequestIDsAppend(t *testing.T) {
	db := identityTestDatabase(t)
	userID := "66666666-6666-6666-6666-666666666666"
	seedAuditUser(t, db, userID)
	store := NewUsageStore(db)

	for _, id := range []string{"gw-req-a", "gw-req-b"} {
		if err := store.Increment(t.Context(), id, audit.Usage{
			UserID: userID, DeviceID: auditFixtureDeviceID,
			Operation: "tools/call", Outcome: "success", Units: 1,
		}); err != nil {
			t.Fatalf("Increment %s: %v", id, err)
		}
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM usage_records WHERE user_id = ?`, userID).
		Scan(&count); err != nil {
		t.Fatalf("count usage rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("usage rows = %d, want 2", count)
	}
}

func TestUsageIncrementRejectsUnknownUser(t *testing.T) {
	db := identityTestDatabase(t)
	store := NewUsageStore(db)
	err := store.Increment(t.Context(), "gw-req-orphan", audit.Usage{
		UserID: "88888888-8888-8888-8888-888888888888", Operation: "tools/call",
		Outcome: "success", Units: 1,
	})
	if err == nil {
		t.Fatal("usage for a missing user was accepted")
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM usage_records`).Scan(&count); err != nil {
		t.Fatalf("count usage rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("usage rows = %d, want 0 after a rejected increment", count)
	}
}

func TestUsageIncrementRejectsInvalidInput(t *testing.T) {
	db := identityTestDatabase(t)
	userID := "77777777-7777-7777-7777-777777777777"
	seedAuditUser(t, db, userID)
	store := NewUsageStore(db)

	if err := store.Increment(t.Context(), "", audit.Usage{
		UserID: userID, Operation: "tools/call", Outcome: "success", Units: 1,
	}); err == nil {
		t.Fatal("empty gateway request id accepted")
	}
	if err := store.Increment(t.Context(), "gw-req-x", audit.Usage{
		UserID: userID, Operation: "", Outcome: "success", Units: 1,
	}); err == nil {
		t.Fatal("empty operation accepted")
	}
	if err := store.Increment(t.Context(), "gw-req-y", audit.Usage{
		UserID: userID, Operation: "tools/call", Outcome: "success", Units: -1,
	}); err == nil {
		t.Fatal("negative units accepted")
	}
}

func TestUsageRecordsAreAppendOnly(t *testing.T) {
	db := identityTestDatabase(t)
	userID := "51515151-5151-5151-5151-515151515151"
	seedAuditUser(t, db, userID)
	store := NewUsageStore(db)
	if err := store.Increment(t.Context(), "gw-req-ro", audit.Usage{
		UserID: userID, Operation: "tools/call", Outcome: "success", Units: 1,
	}); err != nil {
		t.Fatalf("Increment: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE usage_records SET units = 99 WHERE request_id = ?`, "gw-req-ro"); err == nil {
		t.Fatal("usage_records update succeeded; the table must be append-only")
	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM usage_records WHERE request_id = ?`, "gw-req-ro"); err == nil {
		t.Fatal("usage_records delete succeeded; the table must be append-only")
	}
}

func TestUsageStoreRejectsNilDatabase(t *testing.T) {
	store := NewUsageStore(nil)
	if err := store.Increment(t.Context(), "gw-req-nil", audit.Usage{
		UserID: "x", Operation: "tools/call", Outcome: "success", Units: 1,
	}); err == nil {
		t.Fatal("nil database accepted")
	}
}

func TestUsageIncrementHonorsContextCancellation(t *testing.T) {
	db := identityTestDatabase(t)
	userID := "61616161-6161-6161-6161-616161616161"
	seedAuditUser(t, db, userID)
	store := NewUsageStore(db)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := store.Increment(ctx, "gw-req-cancel", audit.Usage{
		UserID: userID, Operation: "tools/call", Outcome: "success", Units: 1,
	}); err == nil {
		t.Fatal("canceled context accepted")
	}
}
