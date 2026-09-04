package httpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ReadinessGate marks lifecycle readiness. *health.Gate implements it.
type ReadinessGate interface {
	MarkUnready(reason string)
}

// HTTPKernel is the HTTP server surface the lifecycle controller drives.
// *http.Server implements it.
type HTTPKernel interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

const (
	// DefaultDrainWindow bounds the shutdown drain when ServerConfig
	// leaves it unset: long enough for in-flight relayed tool calls to
	// land, short enough that a wedged client cannot stall process exit.
	DefaultDrainWindow = 10 * time.Second

	// failSettleWindow bounds how long failed pending work may unwind
	// before the hub-close stage proceeds; a handler that ignores its
	// canceled context cannot stall the shutdown past this.
	failSettleWindow = 2 * time.Second
)

// HubCloser closes the Bridge WebSocket hub. *bridgehub.Hub implements it.
type HubCloser interface {
	Shutdown()
}

// PoolCloser closes the MySQL connection pool. *sql.DB implements it.
type PoolCloser interface {
	Close() error
}

// shuttingDownBody is the fixed sanitized refusal both gated mounts answer
// with while draining. It carries no internal detail.
const shuttingDownBody = `{"error":"server is shutting down"}`

// workCall is one admitted MCP request tracked for the drain.
type workCall struct {
	cancel context.CancelFunc
}

// WorkGate gates the MCP and Bridge mounts against shutdown. New MCP
// requests are admitted and context-tracked so shutdown can drain them;
// new WSS upgrades are admitted untracked, because live Bridge connections
// end at the hub-close stage, not the drain stage. After Close, both mounts
// refuse new work with the fixed sanitized 503. All methods are safe for
// concurrent use.
type WorkGate struct {
	mu     sync.Mutex
	closed bool
	calls  map[*workCall]struct{}
	wake   chan struct{} // closed and replaced whenever a tracked request finishes
}

// NewWorkGate builds an open gate admitting all work.
func NewWorkGate() *WorkGate {
	return &WorkGate{
		calls: make(map[*workCall]struct{}),
		wake:  make(chan struct{}),
	}
}

// MCP wraps the /mcp mount: once the gate is closed, new requests are
// refused with the sanitized 503; admitted requests are tracked so the
// bounded drain can wait for them and then fail the stragglers.
func (g *WorkGate) MCP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		call := &workCall{cancel: cancel}
		if !g.admit(call) {
			writeShuttingDown(w)
			return
		}
		defer g.done(call)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// WSS wraps the Bridge mount: once the gate is closed, new upgrades are
// refused with the sanitized 503 before authentication and the hub is never
// reached.
func (g *WorkGate) WSS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		closed := g.closed
		g.mu.Unlock()
		if closed {
			writeShuttingDown(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Close refuses all new MCP and WSS work. Already-admitted requests keep
// running until the drain settles them.
func (g *WorkGate) Close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// Draining reports whether Close has run.
func (g *WorkGate) Draining() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// InFlight reports the number of tracked MCP requests.
func (g *WorkGate) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.calls)
}

// Drain waits until every tracked request finished or ctx expires; it
// returns the number of requests still in flight when the wait ended.
func (g *WorkGate) Drain(ctx context.Context) int {
	for {
		g.mu.Lock()
		remaining := len(g.calls)
		if remaining == 0 {
			g.mu.Unlock()
			return 0
		}
		wake := g.wake
		g.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			g.mu.Lock()
			defer g.mu.Unlock()
			return len(g.calls)
		}
	}
}

// FailPending cancels the context of every tracked request, failing each
// pending relay call exactly once — the correlation registry's per-session
// cancel semantics observed from the transport side. Repeat calls deliver
// nothing new: canceled contexts stay canceled.
func (g *WorkGate) FailPending() {
	g.mu.Lock()
	calls := make([]*workCall, 0, len(g.calls))
	for call := range g.calls {
		calls = append(calls, call)
	}
	g.mu.Unlock()
	for _, call := range calls {
		call.cancel()
	}
}

func (g *WorkGate) admit(call *workCall) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.calls[call] = struct{}{}
	return true
}

func (g *WorkGate) done(call *workCall) {
	g.mu.Lock()
	delete(g.calls, call)
	close(g.wake)
	g.wake = make(chan struct{})
	g.mu.Unlock()
}

// writeShuttingDown answers the fixed sanitized 503. Status and body are
// identical for every mount, so the refusal never carries internal detail.
func writeShuttingDown(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(shuttingDownBody))
}

// ServerConfig wires the lifecycle controller.
type ServerConfig struct {
	// Addr is the listen address for the built-in kernel.
	Addr string
	// Handler is the composed router; required unless Kernel is injected.
	Handler http.Handler
	// ReadTimeout and WriteTimeout configure the built-in kernel.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// Gate marks lifecycle readiness; required.
	Gate ReadinessGate
	// Work gates the MCP and Bridge mounts; required.
	Work *WorkGate
	// Hub closes the Bridge WebSocket hub; nil skips the stage.
	Hub HubCloser
	// Pool closes the MySQL pool last; nil skips the stage.
	Pool PoolCloser
	// Logger receives structured lifecycle events; nil disables logging.
	Logger *slog.Logger
	// DrainWindow bounds the drain stage; zero selects DefaultDrainWindow
	// and negative values are rejected.
	DrainWindow time.Duration
	// Kernel replaces the built-in *http.Server so tests can inject spies.
	Kernel HTTPKernel
}

// Server drives one gateway process: ListenAndServe serves the composed
// router, and Shutdown tears the process down in the committed order —
// readiness first, then refusals of new MCP/WSS work, then the bounded
// drain, then the Bridge hub, then the HTTP kernel, and the MySQL pool last.
type Server struct {
	kernel       HTTPKernel
	handler      http.Handler
	addr         string
	readTimeout  time.Duration
	writeTimeout time.Duration
	gate         ReadinessGate
	work         *WorkGate
	hub          HubCloser
	pool         PoolCloser
	logger       *slog.Logger
	drainWindow  time.Duration

	once sync.Once
	err  error
}

// NewServer validates the configuration and builds the lifecycle controller.
func NewServer(cfg ServerConfig) (*Server, error) {
	var invalid []string
	if cfg.Gate == nil {
		invalid = append(invalid, "readiness gate is required")
	}
	if cfg.Work == nil {
		invalid = append(invalid, "work gate is required")
	}
	if cfg.Kernel == nil && cfg.Handler == nil {
		invalid = append(invalid, "handler is required")
	}
	if cfg.DrainWindow < 0 {
		invalid = append(invalid, "drain window must not be negative")
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalidConfig, strings.Join(invalid, "; "))
	}
	drainWindow := cfg.DrainWindow
	if drainWindow == 0 {
		drainWindow = DefaultDrainWindow
	}
	kernel := cfg.Kernel
	if kernel == nil {
		kernel = &http.Server{
			Addr:         cfg.Addr,
			Handler:      cfg.Handler,
			ReadTimeout:  cfg.ReadTimeout,
			WriteTimeout: cfg.WriteTimeout,
		}
	}
	return &Server{
		kernel:       kernel,
		handler:      cfg.Handler,
		addr:         cfg.Addr,
		readTimeout:  cfg.ReadTimeout,
		writeTimeout: cfg.WriteTimeout,
		gate:         cfg.Gate,
		work:         cfg.Work,
		hub:          cfg.Hub,
		pool:         cfg.Pool,
		logger:       cfg.Logger,
		drainWindow:  drainWindow,
	}, nil
}

// ListenAndServe serves the router until Shutdown runs. The kernel's error,
// including http.ErrServerClosed, passes through for the caller to filter.
func (s *Server) ListenAndServe() error {
	s.logInfo("server listening", "addr", s.addr)
	return s.kernel.ListenAndServe()
}

// Shutdown executes the committed teardown order exactly once; later calls
// return the first shutdown's outcome without re-running any stage.
func (s *Server) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.once.Do(func() { s.err = s.shutdown(ctx) })
	return s.err
}

func (s *Server) shutdown(ctx context.Context) error {
	s.logInfo("shutdown started")

	// (1) Readiness first: the gate answers probes unavailable
	// immediately, so the load balancer stops routing before any
	// user-visible work is refused. The gate emits its own structured
	// closure event.
	s.gate.MarkUnready("shutdown")

	// (2) Refuse new MCP and WSS work with the sanitized 503.
	s.work.Close()
	s.logInfo("new MCP and WSS work refused")

	// (3) Drain in-flight MCP work within the bounded window, then fail
	// every pending relay call: each waiter observes exactly one failure.
	// The cancellation is given a bounded moment to unwind, so no waiter
	// outlives this stage and the hub-close stage starts from a quiet
	// transport.
	drainCtx := ctx
	drainCancel := context.CancelFunc(func() {})
	if s.drainWindow > 0 {
		drainCtx, drainCancel = context.WithTimeout(ctx, s.drainWindow)
	}
	defer drainCancel()
	if remaining := s.work.Drain(drainCtx); remaining > 0 {
		s.work.FailPending()
		settleCtx, settleCancel := context.WithTimeout(ctx, failSettleWindow)
		defer settleCancel()
		if still := s.work.Drain(settleCtx); still > 0 {
			s.logWarn("pending MCP work did not unwind", "remaining", still)
		}
		s.logInfo("pending MCP work failed", "remaining", remaining)
	} else {
		s.logInfo("MCP work drained")
	}

	// (4) Close the Bridge hub: every live connection receives close 1000.
	if s.hub != nil {
		s.hub.Shutdown()
		s.logInfo("bridge hub closed")
	}

	// (5) Close the HTTP kernel with the caller's bounded context: the
	// listeners close immediately and an expired context surfaces its
	// error instead of hanging.
	kernelErr := s.kernel.Shutdown(ctx)
	if kernelErr != nil {
		s.logWarn("http server shutdown", "error", kernelErr.Error())
	} else {
		s.logInfo("http server closed")
	}

	// (6) MySQL last: nothing queries the pool after it closes.
	var poolErr error
	if s.pool != nil {
		poolErr = s.pool.Close()
		if poolErr != nil {
			s.logWarn("mysql pool close", "error", poolErr.Error())
		} else {
			s.logInfo("mysql pool closed")
		}
	}

	err := kernelErr
	if err == nil {
		err = poolErr
	}
	if err == nil {
		// The budget ran out mid-teardown; surface the expiry per the
		// bounded-shutdown contract.
		err = ctx.Err()
	}
	if err != nil {
		s.logWarn("shutdown finished with error", "error", err.Error())
	} else {
		s.logInfo("shutdown complete")
	}
	return err
}

func (s *Server) logInfo(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Info(msg, args...)
	}
}

func (s *Server) logWarn(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, args...)
	}
}
