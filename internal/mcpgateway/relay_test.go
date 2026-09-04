package mcpgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"robloxkit/internal/mcpoauth"
	"robloxkit/pkg/bridgeproto"
)

// newTestRelay builds a short-timeout relay wired into the fixture's
// envelope dispatch so device responses reach its own Pending registry.
func (fx *gatewayFixture) newTestRelay(t *testing.T, timeout time.Duration) *Relay {
	t.Helper()
	relay, err := NewRelay(RelayConfig{
		Registry:         fx.registry,
		Pending:          NewPending(64),
		Timeout:          timeout,
		MaxEnvelopeBytes: fx.limits.MaxPayloadBytes,
	})
	if err != nil {
		t.Fatalf("new relay: %v", err)
	}
	fx.registerEnvelopeHandler(relay.HandleEnvelope)
	return relay
}

func (fx *gatewayFixture) testGrant() mcpoauth.Grant {
	return mcpoauth.Grant{
		ID:       fx.grantID,
		UserID:   fx.userID,
		ClientID: "client-internal-id",
		DeviceID: fx.deviceID,
		Scopes:   []string{mcpoauth.ScopeConnect, mcpoauth.ScopeStudioRead},
		Resource: testResource,
	}
}

func relayPayload(t *testing.T, method string) json.RawMessage {
	t.Helper()
	switch method {
	case "tools/list":
		return json.RawMessage(`{}`)
	case "tools/call":
		return json.RawMessage(`{"name":"get_instance_tree","arguments":{}}`)
	default:
		return json.RawMessage(`{}`)
	}
}

// awaitRelayStatus blocks until the relay applied the device's status
// snapshot, so Calls resolve targets deterministically.
func awaitRelayStatus(t *testing.T, relay *Relay, fx *gatewayFixture, ready bool, studioCount int) {
	t.Helper()
	waitFor(t, 2*time.Second, fmt.Sprintf("relay status ready=%v studios=%d", ready, studioCount), func() bool {
		snapshot := relay.snapshot(fx.deviceID)
		return snapshot.Ready == ready && snapshot.StudioCount == studioCount
	})
}

func TestRelayTimeoutSendsCancelToDevice(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	relay := fx.newTestRelay(t, 150*time.Millisecond)
	device := fx.connectDevice(t, nil)
	awaitRelayStatus(t, relay, fx, true, 1)
	device.setHold(func(bridgeproto.Envelope) bool { return true })

	start := time.Now()
	_, err := relay.Call(context.Background(), "relay-session", fx.testGrant(), "tools/list", relayPayload(t, "tools/list"))
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("relay error = %v, want ErrDeadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("deadline returned after %s; the timeout must bound the call", elapsed)
	}

	waitFor(t, 2*time.Second, "cancel envelope on the device", func() bool {
		return len(device.cancels()) == 1
	})
	if device.cancels()[0].DeviceID != fx.deviceID {
		t.Fatalf("cancel envelope device = %q, want %q", device.cancels()[0].DeviceID, fx.deviceID)
	}
	fx.pending.mu.Lock()
	left := len(fx.pending.entries)
	fx.pending.mu.Unlock()
	if left != 0 {
		t.Fatalf("gateway pending entries = %d after relay timeout, want 0 (its registry must also drain)", left)
	}
}

func TestRelayContextCancellationSendsCancelAndDropsLateResponse(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	relay := fx.newTestRelay(t, time.Hour)
	device := fx.connectDevice(t, nil)
	awaitRelayStatus(t, relay, fx, true, 1)
	device.setHold(func(bridgeproto.Envelope) bool { return true })

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var callErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := relay.Call(ctx, "relay-session", fx.testGrant(), "tools/call", relayPayload(t, "tools/call"))
		callErr = err
	}()
	waitFor(t, 2*time.Second, "device request", func() bool { return len(device.requests()) == 1 })
	request := device.requests()[0]

	cancel()
	wg.Wait()
	if !errors.Is(callErr, ErrCancelled) {
		t.Fatalf("relay error = %v, want ErrCancelled", callErr)
	}
	waitFor(t, 2*time.Second, "cancel envelope on the device", func() bool {
		return len(device.cancels()) == 1 && device.cancels()[0].GatewayRequestID == request.GatewayRequestID
	})

	// The late device response for the abandoned correlation is dropped.
	late, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(`1`), "result": map[string]any{"content": []any{}},
	})
	if err != nil {
		t.Fatalf("marshal late response: %v", err)
	}
	if err := device.write(bridgeproto.Envelope{
		Version: bridgeproto.Version, Type: bridgeproto.TypeResponse,
		GatewayRequestID: request.GatewayRequestID, DeviceID: fx.deviceID, Payload: late,
	}); err != nil {
		t.Fatalf("write late response: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	relay.pending.mu.Lock()
	left := len(relay.pending.entries)
	relay.pending.mu.Unlock()
	if left != 0 {
		t.Fatalf("relay pending entries = %d, want 0 after late-response drop", left)
	}
}

func TestRelayDeviceDisconnectFailsPendingExactlyOnce(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	relay := fx.newTestRelay(t, time.Hour)
	device := fx.connectDevice(t, nil)
	awaitRelayStatus(t, relay, fx, true, 1)
	device.setHold(func(bridgeproto.Envelope) bool { return true })

	const callers = 3
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := func(i int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := relay.Call(context.Background(), "relay-session", fx.testGrant(),
				"tools/call", relayPayload(t, "tools/call"))
			errs[i] = err
		}()
	}
	for i := 0; i < callers; i++ {
		start(i)
	}
	waitFor(t, 2*time.Second, "all requests on the device", func() bool {
		return len(device.requests()) == callers
	})

	device.close()
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, ErrDeviceFailed) {
			t.Fatalf("caller %d error = %v, want ErrDeviceFailed", i, err)
		}
	}
	// The failed correlations are retired: the registry must be empty, and
	// a fresh call on the same session registers and resolves normally
	// (proving no double-delivery residue).
	time.Sleep(150 * time.Millisecond)
	relay.pending.mu.Lock()
	left := len(relay.pending.entries)
	relay.pending.mu.Unlock()
	if left != 0 {
		t.Fatalf("relay pending entries = %d, want 0 after disconnect", left)
	}
	relay.HandleEnvelope(context.Background(), hubDevice(fx), bridgeproto.Envelope{
		Version: bridgeproto.Version, Type: bridgeproto.TypeResponse,
		GatewayRequestID: device.requests()[0].GatewayRequestID, DeviceID: fx.deviceID,
		Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`),
	})
	relay.pending.mu.Lock()
	left = len(relay.pending.entries)
	relay.pending.mu.Unlock()
	if left != 0 {
		t.Fatalf("relay pending entries = %d, want 0 after post-disconnect response drop", left)
	}
}

func TestRelayUnknownCorrelationResponseIsDropped(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	relay := fx.newTestRelay(t, time.Hour)
	fx.connectDevice(t, nil)

	// An inbound response with no matching registration must not panic or
	// wedge the relay.
	relay.HandleEnvelope(context.Background(), hubDevice(fx), bridgeproto.Envelope{
		Version: bridgeproto.Version, Type: bridgeproto.TypeResponse,
		GatewayRequestID: "gw_does_not_exist", DeviceID: fx.deviceID,
		Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`),
	})
	relay.pending.mu.Lock()
	left := len(relay.pending.entries)
	relay.pending.mu.Unlock()
	if left != 0 {
		t.Fatalf("unknown correlation left %d entries, want 0", left)
	}
}

func TestRelayDeviceOfflineIsSanitized(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	relay := fx.newTestRelay(t, time.Hour)
	// No device connection is ever opened.

	_, err := relay.Call(context.Background(), "relay-session", fx.testGrant(),
		"tools/list", relayPayload(t, "tools/list"))
	if !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("relay error = %v, want ErrDeviceGone", err)
	}
}

func TestRelayStudioNotReadyIsTargetUnavailable(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	relay := fx.newTestRelay(t, time.Hour)
	device := fx.connectDevice(t, nil)
	device.sendStatus(false, 0)
	awaitRelayStatus(t, relay, fx, false, 0)

	_, err := relay.Call(context.Background(), "relay-session", fx.testGrant(),
		"tools/list", relayPayload(t, "tools/list"))
	if !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("relay error = %v, want ErrDeviceGone when no Studio is ready", err)
	}
}

func TestRelayAmbiguousStudioIsRejected(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	relay := fx.newTestRelay(t, time.Hour)
	device := fx.connectDevice(t, nil)
	device.sendStatus(true, 2) // two Studios online, grant is not Studio-bound
	awaitRelayStatus(t, relay, fx, true, 2)

	_, err := relay.Call(context.Background(), "relay-session", fx.testGrant(),
		"tools/list", relayPayload(t, "tools/list"))
	if !errors.Is(err, ErrAmbiguousTarget) {
		t.Fatalf("relay error = %v, want ErrAmbiguousTarget", err)
	}
}

func TestRelayBoundStudioIsPreferredTarget(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	relay := fx.newTestRelay(t, 3*time.Second)
	device := fx.connectDevice(t, nil)
	awaitRelayStatus(t, relay, fx, true, 1)

	grant := fx.testGrant()
	grant.StudioSessionID = "studio-bound-1"
	payload, err := relay.Call(context.Background(), "relay-session", grant, "tools/list", relayPayload(t, "tools/list"))
	if err != nil {
		t.Fatalf("bound-studio relay: %v", err)
	}
	var response struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("decode relay response %s: %v", payload, err)
	}
	waitFor(t, 2*time.Second, "device request", func() bool { return len(device.requests()) == 1 })
	request := device.requests()[0]
	if request.StudioID != "studio-bound-1" {
		t.Fatalf("relayed envelope studio_id = %q, want the bound Studio", request.StudioID)
	}
}

func TestRelayCancelSessionFailsPendingWaiters(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	relay := fx.newTestRelay(t, time.Hour)
	device := fx.connectDevice(t, nil)
	awaitRelayStatus(t, relay, fx, true, 1)
	device.setHold(func(bridgeproto.Envelope) bool { return true })

	var wg sync.WaitGroup
	var callErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := relay.Call(context.Background(), "relay-session", fx.testGrant(),
			"tools/call", relayPayload(t, "tools/call"))
		callErr = err
	}()
	waitFor(t, 2*time.Second, "device request", func() bool { return len(device.requests()) == 1 })

	relay.CancelSession("relay-session")
	wg.Wait()
	if !errors.Is(callErr, ErrCancelled) {
		t.Fatalf("relay error = %v, want ErrCancelled after session end", callErr)
	}
	waitFor(t, 2*time.Second, "cancel envelope on the device", func() bool {
		return len(device.cancels()) == 1
	})
}

func TestRelayResponseWithMismatchedIDIsRejected(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	relay := fx.newTestRelay(t, 3*time.Second)
	device := fx.connectDevice(t, nil)
	awaitRelayStatus(t, relay, fx, true, 1)
	// Answer with a response whose payload id does not match the request.
	device.setHold(func(env bridgeproto.Envelope) bool {
		forged := `{"jsonrpc":"2.0","id":"forged","result":{}}`
		if err := device.write(bridgeproto.Envelope{
			Version: bridgeproto.Version, Type: bridgeproto.TypeResponse,
			GatewayRequestID: env.GatewayRequestID, DeviceID: fx.deviceID,
			Payload: json.RawMessage(forged),
		}); err != nil {
			t.Errorf("forged response write: %v", err)
		}
		return true // the default responder must not also answer
	})

	_, err := relay.Call(context.Background(), "relay-session", fx.testGrant(),
		"tools/list", relayPayload(t, "tools/list"))
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("relay error = %v, want ErrInvalidResponse", err)
	}
}
