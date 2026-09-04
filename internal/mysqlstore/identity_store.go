package mysqlstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"

	"robloxkit/internal/robloxauth"
)

const robloxIdentityProvider = "roblox"

// IdentityStore transactionally binds a Roblox subject to exactly one
// internal user. Provider names are metadata and never participate in lookup.
type IdentityStore struct {
	DB *sql.DB
}

func NewIdentityStore(db *sql.DB) *IdentityStore {
	return &IdentityStore{DB: db}
}

func (s *IdentityStore) UpsertRobloxIdentity(ctx context.Context, identity robloxauth.RobloxIdentity) (robloxauth.User, error) {
	if ctx == nil {
		return robloxauth.User{}, errors.New("mysqlstore: nil context")
	}
	subject := identity.Subject
	if subject == "" {
		return robloxauth.User{}, errors.New("mysqlstore: empty Roblox subject")
	}
	if s == nil || s.DB == nil {
		return robloxauth.User{}, errors.New("mysqlstore: nil database")
	}
	candidateUserID, err := identityUUID()
	if err != nil {
		return robloxauth.User{}, fmt.Errorf("mysqlstore: generate user id: %w", err)
	}
	candidateIdentityID, err := identityUUID()
	if err != nil {
		return robloxauth.User{}, fmt.Errorf("mysqlstore: generate identity id: %w", err)
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return robloxauth.User{}, fmt.Errorf("mysqlstore: begin identity upsert: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users (id) VALUES (?)`, candidateUserID); err != nil {
		return robloxauth.User{}, fmt.Errorf("mysqlstore: create identity user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO user_identities (id,user_id,provider,provider_subject,display_name) VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE display_name = VALUES(display_name)`, candidateIdentityID, candidateUserID, robloxIdentityProvider, subject, nullableString(identity.DisplayName)); err != nil {
		return robloxauth.User{}, fmt.Errorf("mysqlstore: upsert Roblox identity: %w", err)
	}

	var out robloxauth.User
	var displayName sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT u.id, i.id, i.provider_subject, i.display_name FROM user_identities i JOIN users u ON u.id = i.user_id WHERE i.provider = ? AND i.provider_subject = ? FOR UPDATE`, robloxIdentityProvider, subject).Scan(&out.ID, &out.IdentityID, &out.RobloxSubject, &displayName); err != nil {
		return robloxauth.User{}, fmt.Errorf("mysqlstore: read Roblox identity: %w", err)
	}
	out.DisplayName = displayName.String
	if out.ID != candidateUserID {
		if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, candidateUserID); err != nil {
			return robloxauth.User{}, fmt.Errorf("mysqlstore: discard collision user: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return robloxauth.User{}, fmt.Errorf("mysqlstore: commit identity upsert: %w", err)
	}
	return out, nil
}

func identityUUID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
