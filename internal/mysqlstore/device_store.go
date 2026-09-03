package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"robloxkit/internal/device"
)

// DeviceStore persists enrollment codes and the browser-visible identity
// metadata of device owners. Enrollment codes are stored as keyed digests
// only and are consumed under a row lock.
type DeviceStore struct {
	DB *sql.DB
}

func NewDeviceStore(db *sql.DB) *DeviceStore {
	return &DeviceStore{DB: db}
}

func (s *DeviceStore) check(ctx context.Context) error {
	if ctx == nil {
		return errors.New("mysqlstore: nil context")
	}
	if s == nil || s.DB == nil {
		return errors.New("mysqlstore: nil database")
	}
	return nil
}

// InsertEnrollmentCode persists one digest-keyed enrollment code owned by the
// approving user.
func (s *DeviceStore) InsertEnrollmentCode(ctx context.Context, code device.EnrollmentCode) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx,
		`INSERT INTO device_enrollment_codes (id,user_id,device_id,code_digest,expires_at,consumed_at) VALUES (?, ?, NULL, ?, ?, NULL)`,
		code.ID, code.UserID, code.CodeDigest[:], code.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("mysqlstore: insert enrollment code: %w", err)
	}
	return nil
}

// ConsumeEnrollmentCode atomically marks an unconsumed, unexpired enrollment
// code as consumed and returns its owner plus Roblox identity. The row lock
// is the single-use linearization point: concurrent consumers observe either
// the fresh row or the consumed state, never both.
func (s *DeviceStore) ConsumeEnrollmentCode(ctx context.Context, digest [32]byte, now time.Time) (device.EnrollmentRecord, error) {
	if err := s.check(ctx); err != nil {
		return device.EnrollmentRecord{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return device.EnrollmentRecord{}, fmt.Errorf("mysqlstore: begin consume: %w", err)
	}
	defer tx.Rollback()

	var record device.EnrollmentRecord
	err = tx.QueryRowContext(ctx, `
SELECT c.id, c.user_id, i.id, i.provider_subject
FROM device_enrollment_codes c
JOIN user_identities i ON i.user_id = c.user_id AND i.provider = ?
WHERE c.code_digest = ? AND c.consumed_at IS NULL AND c.expires_at > ?
FOR UPDATE`, robloxIdentityProvider, digest[:], now.UTC(),
	).Scan(&record.ID, &record.UserID, &record.IdentityID, &record.ProviderSubject)
	if errors.Is(err, sql.ErrNoRows) {
		return device.EnrollmentRecord{}, s.classifyMissingCode(ctx, tx, digest, now)
	}
	if err != nil {
		return device.EnrollmentRecord{}, fmt.Errorf("mysqlstore: find enrollment code: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE device_enrollment_codes SET consumed_at = ? WHERE id = ? AND consumed_at IS NULL`,
		now.UTC(), record.ID); err != nil {
		return device.EnrollmentRecord{}, fmt.Errorf("mysqlstore: consume enrollment code: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return device.EnrollmentRecord{}, fmt.Errorf("mysqlstore: commit consume: %w", err)
	}
	return record, nil
}

func (s *DeviceStore) classifyMissingCode(ctx context.Context, tx *sql.Tx, digest [32]byte, now time.Time) error {
	var consumedAt, expiresAt sql.NullTime
	err := tx.QueryRowContext(ctx,
		`SELECT consumed_at, expires_at FROM device_enrollment_codes WHERE code_digest = ?`,
		digest[:]).Scan(&consumedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return device.ErrCodeNotFound
	}
	if err != nil {
		return fmt.Errorf("mysqlstore: inspect enrollment code: %w", err)
	}
	if consumedAt.Valid {
		return device.ErrCodeConsumed
	}
	if !now.UTC().Before(expiresAt.Time) {
		return device.ErrCodeExpired
	}
	return errors.New("mysqlstore: enrollment owner has no Roblox identity")
}

// RobloxIdentity returns the browser-visible Roblox identity metadata of an
// internal user. It never returns tokens or provider credentials.
func (s *DeviceStore) RobloxIdentity(ctx context.Context, userID string) (device.RobloxIdentity, error) {
	if err := s.check(ctx); err != nil {
		return device.RobloxIdentity{}, err
	}
	if userID == "" {
		return device.RobloxIdentity{}, errors.New("mysqlstore: empty user id")
	}
	var identity device.RobloxIdentity
	var displayName sql.NullString
	err := s.DB.QueryRowContext(ctx,
		`SELECT id, provider_subject, display_name FROM user_identities WHERE user_id = ? AND provider = ?`,
		userID, robloxIdentityProvider,
	).Scan(&identity.IdentityID, &identity.Subject, &displayName)
	if errors.Is(err, sql.ErrNoRows) {
		return device.RobloxIdentity{}, errors.New("mysqlstore: user has no Roblox identity")
	}
	if err != nil {
		return device.RobloxIdentity{}, fmt.Errorf("mysqlstore: read Roblox identity: %w", err)
	}
	identity.DisplayName = displayName.String
	return identity, nil
}
