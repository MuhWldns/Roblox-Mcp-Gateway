package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"

	"robloxkit/internal/audit"
	"robloxkit/internal/entitlement"
)

// EntitlementStore persists trials, licenses, device slots, and their audit
// trail. Clock-dependent scheduling receives an explicit now; audit emission
// flows through the configured audit service inside the same transactions.
type EntitlementStore struct {
	DB    *sql.DB
	Clock entitlement.Clock
	Audit *audit.Service
}

// NewEntitlementStore builds the entitlement store over a verified pool.
func NewEntitlementStore(db *sql.DB, clock entitlement.Clock, auditSvc *audit.Service) *EntitlementStore {
	return &EntitlementStore{DB: db, Clock: clock, Audit: auditSvc}
}

func (s *EntitlementStore) check(ctx context.Context) error {
	if ctx == nil {
		return errors.New("mysqlstore: nil context")
	}
	if s == nil || s.DB == nil {
		return errors.New("mysqlstore: nil database")
	}
	return nil
}

func (s *EntitlementStore) beginTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: begin transaction: %w", err)
	}
	return tx, nil
}

func (s *EntitlementStore) commit(tx *sql.Tx, op string) error {
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysqlstore: commit %s: %w", op, err)
	}
	return nil
}

// recordAudit appends one secret-free audit event inside tx. A nil audit
// service disables emission instead of failing the operation.
func (s *EntitlementStore) recordAudit(ctx context.Context, tx *sql.Tx, event audit.Event) error {
	if s.Audit == nil {
		return nil
	}
	return s.Audit.RecordInTx(ctx, tx, event)
}

func auditCorrelation(preferred string) (string, error) {
	if preferred != "" {
		return preferred, nil
	}
	id, err := identityUUID()
	if err != nil {
		return "", fmt.Errorf("mysqlstore: generate audit correlation: %w", err)
	}
	return id, nil
}

// BindFirstDevice starts the one-time trial and registers the first device and
// its credential atomically: trial, historical identity binding, device,
// credential, and audit event commit or roll back as one unit. When the user
// already holds a trial, the call degrades into an idempotent re-claim: the
// existing device (same owner, still active) gets its old credential revoked
// and the freshly minted one inserted; the trial window is never restarted.
// Re-claims are refused for devices owned by another user or revoked devices.
func (s *EntitlementStore) BindFirstDevice(ctx context.Context, now time.Time, in entitlement.FirstDeviceBinding) (entitlement.Entitlement, entitlement.Binding, error) {
	if err := s.check(ctx); err != nil {
		return entitlement.Entitlement{}, entitlement.Binding{}, err
	}
	if in.UserID == "" || in.Provider == "" || in.ProviderSubject == "" || in.DeviceID == "" {
		return entitlement.Entitlement{}, entitlement.Binding{}, errors.New("mysqlstore: invalid first device binding")
	}
	if in.CredentialDigest == [32]byte{} {
		return entitlement.Entitlement{}, entitlement.Binding{}, errors.New("mysqlstore: empty credential digest")
	}
	correlation, err := auditCorrelation(in.AuditCorrelation)
	if err != nil {
		return entitlement.Entitlement{}, entitlement.Binding{}, err
	}

	tx, err := s.beginTx(ctx)
	if err != nil {
		return entitlement.Entitlement{}, entitlement.Binding{}, err
	}
	defer tx.Rollback()

	if err := ensureUser(ctx, tx, in.UserID); err != nil {
		return entitlement.Entitlement{}, entitlement.Binding{}, err
	}

	// Lock the device row first: re-claim decisions and fresh binds both key
	// on it, and serializing on the device prevents double-claim races.
	var deviceStatus string
	var deviceUserID string
	err = tx.QueryRowContext(ctx,
		`SELECT user_id, status FROM devices WHERE id = ? FOR UPDATE`,
		in.DeviceID,
	).Scan(&deviceUserID, &deviceStatus)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
	default:
		return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: lock device %s: %w", in.DeviceID, err)
	}

	var trialID string
	var startedAt, endsAt time.Time
	err = tx.QueryRowContext(ctx,
		`SELECT id, started_at, ends_at FROM trial_entitlements WHERE user_id = ? FOR UPDATE`,
		in.UserID,
	).Scan(&trialID, &startedAt, &endsAt)
	switch {
	case err == nil:
	case errors.Is(err, sql.ErrNoRows):
	default:
		return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: read trial entitlement for %s: %w", in.UserID, err)
	}

	if trialID != "" {
		// Existing trial: this is a re-claim, never a second trial.
		if deviceUserID == "" {
			return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: device %s not registered for %s: %w", in.DeviceID, in.UserID, entitlement.ErrTrialAlreadyUsed)
		}
		if deviceUserID != in.UserID {
			return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: device %s: %w", in.DeviceID, entitlement.ErrDeviceOwnedByOther)
		}
		if deviceStatus != "active" {
			return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: device %s is %s", in.DeviceID, deviceStatus)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE device_credentials SET revoked_at = ? WHERE device_id = ? AND user_id = ? AND revoked_at IS NULL`,
			now, in.DeviceID, in.UserID,
		); err != nil {
			return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: revoke old credential: %w", err)
		}
		credentialID, err := identityUUID()
		if err != nil {
			return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: generate credential id: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO device_credentials (id, user_id, device_id, credential_digest) VALUES (?, ?, ?, ?)`,
			credentialID, in.UserID, in.DeviceID, in.CredentialDigest[:],
		); err != nil {
			return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: insert device credential: %w", err)
		}
		if err := s.recordAudit(ctx, tx, audit.Event{
			Actor:         audit.Actor{UserID: in.UserID, Kind: audit.ActorUser},
			Action:        "device.reclaim",
			CorrelationID: correlation,
			UserID:        in.UserID,
			TargetType:    "device",
			TargetID:      in.DeviceID,
			After:         map[string]string{"trial_entitlement_id": trialID},
			CreatedAt:     now,
		}); err != nil {
			return entitlement.Entitlement{}, entitlement.Binding{}, err
		}
		if err := s.commit(tx, "device reclaim"); err != nil {
			return entitlement.Entitlement{}, entitlement.Binding{}, err
		}
		return entitlement.Entitlement{ID: trialID, UserID: in.UserID, StartedAt: startedAt, EndsAt: endsAt},
			entitlement.Binding{UserID: in.UserID, DeviceID: in.DeviceID, Status: deviceStatus}, nil
	}

	// Fresh trial path. The user has no trial yet; the device row, when it
	// exists, must still belong to this user (or be absent).
	if deviceUserID != "" && deviceUserID != in.UserID {
		return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: device %s: %w", in.DeviceID, entitlement.ErrDeviceOwnedByOther)
	}
	endsAt = now.Add(entitlement.TrialWindow)
	trialID, err = identityUUID()
	if err != nil {
		return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: generate trial id: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO trial_entitlements (id, user_id, started_at, ends_at) VALUES (?, ?, ?, ?)`,
		trialID, in.UserID, now, endsAt,
	); err != nil {
		if isMySQLDuplicate(err) {
			return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: user %s already used the trial: %w", in.UserID, entitlement.ErrTrialAlreadyUsed)
		}
		return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: insert trial entitlement: %w", err)
	}
	trialIdentityID, err := identityUUID()
	if err != nil {
		return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: generate trial identity id: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO trial_entitlement_identities (id, trial_entitlement_id, user_id, provider, provider_subject) VALUES (?, ?, ?, ?, ?)`,
		trialIdentityID, trialID, in.UserID, in.Provider, in.ProviderSubject,
	); err != nil {
		if isMySQLDuplicate(err) {
			return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: subject %s already used the trial: %w", in.ProviderSubject, entitlement.ErrTrialAlreadyUsed)
		}
		return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: insert trial identity: %w", err)
	}
	if deviceUserID == "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, ?, 'active')`,
			in.DeviceID, in.UserID, in.DeviceID,
		); err != nil {
			return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: insert device: %w", err)
		}
	}
	credentialID, err := identityUUID()
	if err != nil {
		return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: generate credential id: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO device_credentials (id, user_id, device_id, credential_digest) VALUES (?, ?, ?, ?)`,
		credentialID, in.UserID, in.DeviceID, in.CredentialDigest[:],
	); err != nil {
		return entitlement.Entitlement{}, entitlement.Binding{}, fmt.Errorf("mysqlstore: insert device credential: %w", err)
	}
	if err := s.recordAudit(ctx, tx, audit.Event{
		Actor:         audit.Actor{UserID: in.UserID, Kind: audit.ActorUser},
		Action:        "trial.start",
		CorrelationID: correlation,
		UserID:        in.UserID,
		TargetType:    "trial_entitlement",
		TargetID:      trialID,
		After:         map[string]string{"user_id": in.UserID, "device_id": in.DeviceID},
		CreatedAt:     now,
	}); err != nil {
		return entitlement.Entitlement{}, entitlement.Binding{}, err
	}
	if err := s.commit(tx, "first device binding"); err != nil {
		return entitlement.Entitlement{}, entitlement.Binding{}, err
	}
	ent := entitlement.Entitlement{ID: trialID, UserID: in.UserID, StartedAt: now, EndsAt: endsAt}
	binding := entitlement.Binding{UserID: in.UserID, DeviceID: in.DeviceID, Status: "active"}
	return ent, binding, nil
}

// Authorize evaluates the subject's trial window and licenses. It never
// writes, so dashboard and download stay available regardless of outcome.
func (s *EntitlementStore) Authorize(ctx context.Context, now time.Time, subject entitlement.Subject) (entitlement.Decision, error) {
	if err := s.check(ctx); err != nil {
		return entitlement.Decision{}, err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return entitlement.Decision{}, err
	}
	defer tx.Rollback()

	var (
		ent                         entitlement.Entitlement
		extensionReason, extendedBy sql.NullString
		hasTrial                    bool
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, user_id, started_at, ends_at, extension_reason, extended_by FROM trial_entitlements WHERE user_id = ?`,
		subject.UserID,
	).Scan(&ent.ID, &ent.UserID, &ent.StartedAt, &ent.EndsAt, &extensionReason, &extendedBy)
	switch {
	case err == nil:
		hasTrial = true
		ent.ExtensionReason = extensionReason.String
		ent.ExtendedBy = extendedBy.String
	case errors.Is(err, sql.ErrNoRows):
	default:
		return entitlement.Decision{}, fmt.Errorf("mysqlstore: read trial entitlement for %s: %w", subject.UserID, err)
	}
	trialActive := hasTrial && now.Before(ent.EndsAt)

	// The license source is evaluated regardless of the trial so the decision
	// exposes which window is active: binding-gated surfaces only accept
	// license-only access with a paid slot binding.
	var activeLicenses int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM licenses WHERE user_id = ? AND status = 'active'`,
		subject.UserID,
	).Scan(&activeLicenses); err != nil {
		return entitlement.Decision{}, fmt.Errorf("mysqlstore: count active licenses for %s: %w", subject.UserID, err)
	}
	licenseActive := activeLicenses > 0

	return entitlement.Decision{
		Active:        trialActive || licenseActive,
		TrialActive:   trialActive,
		LicenseActive: licenseActive,
		Entitlement:   ent,
	}, nil
}

// TransferDevice moves an active license slot from one device to another and
// records the request. The slot ordinal is preserved; revoked devices keep
// their ordinals, so ordinals are never reused.
func (s *EntitlementStore) TransferDevice(ctx context.Context, now time.Time, actor entitlement.AdminActor, licenseID, oldDeviceID, newDeviceID, reason string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if licenseID == "" || oldDeviceID == "" || newDeviceID == "" {
		return errors.New("mysqlstore: invalid device transfer")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var licenseUser string
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM licenses WHERE id = ? FOR UPDATE`, licenseID).Scan(&licenseUser); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("mysqlstore: license %s: %w", licenseID, entitlement.ErrNotFound)
		}
		return fmt.Errorf("mysqlstore: lock license %s: %w", licenseID, err)
	}
	var (
		bindingID   string
		slotOrdinal int
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT id, slot_ordinal FROM license_device_bindings WHERE license_id = ? AND device_id = ? AND status = 'active' FOR UPDATE`,
		licenseID, oldDeviceID,
	).Scan(&bindingID, &slotOrdinal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("mysqlstore: license %s device %s: %w", licenseID, oldDeviceID, entitlement.ErrBindingNotFound)
		}
		return fmt.Errorf("mysqlstore: lock device binding: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE license_device_bindings SET device_id = ? WHERE id = ?`,
		newDeviceID, bindingID,
	); err != nil {
		return fmt.Errorf("mysqlstore: move device binding: %w", err)
	}
	transferID, err := identityUUID()
	if err != nil {
		return fmt.Errorf("mysqlstore: generate transfer id: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO license_transfer_requests (id, user_id, license_id, old_device_id, new_device_id, status, reason) VALUES (?, ?, ?, ?, ?, 'completed', ?)`,
		transferID, licenseUser, licenseID, oldDeviceID, newDeviceID, reason,
	); err != nil {
		return fmt.Errorf("mysqlstore: record transfer request: %w", err)
	}
	if err := s.recordAudit(ctx, tx, audit.Event{
		Actor:         audit.Actor{UserID: actor.UserID, Kind: audit.ActorAdmin},
		Action:        "license.transfer_device",
		CorrelationID: transferID,
		Reason:        reason,
		UserID:        licenseUser,
		TargetType:    "license",
		TargetID:      licenseID,
		Before:        map[string]string{"device_id": oldDeviceID},
		After:         map[string]string{"device_id": newDeviceID},
		CreatedAt:     now,
	}); err != nil {
		return err
	}
	return s.commit(tx, "device transfer")
}

// RecoverIdentity revokes every live credential and web session for the user
// and records the recovery case. trial_entitlements is never touched, so the
// trial window survives a recovery.
func (s *EntitlementStore) RecoverIdentity(ctx context.Context, now time.Time, actor entitlement.AdminActor, userID, newIdentityID, reason, evidenceRef string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if userID == "" {
		return errors.New("mysqlstore: invalid identity recovery")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var known int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&known); err != nil {
		return fmt.Errorf("mysqlstore: check user %s: %w", userID, err)
	}
	if known == 0 {
		return fmt.Errorf("mysqlstore: user %s: %w", userID, entitlement.ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE device_credentials SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
		now, userID,
	); err != nil {
		return fmt.Errorf("mysqlstore: revoke credentials: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE web_sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
		now, userID,
	); err != nil {
		return fmt.Errorf("mysqlstore: revoke sessions: %w", err)
	}
	caseID, err := identityUUID()
	if err != nil {
		return fmt.Errorf("mysqlstore: generate recovery case id: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO account_recovery_cases (id, user_id, status, reason, evidence_ref) VALUES (?, ?, 'completed', ?, ?)`,
		caseID, userID, reason, evidenceRef,
	); err != nil {
		return fmt.Errorf("mysqlstore: record recovery case: %w", err)
	}
	if err := s.recordAudit(ctx, tx, audit.Event{
		Actor:         audit.Actor{UserID: actor.UserID, Kind: audit.ActorAdmin},
		Action:        "identity.recover",
		CorrelationID: caseID,
		Reason:        reason,
		UserID:        userID,
		TargetType:    "user",
		TargetID:      userID,
		After:         map[string]string{"new_identity_id": newIdentityID},
		CreatedAt:     now,
	}); err != nil {
		return err
	}
	return s.commit(tx, "identity recovery")
}

// ExtendTrial lengthens the existing trial's expiry only. The append-only
// trigger permits exactly this update shape when the new expiry is strictly
// later than the current one.
func (s *EntitlementStore) ExtendTrial(ctx context.Context, actor entitlement.AdminActor, entitlementID string, newEndsAt time.Time, reason string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if entitlementID == "" {
		return errors.New("mysqlstore: invalid trial extension")
	}
	if newEndsAt.IsZero() {
		return errors.New("mysqlstore: trial extension requires a new expiry")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var (
		trialUser string
		current   time.Time
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT user_id, ends_at FROM trial_entitlements WHERE id = ? FOR UPDATE`,
		entitlementID,
	).Scan(&trialUser, &current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("mysqlstore: trial entitlement %s: %w", entitlementID, entitlement.ErrNotFound)
		}
		return fmt.Errorf("mysqlstore: lock trial entitlement %s: %w", entitlementID, err)
	}
	if !newEndsAt.After(current) {
		return fmt.Errorf("mysqlstore: extend trial %s to %s: %w", entitlementID, newEndsAt.Format(time.RFC3339), entitlement.ErrInvalidExtension)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE trial_entitlements SET ends_at = ?, extension_reason = ?, extended_by = ? WHERE id = ?`,
		newEndsAt, nullableString(reason), nullableString(actor.UserID), entitlementID,
	); err != nil {
		return fmt.Errorf("mysqlstore: extend trial entitlement: %w", err)
	}
	correlation, err := auditCorrelation("")
	if err != nil {
		return err
	}
	if err := s.recordAudit(ctx, tx, audit.Event{
		Actor:         audit.Actor{UserID: actor.UserID, Kind: audit.ActorAdmin},
		Action:        "trial.extend",
		CorrelationID: correlation,
		Reason:        reason,
		UserID:        trialUser,
		TargetType:    "trial_entitlement",
		TargetID:      entitlementID,
		Before:        map[string]string{"ends_at": current.Format(time.RFC3339Nano)},
		After:         map[string]string{"ends_at": newEndsAt.Format(time.RFC3339Nano)},
	}); err != nil {
		return err
	}
	return s.commit(tx, "trial extension")
}

// CreateLicense grants a paid policy window with a bounded number of device
// slots.
func (s *EntitlementStore) CreateLicense(ctx context.Context, userID, robloxIdentityID string, deviceSlots int) (entitlement.License, error) {
	if err := s.check(ctx); err != nil {
		return entitlement.License{}, err
	}
	if userID == "" {
		return entitlement.License{}, errors.New("mysqlstore: license requires a user")
	}
	if deviceSlots <= 0 {
		return entitlement.License{}, errors.New("mysqlstore: license requires at least one device slot")
	}
	licenseID, err := identityUUID()
	if err != nil {
		return entitlement.License{}, fmt.Errorf("mysqlstore: generate license id: %w", err)
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return entitlement.License{}, err
	}
	defer tx.Rollback()

	if err := ensureUser(ctx, tx, userID); err != nil {
		return entitlement.License{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO licenses (id, user_id, roblox_identity_id, status, device_slots) VALUES (?, ?, ?, 'active', ?)`,
		licenseID, userID, nullableString(robloxIdentityID), deviceSlots,
	); err != nil {
		return entitlement.License{}, fmt.Errorf("mysqlstore: insert license: %w", err)
	}
	if err := s.commit(tx, "license creation"); err != nil {
		return entitlement.License{}, err
	}
	return entitlement.License{
		ID:               licenseID,
		UserID:           userID,
		RobloxIdentityID: robloxIdentityID,
		DeviceSlots:      deviceSlots,
		Status:           "active",
	}, nil
}

// CreateDevice registers a device for a user.
func (s *EntitlementStore) CreateDevice(ctx context.Context, userID, deviceID, name string) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if userID == "" || deviceID == "" {
		return errors.New("mysqlstore: device requires a user and an id")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := ensureUser(ctx, tx, userID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, ?, 'active')`,
		deviceID, userID, name,
	); err != nil {
		return fmt.Errorf("mysqlstore: insert device: %w", err)
	}
	return s.commit(tx, "device creation")
}

// BindDeviceSlot activates one license slot for a device. The license row and
// the binding range are locked, so concurrent activation of the last slot has
// exactly one winner. Revoked bindings retain their ordinals and keep counting
// against the slot budget.
func (s *EntitlementStore) BindDeviceSlot(ctx context.Context, licenseID, deviceID string) (entitlement.Binding, error) {
	if err := s.check(ctx); err != nil {
		return entitlement.Binding{}, err
	}
	if licenseID == "" || deviceID == "" {
		return entitlement.Binding{}, errors.New("mysqlstore: device slot binding requires a license and a device")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return entitlement.Binding{}, err
	}
	defer tx.Rollback()

	var (
		licenseUser string
		deviceSlots int
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT user_id, device_slots FROM licenses WHERE id = ? FOR UPDATE`,
		licenseID,
	).Scan(&licenseUser, &deviceSlots); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return entitlement.Binding{}, fmt.Errorf("mysqlstore: license %s: %w", licenseID, entitlement.ErrNotFound)
		}
		return entitlement.Binding{}, fmt.Errorf("mysqlstore: lock license %s: %w", licenseID, err)
	}
	var bound int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM license_device_bindings WHERE license_id = ? FOR UPDATE`,
		licenseID,
	).Scan(&bound); err != nil {
		return entitlement.Binding{}, fmt.Errorf("mysqlstore: count device bindings: %w", err)
	}
	if bound >= deviceSlots {
		return entitlement.Binding{}, fmt.Errorf("mysqlstore: license %s has no free device slot: %w", licenseID, entitlement.ErrNoSlot)
	}
	var slotOrdinal int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(slot_ordinal), 0) + 1 FROM license_device_bindings WHERE license_id = ? FOR UPDATE`,
		licenseID,
	).Scan(&slotOrdinal); err != nil {
		return entitlement.Binding{}, fmt.Errorf("mysqlstore: allocate slot ordinal: %w", err)
	}
	bindingID, err := identityUUID()
	if err != nil {
		return entitlement.Binding{}, fmt.Errorf("mysqlstore: generate binding id: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO license_device_bindings (id, user_id, license_id, device_id, slot_ordinal, status) VALUES (?, ?, ?, ?, ?, 'active')`,
		bindingID, licenseUser, licenseID, deviceID, slotOrdinal,
	); err != nil {
		return entitlement.Binding{}, fmt.Errorf("mysqlstore: insert device binding: %w", err)
	}
	correlation, err := auditCorrelation("")
	if err != nil {
		return entitlement.Binding{}, err
	}
	if err := s.recordAudit(ctx, tx, audit.Event{
		Actor:         audit.Actor{UserID: licenseUser, Kind: audit.ActorUser},
		Action:        "license.bind_slot",
		CorrelationID: correlation,
		UserID:        licenseUser,
		TargetType:    "license_device_binding",
		TargetID:      bindingID,
		After:         map[string]string{"license_id": licenseID, "device_id": deviceID, "slot_ordinal": strconv.Itoa(slotOrdinal)},
	}); err != nil {
		return entitlement.Binding{}, err
	}
	if err := s.commit(tx, "device slot binding"); err != nil {
		return entitlement.Binding{}, err
	}
	return entitlement.Binding{
		ID:          bindingID,
		UserID:      licenseUser,
		LicenseID:   licenseID,
		DeviceID:    deviceID,
		SlotOrdinal: slotOrdinal,
		Status:      "active",
	}, nil
}

// RevokeDevice marks a device revoked and releases its active license
// bindings. The slots stay consumed so a revoked device never frees capacity.
func (s *EntitlementStore) RevokeDevice(ctx context.Context, deviceID string, when time.Time) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if deviceID == "" {
		return errors.New("mysqlstore: device revoke requires a device id")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var deviceUser string
	if err := tx.QueryRowContext(ctx, `SELECT user_id FROM devices WHERE id = ? FOR UPDATE`, deviceID).Scan(&deviceUser); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("mysqlstore: device %s: %w", deviceID, entitlement.ErrNotFound)
		}
		return fmt.Errorf("mysqlstore: lock device %s: %w", deviceID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE devices SET status = 'revoked' WHERE id = ?`, deviceID); err != nil {
		return fmt.Errorf("mysqlstore: revoke device: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE license_device_bindings SET status = 'revoked', revoked_at = ? WHERE device_id = ? AND status = 'active'`,
		when, deviceID,
	); err != nil {
		return fmt.Errorf("mysqlstore: revoke device bindings: %w", err)
	}
	correlation, err := auditCorrelation("")
	if err != nil {
		return err
	}
	if err := s.recordAudit(ctx, tx, audit.Event{
		Actor:         audit.Actor{Kind: audit.ActorSystem},
		Action:        "device.revoke",
		CorrelationID: correlation,
		UserID:        deviceUser,
		TargetType:    "device",
		TargetID:      deviceID,
		Before:        map[string]string{"status": "active"},
		After:         map[string]string{"status": "revoked"},
		CreatedAt:     when,
	}); err != nil {
		return err
	}
	return s.commit(tx, "device revoke")
}

// ensureUser creates the users row for userID when absent. It is idempotent,
// so foreign-key references from dependent rows always resolve.
func ensureUser(ctx context.Context, tx *sql.Tx, userID string) error {
	if userID == "" {
		return errors.New("mysqlstore: empty user id")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id) VALUES (?) ON DUPLICATE KEY UPDATE id = id`, userID); err != nil {
		return fmt.Errorf("mysqlstore: ensure user %s: %w", userID, err)
	}
	return nil
}

// isMySQLDuplicate reports whether err is a MySQL duplicate-key failure.
func isMySQLDuplicate(err error) bool {
	var mysqlErr *mysql.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}
