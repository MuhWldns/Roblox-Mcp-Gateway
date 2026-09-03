package bridgeapp

import (
	"bytes"
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
	request := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"RobloxBridge","version":"1.0.0"}}}`, initializeRequestID))
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
	readOnlyTool, probeArgs, err := findReadOnlyTool(toolsFrame)
	if err != nil {
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP has no schema-compatible read-only tool.", err)
	}
	argsJSON, err := json.Marshal(probeArgs)
	if err != nil {
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP readiness call failed.", err)
	}
	safeCall := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, safeCallRequestID, readOnlyTool, argsJSON))
	if err := deps.Process.Send(ctx, safeCall); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP readiness call failed.", err)
	}
	safeResult, err := receiveResponseFrame(ctx, deps.Process.Responses(), waitResult, deps.ResponseTimeout, safeCallRequestID)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errChildExited) {
			return emitReconnect(emit, deps.RetryBackoff, err)
		}
		return emitFatal(emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP readiness call failed.", err)
	}
	if err := validateSafeCallResult(safeResult); err != nil {
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
		// If both channels become ready together, prefer a child exit so a
		// dead MCP cannot be reported as a Studio-only failure.
		select {
		case childErr := <-waitResult:
			if childErr == nil {
				childErr = errors.New("MCP process exited unexpectedly")
			}
			return emitReconnect(emit, deps.RetryBackoff, childErr)
		default:
		}
		if result.err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return emitFatal(emit, "STUDIO_SESSION_UNAVAILABLE", "No Roblox Studio session is ready.", result.err)
		}
		studioCount = result.count
	}
	if err := deps.Process.CommitReadiness(func() error {
		return emit(statusui.Event{State: statusui.Connected, DeviceName: deps.DeviceName, StudioCount: studioCount}, true)
	}); err != nil {
		if errors.Is(err, mcpprocess.ErrReadinessUnavailable) {
			childErr := <-waitResult
			if childErr == nil {
				childErr = errors.New("MCP process exited unexpectedly")
			}
			return emitReconnect(emit, deps.RetryBackoff, childErr)
		}
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

func findReadOnlyTool(result json.RawMessage) (string, map[string]any, error) {
	var payload struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations struct {
				ReadOnlyHint bool `json:"readOnlyHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return "", nil, fmt.Errorf("decode tools/list result: %w", err)
	}
	for _, tool := range payload.Tools {
		if tool.Name == "" || !tool.Annotations.ReadOnlyHint || tool.InputSchema == nil {
			continue
		}
		if typ, ok := tool.InputSchema["type"].(string); !ok || typ != "object" {
			continue
		}
		unsupportedRoot := []string{"minProperties", "maxProperties", "patternProperties", "propertyNames", "dependencies", "dependentRequired", "dependentSchemas", "oneOf", "anyOf", "allOf", "not", "if", "then", "else"}
		valid := true
		for _, constraint := range unsupportedRoot {
			if _, exists := tool.InputSchema[constraint]; exists {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		properties, propertiesPresent := tool.InputSchema["properties"]
		if !propertiesPresent {
			properties = map[string]any{}
		}
		propertyMap, ok := properties.(map[string]any)
		if !ok {
			continue
		}
		args := make(map[string]any)
		for key, raw := range propertyMap {
			property, ok := raw.(map[string]any)
			if !ok {
				valid = false
				break
			}
			for _, constraint := range []string{"pattern", "minLength", "maxLength", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minItems", "maxItems", "uniqueItems", "const", "oneOf", "anyOf", "allOf", "not"} {
				if _, exists := property[constraint]; exists {
					valid = false
					break
				}
			}
			if !valid {
				break
			}
			propertyType, ok := property["type"].(string)
			if !ok {
				valid = false
				break
			}
			switch propertyType {
			case "string":
				args[key] = "RobloxBridge readiness probe"
			case "boolean":
				args[key] = false
			case "number", "integer":
				args[key] = 0
			default:
				valid = false
			}
			if enum, exists := property["enum"]; exists {
				values, ok := enum.([]any)
				if !ok || len(values) == 0 || !enumValueCompatible(values[0], propertyType) {
					valid = false
				} else {
					args[key] = values[0]
				}
			}
		}
		if !valid {
			continue
		}
		if required, exists := tool.InputSchema["required"]; exists {
			requiredValues, ok := required.([]any)
			if !ok {
				continue
			}
			for _, raw := range requiredValues {
				key, ok := raw.(string)
				if !ok {
					valid = false
					break
				}
				if _, exists := args[key]; !exists {
					valid = false
					break
				}
			}
		}
		if additional, exists := tool.InputSchema["additionalProperties"]; exists {
			additionalBool, ok := additional.(bool)
			if !ok {
				continue
			}
			if !additionalBool {
				for key := range args {
					if _, exists := propertyMap[key]; !exists {
						valid = false
					}
				}
			}
		}
		if valid {
			return tool.Name, args, nil
		}
	}
	return "", nil, errors.New("tools/list returned no schema-compatible read-only tool")
}

func validateSafeCallResult(result json.RawMessage) error {
	result = bytes.TrimSpace(result)
	if len(result) == 0 || result[0] != '{' {
		return errors.New("tools/call result must be an object")
	}
	var payload struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &payload); err != nil {
		return fmt.Errorf("decode tools/call result: %w", err)
	}
	if payload.IsError {
		return errors.New("tools/call returned an application error")
	}
	return nil
}

func enumValueCompatible(value any, propertyType string) bool {
	switch propertyType {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := value.(float64)
		return ok
	case "integer":
		n, ok := value.(float64)
		return ok && n == float64(int64(n))
	default:
		return false
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
