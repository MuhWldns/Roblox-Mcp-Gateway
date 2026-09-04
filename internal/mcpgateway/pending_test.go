package mcpgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// future returns a deadline far enough ahead that no test expires by
// accident, even under repeated -count runs.
func future() time.Time {
	return time.Now().Add(time.Hour)
}

// rpcIDOne is the JSON-RPC ID two distinct concurrent clients both use.
func rpcIDOne() json.RawMessage {
	return json.RawMessage(`1`)
}

// mustRecv waits for the single Result a registration is owed. It may only
// be called from the test goroutine.
func mustRecv(t *testing.T, ch <-chan Result) Result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a result")
		return Result{}
	}
}

// recvResult is the worker-goroutine-safe variant of mustRecv.
func recvResult(ch <-chan Result) (Result, bool) {
	select {
	case r := <-ch:
		return r, true
	case <-time.After(5 * time.Second):
		return Result{}, false
	}
}

// assertNone proves that no second Result is ever delivered.
func assertNone(t *testing.T, ch <-chan Result) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("unexpected second result: %+v", r)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestRegisterRoutesConcurrentClientsWithSameRPCID(t *testing.T) {
	p := NewPending(0)

	gwA, chA, err := p.Register("session-a", rpcIDOne(), future())
	if err != nil {
		t.Fatalf("register A: %v", err)
	}
	gwB, chB, err := p.Register("session-b", rpcIDOne(), future())
	if err != nil {
		t.Fatalf("register B: %v", err)
	}
	gwC, chC, err := p.Register("session-a", rpcIDOne(), future())
	if err != nil {
		t.Fatalf("register C: %v", err)
	}
	if gwA == gwB || gwA == gwC || gwB == gwC {
		t.Fatalf("gateway ids must be distinct: %q %q %q", gwA, gwB, gwC)
	}

	// Two concurrent clients both using JSON-RPC ID 1 receive their own
	// payload, never each other's.
	payloadA := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"client":"a"}}`)
	payloadB := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"client":"b"}}`)
	payloadC := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"client":"c"}}`)
	resolves := []struct {
		gatewayID string
		payload   json.RawMessage
	}{{gwA, payloadA}, {gwB, payloadB}, {gwC, payloadC}}

	var wg sync.WaitGroup
	for _, r := range resolves {
		wg.Add(1)
		go func(gatewayID string, payload json.RawMessage) {
			defer wg.Done()
			if err := p.Resolve(gatewayID, Result{Payload: payload}); err != nil {
				t.Errorf("resolve %s: %v", gatewayID, err)
			}
		}(r.gatewayID, r.payload)
	}
	wg.Wait()

	waiters := []struct {
		ch      <-chan Result
		payload json.RawMessage
	}{{chA, payloadA}, {chB, payloadB}, {chC, payloadC}}
	for _, w := range waiters {
		got := mustRecv(t, w.ch)
		if got.Err != nil {
			t.Fatalf("unexpected error result: %v", got.Err)
		}
		if string(got.Payload) != string(w.payload) {
			t.Fatalf("payload %s, want %s", got.Payload, w.payload)
		}
	}
}

func TestLateResponseAfterDeadlineRejected(t *testing.T) {
	p := NewPending(0)

	gw, ch, err := p.Register("session", rpcIDOne(), time.Now().Add(20*time.Millisecond))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	got := mustRecv(t, ch)
	if !errors.Is(got.Err, ErrDeadlineExceeded) {
		t.Fatalf("result error %v, want ErrDeadlineExceeded", got.Err)
	}
	if got.Payload != nil {
		t.Fatalf("deadline result must not carry a payload, got %s", got.Payload)
	}

	// The late response arrives after the registry entry was cleaned up.
	if err := p.Resolve(gw, Result{Payload: rpcIDOne()}); !errors.Is(err, ErrUnknownCorrelation) {
		t.Fatalf("late resolve error %v, want ErrUnknownCorrelation", err)
	}
	assertNone(t, ch)
}

func TestRegisterWithPastDeadlineCompletesImmediately(t *testing.T) {
	p := NewPending(0)

	gw, ch, err := p.Register("session", rpcIDOne(), time.Now().Add(-time.Second))
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	got := mustRecv(t, ch)
	if !errors.Is(got.Err, ErrDeadlineExceeded) {
		t.Fatalf("result error %v, want ErrDeadlineExceeded", got.Err)
	}
	if err := p.Resolve(gw, Result{}); !errors.Is(err, ErrUnknownCorrelation) {
		t.Fatalf("resolve after immediate expiry: %v, want ErrUnknownCorrelation", err)
	}
}

func TestDuplicateResponseRejected(t *testing.T) {
	p := NewPending(0)

	gw, ch, err := p.Register("session", rpcIDOne(), future())
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	first := Result{Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)}
	if err := p.Resolve(gw, first); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	got := mustRecv(t, ch)
	if got.Err != nil || string(got.Payload) != string(first.Payload) {
		t.Fatalf("result %+v, want payload %s without error", got, first.Payload)
	}

	// The duplicate response is rejected and re-delivers nothing.
	second := Result{Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"error":{"code":-1}}`)}
	if err := p.Resolve(gw, second); !errors.Is(err, ErrUnknownCorrelation) {
		t.Fatalf("duplicate resolve error %v, want ErrUnknownCorrelation", err)
	}
	assertNone(t, ch)
}

func TestCancelSessionDeliversCancellationOnce(t *testing.T) {
	p := NewPending(0)

	cancelled := make([]<-chan Result, 3)
	cancelledIDs := make([]string, 3)
	for i := range cancelled {
		gw, ch, err := p.Register("session", rpcIDOne(), future())
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
		cancelled[i] = ch
		cancelledIDs[i] = gw
	}
	_, chOther, err := p.Register("other-session", rpcIDOne(), future())
	if err != nil {
		t.Fatalf("register other session: %v", err)
	}

	p.CancelSession("session")

	// Every waiter of the session sees a cancellation Result.
	for i, ch := range cancelled {
		got := mustRecv(t, ch)
		if !errors.Is(got.Err, ErrCancelled) {
			t.Fatalf("waiter %d error %v, want ErrCancelled", i, got.Err)
		}
	}
	// Cancelling again re-delivers nothing: each waiter saw exactly one.
	p.CancelSession("session")
	for _, ch := range cancelled {
		assertNone(t, ch)
	}
	// The other session is untouched and still pending.
	assertNone(t, chOther)
	// The cancelled entries are gone from the registry.
	if err := p.Resolve(cancelledIDs[0], Result{}); !errors.Is(err, ErrUnknownCorrelation) {
		t.Fatalf("resolve after cancel error %v, want ErrUnknownCorrelation", err)
	}
}

func TestFailDeviceFailsEveryPendingRequestOnce(t *testing.T) {
	p := NewPending(0)
	cause := errors.New("bridge disconnected")

	failed := make([]<-chan Result, 3)
	failedIDs := make([]string, 3)
	for i := range failed {
		gw, ch, err := p.Register(fmt.Sprintf("session-%d", i), rpcIDOne(), future())
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
		if err := p.Associate(gw, "device-1"); err != nil {
			t.Fatalf("associate %d: %v", i, err)
		}
		failed[i] = ch
		failedIDs[i] = gw
	}
	gwOther, chOther, err := p.Register("session-x", rpcIDOne(), future())
	if err != nil {
		t.Fatalf("register other device: %v", err)
	}
	if err := p.Associate(gwOther, "device-2"); err != nil {
		t.Fatalf("associate other device: %v", err)
	}
	_, chUnbound, err := p.Register("session-y", rpcIDOne(), future())
	if err != nil {
		t.Fatalf("register unbound: %v", err)
	}

	p.FailDevice("device-1", cause)

	// Every waiter of the device sees the failure cause once.
	for i, ch := range failed {
		got := mustRecv(t, ch)
		if !errors.Is(got.Err, cause) {
			t.Fatalf("waiter %d error %v, want the failure cause", i, got.Err)
		}
	}
	// Failing the device again re-delivers nothing.
	p.FailDevice("device-1", cause)
	for _, ch := range failed {
		assertNone(t, ch)
	}
	// Other-device and unassociated requests are untouched.
	assertNone(t, chOther)
	assertNone(t, chUnbound)

	// A nil cause defaults to ErrDeviceFailed.
	p.FailDevice("device-2", nil)
	if got := mustRecv(t, chOther); !errors.Is(got.Err, ErrDeviceFailed) {
		t.Fatalf("nil-cause error %v, want ErrDeviceFailed", got.Err)
	}
	// The failed entries are gone from the registry.
	if err := p.Resolve(failedIDs[0], Result{}); !errors.Is(err, ErrUnknownCorrelation) {
		t.Fatalf("resolve after device failure %v, want ErrUnknownCorrelation", err)
	}
}

func TestRegisterRejectsWhenBoundExceeded(t *testing.T) {
	p := NewPending(2)

	gw1, _, err := p.Register("session-1", rpcIDOne(), future())
	if err != nil {
		t.Fatalf("register 1: %v", err)
	}
	if _, _, err := p.Register("session-2", rpcIDOne(), future()); err != nil {
		t.Fatalf("register 2: %v", err)
	}
	// The registry is full: a distinct error rejects the third request.
	if _, _, err := p.Register("session-3", rpcIDOne(), future()); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("bound exceeded error %v, want ErrTooManyPending", err)
	}

	// Completing one request removes its entry exactly once and frees a slot.
	if err := p.Resolve(gw1, Result{Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	if _, _, err := p.Register("session-3", rpcIDOne(), future()); err != nil {
		t.Fatalf("register after cleanup: %v", err)
	}

	// Deadline expiry also frees its slot.
	q := NewPending(1)
	gwShort, chShort, err := q.Register("session-q", rpcIDOne(), time.Now().Add(15*time.Millisecond))
	if err != nil {
		t.Fatalf("register short: %v", err)
	}
	if _, _, err := q.Register("session-q2", rpcIDOne(), future()); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("bound exceeded error %v, want ErrTooManyPending", err)
	}
	if got := mustRecv(t, chShort); !errors.Is(got.Err, ErrDeadlineExceeded) {
		t.Fatalf("short deadline error %v, want ErrDeadlineExceeded", got.Err)
	}
	if _, _, err := q.Register("session-q3", rpcIDOne(), future()); err != nil {
		t.Fatalf("register after deadline cleanup: %v", err)
	}
	_ = gwShort
}

func TestRegisterAndAssociateValidation(t *testing.T) {
	p := NewPending(0)

	if _, _, err := p.Register("", rpcIDOne(), future()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty session error %v, want ErrInvalidRequest", err)
	}
	if _, _, err := p.Register("session", nil, future()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil id error %v, want ErrInvalidRequest", err)
	}
	if _, _, err := p.Register("session", json.RawMessage{}, future()); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty id error %v, want ErrInvalidRequest", err)
	}

	if err := p.Associate("", "device"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty gateway error %v, want ErrInvalidRequest", err)
	}
	if err := p.Associate("gw", ""); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty device error %v, want ErrInvalidRequest", err)
	}
	if err := p.Associate("gw-missing", "device"); !errors.Is(err, ErrUnknownCorrelation) {
		t.Fatalf("unknown gateway error %v, want ErrUnknownCorrelation", err)
	}

	gw, ch, err := p.Register("session", rpcIDOne(), future())
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := p.Associate(gw, "device"); err != nil {
		t.Fatalf("associate: %v", err)
	}
	if err := p.Resolve(gw, Result{Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	mustRecv(t, ch)
	// Associating a completed request is an unknown correlation.
	if err := p.Associate(gw, "device"); !errors.Is(err, ErrUnknownCorrelation) {
		t.Fatalf("associate after completion error %v, want ErrUnknownCorrelation", err)
	}
}

func TestChurnLeavesNoRegistryEntriesOrGoroutines(t *testing.T) {
	const bound = 8
	p := NewPending(bound)

	// Let the runtime settle before measuring the goroutine baseline.
	runtime.GC()
	baseline := runtime.NumGoroutine()

	// Sequential churn: register, associate, resolve, drain.
	for i := 0; i < 400; i++ {
		gw, ch, err := p.Register(fmt.Sprintf("session-%d", i%16), rpcIDOne(), future())
		if err != nil {
			t.Fatalf("register %d: %v", i, err)
		}
		if err := p.Associate(gw, fmt.Sprintf("device-%d", i%4)); err != nil {
			t.Fatalf("associate %d: %v", i, err)
		}
		if err := p.Resolve(gw, Result{Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		mustRecv(t, ch)
	}

	// Deadline churn: requests nobody answers must still expire and clean up.
	for i := 0; i < 100; i++ {
		_, ch, err := p.Register(fmt.Sprintf("expiry-%d", i), rpcIDOne(), time.Now().Add(2*time.Millisecond))
		if err != nil {
			t.Fatalf("register expiry %d: %v", i, err)
		}
		got := mustRecv(t, ch)
		if !errors.Is(got.Err, ErrDeadlineExceeded) {
			t.Fatalf("expiry %d error %v, want ErrDeadlineExceeded", i, got.Err)
		}
	}

	// Concurrent mixed churn: resolves, session cancels, and expiries race
	// against each other. ErrTooManyPending is expected under contention.
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				session := fmt.Sprintf("churn-%d-%d", w, i%5)
				deadline := future()
				if i%3 == 2 {
					// Left unanswered so its own expiry completes it.
					deadline = time.Now().Add(3 * time.Millisecond)
				}
				gw, ch, err := p.Register(session, rpcIDOne(), deadline)
				if err != nil {
					if errors.Is(err, ErrTooManyPending) {
						continue
					}
					t.Errorf("register %d/%d: %v", w, i, err)
					return
				}
				if err := p.Associate(gw, fmt.Sprintf("device-%d", w)); err != nil {
					t.Errorf("associate %d/%d: %v", w, i, err)
					return
				}
				switch i % 3 {
				case 0:
					if err := p.Resolve(gw, Result{Payload: json.RawMessage(`{}`)}); err != nil {
						t.Errorf("resolve %d/%d: %v", w, i, err)
						return
					}
				case 1:
					p.CancelSession(session)
				}
				got, ok := recvResult(ch)
				if !ok {
					t.Errorf("waiter %d/%d got no result", w, i)
					return
				}
				switch i % 3 {
				case 0:
					if got.Err != nil {
						t.Errorf("resolved %d/%d error %v, want none", w, i, got.Err)
						return
					}
				case 1:
					if !errors.Is(got.Err, ErrCancelled) {
						t.Errorf("cancelled %d/%d error %v, want ErrCancelled", w, i, got.Err)
						return
					}
				default:
					if !errors.Is(got.Err, ErrDeadlineExceeded) {
						t.Errorf("expired %d/%d error %v, want ErrDeadlineExceeded", w, i, got.Err)
						return
					}
				}
			}
		}(w)
	}
	wg.Wait()

	// The registry must be exactly empty: every bounded slot is available
	// again, proving no churn leaked an entry.
	ids := make([]string, 0, bound)
	for i := 0; i < bound; i++ {
		gw, _, err := p.Register("drain", rpcIDOne(), future())
		if err != nil {
			t.Fatalf("slot %d not available: %v (registry leaked entries)", i, err)
		}
		ids = append(ids, gw)
	}
	if _, _, err := p.Register("drain", rpcIDOne(), future()); !errors.Is(err, ErrTooManyPending) {
		t.Fatalf("bound error %v, want ErrTooManyPending", err)
	}
	for _, gw := range ids {
		if err := p.Resolve(gw, Result{Payload: json.RawMessage(`{}`)}); err != nil {
			t.Fatalf("resolve drain %s: %v", gw, err)
		}
	}

	// Every timer fired or was stopped and every callback returned: the
	// goroutine count must settle back to the measured baseline.
	limit := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		if runtime.NumGoroutine() <= baseline+2 {
			break
		}
		if time.Now().After(limit) {
			t.Fatalf("goroutines did not settle: baseline %d, now %d", baseline, runtime.NumGoroutine())
		}
		time.Sleep(25 * time.Millisecond)
	}
}
