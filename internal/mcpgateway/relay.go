package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"robloxkit/internal/bridgehub"
	"robloxkit/internal/mcpoauth"
	"robloxkit/internal/routing"
	"robloxkit/pkg/bridgeproto"
)

// Relay classification errors. They are internal sentinels: tools.go maps
// every one to a fixed sanitized JSON-RPC error; the sentinel text never
// reaches a connector. Session cancellation and device-connection failure
// reuse the committed correlation sentinels ErrCancelled and ErrDeviceFailed,
// which are the same outcomes at every layer.
var (
	// ErrDeviceGone reports that the grant's device has no ready Bridge
	// target: it is offline, reports no Studio, or failed delivery.
	ErrDeviceGone = errors.New("mcpgateway: target device is offline")
	// ErrStudioUnavailable reports that the grant's bound Studio is not
	// online on the target device.
	ErrStudioUnavailable = errors.New("mcpgateway: target Studio is offline")
	// ErrAmbiguousTarget reports several online Studios and no bound or
	// requested Studio to pick from.
	ErrAmbiguousTarget = errors.New("mcpgateway: target Studio is ambiguous")
	// ErrDeadline reports that the relay timeout elapsed before the device
	// answered. It classifies the committed ErrDeadlineExceeded result.
	ErrDeadline = errors.New("mcpgateway: request timed out")
	// ErrBusy reports the correlation registry is at capacity.
	ErrBusy = errors.New("mcpgateway: pending request capacity exceeded")
	// ErrInvalidResponse reports a device response that does not match the
	// correlated request.
	ErrInvalidResponse = errors.New("mcpgateway: invalid relay response")
)

const defaultMaxEnvelopeBytes = 1 << 20

// RelayConfig builds a relay over the hub's live device registry.
type RelayConfig struct {
	// Registry is the hub's live connection registry.
	Registry *bridgehub.Registry
	// Pending correlates relayed requests with device responses.
	Pending *Pending
	// Timeout bounds one relayed request before its deadline fires.
	Timeout time.Duration
	// MaxEnvelopeBytes bounds relayed envelopes; zero selects the default.
	MaxEnvelopeBytes int
}

// Relay bridges SDK method calls to the grant target's Bridge connection:
// it resolves the Studio target, registers the request in the correlation
// registry, delivers it, and relays the correlated response back. Cancellations
// and timeouts are pushed to the device as cancel envelopes.
type Relay struct {
	registry *bridgehub.Registry
	pending  *Pending
	timeout  time.Duration
	limits   bridgeproto.Limits

	ids atomic.Int64

	statusMu sync.Mutex
	statuses map[string]deviceSnapshot
}

// deviceSnapshot is the last reported Bridge status of one device, per the
// device contract: readiness plus the count of open Studios.
type deviceSnapshot struct {
	Ready       bool
	StudioCount int
}

// NewRelay validates the configuration and builds the relay.
func NewRelay(cfg RelayConfig) (*Relay, error) {
	if cfg.Registry == nil {
		return nil, errors.New("mcpgateway: relay requires a registry")
	}
	if cfg.Pending == nil {
		return nil, errors.New("mcpgateway: relay requires a pending registry")
	}
	if cfg.Timeout <= 0 {
		return nil, errors.New("mcpgateway: relay requires a positive timeout")
	}
	if cfg.MaxEnvelopeBytes <= 0 {
		cfg.MaxEnvelopeBytes = defaultMaxEnvelopeBytes
	}
	return &Relay{
		registry: cfg.Registry,
		pending:  cfg.Pending,
		timeout:  cfg.Timeout,
		limits:   bridgeproto.Limits{MaxPayloadBytes: cfg.MaxEnvelopeBytes},
		statuses: make(map[string]deviceSnapshot),
	}, nil
}

// HandleEnvelope consumes one validated inbound device envelope: responses
// resolve their correlated request; status snapshots update the device's
// routing state. Unknown correlations — late or duplicate responses — are
// dropped. The signature matches bridgehub's OnEnvelope hook.
func (r *Relay) HandleEnvelope(_ context.Context, device bridgehub.Device, env bridgeproto.Envelope) {
	if r == nil {
		return
	}
	switch env.Type {
	case bridgeproto.TypeResponse:
		// Late and duplicate responses surface as unknown correlations;
		// dropping them is the correct outcome.
		_ = r.pending.Resolve(env.GatewayRequestID, Result{Payload: env.Payload})
	case bridgeproto.TypeStatus:
		r.applyStatus(device.DeviceID, env.Payload)
	}
}

// CancelSession completes every pending request of an MCP session, pushing
// cancel envelopes to their devices. Connector revocation calls this to
// tear down live sessions' in-flight work.
func (r *Relay) CancelSession(sessionID string) {
	if r == nil {
		return
	}
	r.pending.CancelSession(sessionID)
}

// CallTrace reports where one relayed call was delivered. The gateway
// request id keys usage accounting; the device and studio identify the
// resolved target.
type CallTrace struct {
	GatewayRequestID string
	DeviceID         string
	StudioID         string
}

// Call relays one JSON-RPC request to the grant's resolved target and
// returns the device's raw JSON-RPC response payload. It blocks until the
// response arrives, the timeout elapses, or the caller cancels; timeout
// and cancellation also deliver a cancel envelope to the device so Studio
// stops working immediately.
func (r *Relay) Call(ctx context.Context, sessionID string, grant mcpoauth.Grant, method string, params json.RawMessage) (json.RawMessage, error) {
	payload, _, err := r.CallDetailed(ctx, sessionID, grant, method, params)
	return payload, err
}

// CallDetailed relays one JSON-RPC request and additionally reports the
// trace of where it was delivered. The correlation rules are Call's.
func (r *Relay) CallDetailed(ctx context.Context, sessionID string, grant mcpoauth.Grant, method string, params json.RawMessage) (json.RawMessage, CallTrace, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sessionID == "" {
		return nil, CallTrace{}, fmt.Errorf("%w: session is required", ErrInvalidRequest)
	}
	target, err := r.resolveTarget(grant)
	if err != nil {
		return nil, CallTrace{}, err
	}

	requestID := r.nextRequestID()
	payload, err := json.Marshal(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      requestID,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return nil, CallTrace{}, fmt.Errorf("%w: encode request", ErrInvalidResponse)
	}
	deadline := time.Now().Add(r.timeout)

	gatewayID, results, err := r.pending.Register(sessionID, requestID, deadline)
	if err != nil {
		if errors.Is(err, ErrTooManyPending) {
			return nil, CallTrace{}, ErrBusy
		}
		return nil, CallTrace{}, fmt.Errorf("%w: register request", ErrInvalidResponse)
	}
	if err := r.pending.Associate(gatewayID, target.DeviceID); err != nil {
		// Associate before delivery is the contract; a failure here means
		// the request already completed, which cannot happen on a fresh id.
		_ = r.pending.Resolve(gatewayID, Result{Err: ErrInvalidResponse})
		return nil, CallTrace{}, fmt.Errorf("%w: associate request", ErrInvalidResponse)
	}

	// Retire the correlation when the live connection ends, so device
	// disconnects fail their in-flight requests exactly once.
	conn, online := r.registry.Get(target.DeviceID)
	if !online {
		_ = r.pending.Resolve(gatewayID, Result{Err: ErrDeviceGone})
		return nil, CallTrace{}, ErrDeviceGone
	}
	watcherDone := make(chan struct{})
	go func() {
		select {
		case <-conn.Done():
			r.pending.FailDevice(target.DeviceID, ErrDeviceFailed)
		case <-watcherDone:
		}
	}()
	defer close(watcherDone)

	if err := r.registry.Send(ctx, target.DeviceID, bridgeproto.Envelope{
		Version:          bridgeproto.Version,
		Type:             bridgeproto.TypeRequest,
		GatewayRequestID: gatewayID,
		DeviceID:         target.DeviceID,
		StudioID:         target.StudioID,
		Deadline:         deadline,
		Payload:          payload,
	}); err != nil {
		_ = r.pending.Resolve(gatewayID, Result{Err: ErrDeviceGone})
		return nil, CallTrace{}, ErrDeviceGone
	}

	select {
	case result := <-results:
		if result.Err != nil {
			// The device is still working: push a cancel for bounded
			// outcomes so Studio aborts promptly.
			if errors.Is(result.Err, ErrDeadlineExceeded) || errors.Is(result.Err, ErrCancelled) {
				r.sendCancel(target.DeviceID, gatewayID)
			}
			return nil, CallTrace{GatewayRequestID: gatewayID, DeviceID: target.DeviceID, StudioID: target.StudioID}, classifyResult(result.Err)
		}
		if err := validateRelayResponse(result.Payload, requestID); err != nil {
			return nil, CallTrace{GatewayRequestID: gatewayID, DeviceID: target.DeviceID, StudioID: target.StudioID}, err
		}
		return result.Payload, CallTrace{GatewayRequestID: gatewayID, DeviceID: target.DeviceID, StudioID: target.StudioID}, nil
	case <-ctx.Done():
		r.sendCancel(target.DeviceID, gatewayID)
		return nil, CallTrace{GatewayRequestID: gatewayID, DeviceID: target.DeviceID, StudioID: target.StudioID}, ErrCancelled
	}
}

// validateRelayResponse checks that the device's response answers the
// correlated request: JSON-RPC 2.0 carrying the exact relayed request id,
// with either a result or a structured error.
func validateRelayResponse(payload json.RawMessage, requestID json.RawMessage) error {
	var response struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return fmt.Errorf("%w: decode response", ErrInvalidResponse)
	}
	var want, got any
	if err := json.Unmarshal(requestID, &want); err != nil {
		return ErrInvalidResponse
	}
	if err := json.Unmarshal(response.ID, &got); err != nil {
		return ErrInvalidResponse
	}
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("%w: response id does not match the request", ErrInvalidResponse)
	}
	if len(response.Result) == 0 && len(response.Error) == 0 {
		return fmt.Errorf("%w: response carries neither result nor error", ErrInvalidResponse)
	}
	return nil
}

// jsonrpcRequest is the relayed JSON-RPC request envelope.
type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// nextRequestID mints a relay-scoped unique JSON-RPC id. Correlation rides
// the envelope's gateway_request_id; this id only keeps the device's
// response payload self-consistent.
func (r *Relay) nextRequestID() json.RawMessage {
	return json.RawMessage(strconv.FormatInt(r.ids.Add(1), 10))
}

// classifyResult maps a Pending outcome to its relay sentinel.
func classifyResult(err error) error {
	switch {
	case errors.Is(err, ErrDeadlineExceeded):
		return ErrDeadline
	case errors.Is(err, ErrCancelled):
		return ErrCancelled
	case errors.Is(err, ErrDeviceFailed):
		return ErrDeviceFailed
	default:
		return fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
}

// sendCancel best-effort delivers a cancel envelope so the device aborts
// the abandoned request. Delivery never blocks and failures are ignored:
// the correlation is already retired locally.
func (r *Relay) sendCancel(deviceID, gatewayID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = r.registry.Send(ctx, deviceID, bridgeproto.Envelope{
		Version:          bridgeproto.Version,
		Type:             bridgeproto.TypeCancel,
		GatewayRequestID: gatewayID,
		DeviceID:         deviceID,
	})
}

// resolveTarget picks the delivery target through the committed routing
// policy, using the device's last status snapshot: a Studio-bound grant
// must find its bound Studio ready; an unbound grant resolves the device's
// sole open Studio; several open Studios without a binding are ambiguous.
func (r *Relay) resolveTarget(grant mcpoauth.Grant) (routing.ResolvedTarget, error) {
	if grant.DeviceID == "" {
		return routing.ResolvedTarget{}, fmt.Errorf("%w: grant has no device", ErrDeviceGone)
	}
	snapshot := r.snapshot(grant.DeviceID)
	if !snapshot.Ready || snapshot.StudioCount == 0 {
		return routing.ResolvedTarget{}, ErrDeviceGone
	}

	var online []routing.Studio
	switch {
	case grant.StudioSessionID != "":
		// The device performs the exact session match; the gateway trusts
		// readiness and routes the bound Studio.
		online = []routing.Studio{{StudioID: grant.StudioSessionID, DeviceID: grant.DeviceID}}
	case snapshot.StudioCount == 1:
		online = []routing.Studio{{StudioID: "", DeviceID: grant.DeviceID}}
	default:
		// Multiple open Studios and no binding: routing reports the
		// ambiguity with the real count; the synthesized names never
		// reach the wire.
		online = []routing.Studio{
			{StudioID: "studio-candidate-a", DeviceID: grant.DeviceID},
			{StudioID: "studio-candidate-b", DeviceID: grant.DeviceID},
		}
	}

	target, err := routing.Resolve(
		routing.GrantTarget{DeviceID: grant.DeviceID, StudioID: grant.StudioSessionID},
		routing.RequestTarget{},
		online,
	)
	if err != nil {
		switch {
		case errors.Is(err, routing.ErrAmbiguousStudio):
			return routing.ResolvedTarget{}, ErrAmbiguousTarget
		case errors.Is(err, routing.ErrStudioOffline):
			return routing.ResolvedTarget{}, ErrStudioUnavailable
		default:
			return routing.ResolvedTarget{}, ErrDeviceGone
		}
	}
	return target, nil
}

// snapshot returns the device's last reported status.
func (r *Relay) snapshot(deviceID string) deviceSnapshot {
	r.statusMu.Lock()
	defer r.statusMu.Unlock()
	return r.statuses[deviceID]
}

// applyStatus records one device status snapshot. A payload that does not
// carry the contract fields marks the device not ready: readiness fails
// closed on anything the gateway cannot verify.
func (r *Relay) applyStatus(deviceID string, payload json.RawMessage) {
	var snapshot struct {
		MCPReady    bool `json:"mcp_ready"`
		StudioCount int  `json:"studio_count"`
	}
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		snapshot.MCPReady = false
		snapshot.StudioCount = 0
	}
	r.statusMu.Lock()
	r.statuses[deviceID] = deviceSnapshot{Ready: snapshot.MCPReady, StudioCount: snapshot.StudioCount}
	r.statusMu.Unlock()
}
