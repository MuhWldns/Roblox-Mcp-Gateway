package bridgeapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"robloxkit/internal/mcpprocess"
	"robloxkit/internal/statusui"
)

// LocalDeps contains the local dependencies used by RunLocal. EventSink is
// useful to callers that own rendering; when it is nil, Renderer writes to
// Output instead.
type LocalDeps struct {
	Machine         *statusui.Machine
	Process         mcpprocess.Process
	Output          io.Writer
	EventSink       func(statusui.Event) error
	StudioReady     func(context.Context) (int, error)
	DeviceName      string
	ResponseTimeout time.Duration
	RetryBackoff    time.Duration
}

const (
	initializeRequestID    = 1
	defaultResponseTimeout = 10 * time.Second
	defaultRetryBackoff    = time.Second
)

// RunLocal starts the configured MCP child, performs its local MCP
// initialization, waits for Studio readiness, and then owns the process until
// ctx is cancelled or the child exits. Connected is emitted only after both
// initialization and Studio readiness have succeeded.
func RunLocal(ctx context.Context, deps LocalDeps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if deps.Process == nil {
		return errors.New("bridgeapp: MCP process is required")
	}
	machine := deps.Machine
	if machine == nil {
		machine = statusui.NewMachine()
	}
	if deps.ResponseTimeout <= 0 {
		deps.ResponseTimeout = defaultResponseTimeout
	}
	if deps.RetryBackoff <= 0 {
		deps.RetryBackoff = defaultRetryBackoff
	}
	if deps.DeviceName == "" {
		deps.DeviceName, _ = os.Hostname()
	}
	if deps.StudioReady == nil {
		deps.StudioReady = func(context.Context) (int, error) { return 1, nil }
	}
	if deps.EventSink == nil && deps.Output == nil {
		deps.Output = io.Discard
	}

	started := false
	defer func() {
		if started {
			stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = deps.Process.Stop(stopCtx)
			cancel()
		}
	}()

	emit := func(event statusui.Event, ready bool) error {
		var err error
		if ready {
			err = machine.MarkReady(statusui.Readiness{Gateway: true, MCP: true})
		} else if event.State != statusui.Initializing {
			err = machine.Transition(event)
		}
		if err != nil {
			return err
		}
		if deps.EventSink != nil {
			return deps.EventSink(event)
		}
		return (statusui.Renderer{}).Render(deps.Output, event)
	}

	if err := emit(statusui.Event{State: statusui.Initializing}, false); err != nil {
		return err
	}
	if err := emit(statusui.Event{State: statusui.Connecting}, false); err != nil {
		return err
	}
	if err := emit(statusui.Event{State: statusui.MCPStarting}, false); err != nil {
		return err
	}
	if err := deps.Process.Start(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return emitFatal(emit, "MCP_PROCESS_UNAVAILABLE", "Official Roblox MCP could not be started.", err)
	}
	started = true

	waitResult := make(chan error, 1)
	go func() { waitResult <- deps.Process.Wait() }()

	request := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{}}`, initializeRequestID))
	if err := deps.Process.Send(ctx, request); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP initialization failed.", err)
	}
	if err := receiveResponse(ctx, deps.Process.Responses(), waitResult, deps.ResponseTimeout, initializeRequestID); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errChildExited) {
			return emitReconnect(emit, deps.RetryBackoff, err)
		}
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP initialization failed.", err)
	}
	if err := deps.Process.Send(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP initialization failed.", err)
	}

	if err := emit(statusui.Event{State: statusui.StudioDetecting}, false); err != nil {
		return err
	}
	studioCount, err := deps.StudioReady(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return emitFatal(emit, "STUDIO_SESSION_UNAVAILABLE", "No Roblox Studio session is ready.", err)
	}
	if err := emit(statusui.Event{State: statusui.Connected, DeviceName: deps.DeviceName, StudioCount: studioCount}, true); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-waitResult:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			err = errors.New("MCP process exited unexpectedly")
		}
		return emitReconnect(emit, deps.RetryBackoff, err)
	}
}

var errChildExited = errors.New("MCP process exited before initialization completed")

func receiveResponse(ctx context.Context, responses <-chan json.RawMessage, waitResult <-chan error, timeout time.Duration, expectedID int) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-waitResult:
			if err == nil {
				return errChildExited
			}
			return fmt.Errorf("%w: %v", errChildExited, err)
		case frame, ok := <-responses:
			if !ok {
				return errChildExited
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(frame, &object); err != nil {
				return fmt.Errorf("invalid MCP initialize response: %w", err)
			}
			var id int
			if err := json.Unmarshal(object["id"], &id); err != nil || id != expectedID {
				continue
			}
			if _, ok := object["error"]; ok {
				return errors.New("MCP initialize returned an error")
			}
			if _, ok := object["result"]; !ok {
				return errors.New("MCP initialize response has no result")
			}
			return nil
		case <-timer.C:
			return errors.New("timed out waiting for MCP initialize response")
		}
	}
}

func emitFatal(emit func(statusui.Event, bool) error, code, message string, cause error) error {
	event := statusui.Event{State: statusui.Fatal, Code: code, SafeMessage: message}
	if cause != nil {
		event.InternalDiagnostic = cause.Error()
	}
	if err := emit(event, false); err != nil {
		return err
	}
	return cause
}

func emitReconnect(emit func(statusui.Event, bool) error, backoff time.Duration, cause error) error {
	event := statusui.Event{State: statusui.Reconnecting, RetryAfter: backoff}
	if cause != nil {
		event.InternalDiagnostic = cause.Error()
	}
	if err := emit(event, false); err != nil {
		return err
	}
	return cause
}
