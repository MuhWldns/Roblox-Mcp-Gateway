package mysqlstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"robloxkit/internal/audit"
	"robloxkit/internal/dashboard"
)

// dashboardSeed inserts one bare user row and returns its id.
func dashboardSeedUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	id, err := identityUUID()
	if err != nil {
		t.Fatalf("generate user id: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO users (id) VALUES (?)`, id); err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	return id
}

func dashboardSeedDevice(t *testing.T, db *sql.DB, userID, name string) string {
	t.Helper()
	id, err := identityUUID()
	if err != nil {
		t.Fatalf("generate device id: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO devices (id, user_id, name) VALUES (?, ?, ?)`, id, userID, name); err != nil {
		t.Fatalf("insert test device: %v", err)
	}
	return id
}

func dashboardSeedStudio(t *testing.T, db *sql.DB, userID, deviceID, status string) string {
	t.Helper()
	id, err := identityUUID()
	if err != nil {
		t.Fatalf("generate studio id: %v", err)
	}
	var ended any
	if status != "active" {
		ended = time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO studio_sessions (id, user_id, device_id, studio_id, status, started_at, ended_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, userID, deviceID, "studio-"+id[:8], status, time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC), ended); err != nil {
		t.Fatalf("insert test studio session: %v", err)
	}
	return id
}

func dashboardSeedClient(t *testing.T, db *sql.DB) string {
	t.Helper()
	id, err := identityUUID()
	if err != nil {
		t.Fatalf("generate client id: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO oauth_clients (id, client_id, client_name, redirect_uris) VALUES (?, ?, 'ChatGPT', '["https://chatgpt.com/aip/oauth/callback"]')`,
		id, "https://chatgpt.com/aip/mcp"); err != nil {
		t.Fatalf("insert test client: %v", err)
	}
	return id
}

func dashboardSeedGrant(t *testing.T, db *sql.DB, userID, clientID, deviceID string) string {
	t.Helper()
	id, err := identityUUID()
	if err != nil {
		t.Fatalf("generate grant id: %v", err)
	}
	var device any
	if deviceID != "" {
		device = deviceID
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO oauth_grants (id, user_id, client_id, device_id, scopes, resource, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, userID, clientID, device, `["mcp:connect"]`, "https://gateway.example.test/mcp",
		time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("insert test grant: %v", err)
	}
	return id
}

func dashboardSeedTokens(t *testing.T, db *sql.DB, userID, grantID string) {
	t.Helper()
	accessDigest := sha256.Sum256([]byte("dashboard-access:" + grantID))
	refreshDigest := sha256.Sum256([]byte("dashboard-refresh:" + grantID))
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO oauth_access_tokens (id, user_id, grant_id, token_digest, expires_at) VALUES (?, ?, ?, ?, ?)`,
		"access"+grantID[:8], userID, grantID, accessDigest[:], time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("insert access token: %v", err)
	}
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO oauth_refresh_tokens (id, user_id, grant_id, family_id, token_digest, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		"refresh"+grantID[:8], userID, grantID, "refresh"+grantID[:8], refreshDigest[:], time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("insert refresh token: %v", err)
	}
}

func dashboardSeedCredential(t *testing.T, db *sql.DB, userID, deviceID string) {
	t.Helper()
	digest := sha256.Sum256([]byte("dashboard-credential:" + deviceID))
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO device_credentials (id, user_id, device_id, credential_digest) VALUES (?, ?, ?, ?)`,
		"cred"+deviceID[:8], userID, deviceID, digest[:]); err != nil {
		t.Fatalf("insert device credential: %v", err)
	}
}

// dashboardAudit reads the audit events recorded for one action.
func dashboardAudit(t *testing.T, db *sql.DB, action string) []audit.Event {
	t.Helper()
	rows, err := db.QueryContext(t.Context(),
		`SELECT action, correlation_id, reason, target_type, target_id, metadata, created_at FROM audit_logs WHERE action = ? ORDER BY created_at`,
		action)
	if err != nil {
		t.Fatalf("query audit events: %v", err)
	}
	defer rows.Close()
	var events []audit.Event
	for rows.Next() {
		var (
			event       audit.Event
			correlation string
			reason      sql.NullString
			targetType  sql.NullString
			targetID    sql.NullString
			metadata    sql.NullString
		)
		if err := rows.Scan(&event.Action, &correlation, &reason, &targetType, &targetID, &metadata, &event.CreatedAt); err != nil {
			t.Fatalf("scan audit event: %v", err)
		}
		event.CorrelationID = correlation
		event.Reason = reason.String
		event.TargetType = targetType.String
		event.TargetID = targetID.String
		if metadata.Valid {
			var packed struct {
				Before map[string]string `json:"before"`
				After  map[string]string `json:"after"`
			}
			if err := json.Unmarshal([]byte(metadata.String), &packed); err != nil {
				t.Fatalf("decode audit metadata %q: %v", metadata.String, err)
			}
			event.Before, event.After = packed.Before, packed.After
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit events: %v", err)
	}
	return events
}

func newDashboardTestStore(t *testing.T) (*DashboardStore, *sql.DB) {
	t.Helper()
	db := identityTestDatabase(t)
	store := NewDashboardStore(db, audit.NewService(NewAuditStore(db)), []byte("test-pepper"))
	return store, db
}

func TestDashboardStoreReadsAreUserScoped(t *testing.T) {
	store, db := newDashboardTestStore(t)
	ctx := t.Context()

	userA := dashboardSeedUser(t, db)
	userB := dashboardSeedUser(t, db)
	deviceA := dashboardSeedDevice(t, db, userA, "Laptop A")
	deviceB := dashboardSeedDevice(t, db, userB, "Laptop B")
	studioA := dashboardSeedStudio(t, db, userA, deviceA, "active")
	studioA2 := dashboardSeedStudio(t, db, userA, deviceA, "ended")
	dashboardSeedStudio(t, db, userB, deviceB, "active")
	clientID := dashboardSeedClient(t, db)
	grantA := dashboardSeedGrant(t, db, userA, clientID, deviceA)
	grantB := dashboardSeedGrant(t, db, userB, clientID, deviceB)

	devices, err := store.Devices(ctx, userA)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devices) != 1 || devices[0].ID != deviceA || devices[0].Name != "Laptop A" || devices[0].Status != "active" {
		t.Fatalf("user A devices = %+v", devices)
	}
	if devices[0].CreatedAt.IsZero() || devices[0].UpdatedAt.IsZero() {
		t.Fatalf("device timestamps = %+v", devices[0])
	}

	studios, err := store.Studios(ctx, userA)
	if err != nil {
		t.Fatalf("Studios: %v", err)
	}
	if len(studios) != 2 {
		t.Fatalf("user A studios = %+v, want 2", studios)
	}
	if studios[0].ID != studioA && studios[1].ID != studioA {
		t.Fatalf("studio rows = %+v, want %q among them", studios, studioA)
	}
	for _, studio := range studios {
		if studio.DeviceID != deviceA || studio.StudioID == "" || studio.Status == "" || studio.StartedAt.IsZero() {
			t.Fatalf("studio fields = %+v", studio)
		}
	}
	if studios[0].ID == studioA2 && studios[0].EndedAt == nil {
		t.Fatalf("ended studio missing ended_at: %+v", studios[0])
	}

	connectors, err := store.Connectors(ctx, userA)
	if err != nil {
		t.Fatalf("Connectors: %v", err)
	}
	if len(connectors) != 1 || connectors[0].ID != grantA || connectors[0].ClientName != "ChatGPT" ||
		connectors[0].ClientID != "https://chatgpt.com/aip/mcp" || connectors[0].DeviceID != deviceA ||
		connectors[0].Resource != "https://gateway.example.test/mcp" || connectors[0].RevokedAt != nil {
		t.Fatalf("user A connectors = %+v", connectors)
	}
	if len(connectors[0].Scopes) != 1 || connectors[0].Scopes[0] != "mcp:connect" {
		t.Fatalf("connector scopes = %+v", connectors[0].Scopes)
	}
	if _, err := store.Connectors(ctx, userB); err != nil {
		t.Fatalf("Connectors for user B: %v", err)
	}

	active, err := store.StudioSessionsActive(ctx, userA)
	if err != nil {
		t.Fatalf("StudioSessionsActive: %v", err)
	}
	if active != 1 {
		t.Fatalf("active studios = %d, want 1", active)
	}

	// License reads report slot state only when a license exists.
	if got, err := store.License(ctx, userA); err != nil || got != nil {
		t.Fatalf("license without rows = %v, %v; want nil", got, err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO licenses (id, user_id, status, device_slots) VALUES (?, ?, 'active', 2)`, "lic-1", userA); err != nil {
		t.Fatalf("insert license: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO license_device_bindings (id, user_id, license_id, device_id, slot_ordinal, status) VALUES (?, ?, ?, ?, 1, 'active')`,
		"bind-1", userA, "lic-1", deviceA); err != nil {
		t.Fatalf("insert binding: %v", err)
	}
	license, err := store.License(ctx, userA)
	if err != nil {
		t.Fatalf("License: %v", err)
	}
	if license == nil || license.Status != "active" || license.DeviceSlots != 2 || license.ActiveBindings != 1 {
		t.Fatalf("license = %+v", license)
	}
	if got, err := store.License(ctx, userB); err != nil || got != nil {
		t.Fatalf("user B license = %v, %v; want nil", got, err)
	}
	_ = grantB
}

func TestDashboardStoreRenameDevice(t *testing.T) {
	store, db := newDashboardTestStore(t)
	ctx := t.Context()
	userA := dashboardSeedUser(t, db)
	userB := dashboardSeedUser(t, db)
	deviceA := dashboardSeedDevice(t, db, userA, "Old Name")

	if err := store.RenameDevice(ctx, "corr-1", userB, deviceA, "Stolen"); !errors.Is(err, dashboard.ErrNotFound) {
		t.Fatalf("cross-user rename error = %v, want ErrDashboardNotFound", err)
	}
	var name string
	if err := db.QueryRowContext(ctx, `SELECT name FROM devices WHERE id = ?`, deviceA).Scan(&name); err != nil || name != "Old Name" {
		t.Fatalf("device renamed by foreigner: %q %v", name, err)
	}

	if err := store.RenameDevice(ctx, "corr-2", userA, deviceA, "New Name"); err != nil {
		t.Fatalf("RenameDevice: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT name FROM devices WHERE id = ?`, deviceA).Scan(&name); err != nil || name != "New Name" {
		t.Fatalf("rename not applied: %q %v", name, err)
	}

	events := dashboardAudit(t, db, "device.rename")
	if len(events) != 1 {
		t.Fatalf("device.rename events = %d, want 1", len(events))
	}
	if events[0].CorrelationID != "corr-2" || events[0].TargetID != deviceA || events[0].TargetType != "device" {
		t.Fatalf("rename audit event = %+v", events[0])
	}
	if events[0].Before["name"] != "Old Name" || events[0].After["name"] != "New Name" {
		t.Fatalf("rename audit before/after = %+v %+v", events[0].Before, events[0].After)
	}
}

func TestDashboardStoreRevokeDevice(t *testing.T) {
	store, db := newDashboardTestStore(t)
	ctx := t.Context()
	userA := dashboardSeedUser(t, db)
	userB := dashboardSeedUser(t, db)
	deviceA := dashboardSeedDevice(t, db, userA, "Laptop A")
	deviceB := dashboardSeedDevice(t, db, userB, "Laptop B")
	dashboardSeedCredential(t, db, userA, deviceA)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO licenses (id, user_id, status, device_slots) VALUES (?, ?, 'active', 1)`, "lic-1", userA); err != nil {
		t.Fatalf("insert license: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO license_device_bindings (id, user_id, license_id, device_id, slot_ordinal, status) VALUES (?, ?, ?, ?, 1, 'active')`,
		"bind-1", userA, "lic-1", deviceA); err != nil {
		t.Fatalf("insert binding: %v", err)
	}

	if err := store.RevokeDevice(ctx, "corr-1", now, userB, deviceA); !errors.Is(err, dashboard.ErrNotFound) {
		t.Fatalf("cross-user revoke error = %v, want ErrDashboardNotFound", err)
	}
	var status string
	if err := db.QueryRowContext(ctx, `SELECT status FROM devices WHERE id = ?`, deviceA).Scan(&status); err != nil || status != "active" {
		t.Fatalf("foreign revoke changed status: %q %v", status, err)
	}

	if err := store.RevokeDevice(ctx, "corr-2", now, userA, deviceA); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM devices WHERE id = ?`, deviceA).Scan(&status); err != nil || status != "revoked" {
		t.Fatalf("device status = %q, want revoked", status)
	}
	var credentialRevoked sql.NullTime
	if err := db.QueryRowContext(ctx,
		`SELECT revoked_at FROM device_credentials WHERE device_id = ?`, deviceA).Scan(&credentialRevoked); err != nil {
		t.Fatalf("read credential: %v", err)
	}
	if !credentialRevoked.Valid || !credentialRevoked.Time.Equal(now) {
		t.Fatalf("credential revoked_at = %v, want %v", credentialRevoked, now)
	}

	// The license slot deliberately stays occupied.
	var bindingStatus string
	var bindingRevoked sql.NullTime
	if err := db.QueryRowContext(ctx,
		`SELECT status, revoked_at FROM license_device_bindings WHERE id = 'bind-1'`).Scan(&bindingStatus, &bindingRevoked); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if bindingStatus != "active" || bindingRevoked.Valid {
		t.Fatalf("binding = %s/%v, want active and not revoked", bindingStatus, bindingRevoked)
	}

	events := dashboardAudit(t, db, "device.revoke")
	if len(events) != 1 {
		t.Fatalf("device.revoke events = %d, want 1", len(events))
	}
	if events[0].CorrelationID != "corr-2" || events[0].TargetID != deviceA ||
		events[0].Before["status"] != "active" || events[0].After["status"] != "revoked" {
		t.Fatalf("revoke audit event = %+v", events[0])
	}

	// A second revoke succeeds without a second event.
	if err := store.RevokeDevice(ctx, "corr-3", now.Add(time.Minute), userA, deviceA); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
	if events := dashboardAudit(t, db, "device.revoke"); len(events) != 1 {
		t.Fatalf("device.revoke audited twice: %d events", len(events))
	}
	_ = deviceB
}

func TestDashboardStoreSetConnectorTarget(t *testing.T) {
	store, db := newDashboardTestStore(t)
	ctx := t.Context()
	userA := dashboardSeedUser(t, db)
	userB := dashboardSeedUser(t, db)
	deviceA := dashboardSeedDevice(t, db, userA, "Laptop A")
	deviceB := dashboardSeedDevice(t, db, userB, "Laptop B")
	studioA := dashboardSeedStudio(t, db, userA, deviceA, "active")
	studioB := dashboardSeedStudio(t, db, userB, deviceB, "active")
	clientID := dashboardSeedClient(t, db)
	grantA := dashboardSeedGrant(t, db, userA, clientID, deviceA)
	grantB := dashboardSeedGrant(t, db, userB, clientID, deviceB)

	if err := store.SetConnectorTarget(ctx, "corr-1", userB, grantA, deviceB, ""); !errors.Is(err, dashboard.ErrNotFound) {
		t.Fatalf("foreign grant target error = %v, want ErrDashboardNotFound", err)
	}
	if err := store.SetConnectorTarget(ctx, "corr-2", userA, grantA, deviceB, ""); !errors.Is(err, dashboard.ErrNotFound) {
		t.Fatalf("foreign device target error = %v, want ErrDashboardNotFound", err)
	}
	if err := store.SetConnectorTarget(ctx, "corr-3", userA, grantA, deviceA, studioB); !errors.Is(err, dashboard.ErrNotFound) {
		t.Fatalf("foreign studio target error = %v, want ErrDashboardNotFound", err)
	}

	if err := store.SetConnectorTarget(ctx, "corr-4", userA, grantA, deviceA, studioA); err != nil {
		t.Fatalf("SetConnectorTarget: %v", err)
	}
	var deviceID, studioID sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT device_id, studio_session_id FROM oauth_grants WHERE id = ?`, grantA).Scan(&deviceID, &studioID); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if deviceID.String != deviceA || studioID.String != studioA {
		t.Fatalf("target = %q/%q, want %q/%q", deviceID.String, studioID.String, deviceA, studioA)
	}
	events := dashboardAudit(t, db, "connector.target")
	if len(events) != 1 || events[0].CorrelationID != "corr-4" || events[0].TargetID != grantA {
		t.Fatalf("connector.target events = %+v", events)
	}

	// Clearing the Studio keeps the device target.
	if err := store.SetConnectorTarget(ctx, "corr-5", userA, grantA, deviceA, ""); err != nil {
		t.Fatalf("SetConnectorTarget clear: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT device_id, studio_session_id FROM oauth_grants WHERE id = ?`, grantA).Scan(&deviceID, &studioID); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	if studioID.Valid {
		t.Fatalf("studio target after clear = %q, want NULL", studioID.String)
	}

	// A revoked grant can no longer be retargeted.
	if _, err := db.ExecContext(ctx,
		`UPDATE oauth_grants SET revoked_at = ? WHERE id = ?`, time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC), grantB); err != nil {
		t.Fatalf("revoke grant B: %v", err)
	}
	if err := store.SetConnectorTarget(ctx, "corr-6", userB, grantB, deviceB, ""); !errors.Is(err, dashboard.ErrNotFound) {
		t.Fatalf("revoked grant target error = %v, want ErrDashboardNotFound", err)
	}
}

func TestDashboardStoreRevokeConnector(t *testing.T) {
	store, db := newDashboardTestStore(t)
	ctx := t.Context()
	userA := dashboardSeedUser(t, db)
	userB := dashboardSeedUser(t, db)
	deviceA := dashboardSeedDevice(t, db, userA, "Laptop A")
	clientID := dashboardSeedClient(t, db)
	grantA := dashboardSeedGrant(t, db, userA, clientID, deviceA)
	dashboardSeedGrant(t, db, userB, clientID, "")
	dashboardSeedTokens(t, db, userA, grantA)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

	if err := store.RevokeConnector(ctx, "corr-1", now, userB, grantA); !errors.Is(err, dashboard.ErrNotFound) {
		t.Fatalf("foreign revoke error = %v, want ErrDashboardNotFound", err)
	}
	if got := revokedAtOf(t, db, "oauth_grants", grantA); got.Valid {
		t.Fatal("foreign revoke touched the grant")
	}

	if err := store.RevokeConnector(ctx, "corr-2", now, userA, grantA); err != nil {
		t.Fatalf("RevokeConnector: %v", err)
	}
	if got := revokedAtOf(t, db, "oauth_grants", grantA); !got.Valid || !got.Time.Equal(now) {
		t.Fatalf("grant revoked_at = %v, want %v", got, now)
	}
	var accessRevoked, refreshRevoked sql.NullTime
	if err := db.QueryRowContext(ctx,
		`SELECT revoked_at FROM oauth_access_tokens WHERE grant_id = ?`, grantA).Scan(&accessRevoked); err != nil {
		t.Fatalf("read access token: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT revoked_at FROM oauth_refresh_tokens WHERE grant_id = ?`, grantA).Scan(&refreshRevoked); err != nil {
		t.Fatalf("read refresh token: %v", err)
	}
	if !accessRevoked.Valid || !refreshRevoked.Valid {
		t.Fatalf("tokens after revoke = access:%v refresh:%v", accessRevoked.Valid, refreshRevoked.Valid)
	}

	events := dashboardAudit(t, db, "connector.revoke")
	if len(events) != 1 || events[0].CorrelationID != "corr-2" || events[0].TargetID != grantA {
		t.Fatalf("connector.revoke events = %+v", events)
	}

	// A second revoke succeeds without a second event.
	if err := store.RevokeConnector(ctx, "corr-3", now.Add(time.Minute), userA, grantA); err != nil {
		t.Fatalf("idempotent connector revoke: %v", err)
	}
	if events := dashboardAudit(t, db, "connector.revoke"); len(events) != 1 {
		t.Fatalf("connector.revoke audited twice: %d events", len(events))
	}
}

func revokedAtOf(t *testing.T, db *sql.DB, table, id string) sql.NullTime {
	t.Helper()
	var revoked sql.NullTime
	if err := db.QueryRowContext(t.Context(),
		`SELECT revoked_at FROM `+table+` WHERE id = ?`, id).Scan(&revoked); err != nil {
		t.Fatalf("read %s revoked_at: %v", table, err)
	}
	return revoked
}
