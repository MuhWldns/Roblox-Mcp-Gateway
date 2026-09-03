package bridgeapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	sent             chan json.RawMessage
	strictProtocol   bool
	applicationError bool
	waitReturned     chan struct{}
	mu               sync.Mutex
}

func newFakeProcess() *fakeProcess {
	return &fakeProcess{
		responses:    make(chan json.RawMessage, 8),
		diags:        make(chan mcpprocess.SafeProcessError, 1),
		stopCh:       make(chan struct{}),
		crashCh:      make(chan error, 1),
		sent:         make(chan json.RawMessage, 8),
		waitReturned: make(chan struct{}),
	}
}
func (p *fakeProcess) Start(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started = true
	return p.startErr
}
func (p *fakeProcess) Send(_ context.Context, frame json.RawMessage) error {
	p.sent <- append(json.RawMessage(nil), frame...)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(frame, &req)
	if p.strictProtocol && req.Method == "initialize" {
		var params struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ClientInfo      map[string]any `json:"clientInfo"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if params.ProtocolVersion == "" || params.Capabilities == nil || params.ClientInfo == nil {
			return errors.New("invalid initialize params")
		}
	}
	if p.strictProtocol && req.Method == "tools/call" {
		var params struct {
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if _, ok := params.Arguments["text"]; !ok {
			p.responses <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32602,"message":"text required"}}`, req.ID))
			return nil
		}
	}
	if len(req.ID) != 0 {
		result := json.RawMessage(`{"ok":true}`)
		switch req.Method {
		case "initialize":
			result = json.RawMessage(`{"protocolVersion":"test"}`)
		case "tools/list":
			result = json.RawMessage(`{"tools":[{"name":"echo","annotations":{"readOnlyHint":true},"inputSchema":{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}}}]}`)
		case "tools/call":
			if p.applicationError {
				result = json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"readiness failed"}]}`)
			} else {
				result = json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`)
			}
		}
		p.responses <- json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, req.ID, result))
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
	defer close(p.waitReturned)
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
	err := RunLocal(context.Background(), LocalDeps{Machine: statusui.NewMachine(), Process: p, Output: io.Discard, StudioReady: func(context.Context) (int, error) { return 1, nil }, EventSink: func(e statusui.Event) error { events = append(events, e); return nil }})
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
	err := RunLocal(context.Background(), LocalDeps{Machine: statusui.NewMachine(), Process: p, Output: io.Discard, RetryBackoff: 25 * time.Millisecond, StudioReady: func(context.Context) (int, error) { return 1, nil }, EventSink: func(e statusui.Event) error { events = append(events, e); return nil }})
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

func TestRunLocalMissingStudioReadinessCannotConnect(t *testing.T) {
	p := newFakeProcess()
	var events []statusui.Event
	result := make(chan error, 1)
	go func() {
		result <- RunLocal(context.Background(), LocalDeps{
			Machine:   statusui.NewMachine(),
			Process:   p,
			Output:    io.Discard,
			EventSink: func(e statusui.Event) error { events = append(events, e); return nil },
		})
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("RunLocal() error = nil, want missing Studio readiness dependency")
		}
	case <-time.After(time.Second):
		t.Fatal("RunLocal blocked despite missing Studio readiness dependency")
	}
	for _, event := range events {
		if event.State == statusui.Connected {
			t.Fatal("connected emitted without Studio readiness dependency")
		}
	}
}

func TestRunLocalRequiresToolsListAndSafeReadOnlyCall(t *testing.T) {
	p := newFakeProcess()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := RunLocal(ctx, LocalDeps{
		Machine: statusui.NewMachine(),
		Process: p,
		Output:  io.Discard,
		StudioReady: func(context.Context) (int, error) {
			return 1, nil
		},
		EventSink: func(e statusui.Event) error {
			if e.State == statusui.Connected {
				cancel()
			}
			return nil
		},
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunLocal() error = %v, want cancellation", err)
	}
	var methods []string
	for len(methods) < 4 {
		select {
		case frame := <-p.sent:
			var req struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(frame, &req); err != nil {
				t.Fatalf("decode sent frame: %v", err)
			}
			methods = append(methods, req.Method)
		case <-time.After(time.Second):
			t.Fatalf("sent methods = %v, want initialize, initialized, tools/list, tools/call", methods)
		}
	}
	want := []string{"initialize", "notifications/initialized", "tools/list", "tools/call"}
	for i := range want {
		if methods[i] != want[i] {
			t.Fatalf("sent methods = %v, want %v", methods, want)
		}
	}
}
func TestRunLocalStrictHandshakeUsesValidParamsAndSafeArguments(t *testing.T) {
	p := newFakeProcess()
	p.strictProtocol = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := RunLocal(ctx, LocalDeps{Machine: statusui.NewMachine(), Process: p, Output: io.Discard, StudioReady: func(context.Context) (int, error) { return 1, nil }, EventSink: func(e statusui.Event) error {
		if e.State == statusui.Connected {
			cancel()
		}
		return nil
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunLocal() error = %v, want cancellation", err)
	}
	var initializeParams, callParams map[string]any
	for len(p.sent) > 0 {
		frame := <-p.sent
		var req map[string]any
		if err := json.Unmarshal(frame, &req); err != nil {
			t.Fatal(err)
		}
		switch req["method"] {
		case "initialize":
			initializeParams, _ = req["params"].(map[string]any)
		case "tools/call":
			callParams, _ = req["params"].(map[string]any)
		}
	}
	if initializeParams == nil || initializeParams["protocolVersion"] == nil || initializeParams["capabilities"] == nil || initializeParams["clientInfo"] == nil {
		t.Fatalf("invalid initialize params: %v", initializeParams)
	}
	args, _ := callParams["arguments"].(map[string]any)
	if args["text"] != "RobloxBridge readiness probe" {
		t.Fatalf("safe call args = %v, want deterministic readiness text", args)
	}
}

func TestRunLocalStudioErrorYieldsToChildExit(t *testing.T) {
	p := newFakeProcess()
	crash := errors.New("child exited before Studio error")
	readyStarted := make(chan struct{})
	readyRelease := make(chan struct{})
	result := make(chan error, 1)
	var eventsMu sync.Mutex
	var events []statusui.Event
	go func() {
		result <- RunLocal(context.Background(), LocalDeps{Machine: statusui.NewMachine(), Process: p, Output: io.Discard, StudioReady: func(context.Context) (int, error) {
			close(readyStarted)
			<-readyRelease
			return 0, errors.New("Studio unavailable")
		}, EventSink: func(e statusui.Event) error {
			eventsMu.Lock()
			events = append(events, e)
			eventsMu.Unlock()
			return nil
		}})
	}()
	<-readyStarted
	p.crashCh <- crash
	select {
	case err := <-result:
		if !errors.Is(err, crash) {
			t.Fatalf("error = %v, want child crash", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunLocal did not prioritize child exit")
	}
	close(readyRelease)
	eventsMu.Lock()
	defer eventsMu.Unlock()
	for _, event := range events {
		if event.State == statusui.Fatal || event.State == statusui.Connected {
			t.Fatalf("event %q emitted after child exit", event.State)
		}
	}
}

func TestRunLocalRejectsApplicationLevelSafeCallError(t *testing.T) {
	p := newFakeProcess()
	p.applicationError = true
	var events []statusui.Event
	err := RunLocal(context.Background(), LocalDeps{Machine: statusui.NewMachine(), Process: p, Output: io.Discard, StudioReady: func(context.Context) (int, error) { return 1, nil }, EventSink: func(e statusui.Event) error { events = append(events, e); return nil }})
	if err == nil {
		t.Fatal("RunLocal() error = nil, want application-level safe-call failure")
	}
	for _, event := range events {
		if event.State == statusui.Connected {
			t.Fatal("connected emitted for application-level safe-call error")
		}
	}
}

func TestRunLocalChildExitDuringStudioReadinessCannotConnect(t *testing.T) {
	p := newFakeProcess()
	crash := errors.New("child exited during Studio readiness")
	readyStarted := make(chan struct{})
	readyRelease := make(chan struct{})
	result := make(chan error, 1)
	var eventsMu sync.Mutex
	var events []statusui.Event
	go func() {
		result <- RunLocal(context.Background(), LocalDeps{
			Machine:      statusui.NewMachine(),
			Process:      p,
			Output:       io.Discard,
			RetryBackoff: 10 * time.Millisecond,
			StudioReady: func(ctx context.Context) (int, error) {
				close(readyStarted)
				select {
				case <-readyRelease:
					return 1, nil
				case <-ctx.Done():
					return 0, ctx.Err()
				}
			},
			EventSink: func(e statusui.Event) error {
				eventsMu.Lock()
				events = append(events, e)
				eventsMu.Unlock()
				return nil
			},
		})
	}()
	<-readyStarted
	p.crashCh <- crash
	select {
	case err := <-result:
		if !errors.Is(err, crash) {
			t.Fatalf("RunLocal() error = %v, want child crash", err)
		}
	case <-time.After(time.Second):
		close(readyRelease)
		t.Fatal("RunLocal blocked in Studio readiness after child exit")
	}
	close(readyRelease)
	eventsMu.Lock()
	defer eventsMu.Unlock()
	for _, event := range events {
		if event.State == statusui.Connected {
			t.Fatal("connected emitted after MCP child exited during Studio readiness")
		}
	}
}
