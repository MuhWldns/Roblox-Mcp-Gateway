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
	toolsListRequestID     = 2
	safeCallRequestID      = 3
	defaultResponseTimeout = 10 * time.Second
	defaultRetryBackoff    = time.Second
)

var errStudioReadinessMissing = errors.New("bridgeapp: Studio readiness dependency is required")

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
		return errStudioReadinessMissing
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
	if _, err := receiveResponseFrame(ctx, deps.Process.Responses(), waitResult, deps.ResponseTimeout, initializeRequestID); err != nil {
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

	toolsList := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":{}}`, toolsListRequestID))
	if err := deps.Process.Send(ctx, toolsList); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP initialization failed.", err)
	}
	toolsFrame, err := receiveResponseFrame(ctx, deps.Process.Responses(), waitResult, deps.ResponseTimeout, toolsListRequestID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errChildExited) {
			return emitReconnect(emit, deps.RetryBackoff, err)
		}
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP initialization failed.", err)
	}
	readOnlyTool, err := findReadOnlyTool(toolsFrame)
	if err != nil {
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP has no safe read-only tool.", err)
	}
	safeCall := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, safeCallRequestID, readOnlyTool))
	if err := deps.Process.Send(ctx, safeCall); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP readiness call failed.", err)
	}
	if _, err := receiveResponseFrame(ctx, deps.Process.Responses(), waitResult, deps.ResponseTimeout, safeCallRequestID); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errChildExited) {
			return emitReconnect(emit, deps.RetryBackoff, err)
		}
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP readiness call failed.", err)
	}

	if err := emit(statusui.Event{State: statusui.StudioDetecting}, false); err != nil {
		return err
	}
	readinessResult := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		count, err := deps.StudioReady(ctx)
		readinessResult <- struct {
			count int
			err   error
		}{count: count, err: err}
	}()
	var studioCount int
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-waitResult:
		if err == nil {
			err = errors.New("MCP process exited unexpectedly")
		}
		return emitReconnect(emit, deps.RetryBackoff, err)
	case result := <-readinessResult:
		if result.err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return emitFatal(emit, "STUDIO_SESSION_UNAVAILABLE", "No Roblox Studio session is ready.", result.err)
		}
		studioCount = result.count
	}
	// Prefer an already-completed child exit over entering Connected. This
	// closes the race where StudioReady returns just as the MCP child dies.
	select {
	case err := <-waitResult:
		if err == nil {
			err = errors.New("MCP process exited unexpectedly")
		}
		return emitReconnect(emit, deps.RetryBackoff, err)
	default:
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

func receiveResponseFrame(ctx context.Context, responses <-chan json.RawMessage, waitResult <-chan error, timeout time.Duration, expectedID int) (json.RawMessage, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-waitResult:
			if err == nil {
				return nil, errChildExited
			}
			return nil, fmt.Errorf("%w: %v", errChildExited, err)
		case frame, ok := <-responses:
			if !ok {
				return nil, errChildExited
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(frame, &object); err != nil {
				return nil, fmt.Errorf("invalid MCP response: %w", err)
			}
			var id int
			if err := json.Unmarshal(object["id"], &id); err != nil || id != expectedID {
				continue
			}
			if _, ok := object["error"]; ok {
				return nil, errors.New("MCP request returned an error")
			}
			result, ok := object["result"]
			if !ok {
				return nil, errors.New("MCP response has no result")
			}
			return result, nil
		case <-timer.C:
			return nil, errors.New("timed out waiting for MCP response")
		}
	}
}

func receiveResponse(ctx context.Context, responses <-chan json.RawMessage, waitResult <-chan error, timeout time.Duration, expectedID int) error {
	_, err := receiveResponseFrame(ctx, responses, waitResult, timeout, expectedID)
	return err
}

func findReadOnlyTool(result json.RawMessage) (string, error) {
	var payload struct {
		Tools []struct {
			Name        string `json:"name"`
			Annotations struct {
				ReadOnlyHint bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return "", fmt.Errorf("decode tools/list result: %w", err)
	}
	for _, tool := range payload.Tools {
		if tool.Name != "" && tool.Annotations.ReadOnlyHint {
			return tool.Name, nil
		}
	}
	return "", errors.New("tools/list returned no read-only tool")
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
