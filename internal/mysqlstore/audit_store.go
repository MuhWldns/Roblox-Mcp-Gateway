package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"robloxkit/internal/audit"
)

// AuditStore persists audit.Event rows to the append-only admin_actions and
// audit_logs tables.
type AuditStore struct {
	DB *sql.DB
}

// NewAuditStore builds the audit store over a verified pool.
func NewAuditStore(db *sql.DB) *AuditStore { return &AuditStore{DB: db} }

func (s *AuditStore) check(ctx context.Context) error {
	if ctx == nil {
		return errors.New("mysqlstore: nil context")
	}
	if s == nil || s.DB == nil {
		return errors.New("mysqlstore: nil audit database")
	}
	return nil
}

// Append writes one event in its own transaction. An audit append never fails
// because of a missing user: user references absent from users are stored as
// NULL.
func (s *AuditStore) Append(ctx context.Context, event audit.Event) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysqlstore: begin audit append: %w", err)
	}
	defer tx.Rollback()
	if err := s.AppendInTx(ctx, tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysqlstore: commit audit append: %w", err)
	}
	return nil
}

// AppendInTx writes one event inside an already-open transaction.
func (s *AuditStore) AppendInTx(ctx context.Context, tx *sql.Tx, event audit.Event) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if tx == nil {
		return errors.New("mysqlstore: nil audit transaction")
	}
	return appendAuditEvent(ctx, tx, event)
}

func appendAuditEvent(ctx context.Context, tx *sql.Tx, event audit.Event) error {
	id, err := identityUUID()
	if err != nil {
		return fmt.Errorf("mysqlstore: generate audit id: %w", err)
	}
	createdAt := event.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	actorID, err := auditUserColumn(ctx, tx, event.Actor.UserID)
	if err != nil {
		return err
	}
	if event.Actor.Kind == audit.ActorAdmin {
		before, after, err := auditStateColumns(event)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO admin_actions (id, actor_user_id, action, correlation_id, reason, target_type, target_id, before_state, after_state, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, actorID, event.Action, event.CorrelationID,
			nullableString(event.Reason), nullableString(event.TargetType), nullableString(event.TargetID),
			before, after, createdAt,
		); err != nil {
			return fmt.Errorf("mysqlstore: append admin action %q: %w", event.Action, err)
		}
		return nil
	}
	userID, err := auditUserColumn(ctx, tx, event.UserID)
	if err != nil {
		return err
	}
	metadata, err := auditMetadataColumn(event)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_logs (id, user_id, actor_user_id, action, correlation_id, reason, target_type, target_id, metadata, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, actorID, event.Action, event.CorrelationID,
		nullableString(event.Reason), nullableString(event.TargetType), nullableString(event.TargetID),
		metadata, createdAt,
	); err != nil {
		return fmt.Errorf("mysqlstore: append audit log %q: %w", event.Action, err)
	}
	return nil
}

// auditUserColumn resolves a user reference to a foreign-key-safe column
// value: NULL for empty or unknown ids so an audit append never fails on a
// missing user row.
func auditUserColumn(ctx context.Context, tx *sql.Tx, userID string) (any, error) {
	if userID == "" {
		return nil, nil
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id = ?`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: resolve audit user %q: %w", userID, err)
	}
	return id, nil
}

// auditStateColumns encodes the before/after maps for admin_actions. Empty
// maps are stored as NULL.
func auditStateColumns(event audit.Event) (before, after any, err error) {
	if before, err = encodeStringMap(event.Before); err != nil {
		return nil, nil, err
	}
	if after, err = encodeStringMap(event.After); err != nil {
		return nil, nil, err
	}
	return before, after, nil
}

func encodeStringMap(value map[string]string) (any, error) {
	if len(value) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: encode audit state: %w", err)
	}
	return encoded, nil
}

// auditMetadataColumn packs before/after into the audit_logs metadata JSON
// column; both maps empty are stored as NULL.
func auditMetadataColumn(event audit.Event) (any, error) {
	if len(event.Before) == 0 && len(event.After) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(struct {
		Before map[string]string `json:"before"`
		After  map[string]string `json:"after"`
	}{
		Before: event.Before,
		After:  event.After,
	})
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: encode audit metadata: %w", err)
	}
	return encoded, nil
}
