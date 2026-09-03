package mysqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"robloxkit/internal/session"
)

// SessionStore persists web sessions without storing their plaintext tokens.
type SessionStore struct {
	DB *sql.DB
}

func NewSessionStore(db *sql.DB) *SessionStore {
	return &SessionStore{DB: db}
}

func (s *SessionStore) check(ctx context.Context) error {
	if ctx == nil {
		return errors.New("mysqlstore: nil context")
	}
	if s == nil || s.DB == nil {
		return errors.New("mysqlstore: nil database")
	}
	return nil
}

func (s *SessionStore) Insert(ctx context.Context, sess session.Session, digest [32]byte) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO web_sessions (id,user_id,token_digest,expires_at,revoked_at,created_at,last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?)`, sess.ID, sess.UserID, digest[:], sess.ExpiresAt.UTC(), nullableTime(sess.RevokedAt), sess.CreatedAt.UTC(), nullableTimeValue(sess.LastSeenAt))
	if err != nil {
		return fmt.Errorf("mysqlstore: insert web session: %w", err)
	}
	return nil
}

// ValidateAndTouch locks the row, checks active state, and updates last_seen_at
// in one transaction. The row lock/update is the authentication linearization point.
func (s *SessionStore) ValidateAndTouch(ctx context.Context, digest [32]byte, when time.Time) (session.Session, error) {
	if err := s.check(ctx); err != nil {
		return session.Session{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return session.Session{}, fmt.Errorf("mysqlstore: begin validate: %w", err)
	}
	defer tx.Rollback()

	var out session.Session
	var revoked, lastSeen sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT id,user_id,expires_at,revoked_at,created_at,last_seen_at FROM web_sessions WHERE token_digest = ? FOR UPDATE`, digest[:]).Scan(&out.ID, &out.UserID, &out.ExpiresAt, &revoked, &out.CreatedAt, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return session.Session{}, session.ErrNotFound
	}
	if err != nil {
		return session.Session{}, fmt.Errorf("mysqlstore: find web session: %w", err)
	}
	if revoked.Valid {
		return session.Session{}, session.ErrRevoked
	}
	if !when.Before(out.ExpiresAt) {
		return session.Session{}, session.ErrExpired
	}
	if _, err := tx.ExecContext(ctx, `UPDATE web_sessions SET last_seen_at = ? WHERE id = ? AND revoked_at IS NULL AND expires_at > ?`, when.UTC(), out.ID, when.UTC()); err != nil {
		return session.Session{}, fmt.Errorf("mysqlstore: touch web session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return session.Session{}, fmt.Errorf("mysqlstore: commit validate: %w", err)
	}
	out.ExpiresAt = out.ExpiresAt.UTC()
	out.CreatedAt = out.CreatedAt.UTC()
	out.LastSeenAt = when.UTC()
	return out, nil
}

// Rotate atomically revokes one active unexpired old token and inserts its replacement.
func (s *SessionStore) Rotate(ctx context.Context, oldDigest [32]byte, next session.Session, nextDigest [32]byte, when time.Time) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysqlstore: begin rotate: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `UPDATE web_sessions SET revoked_at = ? WHERE token_digest = ? AND revoked_at IS NULL AND expires_at > ?`, when.UTC(), oldDigest[:], when.UTC())
	if err != nil {
		return fmt.Errorf("mysqlstore: revoke rotated session: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return session.ErrRevoked
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO web_sessions (id,user_id,token_digest,expires_at,revoked_at,created_at,last_seen_at) VALUES (?, ?, ?, ?, NULL, ?, ?)`, next.ID, next.UserID, nextDigest[:], next.ExpiresAt.UTC(), next.CreatedAt.UTC(), next.LastSeenAt.UTC()); err != nil {
		return fmt.Errorf("mysqlstore: insert rotated session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysqlstore: commit rotate: %w", err)
	}
	return nil
}

func (s *SessionStore) Revoke(ctx context.Context, id string, when time.Time) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE web_sessions SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, when.UTC(), id)
	if err != nil {
		return fmt.Errorf("mysqlstore: revoke web session: %w", err)
	}
	return nil
}

func (s *SessionStore) RevokeAll(ctx context.Context, userID string, when time.Time) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE web_sessions SET revoked_at = COALESCE(revoked_at, ?) WHERE user_id = ? AND revoked_at IS NULL`, when.UTC(), userID)
	if err != nil {
		return fmt.Errorf("mysqlstore: revoke all web sessions: %w", err)
	}
	return nil
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}

func nullableTimeValue(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}
