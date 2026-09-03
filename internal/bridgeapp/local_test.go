package bridgeapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"robloxkit/internal/mcpprocess"
	"robloxkit/internal/statusui"
)

type fakeProcess struct {
	startErr         error
	waitErr          error
	responses        chan json.RawMessage
	diags            chan mcpprocess.SafeProcessError
	started, stopped bool
	stopCh           chan struct{}
	crashCh          chan error
	mu               sync.Mutex
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{responses: make(chan json.RawMessage, 2), diags: make(chan mcpprocess.SafeProcessError, 1), stopCh: make(chan struct{}), crashCh: make(chan error, 1)}
}
func (p *fakeProcess) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
	return p.startErr
}
func (p *fakeProcess) Send(_ context.Context, frame json.RawMessage) error {
	var req map[string]any
	_ = json.Unmarshal(frame, &req)
	if req["id"] != nil {
		p.responses <- json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"test"}}`)
	}
	return nil
}
func (p *fakeProcess) Responses() <-chan json.RawMessage               { return p.responses }
func (p *fakeProcess) Diagnostics() <-chan mcpprocess.SafeProcessError { return p.diags }
func (p *fakeProcess) Stop(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.stopped {
		p.stopped = true
		close(p.stopCh)
	}
	return nil
}
func (p *fakeProcess) Wait() error {
	select {
	case err := <-p.crashCh:
		return err
	case <-p.stopCh:
		return p.waitErr
	}
}

func TestRunLocalEventOrder(t *testing.T) {
	machine := statusui.NewMachine()
	p := newFakeProcess()
	var events []statusui.Event
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := RunLocal(ctx, LocalDeps{Machine: machine, Process: p, Output: io.Discard, EventSink: func(e statusui.Event) error {
		events = append(events, e)
		if e.State == statusui.Connected {
			cancel()
		}
		return nil
	}, StudioReady: func(context.Context) (int, error) { return 1, nil }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunLocal() error = %v, want cancellation", err)
	}
	var got []string
	for _, event := range events {
		got = append(got, string(event.State))
	}
	want := []string{"initializing", "connecting", "MCP-starting", "Studio-detecting", "connected"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("states = %v, want %v", got, want)
	}
}

func TestRunLocalInitializationFailureIsFatal(t *testing.T) {
	p := newFakeProcess()
	p.startErr = errors.New("cannot start")
	var events []statusui.Event
	err := RunLocal(context.Background(), LocalDeps{Machine: statusui.NewMachine(), Process: p, Output: io.Discard, EventSink: func(e statusui.Event) error { events = append(events, e); return nil }})
	if err == nil {
		t.Fatal("RunLocal() error = nil")
	}
	if len(events) == 0 || events[len(events)-1].State != statusui.Fatal {
		t.Fatalf("last event = %v, want fatal", events)
	}
}

func TestRunLocalCancellationStopsChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := newFakeProcess()
	p.waitErr = context.Canceled
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	if err := RunLocal(ctx, LocalDeps{Machine: statusui.NewMachine(), Process: p, Output: io.Discard, StudioReady: func(context.Context) (int, error) { return 1, nil }}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if !stopped {
		t.Fatal("child was not stopped")
	}
}

func TestRunLocalChildCrashEmitsOneReconnectWithoutReplay(t *testing.T) {
	p := newFakeProcess()
	crash := errors.New("child exited with status 1")
	var events []statusui.Event
	go func() { time.Sleep(20 * time.Millisecond); p.crashCh <- crash }()
	err := RunLocal(context.Background(), LocalDeps{Machine: statusui.NewMachine(), Process: p, Output: io.Discard, RetryBackoff: 25 * time.Millisecond, EventSink: func(e statusui.Event) error { events = append(events, e); return nil }})
	if !errors.Is(err, crash) {
		t.Fatalf("RunLocal() error = %v, want child crash", err)
	}
	var reconnects int
	for _, event := range events {
		if event.State == statusui.Reconnecting {
			reconnects++
			if event.RetryAfter != 25*time.Millisecond {
				t.Fatalf("RetryAfter = %s", event.RetryAfter)
			}
		}
	}
	if reconnects != 1 {
		t.Fatalf("reconnect events = %d, want one", reconnects)
	}
	for _, event := range events {
		if event.State == statusui.Fatal {
			t.Fatal("child crash replayed as fatal")
		}
	}
}

func TestRunLocalStudioFailureNeverConnects(t *testing.T) {
	p := newFakeProcess()
	var events []statusui.Event
	err := RunLocal(context.Background(), LocalDeps{Machine: statusui.NewMachine(), Process: p, Output: io.Discard, StudioReady: func(context.Context) (int, error) { return 0, errors.New("Studio unavailable") }, EventSink: func(e statusui.Event) error { events = append(events, e); return nil }})
	if err == nil {
		t.Fatal("RunLocal() error = nil")
	}
	for _, event := range events {
		if event.State == statusui.Connected {
			t.Fatal("connected emitted before Studio readiness")
		}
	}
	if events[len(events)-1].State != statusui.Fatal {
		t.Fatalf("last event = %q, want fatal", events[len(events)-1].State)
	}
}
