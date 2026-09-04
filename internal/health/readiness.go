package health

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// ErrGateClosed is the fixed, secret-free sentinel the gate reports once it
// has been marked unready. Its text is safe for server logs; probe responses
// stay the fixed unavailable document.
var ErrGateClosed = errors.New("health: readiness gate is closed")

// Gate is the lifecycle readiness gate wrapped around a Checker. While open
// it delegates to the wrapped checker (the MySQL pool ping); once MarkUnready
// runs it fails closed immediately with the fixed sentinel, so every
// readiness probe answers 503 without touching a dependency. This is step
// one of the committed shutdown order: the load balancer stops routing
// before any user-visible work is refused. All methods are safe for
// concurrent use.
type Gate struct {
	mu     sync.RWMutex
	ready  Checker
	closed bool
	reason string
	logger *slog.Logger
}

// NewGate builds the gate around ready; a nil Checker reports ready forever.
// logger receives the one structured closure event; nil defaults to
// slog.Default.
func NewGate(ready Checker, logger *slog.Logger) *Gate {
	if logger == nil {
		logger = slog.Default()
	}
	return &Gate{ready: ready, logger: logger}
}

// PingContext implements Checker: a closed gate fails with ErrGateClosed and
// never pings; an open gate delegates to the wrapped checker.
func (g *Gate) PingContext(ctx context.Context) error {
	g.mu.RLock()
	closed, reason, ready := g.closed, g.reason, g.ready
	g.mu.RUnlock()
	if closed {
		return fmt.Errorf("%w: %s", ErrGateClosed, reason)
	}
	if ready == nil {
		return nil
	}
	return ready.PingContext(ctx)
}

// MarkUnready closes the gate: every subsequent readiness probe answers
// unavailable immediately. Idempotent — the first reason wins and later
// calls change nothing.
func (g *Gate) MarkUnready(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	if reason == "" {
		reason = "unspecified"
	}
	g.closed = true
	g.reason = reason
	g.logger.Info("readiness gate closed", "reason", reason)
}

// Ready reports whether the gate currently admits traffic.
func (g *Gate) Ready() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return !g.closed
}
