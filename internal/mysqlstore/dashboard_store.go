package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"robloxkit/internal/audit"
	"robloxkit/internal/credential"
	"robloxkit/internal/dashboard"
)

// DashboardStore implements dashboard.Store. Every query is scoped
// to the owning user, so one user can never read or mutate another user's
// devices, Studio sessions, or connector grants: such objects are
// indistinguishable from missing ones. Each mutation applies its state change
// and its audit event in one transaction, so an audit failure rolls the
type DashboardStore struct {
	DB     *sql.DB
	Audits *audit.Service
	Pepper []byte
}

// NewDashboardStore builds the dashboard store over a verified pool.
func NewDashboardStore(db *sql.DB, audits *audit.Service, pepper []byte) *DashboardStore {
	return &DashboardStore{DB: db, Audits: audits, Pepper: pepper}
}

func (s *DashboardStore) check(ctx context.Context) error {
	if ctx == nil {
		return errors.New("mysqlstore: nil context")
	}
	if s == nil || s.DB == nil {
		return errors.New("mysqlstore: nil dashboard database")
	}
	return nil
}

func (s *DashboardStore) beginTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: begin transaction: %w", err)
	}
	return tx, nil
}

// audit appends one secret-free event inside tx. A nil audit service disables
// emission instead of failing the operation.
func (s *DashboardStore) audit(ctx context.Context, tx *sql.Tx, event audit.Event) error {
	if s.Audits == nil {
		return nil
	}
	return s.Audits.RecordInTx(ctx, tx, event)
}

func (s *DashboardStore) Devices(ctx context.Context, userID string) ([]dashboard.DeviceRow, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if userID == "" {
		return nil, errors.New("mysqlstore: empty user id")
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, hostname, platform, bridge_version, status,
		        last_heartbeat_at, official_mcp_state, reconnect_count, last_error,
		        created_at, updated_at
		 FROM devices WHERE user_id = ? ORDER BY created_at, id`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: list devices: %w", err)
	}
	defer rows.Close()
	devices := []dashboard.DeviceRow{}
	for rows.Next() {
		var row dashboard.DeviceRow
		if err := rows.Scan(
			&row.ID, &row.Name, &row.Hostname, &row.Platform, &row.BridgeVersion,
			&row.Status, &row.LastHeartbeat, &row.MCPState, &row.ReconnectCount,
			&row.LastError, &row.CreatedAt, &row.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("mysqlstore: scan device: %w", err)
		}
		devices = append(devices, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysqlstore: iterate devices: %w", err)
	}
	return devices, nil
}

// Studios lists the user's Studio sessions, newest first, bounded to keep
// responses small.
func (s *DashboardStore) Studios(ctx context.Context, userID string) ([]dashboard.StudioRow, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if userID == "" {
		return nil, errors.New("mysqlstore: empty user id")
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, device_id, studio_id, status, started_at, ended_at FROM studio_sessions WHERE user_id = ? ORDER BY started_at DESC, id LIMIT 200`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: list studios: %w", err)
	}
	defer rows.Close()
	studios := []dashboard.StudioRow{}
	for rows.Next() {
		var row dashboard.StudioRow
		var ended sql.NullTime
		if err := rows.Scan(&row.ID, &row.DeviceID, &row.StudioID, &row.Status, &row.StartedAt, &ended); err != nil {
			return nil, fmt.Errorf("mysqlstore: scan studio: %w", err)
		}
		if ended.Valid {
			endedAt := ended.Time.UTC()
			row.EndedAt = &endedAt
		}
		studios = append(studios, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysqlstore: iterate studios: %w", err)
	}
	return studios, nil
}

// Connectors lists the user's connector grants with their client metadata.
func (s *DashboardStore) Connectors(ctx context.Context, userID string) ([]dashboard.ConnectorRow, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if userID == "" {
		return nil, errors.New("mysqlstore: empty user id")
	}
	rows, err := s.DB.QueryContext(ctx,
		`SELECT g.id, c.client_id, c.client_name, g.device_id, g.studio_session_id, g.scopes, g.resource, g.created_at, g.revoked_at
		 FROM oauth_grants g
		 JOIN oauth_clients c ON c.id = g.client_id
		 WHERE g.user_id = ?
		 ORDER BY g.created_at DESC, g.id LIMIT 200`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: list connectors: %w", err)
	}
	defer rows.Close()
	connectors := []dashboard.ConnectorRow{}
	for rows.Next() {
		var (
			row     dashboard.ConnectorRow
			device  sql.NullString
			studio  sql.NullString
			scopes  []byte
			revoked sql.NullTime
		)
		if err := rows.Scan(&row.ID, &row.ClientID, &row.ClientName, &device, &studio, &scopes, &row.Resource, &row.CreatedAt, &revoked); err != nil {
			return nil, fmt.Errorf("mysqlstore: scan connector: %w", err)
		}
		row.DeviceID = device.String
		row.StudioSessionID = studio.String
		if err := json.Unmarshal(scopes, &row.Scopes); err != nil {
			return nil, fmt.Errorf("mysqlstore: decode connector scopes: %w", err)
		}
		if revoked.Valid {
			revokedAt := revoked.Time.UTC()
			row.RevokedAt = &revokedAt
		}
		connectors = append(connectors, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysqlstore: iterate connectors: %w", err)
	}
	return connectors, nil
}

// License returns the user's newest active license together with owner
// identity, trial, subscription, transfer, recovery, and usage totals.
func (s *DashboardStore) License(ctx context.Context, userID string) (*dashboard.LicenseRow, error) {
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if userID == "" {
		return nil, errors.New("mysqlstore: empty user id")
	}
	var row dashboard.LicenseRow
	err := s.DB.QueryRowContext(ctx,
		`SELECT l.status, l.device_slots,
		        ui.provider_subject AS roblox_username,
		        l.id AS license_id,
		        sub.id AS subscription_id, sub.status AS subscription_state
		 FROM licenses l
		 LEFT JOIN user_identities ui ON ui.user_id = l.user_id AND ui.provider = 'roblox' AND ui.status = 'active'
		 LEFT JOIN subscriptions sub ON sub.id = l.subscription_id AND sub.user_id = l.user_id
		 WHERE l.user_id = ? AND l.status = 'active'
		 ORDER BY l.created_at DESC LIMIT 1`,
		userID).Scan(&row.Status, &row.DeviceSlots, &row.RobloxUsername,
		&row.LicenseID, &row.SubscriptionID, &row.SubscriptionState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: read license: %w", err)
	}
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM license_device_bindings WHERE user_id = ? AND license_id = ? AND status = 'active' AND revoked_at IS NULL`,
		userID, row.LicenseID).Scan(&row.ActiveBindings); err != nil {
		return nil, fmt.Errorf("mysqlstore: count active bindings: %w", err)
	}

	// Trial window.
	_ = s.DB.QueryRowContext(ctx,
		`SELECT started_at, ends_at FROM trial_entitlements WHERE user_id = ? ORDER BY ends_at DESC LIMIT 1`,
		userID).Scan(&row.TrialStartedAt, &row.TrialEndsAt)
	if row.TrialEndsAt != nil {
		active := time.Now().Before(*row.TrialEndsAt)
		row.TrialActive = &active
	}

	// Pending transfer requests.
	_ = s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM license_transfer_requests WHERE user_id = ? AND status = 'pending'`,
		userID).Scan(&row.PendingTransfers)

	// Open recovery cases.
	_ = s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM account_recovery_cases WHERE user_id = ? AND status = 'open'`,
		userID).Scan(&row.OpenRecoveryCases)

	// Usage totals.
	_ = s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_records WHERE user_id = ? AND occurred_at >= ?`,
		userID, time.Now().AddDate(0, 0, -30)).Scan(&row.UsageLast30Days)
	_ = s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM usage_records WHERE user_id = ? AND occurred_at >= ?`,
		userID, time.Now().AddDate(0, 0, -7)).Scan(&row.UsageLast7Days)

	return &row, nil
}

// StudioSessionsActive counts the user's Studio sessions that are live
// right now.
func (s *DashboardStore) StudioSessionsActive(ctx context.Context, userID string) (int, error) {
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	if userID == "" {
		return 0, errors.New("mysqlstore: empty user id")
	}
	var count int
	if err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM studio_sessions WHERE user_id = ? AND status = 'active' AND ended_at IS NULL`,
		userID).Scan(&count); err != nil {
		return 0, fmt.Errorf("mysqlstore: count active studios: %w", err)
	}
	return count, nil
}

// RenameDevice renames an owned device and audits the change atomically.
func (s *DashboardStore) RenameDevice(ctx context.Context, correlation, userID, deviceID, name string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if userID == "" || deviceID == "" || name == "" {
		return errors.New("mysqlstore: rename requires user, device, and name")
	}
	correlation, err := auditCorrelation(correlation)
	if err != nil {
		return err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var oldName string
	err = tx.QueryRowContext(ctx,
		`SELECT name FROM devices WHERE id = ? AND user_id = ? FOR UPDATE`, deviceID, userID).Scan(&oldName)
	if errors.Is(err, sql.ErrNoRows) {
		return dashboard.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("mysqlstore: find device for rename: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE devices SET name = ? WHERE id = ? AND user_id = ?`, name, deviceID, userID); err != nil {
		return fmt.Errorf("mysqlstore: rename device: %w", err)
	}
	if err := s.audit(ctx, tx, audit.Event{
		Actor:         audit.Actor{UserID: userID, Kind: audit.ActorUser},
		Action:        "device.rename",
		CorrelationID: correlation,
		UserID:        userID,
		TargetType:    "device",
		TargetID:      deviceID,
		Before:        map[string]string{"name": oldName},
		After:         map[string]string{"name": name},
	}); err != nil {
		return fmt.Errorf("mysqlstore: audit device rename: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysqlstore: commit device rename: %w", err)
	}
	return nil
}

// RevokeDevice revokes an owned device and its credentials in one
// transaction, audits the transition, and deliberately leaves the license
// binding untouched: revocation never frees a slot. Revoking an already
// revoked device succeeds without a second audit event.
func (s *DashboardStore) RevokeDevice(ctx context.Context, correlation string, now time.Time, userID, deviceID string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if userID == "" || deviceID == "" {
		return errors.New("mysqlstore: revoke requires user and device")
	}
	if now.IsZero() {
		return errors.New("mysqlstore: revocation time is required")
	}
	correlation, err := auditCorrelation(correlation)
	if err != nil {
		return err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM devices WHERE id = ? AND user_id = ? FOR UPDATE`, deviceID, userID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return dashboard.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("mysqlstore: find device for revocation: %w", err)
	}
	if status == "revoked" {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE devices SET status = 'revoked' WHERE id = ? AND user_id = ?`, deviceID, userID); err != nil {
		return fmt.Errorf("mysqlstore: revoke device: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE device_credentials SET revoked_at = ? WHERE device_id = ? AND user_id = ? AND revoked_at IS NULL`,
		now.UTC(), deviceID, userID); err != nil {
		return fmt.Errorf("mysqlstore: revoke device credentials: %w", err)
	}
	if err := s.audit(ctx, tx, audit.Event{
		Actor:         audit.Actor{UserID: userID, Kind: audit.ActorUser},
		Action:        "device.revoke",
		CorrelationID: correlation,
		UserID:        userID,
		TargetType:    "device",
		TargetID:      deviceID,
		Before:        map[string]string{"status": status},
		After:         map[string]string{"status": "revoked"},
	}); err != nil {
		return fmt.Errorf("mysqlstore: audit device revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysqlstore: commit device revocation: %w", err)
	}
	return nil
}

// RotateDeviceCredential replaces the active credential for an owned, active
// device with a new opaque token, audits the change, and returns the new
// plaintext credential. The caller must deliver the new credential to the
// Bridge; the server never retains the plaintext.
func (s *DashboardStore) RotateDeviceCredential(ctx context.Context, correlation, userID, deviceID string) (string, error) {
	if err := s.check(ctx); err != nil {
		return "", err
	}
	if userID == "" || deviceID == "" {
		return "", errors.New("mysqlstore: rotation requires user and device")
	}
	if len(s.Pepper) == 0 {
		return "", errors.New("mysqlstore: credential pepper is required")
	}
	correlation, err := auditCorrelation(correlation)
	if err != nil {
		return "", err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	// Verify the device exists, is active, and is owned by the user.
	var status string
	if err := tx.QueryRowContext(ctx,
		`SELECT status FROM devices WHERE id = ? AND user_id = ? FOR UPDATE`,
		deviceID, userID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return "", dashboard.ErrNotFound
	} else if err != nil {
		return "", fmt.Errorf("mysqlstore: find device for rotation: %w", err)
	}
	if status != "active" {
		return "", dashboard.ErrNotFound
	}

	// Generate a new opaque credential.
	plain, digest, err := credential.Generate("rks_", 24, s.Pepper)
	if err != nil {
		return "", fmt.Errorf("mysqlstore: generate credential: %w", err)
	}

	// Revoke the current active credential and insert the new one.
	if _, err := tx.ExecContext(ctx,
		`UPDATE device_credentials SET revoked_at = NOW(6) WHERE device_id = ? AND user_id = ? AND revoked_at IS NULL`,
		deviceID, userID); err != nil {
		return "", fmt.Errorf("mysqlstore: revoke old credential: %w", err)
	}
	credID, err := identityUUID()
	if err != nil {
		return "", fmt.Errorf("mysqlstore: generate credential id: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO device_credentials (id, user_id, device_id, credential_digest, created_at)
		 VALUES (?, ?, ?, ?, NOW(6))`,
		credID, userID, deviceID, digest[:]); err != nil {
		return "", fmt.Errorf("mysqlstore: insert new credential: %w", err)
	}

	if err := s.audit(ctx, tx, audit.Event{
		Actor:         audit.Actor{UserID: userID, Kind: audit.ActorUser},
		Action:        "device.credential_rotate",
		CorrelationID: correlation,
		UserID:        userID,
		TargetType:    "device",
		TargetID:      deviceID,
		After:         map[string]string{"credential_id": credID},
	}); err != nil {
		return "", fmt.Errorf("mysqlstore: audit credential rotation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("mysqlstore: commit credential rotation: %w", err)
	}
	return plain, nil
}

// SetConnectorTarget repoints an owned, unrevoked connector grant at an
// owned device and an optional owned Studio session, and audits the change.
// Every referenced object must belong to the user; foreign or missing
// objects fail with ErrDashboardNotFound.
func (s *DashboardStore) SetConnectorTarget(ctx context.Context, correlation, userID, grantID, deviceID, studioSessionID string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if userID == "" || grantID == "" || deviceID == "" {
		return errors.New("mysqlstore: target change requires user, grant, and device")
	}
	correlation, err := auditCorrelation(correlation)
	if err != nil {
		return err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var beforeDevice, beforeStudio sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT device_id, studio_session_id FROM oauth_grants WHERE id = ? AND user_id = ? AND revoked_at IS NULL FOR UPDATE`,
		grantID, userID).Scan(&beforeDevice, &beforeStudio)
	if errors.Is(err, sql.ErrNoRows) {
		return dashboard.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("mysqlstore: find grant for target change: %w", err)
	}

	var owned string
	err = tx.QueryRowContext(ctx, `SELECT id FROM devices WHERE id = ? AND user_id = ?`, deviceID, userID).Scan(&owned)
	if errors.Is(err, sql.ErrNoRows) {
		return dashboard.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("mysqlstore: verify target device: %w", err)
	}
	var targetStudio any
	if studioSessionID != "" {
		var ownedStudio string
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM studio_sessions WHERE id = ? AND user_id = ?`, studioSessionID, userID).Scan(&ownedStudio)
		if errors.Is(err, sql.ErrNoRows) {
			return dashboard.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("mysqlstore: verify target studio: %w", err)
		}
		targetStudio = studioSessionID
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_grants SET device_id = ?, studio_session_id = ? WHERE id = ? AND user_id = ?`,
		deviceID, targetStudio, grantID, userID); err != nil {
		return fmt.Errorf("mysqlstore: update connector target: %w", err)
	}
	if err := s.audit(ctx, tx, audit.Event{
		Actor:         audit.Actor{UserID: userID, Kind: audit.ActorUser},
		Action:        "connector.target",
		CorrelationID: correlation,
		UserID:        userID,
		TargetType:    "connector_grant",
		TargetID:      grantID,
		Before:        map[string]string{"device_id": beforeDevice.String, "studio_session_id": beforeStudio.String},
		After:         map[string]string{"device_id": deviceID, "studio_session_id": studioSessionID},
	}); err != nil {
		return fmt.Errorf("mysqlstore: audit connector target change: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysqlstore: commit connector target change: %w", err)
	}
	return nil
}

// RevokeConnector revokes an owned connector grant together with every
// access and refresh token issued under it, in one transaction, and audits
// the transition. Revoking twice succeeds without a second audit event.
func (s *DashboardStore) RevokeConnector(ctx context.Context, correlation string, now time.Time, userID, grantID string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if userID == "" || grantID == "" {
		return errors.New("mysqlstore: connector revocation requires user and grant")
	}
	if now.IsZero() {
		return errors.New("mysqlstore: revocation time is required")
	}
	correlation, err := auditCorrelation(correlation)
	if err != nil {
		return err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var revokedAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT revoked_at FROM oauth_grants WHERE id = ? AND user_id = ? FOR UPDATE`, grantID, userID).Scan(&revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return dashboard.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("mysqlstore: find grant for revocation: %w", err)
	}
	if revokedAt.Valid {
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_grants SET revoked_at = ? WHERE id = ? AND user_id = ?`, now.UTC(), grantID, userID); err != nil {
		return fmt.Errorf("mysqlstore: revoke connector grant: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_access_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE grant_id = ? AND user_id = ?`,
		now.UTC(), grantID, userID); err != nil {
		return fmt.Errorf("mysqlstore: revoke connector access tokens: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_refresh_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE grant_id = ? AND user_id = ?`,
		now.UTC(), grantID, userID); err != nil {
		return fmt.Errorf("mysqlstore: revoke connector refresh tokens: %w", err)
	}
	if err := s.audit(ctx, tx, audit.Event{
		Actor:         audit.Actor{UserID: userID, Kind: audit.ActorUser},
		Action:        "connector.revoke",
		CorrelationID: correlation,
		UserID:        userID,
		TargetType:    "connector_grant",
		TargetID:      grantID,
		Before:        map[string]string{"revoked": "false"},
		After:         map[string]string{"revoked": "true"},
	}); err != nil {
		return fmt.Errorf("mysqlstore: audit connector revocation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysqlstore: commit connector revocation: %w", err)
	}
	return nil
}
