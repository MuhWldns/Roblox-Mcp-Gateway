package audit

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Store persists append-only events. Implementations must write actor, action,
// correlation, reason, target, and metadata without ever persisting secrets.
type Store interface {
	Append(ctx context.Context, event Event) error
	AppendInTx(ctx context.Context, tx *sql.Tx, event Event) error
}

// Service records append-only events through a Store.
type Service struct {
	store Store
}

// NewService constructs an audit service over a persistence store.
func NewService(store Store) *Service { return &Service{store: store} }

func (s *Service) validate(ctx context.Context, event *Event) error {
	if ctx == nil {
		return errors.New("audit: nil context")
	}
	if s == nil || s.store == nil {
		return errors.New("audit: nil store")
	}
	if event.Actor.Kind == "" {
		event.Actor.Kind = "system"
	}
	if event.Action == "" || event.CorrelationID == "" {
		return errors.New("audit: event requires action and correlation")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	return nil
}

// Record appends an event in its own transaction.
func (s *Service) Record(ctx context.Context, event Event) error {
	if err := s.validate(ctx, &event); err != nil {
		return err
	}
	return s.store.Append(ctx, event)
}

// RecordInTx appends an event inside an already-open transaction.
func (s *Service) RecordInTx(ctx context.Context, tx *sql.Tx, event Event) error {
	if tx == nil {
		return errors.New("audit: nil transaction")
	}
	if err := s.validate(ctx, &event); err != nil {
		return err
	}
	return s.store.AppendInTx(ctx, tx, event)
}
