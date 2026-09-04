package mysqlstore

import (
	"context"
	"crypto/sha1"
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
	// Defense in depth: the service redacts before persistence; the store
	// redacts again at the boundary so no caller can bypass it.
	event = audit.Redact(event)
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

// UsageStore appends usage records to the append-only usage_records table.
// Idempotency is keyed on the gateway request id: the same request counted
// twice (a retried increment) writes one row.
type UsageStore struct {
	DB *sql.DB
}

// NewUsageStore builds the usage store over a verified pool.
func NewUsageStore(db *sql.DB) *UsageStore { return &UsageStore{DB: db} }

func (s *UsageStore) check(ctx context.Context) error {
	if ctx == nil {
		return errors.New("mysqlstore: nil context")
	}
	if s == nil || s.DB == nil {
		return errors.New("mysqlstore: nil usage database")
	}
	return nil
}

// usageRecordNamespace is the fixed UUIDv5 namespace usage record ids are
// derived from, making every increment of one gateway request id collide
// on the primary key instead of double-counting.
var usageRecordNamespace = [16]byte{
	0x72, 0x6f, 0x62, 0x6c, 0x6f, 0x78, 0x6b, 0x69,
	0x74, 0x2d, 0x75, 0x73, 0x61, 0x67, 0x65, 0x31,
}

// usageRecordID derives the deterministic record id for one gateway
// request id (UUIDv5 layout, SHA-1 based).
func usageRecordID(gatewayRequestID string) string {
	sum := sha1.New()
	sum.Write(usageRecordNamespace[:])
	sum.Write([]byte(gatewayRequestID))
	var digest [20]byte
	copy(digest[:], sum.Sum(nil))
	digest[6] = (digest[6] & 0x0f) | 0x50
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

// Increment appends one usage record for the gateway request. Re-incrementing
// the same gateway request id is a no-op: the deterministic record id collides
// with the existing row on the append-only primary key and the duplicate
// insert is dropped. Metadata values pass through audit redaction before
// persistence.
func (s *UsageStore) Increment(ctx context.Context, gatewayRequestID string, usage audit.Usage) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if gatewayRequestID == "" {
		return errors.New("mysqlstore: usage requires a gateway request id")
	}
	if usage.UserID == "" {
		return errors.New("mysqlstore: usage requires a user")
	}
	if usage.Operation == "" || usage.Outcome == "" {
		return errors.New("mysqlstore: usage requires an operation and outcome")
	}
	if usage.Units < 0 {
		return errors.New("mysqlstore: usage units must not be negative")
	}
	metadata, err := usageMetadataColumn(usage.Metadata)
	if err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO usage_records (id, user_id, device_id, studio_session_id, operation, outcome, units, request_id, metadata)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		usageRecordID(gatewayRequestID), usage.UserID,
		nullableString(usage.DeviceID), nullableString(usage.StudioSessionID),
		usage.Operation, usage.Outcome, usage.Units,
		nullableString(gatewayRequestID), metadata,
	); err != nil {
		// usage_records is append-only, so idempotency must never update:
		// re-incrementing the same gateway request id collides on the
		// deterministic primary key and is dropped on the floor.
		if isMySQLDuplicate(err) {
			return nil
		}
		return fmt.Errorf("mysqlstore: append usage record %q: %w", usage.Operation, err)
	}
	return nil
}

func usageMetadataColumn(values map[string]string) (any, error) {
	if len(values) == 0 {
		return nil, nil
	}
	redacted := make(map[string]string, len(values))
	for key, value := range values {
		redacted[audit.RedactString(key, 128)] = audit.RedactString(value, 512)
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: encode usage metadata: %w", err)
	}
	return encoded, nil
}
