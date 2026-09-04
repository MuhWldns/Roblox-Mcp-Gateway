package bridgeapp

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"robloxkit/internal/mcpprocess"
	"robloxkit/internal/statusui"
	"robloxkit/pkg/bridgeproto"
)

// RemoteDeps contains the dependencies used by RunRemote. EventSink is
// useful to callers that own rendering; when it is nil, Renderer writes to
// Output instead. NewProcess must return a fresh MCP child per call: the
// committed mcpprocess implementation is single-use, and RunRemote creates one
// child per connection cycle so in-flight calls can never leak across a
// reconnect.
type RemoteDeps struct {
	Machine       *statusui.Machine
	NewProcess    func() mcpprocess.Process
	Credential    CredentialStore
	GatewayURL    string
	DeviceID      string
	DeviceName    string
	HTTPClient    *http.Client
	Output        io.Writer
	EventSink     func(statusui.Event) error
	StudioReady   func(context.Context) (int, error)
	BridgeVersion string

	ConnectTimeout  time.Duration
	ResponseTimeout time.Duration
	WriteTimeout    time.Duration
	QueueLimit      int
	MaxMessageBytes int
	Backoff         Backoff
	Random          io.Reader
}

// Fatal status codes reported through statusui.Event.Code.
const (
	codeCredentialRejected = "DEVICE_CREDENTIAL_REJECTED"
	codeCredentialRevoked  = "DEVICE_CREDENTIAL_REVOKED"
	codeCredentialStore    = "CREDENTIAL_STORE_UNAVAILABLE"
)

const (
	defaultRemoteConnectTimeout   = 10 * time.Second
	defaultRemoteResponseTimeout  = 10 * time.Second
	defaultRemoteWriteTimeout     = 10 * time.Second
	defaultRemoteQueueDepth       = 64
	defaultRemoteMaxEnvelopeBytes = 1 << 20
	defaultRemoteStopTimeout      = 2 * time.Second
	defaultRemoteBackoffBase      = 500 * time.Millisecond
	defaultRemoteBackoffMax       = 30 * time.Second
	defaultRemoteBackoffJitter    = 250 * time.Millisecond
)

var (
	errRemoteProcessFactoryMissing  = errors.New("bridgeapp: MCP process factory is required")
	errRemoteCredentialStoreMissing = errors.New("bridgeapp: credential store is required")
	errRemoteGatewayURLMissing      = errors.New("bridgeapp: gateway URL is required")
	errRemoteDeviceIDMissing        = errors.New("bridgeapp: device ID is required")
	errEnrollmentRequired           = errors.New("bridgeapp: device enrollment credential is missing")
)

// remoteHello is the hello payload announcing the Bridge on every connection.
type remoteHello struct {
	BridgeVersion string   `json:"bridge_version"`
	Platform      string   `json:"platform"`
	Capabilities  []string `json:"capabilities"`
}

// remoteStatusSnapshot is the full device state the gateway receives after
// every authentication, so it can render device status and route without
// guessing. All fields are user-safe.
type remoteStatusSnapshot struct {
	State            string   `json:"state"`
	DeviceName       string   `json:"device_name"`
	StudioCount      int      `json:"studio_count"`
	MCPReady         bool     `json:"mcp_ready"`
	GatewayConnected bool     `json:"gateway_connected"`
	BridgeVersion    string   `json:"bridge_version"`
	Capabilities     []string `json:"capabilities"`
}

// RunRemote connects the Bridge to the authenticated gateway WSS hub, relays
// gateway tool calls to a local MCP child, and reports lifecycle state
// through the terminal status machine.
//
// Reconnect policy: transient failures (dropped sockets, unreachable
// gateways, exited MCP children) retry forever with capped jittered
// exponential backoff; every reconnect drops all in-flight correlations, so a
// tool call is never re-executed or re-delivered (no replay) — the gateway
// fails its pending requests on disconnect instead. Terminal auth failures
// (revoked or rejected credentials) stop permanently with a fatal event.
// Cancelling ctx is a clean shutdown and returns promptly with ctx.Err.
func RunRemote(ctx context.Context, deps RemoteDeps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if deps.NewProcess == nil {
		return errRemoteProcessFactoryMissing
	}
	if deps.Credential == nil {
		return errRemoteCredentialStoreMissing
	}
	if deps.GatewayURL == "" {
		return errRemoteGatewayURLMissing
	}
	if deps.DeviceID == "" {
		return errRemoteDeviceIDMissing
	}
	if deps.StudioReady == nil {
		return errStudioReadinessMissing
	}
	machine := deps.Machine
	if machine == nil {
		machine = statusui.NewMachine()
	}
	if deps.ConnectTimeout <= 0 {
		deps.ConnectTimeout = defaultRemoteConnectTimeout
	}
	if deps.ResponseTimeout <= 0 {
		deps.ResponseTimeout = defaultRemoteResponseTimeout
	}
	if deps.WriteTimeout <= 0 {
		deps.WriteTimeout = defaultRemoteWriteTimeout
	}
	if deps.QueueLimit <= 0 {
		deps.QueueLimit = defaultRemoteQueueDepth
	}
	if deps.MaxMessageBytes <= 0 {
		deps.MaxMessageBytes = defaultRemoteMaxEnvelopeBytes
	}
	if deps.Backoff == (Backoff{}) {
		deps.Backoff = Backoff{
			Base:   defaultRemoteBackoffBase,
			Max:    defaultRemoteBackoffMax,
			Jitter: defaultRemoteBackoffJitter,
		}
	}
	if deps.Random == nil {
		deps.Random = rand.Reader
	}
	if deps.DeviceName == "" {
		deps.DeviceName, _ = os.Hostname()
	}
	if deps.BridgeVersion == "" {
		deps.BridgeVersion = "unknown"
	}
	if deps.EventSink == nil && deps.Output == nil {
		deps.Output = io.Discard
	}

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

	runner := &remoteRunner{
		deps:   deps,
		emit:   emit,
		limits: bridgeproto.Limits{MaxPayloadBytes: deps.MaxMessageBytes},
	}

	if err := emit(statusui.Event{State: statusui.Initializing}, false); err != nil {
		return err
	}

	credential, err := runner.loadCredential()
	if err != nil {
		return err
	}
	runner.credential = credential

	if err := emit(statusui.Event{State: statusui.Authenticating}, false); err != nil {
		return err
	}

	attempt := 0
	for {
		if err := emit(statusui.Event{State: statusui.Connecting}, false); err != nil {
			return err
		}
		session, dialErr := runner.dial(ctx)
		if dialErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(dialErr, errTerminalAuth) {
				return emitFatal(emit, codeCredentialRejected, "The device enrollment credential was rejected by the gateway.", dialErr)
			}
			delay := deps.Backoff.Next(attempt, deps.Random)
			attempt++
			if err := runner.emitReconnectingEvent(delay, dialErr); err != nil {
				return err
			}
			if err := sleepContext(ctx, delay); err != nil {
				return err
			}
			continue
		}
		attempt = 0

		delay, err := runner.connectedCycle(ctx, session, attempt)
		if err != nil {
			return err
		}
		attempt++
		if err := sleepContext(ctx, delay); err != nil {
			return err
		}
	}
}

// remoteRunner carries one RunRemote invocation's shared state.
type remoteRunner struct {
	deps       RemoteDeps
	emit       func(statusui.Event, bool) error
	limits     bridgeproto.Limits
	credential string
}

// loadCredential reads the device credential from the secure store. A missing
// credential means the device has never been enrolled; anything else is a
// local store failure.
func (r *remoteRunner) loadCredential() (string, error) {
	data, err := r.deps.Credential.Load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if emitErr := r.emit(statusui.Event{State: statusui.EnrollmentRequired}, false); emitErr != nil {
				return "", emitErr
			}
			return "", fmt.Errorf("%w: %v", errEnrollmentRequired, err)
		}
		return "", emitFatal(r.emit, codeCredentialStore, "The device enrollment credential could not be read.", err)
	}
	credential := strings.TrimSpace(string(data))
	if credential == "" {
		if emitErr := r.emit(statusui.Event{State: statusui.EnrollmentRequired}, false); emitErr != nil {
			return "", emitErr
		}
		return "", errEnrollmentRequired
	}
	return credential, nil
}

func (r *remoteRunner) dial(ctx context.Context) (*bridgeSession, error) {
	return dialBridge(ctx, dialConfig{
		URL:            r.deps.GatewayURL,
		Credential:     r.credential,
		DeviceID:       r.deps.DeviceID,
		HTTPClient:     r.deps.HTTPClient,
		ConnectTimeout: r.deps.ConnectTimeout,
		WriteTimeout:   r.deps.WriteTimeout,
		QueueDepth:     r.deps.QueueLimit,
		Limits:         r.limits,
	})
}

func (r *remoteRunner) helloEnvelope() bridgeproto.Envelope {
	payload, err := json.Marshal(remoteHello{
		BridgeVersion: r.deps.BridgeVersion,
		Platform:      runtime.GOOS,
		Capabilities:  []string{},
	})
	if err != nil {
		payload = json.RawMessage(`{"bridge_version":"unknown"}`)
	}
	return bridgeproto.Envelope{
		Version:  bridgeproto.Version,
		Type:     bridgeproto.TypeHello,
		DeviceID: r.deps.DeviceID,
		Payload:  payload,
	}
}

func (r *remoteRunner) statusEnvelope(studioCount int) bridgeproto.Envelope {
	payload, err := json.Marshal(remoteStatusSnapshot{
		State:            string(statusui.Connected),
		DeviceName:       r.deps.DeviceName,
		StudioCount:      studioCount,
		MCPReady:         true,
		GatewayConnected: true,
		BridgeVersion:    r.deps.BridgeVersion,
		Capabilities:     []string{},
	})
	if err != nil {
		payload = json.RawMessage(`{"state":"connected"}`)
	}
	return bridgeproto.Envelope{
		Version:  bridgeproto.Version,
		Type:     bridgeproto.TypeStatus,
		DeviceID: r.deps.DeviceID,
		Payload:  payload,
	}
}

// connectedCycle drives one connection lifecycle: hello, MCP bring-up, Studio
// detection, the connected transition with a full status snapshot, and the
// relay loop. On return the cycle is fully torn down. A nil error with a
// non-zero delay means reconnect after the delay; a non-nil error means the
// loop ended (terminal or cancelled) and RunRemote must return it.
func (r *remoteRunner) connectedCycle(ctx context.Context, session *bridgeSession, attempt int) (time.Duration, error) {
	child := r.deps.NewProcess()
	childStarted := false
	defer func() {
		if childStarted {
			stopProcess(child)
		}
		session.close(websocket.StatusNormalClosure, closeReasonLocalShutdown)
	}()

	// The hub requires hello as the first envelope on every connection.
	if err := session.enqueue(r.helloEnvelope()); err != nil {
		return r.reconnectAfterDrop(attempt, err)
	}

	if err := r.emit(statusui.Event{State: statusui.MCPStarting}, false); err != nil {
		return 0, err
	}
	if err := child.Start(ctx); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, emitFatal(r.emit, "MCP_PROCESS_UNAVAILABLE", "Official Roblox MCP could not be started.", err)
	}
	childStarted = true

	waitResult := make(chan error, 1)
	go func() { waitResult <- child.Wait() }()

	if err := r.bringUpChild(ctx, child, waitResult); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if errors.Is(err, errChildExited) {
			return r.reconnectAfterDrop(attempt, err)
		}
		return 0, emitFatal(r.emit, "MCP_INITIALIZATION_FAILED", "Official Roblox MCP initialization failed.", err)
	}

	if err := r.emit(statusui.Event{State: statusui.StudioDetecting}, false); err != nil {
		return 0, err
	}
	studioCount, err := r.probeStudio(ctx, waitResult)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if errors.Is(err, errChildExited) {
			return r.reconnectAfterDrop(attempt, err)
		}
		return 0, emitFatal(r.emit, "STUDIO_SESSION_UNAVAILABLE", "No Roblox Studio session is ready.", err)
	}

	// Connected is emitted inside CommitReadiness so the child cannot die
	// between the liveness check and the report; the full status snapshot the
	// gateway needs leaves in the same critical section.
	if err := child.CommitReadiness(func() error {
		if err := r.emit(statusui.Event{State: statusui.Connected, DeviceName: r.deps.DeviceName, StudioCount: studioCount}, true); err != nil {
			return err
		}
		if err := session.enqueue(r.statusEnvelope(studioCount)); err != nil {
			return err
		}
		return nil
	}); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if errors.Is(err, mcpprocess.ErrReadinessUnavailable) {
			childErr := <-waitResult
			if childErr == nil {
				childErr = errors.New("MCP process exited unexpectedly")
			}
			return r.reconnectAfterDrop(attempt, childErr)
		}
		if errors.Is(err, errSendQueueFull) || errors.Is(err, errSessionClosed) {
			return r.reconnectAfterDrop(attempt, err)
		}
		return 0, err
	}

	return r.relayLoop(ctx, session, child, waitResult, attempt)
}

// bringUpChild performs the local MCP handshake: initialize, the initialized
// notification, tools/list, and the schema-derived read-only safe call.
func (r *remoteRunner) bringUpChild(ctx context.Context, child mcpprocess.Process, waitResult <-chan error) error {
	responses := child.Responses()

	initialize := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"RobloxBridge","version":"1.0.0"}}}`, initializeRequestID))
	if err := child.Send(ctx, initialize); err != nil {
		return err
	}
	if _, err := receiveResponseFrame(ctx, responses, waitResult, r.deps.ResponseTimeout, initializeRequestID); err != nil {
		return err
	}
	if err := child.Send(ctx, json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		return err
	}

	toolsList := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":{}}`, toolsListRequestID))
	if err := child.Send(ctx, toolsList); err != nil {
		return err
	}
	toolsFrame, err := receiveResponseFrame(ctx, responses, waitResult, r.deps.ResponseTimeout, toolsListRequestID)
	if err != nil {
		return err
	}
	readOnlyTool, probeArgs, err := findReadOnlyTool(toolsFrame)
	if err != nil {
		return err
	}
	argsJSON, err := json.Marshal(probeArgs)
	if err != nil {
		return err
	}
	safeCall := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, safeCallRequestID, readOnlyTool, argsJSON))
	if err := child.Send(ctx, safeCall); err != nil {
		return err
	}
	safeResult, err := receiveResponseFrame(ctx, responses, waitResult, r.deps.ResponseTimeout, safeCallRequestID)
	if err != nil {
		return err
	}
	return validateSafeCallResult(safeResult)
}

// probeStudio waits for Studio readiness. A child exit during the probe wins,
// so a dead MCP is never reported as a Studio-only failure.
func (r *remoteRunner) probeStudio(ctx context.Context, waitResult <-chan error) (int, error) {
	readinessResult := make(chan struct {
		count int
		err   error
	}, 1)
	go func() {
		count, err := r.deps.StudioReady(ctx)
		readinessResult <- struct {
			count int
			err   error
		}{count: count, err: err}
	}()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case err := <-waitResult:
		if err == nil {
			err = errors.New("MCP process exited unexpectedly")
		}
		return 0, fmt.Errorf("%w: %v", errChildExited, err)
	case result := <-readinessResult:
		// If both channels are ready together, prefer a child exit so a dead
		// MCP cannot be reported as a Studio-only failure.
		select {
		case childErr := <-waitResult:
			if childErr == nil {
				childErr = errors.New("MCP process exited unexpectedly")
			}
			return 0, fmt.Errorf("%w: %v", errChildExited, childErr)
		default:
		}
		if result.err != nil {
			return 0, result.err
		}
		return result.count, nil
	}
}

// reconnectAfterDrop emits the reconnecting event with the next backoff delay
// and hands the delay back for the outer loop to sleep.
func (r *remoteRunner) reconnectAfterDrop(attempt int, cause error) (time.Duration, error) {
	delay := r.deps.Backoff.Next(attempt, r.deps.Random)
	if err := r.emitReconnectingEvent(delay, cause); err != nil {
		return 0, err
	}
	return delay, nil
}

// emitReconnectingEvent emits the reconnecting event and returns only emit
// failures. It differs from local.go's one-shot emitReconnect, which returns
// the cause on success: the remote loop must keep retrying, so the cause only
// ever becomes a diagnostic, never a return value.
func (r *remoteRunner) emitReconnectingEvent(delay time.Duration, cause error) error {
	event := statusui.Event{State: statusui.Reconnecting, RetryAfter: delay}
	if cause != nil {
		event.InternalDiagnostic = cause.Error()
	}
	return r.emit(event, false)
}

// relayLoop is the steady state: gateway requests are forwarded to the MCP
// child under bridge-local ids, child responses are routed back to their
// gateway correlations, and any session or child failure ends the cycle. All
// in-flight correlations live in the relay state, which is discarded when the
// cycle ends — a call is never resent (no replay) and a late response is never
// forwarded.
func (r *remoteRunner) relayLoop(ctx context.Context, session *bridgeSession, child mcpprocess.Process, waitResult <-chan error, attempt int) (time.Duration, error) {
	relay := newRelayState(r.deps.DeviceID, session, child)
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-session.finished():
			cause := session.terminalCause()
			if isTerminalAuthFailure(cause) {
				terminal := fmt.Errorf("%w: %v", errTerminalAuth, cause)
				return 0, emitFatal(r.emit, codeCredentialRevoked, "The device enrollment credential was revoked.", terminal)
			}
			if cause == nil {
				cause = errors.New("gateway connection closed")
			}
			return r.reconnectAfterDrop(attempt, cause)
		case err := <-waitResult:
			if err == nil {
				err = errors.New("MCP process exited unexpectedly")
			}
			return r.reconnectAfterDrop(attempt, err)
		case env := <-session.inbound:
			// Dispatch failures are either a dead session (the finished arm
			// picks it up next iteration) or a dead child (waitResult arm);
			// neither is fatal here.
			_ = relay.dispatch(ctx, env)
		case frame := <-child.Responses():
			relay.routeChildFrame(frame)
		case <-child.Diagnostics():
			// Child stderr diagnostics carry no Bridge action; drain.
		}
	}
}

// relayCall is one in-flight gateway request correlated with its bridge-local
// child request id.
type relayCall struct {
	gatewayID  string
	originalID json.RawMessage
}

// relayState correlates gateway requests with MCP child frames for one
// connection cycle. It is owned by the relay loop goroutine only.
type relayState struct {
	deviceID    string
	session     *bridgeSession
	child       mcpprocess.Process
	nextLocalID int64
	localByID   map[string]*relayCall
	byGatewayID map[string]json.RawMessage
}

func newRelayState(deviceID string, session *bridgeSession, child mcpprocess.Process) *relayState {
	return &relayState{
		deviceID:    deviceID,
		session:     session,
		child:       child,
		nextLocalID: 1,
		localByID:   make(map[string]*relayCall),
		byGatewayID: make(map[string]json.RawMessage),
	}
}

// dispatch applies one inbound gateway envelope.
func (r *relayState) dispatch(ctx context.Context, env bridgeproto.Envelope) error {
	switch env.Type {
	case bridgeproto.TypeRequest:
		return r.forwardRequest(ctx, env)
	case bridgeproto.TypeCancel:
		r.forwardCancel(ctx, env)
		return nil
	case bridgeproto.TypeNotification:
		return r.child.Send(ctx, env.Payload)
	default:
		// Status and error envelopes from the gateway carry no Bridge action.
		return nil
	}
}

// forwardRequest rewrites the payload's JSON-RPC id to a fresh bridge-local
// id before sending it to the child, so two concurrent gateway requests that
// share an original id can never collide inside the child. The original id is
// remembered per correlation and restored in the response payload. Requests
// already past their deadline are dropped: a side-effecting call must never
// execute after its window closes.
func (r *relayState) forwardRequest(ctx context.Context, env bridgeproto.Envelope) error {
	if requestExpired(env.Deadline) {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(env.Payload, &object); err != nil {
		return fmt.Errorf("bridgeapp: decode request payload: %w", err)
	}
	originalID, hasID := object["id"]
	if !hasID {
		// A JSON-RPC notification: forward verbatim, nothing to correlate.
		return r.child.Send(ctx, env.Payload)
	}
	localID := json.RawMessage(strconv.FormatInt(r.nextLocalID, 10))
	r.nextLocalID++
	object["id"] = localID
	frame, err := json.Marshal(object)
	if err != nil {
		return fmt.Errorf("bridgeapp: rebuild request frame: %w", err)
	}
	if err := r.child.Send(ctx, frame); err != nil {
		// The correlation is not registered: a dead child surfaces through
		// waitResult and the cycle restarts.
		return err
	}
	call := &relayCall{
		gatewayID:  env.GatewayRequestID,
		originalID: append(json.RawMessage(nil), originalID...),
	}
	r.localByID[frameIDKey(localID)] = call
	r.byGatewayID[env.GatewayRequestID] = localID
	return nil
}

// forwardCancel maps a gateway cancellation onto the child's bridge-local
// request id and drops the correlation, so the child's eventual response is
// discarded instead of forwarded.
func (r *relayState) forwardCancel(ctx context.Context, env bridgeproto.Envelope) {
	localID, ok := r.byGatewayID[env.GatewayRequestID]
	if !ok {
		return
	}
	delete(r.byGatewayID, env.GatewayRequestID)
	delete(r.localByID, frameIDKey(localID))
	frame := json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%s}}`, localID))
	_ = r.child.Send(ctx, frame)
}

// routeChildFrame maps one MCP child response back to its gateway
// correlation. Frames with no live correlation are late, stale, or cancelled:
// they are discarded and never resent.
func (r *relayState) routeChildFrame(frame json.RawMessage) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(frame, &object); err != nil {
		return
	}
	idRaw, ok := object["id"]
	if !ok {
		// A child notification: the Bridge has no consumer for these.
		return
	}
	call, ok := r.localByID[frameIDKey(idRaw)]
	if !ok {
		return
	}
	delete(r.localByID, frameIDKey(idRaw))
	delete(r.byGatewayID, call.gatewayID)
	object["id"] = call.originalID
	payload, err := json.Marshal(object)
	if err != nil {
		return
	}
	_ = r.session.enqueue(bridgeproto.Envelope{
		Version:          bridgeproto.Version,
		Type:             bridgeproto.TypeResponse,
		GatewayRequestID: call.gatewayID,
		DeviceID:         r.deviceID,
		Payload:          payload,
	})
}

// frameIDKey normalizes a JSON-RPC id exactly like the MCP child does, so a
// correlation key can never confuse the number 1 with the string "1".
func frameIDKey(id json.RawMessage) string {
	var text string
	if err := json.Unmarshal(id, &text); err == nil {
		return "s:" + text
	}
	return "n:" + string(id)
}

// requestExpired reports whether a gateway request's deadline already passed.
func requestExpired(deadline time.Time) bool {
	return !deadline.IsZero() && time.Now().After(deadline)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// stopProcess stops one MCP child with a bounded grace period. The stop
// context deliberately does not derive from the run context: cancelling the
// run must not truncate the child's graceful shutdown.
func stopProcess(child mcpprocess.Process) {
	if child == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultRemoteStopTimeout)
	defer cancel()
	_ = child.Stop(ctx)
}
