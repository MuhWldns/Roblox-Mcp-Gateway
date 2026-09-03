package bridgehub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"robloxkit/pkg/bridgeproto"
)

// testPair opens one real websocket pair without authentication: the server
// side wraps a Connection directly, so registry behavior is exercised in
// isolation from the hub handshake.

type testPair struct {
	t        *testing.T
	registry *Registry
	ws       *websocket.Conn
	conn     *Connection
	deviceID string
}

func defaultTestOptions() connectionOptions {
	return connectionOptions{
		queueDepth:       8,
		maxEnvelopeBytes: 256 * 1024,
		writeTimeout:     5 * time.Second,
	}
}

func newTestPair(t *testing.T, deviceID string, opts connectionOptions) *testPair {
	t.Helper()
	conns := make(chan *Connection, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/bridge", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		ws.SetReadLimit(int64(opts.maxEnvelopeBytes))
		conn := newConnection(context.Background(), ws, Device{UserID: "user_test", DeviceID: deviceID}, opts)
		conn.start()
		select {
		case conns <- conn:
		default:
		}
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(server.URL, "https")+"/bridge", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatalf("dial test pair: %v", err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })

	var conn *Connection
	select {
	case conn = <-conns:
	case <-time.After(2 * time.Second):
		t.Fatal("server connection was not created")
	}
	return &testPair{t: t, ws: ws, conn: conn, deviceID: deviceID}
}

func testStatusEnvelope(deviceID string) bridgeproto.Envelope {
	return bridgeproto.Envelope{
		Version:  bridgeproto.Version,
		Type:     bridgeproto.TypeStatus,
		DeviceID: deviceID,
		Payload:  json.RawMessage(`{"state":"connected"}`),
	}
}

func TestRegistryRegisterReturnsReplacedConnection(t *testing.T) {
	registry := NewRegistry()
	pairA := newTestPair(t, "device_1", defaultTestOptions())
	pairB := newTestPair(t, "device_1", defaultTestOptions())

	if replaced := registry.Register("device_1", pairA.conn); replaced != nil {
		t.Fatal("first register must not replace a connection")
	}
	replaced := registry.Register("device_1", pairB.conn)
	if replaced != pairA.conn {
		t.Fatal("second register must return the replaced connection")
	}
	if n := registry.Len(); n != 1 {
		t.Fatalf("registry len = %d, want 1", n)
	}
	conn, ok := registry.Get("device_1")
	if !ok || conn != pairB.conn {
		t.Fatal("registry must hold the newest connection")
	}

	// Register only hands the replaced connection back: like the hub, the
	// caller closes it with the duplicate-device policy.
	replaced.close(websocket.StatusPolicyViolation, reasonSuperseded)
	select {
	case <-pairA.conn.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("replaced connection was not closed")
	}
	expectCloseStatus(t, pairA.ws, websocket.StatusPolicyViolation)
}

func TestRegistrySendUnknownDeviceFails(t *testing.T) {
	registry := NewRegistry()
	err := registry.Send(context.Background(), "missing_device", testStatusEnvelope("missing_device"))
	if !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("send err = %v, want ErrDeviceOffline", err)
	}
}

func TestRegistrySendDeliversEnvelope(t *testing.T) {
	registry := NewRegistry()
	pair := newTestPair(t, "device_1", defaultTestOptions())
	registry.Register("device_1", pair.conn)

	env := testStatusEnvelope("device_1")
	if err := registry.Send(context.Background(), "device_1", env); err != nil {
		t.Fatalf("send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := pair.ws.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	received, err := bridgeproto.Decode(data, bridgeproto.Limits{MaxPayloadBytes: 4 << 20})
	if err != nil {
		t.Fatalf("decode delivered envelope: %v", err)
	}
	if received.Type != env.Type || received.DeviceID != env.DeviceID {
		t.Fatalf("delivered envelope = %#v, want %#v", received, env)
	}
}

func TestRegistrySendRejectsInvalidEnvelope(t *testing.T) {
	registry := NewRegistry()
	pair := newTestPair(t, "device_1", defaultTestOptions())
	registry.Register("device_1", pair.conn)

	// Missing device_id violates the wire contract and must fail without
	// touching the queue.
	err := registry.Send(context.Background(), "device_1", bridgeproto.Envelope{
		Version: bridgeproto.Version,
		Type:    bridgeproto.TypeStatus,
	})
	if err == nil {
		t.Fatal("expected encode error for envelope without device_id")
	}
}

func TestRegistrySendSlowConsumerClosesConnection(t *testing.T) {
	registry := NewRegistry()
	opts := defaultTestOptions()
	opts.queueDepth = 1
	pair := newTestPair(t, "device_1", opts)
	registry.Register("device_1", pair.conn)
	// The client never reads, so the writer goroutine wedges in kernel buffers.

	payload := `"` + strings.Repeat("x", 200*1024) + `"`
	started := time.Now()
	slowConsumer := false
	for i := 0; i < 128 && !slowConsumer; i++ {
		env := testStatusEnvelope("device_1")
		env.Payload = json.RawMessage(payload)
		err := registry.Send(context.Background(), "device_1", env)
		switch {
		case err == nil:
		case errors.Is(err, ErrSlowConsumer):
			slowConsumer = true
		case errors.Is(err, ErrConnectionClosed), errors.Is(err, ErrDeviceOffline):
		default:
			t.Fatalf("unexpected send error: %v", err)
		}
	}
	if !slowConsumer {
		t.Fatal("expected ErrSlowConsumer from the bounded writer queue")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("send loop blocked for %s; the registry must never block", elapsed)
	}

	select {
	case <-pair.conn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("slow consumer was not disconnected")
	}

	// After cleanup the device reports offline instead of dangling.
	registry.Disconnect("device_1", "slow consumer drained")
	err := registry.Send(context.Background(), "device_1", testStatusEnvelope("device_1"))
	if !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("send after disconnect err = %v, want ErrDeviceOffline", err)
	}
}

func TestRegistryDisconnectClosesAndRemoves(t *testing.T) {
	registry := NewRegistry()
	pair := newTestPair(t, "device_1", defaultTestOptions())
	registry.Register("device_1", pair.conn)

	registry.Disconnect("device_1", "revoked")
	expectCloseStatus(t, pair.ws, websocket.StatusPolicyViolation)
	if n := registry.Len(); n != 0 {
		t.Fatalf("registry len = %d, want 0 after disconnect", n)
	}
	if err := registry.Send(context.Background(), "device_1", testStatusEnvelope("device_1")); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("send after disconnect err = %v, want ErrDeviceOffline", err)
	}

	// Disconnecting an absent device is a no-op.
	registry.Disconnect("absent", "no-op")
}

func TestRegistryConcurrentOperations(t *testing.T) {
	registry := NewRegistry()
	const devices = 4
	pairs := make([]*testPair, devices)
	for i := range pairs {
		deviceID := fmt.Sprintf("device_%d", i)
		pairs[i] = newTestPair(t, deviceID, defaultTestOptions())
		registry.Register(deviceID, pairs[i].conn)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			for j := range pairs {
				_ = registry.Send(context.Background(), fmt.Sprintf("device_%d", j), testStatusEnvelope(fmt.Sprintf("device_%d", j)))
				_ = registry.Send(context.Background(), "unknown_device", testStatusEnvelope("unknown_device"))
			}
			// Alternate register/disconnect churn on the first device.
			registry.Register("device_0", pairs[0].conn)
			registry.Disconnect("device_0", "churn")
		}
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("registry deadlocked under concurrent operations")
	}

	for j := range pairs {
		registry.Disconnect(fmt.Sprintf("device_%d", j), "cleanup")
	}
	if n := registry.Len(); n != 0 {
		t.Fatalf("registry len = %d, want 0 after cleanup", n)
	}
}
