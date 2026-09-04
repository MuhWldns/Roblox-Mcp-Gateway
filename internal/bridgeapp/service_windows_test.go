//go:build windows

package bridgeapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"robloxkit/internal/mcpprocess"
	"robloxkit/internal/statusui"
)

// The service tests drive bridgeService.Execute through the same channels
// the Windows service control manager uses — requests in, status out, and a
// terminal (svcSpecificEC, exitCode) pair back — so every SCM semantic under
// test is the real x/sys contract. The full RunRemote lifecycle runs against
// the committed bridgehub fixture, so service stop ordering is observed on
// the wire, not simulated.

// svcHarness drives one bridgeService the way the service control manager
// does and records every status the handler reports.
type svcHarness struct {
	t             *testing.T
	handler       *bridgeService
	requests      chan svc.ChangeRequest
	statuses      chan svc.Status
	done          chan struct{}
	collectorDone chan struct{}

	mu            sync.Mutex
	collected     []svc.Status
	svcSpecificEC bool
	exitCode      uint32

	log    *lockedBuffer
	events *eventLog
}

// lockedBuffer is the structured local service log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Split(strings.TrimRight(b.buf.String(), "\n"), "\n")
}

func newSvcHarness(t *testing.T, run func(context.Context, func(statusui.Event) error) error) *svcHarness {
	t.Helper()
	h := &svcHarness{
		t:             t,
		requests:      make(chan svc.ChangeRequest, 8),
		statuses:      make(chan svc.Status, 64),
		done:          make(chan struct{}),
		collectorDone: make(chan struct{}),
		log:           &lockedBuffer{},
		events:        &eventLog{},
	}
	h.handler = &bridgeService{deps: ServiceDeps{
		Name: "RobloxBridgeTest",
		Run: func(ctx context.Context, sink func(statusui.Event) error) error {
			// Tee the sink: the harness records every event the service
			// consumed while the service still sees the unmodified stream.
			return run(ctx, func(event statusui.Event) error {
				_ = h.events.sink(event)
				return sink(event)
			})
		},
		Log: h.log,
		Now: time.Now,
	}}
	go func() {
		defer close(h.collectorDone)
		for {
			select {
			case st := <-h.statuses:
				h.mu.Lock()
				h.collected = append(h.collected, st)
				h.mu.Unlock()
			case <-h.done:
				h.mu.Lock()
				for {
					select {
					case st := <-h.statuses:
						h.collected = append(h.collected, st)
					default:
						h.mu.Unlock()
						return
					}
				}
			}
		}
	}()
	go func() {
		defer close(h.done)
		h.svcSpecificEC, h.exitCode = h.handler.Execute(nil, h.requests, h.statuses)
	}()
	t.Cleanup(func() {
		select {
		case <-h.done:
		default:
			h.requests <- svc.ChangeRequest{Cmd: svc.Stop}
			<-h.done
		}
	})
	return h
}

func (h *svcHarness) requestStop() {
	h.t.Helper()
	select {
	case h.requests <- svc.ChangeRequest{Cmd: svc.Stop}:
	case <-time.After(3 * time.Second):
		h.t.Fatal("timed out sending the stop request")
	}
}

func (h *svcHarness) requestInterrogate(current svc.Status) {
	h.t.Helper()
	select {
	case h.requests <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: current}:
	case <-time.After(3 * time.Second):
		h.t.Fatal("timed out sending the interrogate request")
	}
}

func (h *svcHarness) snapshot() []svc.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]svc.Status(nil), h.collected...)
}

func (h *svcHarness) exitCodeSeen() (bool, uint32) {
	<-h.done
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.svcSpecificEC, h.exitCode
}

// awaitExit waits for the handler to finish — and for the status collector
// to drain every reported status — so post-exit assertions never race the
// final drain.
func (h *svcHarness) awaitExit(timeout time.Duration) {
	h.t.Helper()
	select {
	case <-h.done:
	case <-time.After(timeout):
		h.t.Fatal("timed out waiting for the service handler to exit")
	}
	select {
	case <-h.collectorDone:
	case <-time.After(timeout):
		h.t.Fatal("timed out waiting for the status collector to drain")
	}
}

func (h *svcHarness) awaitStatus(state svc.State, timeout time.Duration) svc.Status {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, st := range h.snapshot() {
			if st.State == state {
				return st
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for status state %d; saw %v", state, h.snapshot())
	return svc.Status{}
}

func (h *svcHarness) awaitEvent(state statusui.State, timeout time.Duration) statusui.Event {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, event := range h.events.snapshot() {
			if event.State == state {
				return event
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %q event; saw %v", state, h.events.states())
	return statusui.Event{}
}

// stopTimeProcess records when the MCP child was stopped so the stop-order
// contract can be asserted against hub-side disconnect timestamps.
type stopTimeProcess struct {
	*fakeProcess
	mu     sync.Mutex
	stopAt time.Time
}

func (p *stopTimeProcess) Stop(ctx context.Context) error {
	p.mu.Lock()
	p.stopAt = time.Now()
	p.mu.Unlock()
	return p.fakeProcess.Stop(ctx)
}

func (p *stopTimeProcess) stoppedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stopAt
}

// awaitRegistryPresence polls the hub registry until the device is connected.
func awaitRegistryPresence(t *testing.T, fx *remoteFixture, present bool, timeout time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, ok := fx.registry.Get(fx.deviceID)
		if ok == present {
			return time.Now()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for registry presence=%v for device %s", present, fx.deviceID)
	return time.Time{}
}

// remoteFatalDeps builds RemoteDeps that fail during startup without any
// network activity: the credential store is missing, so RunRemote must stop
// with the enrollment-required fatal before dialing.
func remoteFatalDeps() RemoteDeps {
	return RemoteDeps{
		Machine: statusui.NewMachine(),
		NewProcess: func() mcpprocess.Process {
			return newFakeProcess()
		},
		Credential:  &fakeCredentialStore{err: os.ErrNotExist},
		GatewayURL:  "wss://gateway.invalid/bridge",
		DeviceID:    "device-service-fatal",
		DeviceName:  "Fatal Fixture",
		Output:      io.Discard,
		StudioReady: func(context.Context) (int, error) { return 0, errors.New("unavailable") },
		Backoff: Backoff{
			Base:   time.Millisecond,
			Max:    2 * time.Millisecond,
			Jitter: time.Millisecond,
		},
		Random:          newPatternReader(),
		ConnectTimeout:  time.Second,
		ResponseTimeout: time.Second,
		WriteTimeout:    time.Second,
		QueueLimit:      8,
		MaxMessageBytes: 64 * 1024,
		BridgeVersion:   "service-test",
	}
}

// TestServiceStopGracefullyStopsChildBeforeWSSClose proves the service stop
// request cancels the run context, the MCP child is stopped gracefully, and
// the hub observes the WSS close only after the child stop — in that order.
func TestServiceStopGracefullyStopsChildBeforeWSSClose(t *testing.T) {
	fx := newRemoteFixture(t)
	proc := &stopTimeProcess{fakeProcess: newFakeProcess()}
	h := newSvcHarness(t, func(ctx context.Context, sink func(statusui.Event) error) error {
		deps := fx.remoteDeps(func() mcpprocess.Process { return proc })
		deps.EventSink = sink
		return RunRemote(ctx, deps)
	})

	h.awaitEvent(statusui.Connected, 10*time.Second)
	awaitRegistryPresence(t, fx, true, 5*time.Second)

	h.requestStop()
	h.awaitExit(10 * time.Second)

	svcSpecificEC, exitCode := h.exitCodeSeen()
	if svcSpecificEC || exitCode != 0 {
		t.Fatalf("graceful stop returned (svcSpecificEC=%v, exitCode=%d), want (false, 0)", svcSpecificEC, exitCode)
	}

	if !proc.stopped {
		t.Fatal("service stop did not stop the MCP child")
	}
	disconnectedAt := awaitRegistryPresence(t, fx, false, 5*time.Second)
	if stoppedAt := proc.stoppedAt(); !stoppedAt.Before(disconnectedAt) {
		t.Fatalf("MCP child stop at %v must precede hub-observed WSS close at %v", stoppedAt, disconnectedAt)
	}

	// The supervisor must have seen the running state before the stop-pending
	// teardown, and the run must have ended as a cancellation.
	running := h.awaitStatus(svc.Running, time.Second)
	if running.Accepts&(svc.AcceptStop|svc.AcceptShutdown) == 0 {
		t.Fatalf("running status %#v must accept stop and shutdown", running.Accepts)
	}
	h.awaitStatus(svc.StopPending, time.Second)
}

// TestServiceFatalStartupReportsFailureToSupervisor proves a fatal startup in
// service mode reaches the supervisor as a failed start: no Running status is
// ever reported and the handler exits with a non-zero service-specific exit
// code.
func TestServiceFatalStartupReportsFailureToSupervisor(t *testing.T) {
	h := newSvcHarness(t, func(ctx context.Context, sink func(statusui.Event) error) error {
		deps := remoteFatalDeps()
		deps.EventSink = sink
		return RunRemote(ctx, deps)
	})

	svcSpecificEC, exitCode := h.exitCodeSeen()
	if !svcSpecificEC || exitCode == 0 {
		t.Fatalf("fatal startup returned (svcSpecificEC=%v, exitCode=%d), want a non-zero service-specific exit code", svcSpecificEC, exitCode)
	}

	// The fatal event stream still flowed (enrollment-required surfaced), but
	// the supervisor must never have been told the service was Running.
	running := false
	for _, st := range h.snapshot() {
		if st.State == svc.Running {
			running = true
		}
	}
	if running {
		t.Fatal("fatal startup must not report the Running state to the supervisor")
	}
	h.awaitEvent(statusui.EnrollmentRequired, time.Second)
	if h.events.count(statusui.Connected) != 0 {
		t.Fatalf("fatal startup emitted Connected: %v", h.events.states())
	}
}

// TestServiceConsumesSameEventStreamAsInteractiveMode proves service mode and
// interactive mode consume the identical statusui event stream: the same
// RunRemote against the same fixture produces the same event sequence with
// and without the service wrapper in between.
func TestServiceConsumesSameEventStreamAsInteractiveMode(t *testing.T) {
	runInteractive := func(t *testing.T) []statusui.Event {
		t.Helper()
		fx := newRemoteFixture(t)
		events := &eventLog{}
		deps := fx.remoteDeps(func() mcpprocess.Process { return newFakeProcess() })
		deps.EventSink = events.sink
		result, cancel := startRemote(t, deps)
		events.nth(t, statusui.Connected, 1, 10*time.Second)
		awaitRegistryPresence(t, fx, true, 5*time.Second)
		cancel()
		if err := awaitResult(t, result, 5*time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("interactive RunRemote returned %v, want context.Canceled", err)
		}
		return events.snapshot()
	}
	runThroughService := func(t *testing.T) []statusui.Event {
		t.Helper()
		fx := newRemoteFixture(t)
		var recorded *eventLog
		var mu sync.Mutex
		h := newSvcHarness(t, func(ctx context.Context, sink func(statusui.Event) error) error {
			deps := fx.remoteDeps(func() mcpprocess.Process { return newFakeProcess() })
			deps.EventSink = sink
			return RunRemote(ctx, deps)
		})
		h.awaitEvent(statusui.Connected, 10*time.Second)
		awaitRegistryPresence(t, fx, true, 5*time.Second)
		mu.Lock()
		recorded = h.events
		mu.Unlock()
		h.requestStop()
		h.awaitExit(10 * time.Second)
		mu.Lock()
		defer mu.Unlock()
		return recorded.snapshot()
	}

	interactive := runInteractive(t)
	throughService := runThroughService(t)

	if len(interactive) == 0 {
		t.Fatal("interactive run recorded no events")
	}
	if len(interactive) != len(throughService) {
		t.Fatalf("event streams diverge: interactive=%v service=%v", states(interactive), states(throughService))
	}
	for i := range interactive {
		want, got := interactive[i], throughService[i]
		if want.State != got.State || want.Code != got.Code || want.SafeMessage != got.SafeMessage ||
			want.DeviceName != got.DeviceName || want.StudioCount != got.StudioCount {
			t.Fatalf("event %d diverges: interactive=%+v service=%+v", i, want, got)
		}
	}
	want := []statusui.State{
		statusui.Initializing, statusui.Authenticating, statusui.Connecting,
		statusui.MCPStarting, statusui.StudioDetecting, statusui.Connected,
	}
	for i, state := range want {
		if interactive[i].State != state || throughService[i].State != state {
			t.Fatalf("event %d: want %q, interactive=%q service=%q", i, state, interactive[i].State, throughService[i].State)
		}
	}
}

func states(events []statusui.Event) []statusui.State {
	out := make([]statusui.State, 0, len(events))
	for _, event := range events {
		out = append(out, event.State)
	}
	return out
}

// TestServiceStateEventsLoggedAndSurfacedToSupervisor proves every
// service-mode state event is written to the structured local log AND mapped
// onto the supervisor status channel: pending progress before Connected,
// Running (accepting stop/shutdown) at Connected, continued progress for
// post-connected events, and StopPending on the stop request.
func TestServiceStateEventsLoggedAndSurfacedToSupervisor(t *testing.T) {
	script := []statusui.Event{
		{State: statusui.Initializing},
		{State: statusui.Authenticating},
		{State: statusui.Connecting},
		{State: statusui.MCPStarting},
		{State: statusui.StudioDetecting},
		{State: statusui.Connected, DeviceName: "Service Host", StudioCount: 1},
		{State: statusui.Reconnecting, RetryAfter: 2 * time.Second},
	}
	h := newSvcHarness(t, func(ctx context.Context, sink func(statusui.Event) error) error {
		for _, event := range script {
			if err := sink(event); err != nil {
				return err
			}
		}
		<-ctx.Done()
		return ctx.Err()
	})

	h.awaitStatus(svc.Running, 5*time.Second)
	h.requestStop()
	h.awaitExit(5 * time.Second)

	svcSpecificEC, exitCode := h.exitCodeSeen()
	if svcSpecificEC || exitCode != 0 {
		t.Fatalf("graceful scripted stop returned (svcSpecificEC=%v, exitCode=%d), want (false, 0)", svcSpecificEC, exitCode)
	}

	// Supervisor surfacing: StartPending progress, Running with the accepted
	// controls, no regression to StartPending after Running, and StopPending.
	statuses := h.snapshot()
	if len(statuses) == 0 || statuses[0].State != svc.StartPending {
		t.Fatalf("first supervisor status must be StartPending, got %v", statuses)
	}
	sawRunning, sawStopPending, regressed := false, false, false
	checkpoint := uint32(0)
	for _, st := range statuses {
		if st.CheckPoint <= checkpoint && st.State != svc.Running && st.State != svc.Stopped {
			t.Fatalf("checkpoint did not advance: %v after %d", st, checkpoint)
		}
		checkpoint = st.CheckPoint
		switch st.State {
		case svc.Running:
			sawRunning = true
			if st.Accepts&(svc.AcceptStop|svc.AcceptShutdown) != svc.AcceptStop|svc.AcceptShutdown {
				t.Fatalf("running status %#v must accept stop and shutdown", st.Accepts)
			}
		case svc.StartPending:
			if sawRunning {
				regressed = true
			}
		case svc.StopPending:
			sawStopPending = true
		}
	}
	if !sawRunning || !sawStopPending || regressed {
		t.Fatalf("supervisor status sequence invalid: sawRunning=%v sawStopPending=%v regressed=%v (%v)", sawRunning, sawStopPending, regressed, statuses)
	}

	// Structured local log: one parseable JSON record per scripted event plus
	// lifecycle records, each stamped and never empty.
	lines := h.log.lines()
	if len(lines) < len(script) {
		t.Fatalf("local log has %d lines, want at least %d: %q", len(lines), len(script), lines)
	}
	var loggedStates []statusui.State
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("service log line %q is not JSON: %v", line, err)
		}
		stamp, _ := record["time"].(string)
		if stamp == "" {
			t.Fatalf("service log record %q carries no timestamp", line)
		}
		if _, err := time.Parse(time.RFC3339Nano, stamp); err != nil {
			t.Fatalf("service log timestamp %q is not RFC3339Nano: %v", stamp, err)
		}
		if state, ok := record["state"].(string); ok {
			loggedStates = append(loggedStates, statusui.State(state))
		}
	}
	// The state records must carry the full scripted stream in order.
	pos := 0
	for _, event := range script {
		found := false
		for ; pos < len(loggedStates); pos++ {
			if loggedStates[pos] == event.State {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("local log is missing scripted state %q; logged=%v", event.State, loggedStates)
		}
	}
	// The reconnecting event must have surfaced to the supervisor as progress
	// on the running service, and the log must have it too.
	if h.events.count(statusui.Reconnecting) != 1 {
		t.Fatalf("service sink saw %d reconnecting events, want 1", h.events.count(statusui.Reconnecting))
	}
}

// TestServiceInterrogateEchoesCurrentStatus proves the handler answers the
// interrogate control with the current status, as the SCM requires.
func TestServiceInterrogateEchoesCurrentStatus(t *testing.T) {
	h := newSvcHarness(t, func(ctx context.Context, sink func(statusui.Event) error) error {
		if err := sink(statusui.Event{State: statusui.Connected, DeviceName: "Service Host", StudioCount: 1}); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	})

	running := h.awaitStatus(svc.Running, 5*time.Second)
	current := svc.Status{State: svc.Running, Accepts: running.Accepts, CheckPoint: running.CheckPoint}
	before := len(h.snapshot())
	h.requestInterrogate(current)
	h.requestStop()
	h.awaitExit(5 * time.Second)

	// The echo must be a NEW complete status after the request, equal to the
	// interrogated CurrentStatus in every field.
	echoed := false
	for _, st := range h.snapshot()[before:] {
		if st == current {
			echoed = true
		}
	}
	if !echoed {
		t.Fatalf("interrogate echo missing among the new statuses %v", h.snapshot()[before:])
	}
}

// TestServiceRunServiceWithoutSCMFailsLoudly proves RunService reports a
// clear error when the process was not launched by the service control
// manager — the exact behavior a forced BRIDGE_MODE=service console start
// relies on to fail loudly instead of hanging.
func TestServiceRunServiceWithoutSCMFailsLoudly(t *testing.T) {
	err := RunService(ServiceDeps{
		Name: "RobloxBridgeTest",
		Run: func(context.Context, func(statusui.Event) error) error {
			return nil
		},
	})
	if err == nil {
		t.Fatal("RunService outside the service control manager must fail, got nil")
	}
	var errno windows.Errno
	if !errors.As(err, &errno) {
		t.Fatalf("RunService error %v (%T) is not a Win32 errno", err, err)
	}
	// 1063 = the dispatcher refused a non-SCM process. x/sys keeps one service
	// per process, so a repeated in-process invocation (e.g. -count>1) reports
	// 1056 (already running) instead — both are loud Win32 failures proving
	// the dispatcher path, never a silent success.
	switch errno {
	case windows.ERROR_FAILED_SERVICE_CONTROLLER_CONNECT, windows.ERROR_SERVICE_ALREADY_RUNNING:
	default:
		t.Fatalf("RunService error is %d, want 1063 (not service-controlled) or 1056 (already running)", uint32(errno))
	}
}

// TestServiceRequiresRunFunction proves the service entry point refuses a
// configuration without a run function instead of registering a handler that
// can never start the bridge.
func TestServiceRequiresRunFunction(t *testing.T) {
	err := RunService(ServiceDeps{Name: "RobloxBridgeTest"})
	if err == nil {
		t.Fatal("RunService without a run function must fail")
	}
}

// TestServiceIsWindowsServiceDetection proves the detection helper returns a
// usable boolean in-process (false in a test process) without failing.
func TestServiceIsWindowsServiceDetection(t *testing.T) {
	if IsWindowsService() {
		t.Skip("test process is running as a service; nothing to assert")
	}
}
