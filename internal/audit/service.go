package audit

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// Store persists append-only events. Implementations must write actor, action,
// correlation, reason, target, and metadata without ever persisting secrets.
type Store interface {
	Append(ctx context.Context, event Event) error
	AppendInTx(ctx context.Context, tx *sql.Tx, event Event) error
}

// Service records append-only events through a Store. Every event is
// redacted before it reaches the store, so a caller that embeds a credential
// in a free-form field cannot leak it into persistence or any log sink.
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

// Record appends an event in its own transaction. The event is redacted
// before persistence.
func (s *Service) Record(ctx context.Context, event Event) error {
	if err := s.validate(ctx, &event); err != nil {
		return err
	}
	return s.store.Append(ctx, Redact(event))
}

// RecordInTx appends an event inside an already-open transaction. The event
// is redacted before persistence.
func (s *Service) RecordInTx(ctx context.Context, tx *sql.Tx, event Event) error {
	if tx == nil {
		return errors.New("audit: nil transaction")
	}
	if err := s.validate(ctx, &event); err != nil {
		return err
	}
	return s.store.AppendInTx(ctx, tx, Redact(event))
}

// Queue buffers best-effort audit events — the success path — in a bounded
// in-memory queue and persists them from a background drain. Enqueueing
// never blocks: when the queue is full the event is dropped and counted, so
// accounting pressure can never stall the request that produced it.
//
// Denials must not use the queue: they stay synchronous so their events are
// durable before the response is written.
type Queue struct {
	service  *Service
	capacity int

	mu       sync.Mutex
	pending  []Event
	dropped  int64
	inflight int
	wake     chan struct{}
}

// NewQueue builds a bounded queue over the service. The capacity bounds the
// buffered events; zero or negative is rejected.
func NewQueue(service *Service, capacity int) (*Queue, error) {
	if service == nil || service.store == nil {
		return nil, errors.New("audit: queue requires a service with a store")
	}
	if capacity <= 0 {
		return nil, errors.New("audit: queue requires a positive capacity")
	}
	return &Queue{
		service:  service,
		capacity: capacity,
		wake:     make(chan struct{}, 1),
	}, nil
}

// Record enqueues one event, dropping it when the queue is full. Drops are
// counted and never block the caller.
func (q *Queue) Record(event Event) {
	if q == nil {
		return
	}
	q.mu.Lock()
	if len(q.pending) >= q.capacity {
		q.dropped++
		q.mu.Unlock()
		return
	}
	q.pending = append(q.pending, event)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Dropped reports how many events were discarded because the queue was full.
func (q *Queue) Dropped() int64 {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// Pending reports how many events are buffered but not yet persisted.
func (q *Queue) Pending() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending) + q.inflight
}

// Serve drains the queue until ctx is done. Owners run it in one background
// goroutine; Record stays non-blocking regardless of persistence latency.
func (q *Queue) Serve(ctx context.Context) {
	if q == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		}
		for q.drainOnce(ctx) {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}
}

// Flush drains every buffered event synchronously and waits until the
// records have been handed to the service. It exists for tests and for
// bounded shutdown draining.
func (q *Queue) Flush(ctx context.Context) {
	if q == nil {
		return
	}
	for {
		q.drainOnce(ctx)
		q.mu.Lock()
		done := len(q.pending) == 0 && q.inflight == 0
		q.mu.Unlock()
		if done {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Millisecond):
		}
	}
}

// drainOnce moves the current batch out of the queue and records each event.
// It reports whether any event was drained.
func (q *Queue) drainOnce(ctx context.Context) bool {
	q.mu.Lock()
	if len(q.pending) == 0 {
		q.mu.Unlock()
		return false
	}
	events := q.pending
	q.pending = nil
	q.inflight += len(events)
	q.mu.Unlock()

	for _, event := range events {
		// Best-effort by contract: a failed success audit must not crash
		// or retry-storm; the drop counter covers queue pressure and the
		// event is lost with the batch.
		_ = q.service.Record(ctx, event)
	}
	q.mu.Lock()
	q.inflight -= len(events)
	q.mu.Unlock()
	return true
}
