package httpserver_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"robloxkit/internal/health"
	"robloxkit/internal/httpserver"
)

// recordf appends one formatted event to the shared eventLog timeline.
func (l *eventLog) recordf(format string, args ...any) {
	l.record(fmt.Sprintf(format, args...))
}

// countPrefix counts the events carrying the prefix.
func (l *eventLog) countPrefix(prefix string) int {
	n := 0
	for _, event := range l.snapshot() {
		if strings.HasPrefix(event, prefix) {
			n++
		}
	}
	return n
}

func requireOrder(t *testing.T, events []string, want ...string) {
	t.Helper()
	cursor := 0
	for _, event := range events {
		if cursor < len(want) && strings.HasPrefix(event, want[cursor]) {
			cursor++
		}
	}
	if cursor < len(want) {
		t.Fatalf("expected events in order %v, got %v", want, events)
	}
}

func waitForEventCount(t *testing.T, log *eventLog, prefix string, want int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if log.countPrefix(prefix) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("events %q reached %d, want %d within %s; events: %v",
		prefix, log.countPrefix(prefix), want, within, log.snapshot())
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// spyGate records the unready marking and snapshots the work gate state at
// that instant: readiness must close before new work is refused.
type spyGate struct {
	log  *eventLog
	work *httpserver.WorkGate
}

func (g *spyGate) MarkUnready(reason string) {
	g.log.recordf("unready draining=%v reason=%s", g.work.Draining(), reason)
}

// spyHub records the hub close and, at that instant, probes both mounted
// transports the way the router would serve them: refusals must already be
// active and the inner handlers must never run.
type spyHub struct {
	log      *eventLog
	mcpMount http.Handler
	wssMount http.Handler

	mu        sync.Mutex
	shutdowns int
}

func (h *spyHub) Shutdown() {
	h.mu.Lock()
	h.shutdowns++
	h.mu.Unlock()
	h.log.recordf("hub mcp=%d wss=%d", probeStatus(h.mcpMount), probeStatus(h.wssMount))
}

func probeStatus(mount http.Handler) int {
	if mount == nil {
		return 0
	}
	recorder := httptest.NewRecorder()
	mount.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe", nil))
	return recorder.Code
}

// spyKernel stands in for the HTTP kernel. block makes Shutdown wait for the
// context to expire, proving bounded-timeout enforcement.
type spyKernel struct {
	log   *eventLog
	block bool

	mu        sync.Mutex
	shutdowns int
}

func (k *spyKernel) ListenAndServe() error { return http.ErrServerClosed }

func (k *spyKernel) Shutdown(ctx context.Context) error {
	k.mu.Lock()
	k.shutdowns++
	k.mu.Unlock()
	k.log.record("kernel")
	if k.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

type spyPool struct {
	log *eventLog

	mu     sync.Mutex
	closes int
}

func (p *spyPool) Close() error {
	p.mu.Lock()
	p.closes++
	p.mu.Unlock()
	p.log.record("pool")
	return nil
}

// blockingHandler admits exactly one request, records it, and blocks until
// its request context is canceled — an in-flight relayed tool call.
func blockingHandler(log *eventLog, admitted, done string) http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		log.record(admitted)
		<-r.Context().Done()
		log.record(done)
	})
}

func TestServerShutdownOrder(t *testing.T) {
	log := &eventLog{}
	work := httpserver.NewWorkGate()

	mcpMount := work.MCP(blockingHandler(log, "mcp admitted", "inflight done"))
	wssMount := work.WSS(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		log.record("wss reached") // must never run once the gate is closed
	}))

	gate := &spyGate{log: log, work: work}
	hub := &spyHub{log: log, mcpMount: mcpMount, wssMount: wssMount}
	kernel := &spyKernel{log: log}
	pool := &spyPool{log: log}

	server, err := httpserver.NewServer(httpserver.ServerConfig{
		Gate: gate, Work: work, Hub: hub, Pool: pool, Kernel: kernel,
		Logger: quietLogger(), DrainWindow: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	// One in-flight MCP request the bounded drain must settle.
	go mcpMount.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	waitForEventCount(t, log, "mcp admitted", 1, 2*time.Second)

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	events := log.snapshot()
	// Readiness closes before refusals (draining=false at that instant);
	// the drain settles in-flight work before the hub closes; the hub
	// closes before the kernel; the pool closes last.
	requireOrder(t, events,
		"unready draining=false",
		"inflight done",
		"hub mcp=503 wss=503",
		"kernel",
		"pool",
	)
	firstUnready := ""
	for _, event := range events {
		if strings.HasPrefix(event, "unready") {
			firstUnready = event
			break
		}
	}
	if !strings.Contains(firstUnready, "reason=shutdown") {
		t.Fatalf("unready event = %q, want the lifecycle unready reason", firstUnready)
	}
	if n := log.countPrefix("mcp admitted"); n != 1 {
		t.Fatalf("mcp admitted count = %d, want 1 (no new work during draining)", n)
	}
	if log.countPrefix("wss reached") != 0 {
		t.Fatal("an upgrade reached the hub after the gate closed")
	}
}

func TestServerShutdownRejectsNewWorkWhileDraining(t *testing.T) {
	log := &eventLog{}
	work := httpserver.NewWorkGate()
	mcpMount := work.MCP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	wssMount := work.WSS(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	server, err := httpserver.NewServer(httpserver.ServerConfig{
		Gate: &spyGate{log: log, work: work}, Work: work,
		Hub: &spyHub{log: log}, Pool: &spyPool{log: log}, Kernel: &spyKernel{log: log},
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- server.Shutdown(context.Background()) }()

	// The instant draining begins, both mounts refuse new work with the
	// fixed sanitized 503 body.
	deadline := time.Now().Add(2 * time.Second)
	for !work.Draining() {
		if time.Now().After(deadline) {
			t.Fatal("the work gate never began draining")
		}
		time.Sleep(time.Millisecond)
	}
	for _, mount := range []http.Handler{mcpMount, wssMount} {
		recorder := httptest.NewRecorder()
		mount.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/probe", nil))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status while draining = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
		}
		const wantBody = `{"error":"server is shutting down"}`
		if got := recorder.Body.String(); got != wantBody {
			t.Fatalf("refusal body = %q, want %q", got, wantBody)
		}
	}

	if err := <-done; err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestServerShutdownBoundedByContextExpiry(t *testing.T) {
	log := &eventLog{}
	work := httpserver.NewWorkGate()

	server, err := httpserver.NewServer(httpserver.ServerConfig{
		Gate: &spyGate{log: log, work: work}, Work: work,
		Hub: &spyHub{log: log}, Pool: &spyPool{log: log},
		Kernel: &spyKernel{log: log, block: true},
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = server.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("shutdown took %s, want a bounded return", elapsed)
	}
	// Expiry surfaces an error but the remaining stages still run.
	requireOrder(t, log.snapshot(), "hub", "kernel", "pool")
}

func TestServerShutdownIsIdempotent(t *testing.T) {
	log := &eventLog{}
	work := httpserver.NewWorkGate()
	gate := &spyGate{log: log, work: work}
	hub := &spyHub{log: log}
	kernel := &spyKernel{log: log}
	pool := &spyPool{log: log}

	server, err := httpserver.NewServer(httpserver.ServerConfig{
		Gate: gate, Work: work, Hub: hub, Pool: pool, Kernel: kernel,
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("first shutdown: %v", err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}

	if kernel.shutdowns != 1 || pool.closes != 1 || hub.shutdowns != 1 || log.countPrefix("unready") != 1 {
		t.Fatalf("shutdown stages re-ran: kernel=%d pool=%d hub=%d unready=%d",
			kernel.shutdowns, pool.closes, hub.shutdowns, log.countPrefix("unready"))
	}
}

func TestServerShutdownFailsEveryPendingCallExactlyOnce(t *testing.T) {
	log := &eventLog{}
	work := httpserver.NewWorkGate()

	const pending = 3
	for i := 0; i < pending; i++ {
		mount := work.MCP(blockingHandler(log, "pending admitted", "pending failed"))
		go mount.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/mcp", nil))
	}
	waitForEventCount(t, log, "pending admitted", pending, 2*time.Second)

	server, err := httpserver.NewServer(httpserver.ServerConfig{
		Gate: &spyGate{log: log, work: work}, Work: work,
		Hub: &spyHub{log: log}, Pool: &spyPool{log: log}, Kernel: &spyKernel{log: log},
		Logger: quietLogger(), DrainWindow: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if got := log.countPrefix("pending failed"); got != pending {
		t.Fatalf("failed pending calls = %d, want %d (drain fan-out missed a waiter)", got, pending)
	}
	if got := log.countPrefix("pending admitted"); got != pending {
		t.Fatalf("admitted pending calls = %d, want %d", got, pending)
	}
	// Every failed waiter settled before the hub closed.
	requireOrder(t, log.snapshot(), "pending failed", "hub", "kernel", "pool")
}

// listenerKernel serves a real HTTP server over an existing listener so the
// lifecycle contract is exercised over an actual socket.
type listenerKernel struct {
	ln net.Listener
	hs *http.Server
}

func (k *listenerKernel) ListenAndServe() error { return k.hs.Serve(k.ln) }

func (k *listenerKernel) Shutdown(ctx context.Context) error { return k.hs.Shutdown(ctx) }

func TestServerServesHTTPAndStopsOnShutdown(t *testing.T) {
	log := &eventLog{}
	work := httpserver.NewWorkGate()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	kernel := &listenerKernel{
		ln: listener,
		hs: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})},
	}
	server, err := httpserver.NewServer(httpserver.ServerConfig{
		Gate: &spyGate{log: log, work: work}, Work: work,
		Hub: &spyHub{log: log}, Pool: &spyPool{log: log}, Kernel: kernel,
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServe() }()

	res, err := http.Get("http://" + listener.Addr().String() + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("served status = %d, want %d", res.StatusCode, http.StatusOK)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("listenAndServe error = %v, want http.ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return after Shutdown")
	}
}

func TestServerConfigValidation(t *testing.T) {
	log := &eventLog{}
	work := httpserver.NewWorkGate()
	gate := &spyGate{log: log, work: work}
	kernel := &spyKernel{log: log}

	if _, err := httpserver.NewServer(httpserver.ServerConfig{Work: work, Kernel: kernel}); !errors.Is(err, httpserver.ErrInvalidConfig) {
		t.Fatalf("missing gate error = %v, want ErrInvalidConfig", err)
	}
	if _, err := httpserver.NewServer(httpserver.ServerConfig{Gate: gate, Kernel: kernel}); !errors.Is(err, httpserver.ErrInvalidConfig) {
		t.Fatalf("missing work gate error = %v, want ErrInvalidConfig", err)
	}
	if _, err := httpserver.NewServer(httpserver.ServerConfig{Gate: gate, Work: work}); !errors.Is(err, httpserver.ErrInvalidConfig) {
		t.Fatalf("missing handler error = %v, want ErrInvalidConfig", err)
	}
	if _, err := httpserver.NewServer(httpserver.ServerConfig{
		Gate: gate, Work: work, Handler: http.NotFoundHandler(), DrainWindow: -time.Second,
	}); !errors.Is(err, httpserver.ErrInvalidConfig) {
		t.Fatalf("negative drain window error = %v, want ErrInvalidConfig", err)
	}
}

// okChecker counts its pings so the tests can prove the unready gate answers
// without touching the dependency.
type okChecker struct {
	mu    sync.Mutex
	pings int
}

func (c *okChecker) PingContext(context.Context) error {
	c.mu.Lock()
	c.pings++
	c.mu.Unlock()
	return nil
}

func (c *okChecker) pingCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pings
}

// leakingChecker fails with an error carrying data that must never reach a
// probe response.
type leakingChecker struct{}

func (leakingChecker) PingContext(context.Context) error {
	return errors.New("dial tcp 10.0.0.9:3306: connection refused (dsn root:hunter2@tcp(10.0.0.9:3306)/prod)")
}

func assertProbe(t *testing.T, handler http.HandlerFunc, wantStatus int, wantBody string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/probe", nil))
	if recorder.Code != wantStatus {
		t.Fatalf("probe status = %d, want %d", recorder.Code, wantStatus)
	}
	body := recorder.Body.String()
	if body != wantBody {
		t.Fatalf("probe body = %q, want %q", body, wantBody)
	}
	// The fixed bodies never carry a DSN, driver diagnostic, user count, or
	// device identifier.
	for _, forbidden := range []string{"3306", "hunter2", "root@", "device", "user", "count"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("probe body leaked %q: %s", forbidden, body)
		}
	}
}

func TestServerHealthAnswersUnreadyImmediatelyWithoutLeaking(t *testing.T) {
	checker := &okChecker{}
	gate := health.NewGate(checker, quietLogger())
	handler := health.NewHandler(gate, log.New(io.Discard, "", 0))

	assertProbe(t, handler.Ready, http.StatusOK, `{"status":"ok"}`)

	gate.MarkUnready("shutdown")

	assertProbe(t, handler.Ready, http.StatusServiceUnavailable, `{"status":"unavailable"}`)
	if checker.pingCount() != 1 {
		t.Fatalf("readiness pings = %d, want 1 (once unready, the gate answers without pinging)", checker.pingCount())
	}

	// Liveness stays independent of the readiness gate.
	assertProbe(t, handler.Live, http.StatusOK, `{"status":"ok"}`)
}

func TestServerHealthFailureStaysFixedWhileOpen(t *testing.T) {
	gate := health.NewGate(leakingChecker{}, quietLogger())
	handler := health.NewHandler(gate, log.New(io.Discard, "", 0))

	assertProbe(t, handler.Ready, http.StatusServiceUnavailable, `{"status":"unavailable"}`)
}
