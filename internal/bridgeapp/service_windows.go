//go:build windows

package bridgeapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/svc"

	"robloxkit/internal/statusui"
)

// ServiceName is the service control manager name the installer registers
// and cmd/bridge reports under.
const ServiceName = "RobloxBridge"

// Exit codes and wait hints reported to the service control manager.
const (
	// serviceExitOK is a clean, successful stop (Win32 NO_ERROR).
	serviceExitOK uint32 = 0
	// serviceExitFailure is the service-specific exit code reported when the
	// bridge run ended in a fatal error; the SCM marks the service FAILED.
	serviceExitFailure uint32 = 1
	// serviceStartWaitHint is the estimated start budget the supervisor sees
	// while the bridge is still starting (dial, MCP child, Studio probe).
	serviceStartWaitHint = 30 * time.Second
	// serviceStopWaitHint is the estimated stop budget: the MCP child's
	// bounded graceful stop plus the WebSocket close handshake.
	serviceStopWaitHint = 15 * time.Second
)

// ServiceDeps drives one bridge run under the Windows service control
// manager. Run is the same run function interactive mode uses (RunRemote for
// the enrolled gateway bridge), reporting lifecycle state through the sink —
// the identical statusui event stream interactive mode consumes, so both
// modes never diverge. Log receives one structured JSON line per state event
// and lifecycle transition; failures to write it are ignored (best-effort)
// and never stop the bridge.
type ServiceDeps struct {
	Name string
	Run  func(ctx context.Context, sink func(statusui.Event) error) error
	Log  io.Writer
	Now  func() time.Time
}

// IsWindowsService reports whether the process was launched by the Windows
// service control manager. A detection error reports false: an interactive
// process must never enter service mode by accident.
func IsWindowsService() bool {
	inService, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return inService
}

// RunService executes the bridge under the Windows service control manager.
// It blocks until the service stops and returns the SCM dispatch error (for
// example when the process was not launched by the service controller, so a
// forced service mode in a console fails loudly instead of hanging).
func RunService(deps ServiceDeps) error {
	if deps.Run == nil {
		return errors.New("bridgeapp: service run function is required")
	}
	name := strings.TrimSpace(deps.Name)
	if name == "" {
		name = ServiceName
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return svc.Run(name, &bridgeService{deps: deps})
}

// bridgeService is the svc.Handler mapping the bridge lifecycle onto the
// service control manager contract: StartPending with advancing checkpoints
// while starting, Running (accepting stop and shutdown) once connected,
// continued progress for post-connected events, StopPending on the stop
// request, and a non-zero service-specific exit code for a fatal run.
type bridgeService struct {
	deps ServiceDeps
}

func (s *bridgeService) Execute(args []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	log := &serviceLogger{w: s.deps.Log, now: s.deps.Now}

	// supervisor serializes every status send: the run's event sink and the
	// Execute loop (stop handling, interrogate) both report on the channel.
	var supervisor struct {
		mu         sync.Mutex
		checkpoint uint32
		running    bool
	}
	report := func(state svc.State, accepts svc.Accepted, waitHint time.Duration) {
		supervisor.mu.Lock()
		defer supervisor.mu.Unlock()
		supervisor.checkpoint++
		if state == svc.Running {
			supervisor.running = true
		}
		status <- svc.Status{
			State:      state,
			Accepts:    accepts,
			CheckPoint: supervisor.checkpoint,
			WaitHint:   uint32(waitHint / time.Millisecond),
		}
	}
	echo := func(current svc.Status) {
		supervisor.mu.Lock()
		defer supervisor.mu.Unlock()
		status <- current
	}
	isRunning := func() bool {
		supervisor.mu.Lock()
		defer supervisor.mu.Unlock()
		return supervisor.running
	}

	log.lifecycle("service_start")
	report(svc.StartPending, 0, serviceStartWaitHint)

	// The run context is deliberately detached from any request scope: the
	// service owns the bridge lifetime, and only a stop/shutdown control or
	// a fatal run error ends it.
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	runDone := make(chan error, 1)
	go func() {
		runDone <- s.deps.Run(runCtx, s.eventSink(log, report, isRunning))
	}()

	for {
		select {
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				echo(request.CurrentStatus)
			case svc.Stop, svc.Shutdown:
				log.lifecycle("stop_requested")
				cancelRun()
				report(svc.StopPending, 0, serviceStopWaitHint)
				runErr := <-runDone
				log.runExit(runErr)
				if runErr != nil && !errors.Is(runErr, context.Canceled) {
					return true, serviceExitFailure
				}
				return false, serviceExitOK
			default:
				log.unexpectedControl(request.Cmd)
			}
		case runErr := <-runDone:
			// The run ended on its own: a fatal bridge error before any stop
			// request. Report the failure to the supervisor.
			log.runExit(runErr)
			return true, serviceExitFailure
		}
	}
}

// eventSink returns the sink Run reports through: every event goes to the
// structured local log AND the service supervisor — StartPending progress
// while starting, Running at the Connected transition, continued progress
// for post-connected events, and no state change for the fatal event (the
// handler reports the failure exit when the run returns).
func (s *bridgeService) eventSink(log *serviceLogger, report func(svc.State, svc.Accepted, time.Duration), isRunning func() bool) func(statusui.Event) error {
	return func(event statusui.Event) error {
		log.event(event)
		switch {
		case event.State == statusui.Connected:
			report(svc.Running, svc.AcceptStop|svc.AcceptShutdown, 0)
		case isRunning():
			report(svc.Running, svc.AcceptStop|svc.AcceptShutdown, 0)
		case event.State != statusui.Fatal:
			report(svc.StartPending, 0, serviceStartWaitHint)
		}
		return nil
	}
}

// serviceLogger writes the structured local service log: one JSON object per
// line, timestamps in UTC, state events carrying the user-safe fields only.
// Write failures are ignored — the local log is best-effort and must never
// stop the bridge or corrupt the supervisor reporting.
type serviceLogger struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

func (l *serviceLogger) write(record map[string]any) {
	if l.w == nil {
		return
	}
	record["time"] = l.now().UTC().Format(time.RFC3339Nano)
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(append(line, '\n'))
}

// event records one statusui state event.
func (l *serviceLogger) event(event statusui.Event) {
	record := map[string]any{
		"event": "bridge_state",
		"state": string(event.State),
	}
	if event.Code != "" {
		record["code"] = event.Code
	}
	if event.SafeMessage != "" {
		record["message"] = event.SafeMessage
	}
	if event.DeviceName != "" {
		record["device_name"] = event.DeviceName
	}
	if event.StudioCount != 0 {
		record["studio_count"] = event.StudioCount
	}
	if event.RetryAfter != 0 {
		record["retry_after_ms"] = event.RetryAfter.Milliseconds()
	}
	l.write(record)
}

// lifecycle records a service lifecycle transition.
func (l *serviceLogger) lifecycle(name string) {
	l.write(map[string]any{"event": name})
}

// runExit records how the bridge run ended. The error text is operational
// diagnostics for the local log only — it never reaches the supervisor or
// the user-facing terminal renderer.
func (l *serviceLogger) runExit(err error) {
	record := map[string]any{"event": "service_exit"}
	if err != nil {
		record["error"] = err.Error()
	}
	l.write(record)
}

// unexpectedControl records an unhandled service control code.
func (l *serviceLogger) unexpectedControl(cmd svc.Cmd) {
	l.write(map[string]any{"event": "unexpected_control", "control": fmt.Sprintf("%d", cmd)})
}
