package bridgeproto

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

const testMessageLimit = 1 << 20

func TestRequestRoundTripPreservesOriginalRPCID(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{name: "number", id: `7`},
		{name: "string", id: `"client-request-7"`},
		{name: "null", id: `null`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := json.RawMessage(`{"jsonrpc":"2.0","id":` + tt.id + `,"method":"tools/list","params":{"vendor_extension":true}}`)
			deadline := time.Date(2026, time.September, 3, 12, 34, 56, 0, time.UTC)
			want := Envelope{
				Version:          1,
				Type:             TypeRequest,
				GatewayRequestID: "gw_1",
				DeviceID:         "dev_1",
				StudioID:         "studio_1",
				Deadline:         deadline,
				Payload:          raw,
			}

			encoded, err := Encode(want, Limits{MaxPayloadBytes: testMessageLimit})
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			got, err := Decode(encoded, Limits{MaxPayloadBytes: testMessageLimit})
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}

			if got.Version != want.Version || got.Type != want.Type {
				t.Fatalf("decoded version/type = %d/%q, want %d/%q", got.Version, got.Type, want.Version, want.Type)
			}
			if got.GatewayRequestID != want.GatewayRequestID || got.DeviceID != want.DeviceID || got.StudioID != want.StudioID {
				t.Fatalf("decoded routing fields = gateway %q, device %q, studio %q", got.GatewayRequestID, got.DeviceID, got.StudioID)
			}
			if !got.Deadline.Equal(deadline) {
				t.Fatalf("decoded deadline = %s, want %s", got.Deadline, deadline)
			}
			assertJSONEqual(t, got.Payload, raw)

			var payload struct {
				ID json.RawMessage `json:"id"`
			}
			if err := json.Unmarshal(got.Payload, &payload); err != nil {
				t.Fatalf("unmarshal decoded payload: %v", err)
			}
			if string(payload.ID) != tt.id {
				t.Fatalf("decoded JSON-RPC id = %s, want %s", payload.ID, tt.id)
			}
		})
	}
}

func TestAllMessageTypesRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		env  Envelope
	}{
		{name: "hello", env: Envelope{Version: 1, Type: TypeHello, DeviceID: "dev_1", Payload: json.RawMessage(`{"bridge_version":"1.0.0","platform":"windows","capabilities":["mcp"]}`)}},
		{name: "heartbeat", env: Envelope{Version: 1, Type: TypeHeartbeat, DeviceID: "dev_1"}},
		{name: "status", env: Envelope{Version: 1, Type: TypeStatus, DeviceID: "dev_1", Payload: json.RawMessage(`{"mcp":"running","studios":["studio_1"]}`)}},
		{name: "request", env: Envelope{Version: 1, Type: TypeRequest, GatewayRequestID: "gw_1", DeviceID: "dev_1", Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)}},
		{name: "response", env: Envelope{Version: 1, Type: TypeResponse, GatewayRequestID: "gw_1", DeviceID: "dev_1", Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)}},
		{name: "notification", env: Envelope{Version: 1, Type: TypeNotification, DeviceID: "dev_1", Payload: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`)}},
		{name: "cancel", env: Envelope{Version: 1, Type: TypeCancel, GatewayRequestID: "gw_1", DeviceID: "dev_1"}},
		{name: "error", env: Envelope{Version: 1, Type: TypeError, GatewayRequestID: "gw_1", DeviceID: "dev_1", Payload: json.RawMessage(`{"code":"mcp_unavailable","message":"MCP process unavailable"}`)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := Encode(tt.env, Limits{MaxPayloadBytes: testMessageLimit})
			if err != nil {
				t.Fatalf("Encode() error = %v", err)
			}
			got, err := Decode(encoded, Limits{MaxPayloadBytes: testMessageLimit})
			if err != nil {
				t.Fatalf("Decode() error = %v", err)
			}
			if got.Type != tt.env.Type || got.DeviceID != tt.env.DeviceID || got.GatewayRequestID != tt.env.GatewayRequestID {
				t.Fatalf("Decode() routing fields = type %q, device %q, gateway %q", got.Type, got.DeviceID, got.GatewayRequestID)
			}
			assertJSONEqual(t, got.Payload, tt.env.Payload)
		})
	}
}

func TestEncodeRejectsUnsupportedVersionAndType(t *testing.T) {
	tests := []struct {
		name string
		env  Envelope
	}{
		{
			name: "unknown version",
			env:  Envelope{Version: 2, Type: TypeHeartbeat, DeviceID: "dev_1"},
		},
		{
			name: "unknown type",
			env:  Envelope{Version: 1, Type: MessageType("future-message"), DeviceID: "dev_1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Encode(tt.env, Limits{MaxPayloadBytes: testMessageLimit}); err == nil {
				t.Fatal("Encode() error = nil, want rejection")
			}
		})
	}
}

func TestDecodeRejectsUnsupportedVersionAndType(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "unknown version", raw: `{"version":2,"type":"heartbeat","device_id":"dev_1"}`},
		{name: "missing version", raw: `{"type":"heartbeat","device_id":"dev_1"}`},
		{name: "unknown type", raw: `{"version":1,"type":"future-message","device_id":"dev_1"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode([]byte(tt.raw), Limits{MaxPayloadBytes: testMessageLimit}); err == nil {
				t.Fatal("Decode() error = nil, want rejection")
			}
		})
	}
}

func TestEncodeRejectsMissingMessageSpecificFields(t *testing.T) {
	tests := []struct {
		name string
		env  Envelope
	}{
		{name: "hello device", env: Envelope{Version: 1, Type: TypeHello, Payload: json.RawMessage(`{"bridge_version":"1.0.0"}`)}},
		{name: "hello payload", env: Envelope{Version: 1, Type: TypeHello, DeviceID: "dev_1"}},
		{name: "heartbeat device", env: Envelope{Version: 1, Type: TypeHeartbeat}},
		{name: "status device", env: Envelope{Version: 1, Type: TypeStatus, Payload: json.RawMessage(`{"mcp":"running"}`)}},
		{name: "status payload", env: Envelope{Version: 1, Type: TypeStatus, DeviceID: "dev_1"}},
		{name: "request correlation", env: Envelope{Version: 1, Type: TypeRequest, DeviceID: "dev_1", Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)}},
		{name: "request device", env: Envelope{Version: 1, Type: TypeRequest, GatewayRequestID: "gw_1", Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)}},
		{name: "request payload", env: Envelope{Version: 1, Type: TypeRequest, GatewayRequestID: "gw_1", DeviceID: "dev_1"}},
		{name: "response correlation", env: Envelope{Version: 1, Type: TypeResponse, DeviceID: "dev_1", Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)}},
		{name: "response device", env: Envelope{Version: 1, Type: TypeResponse, GatewayRequestID: "gw_1", Payload: json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)}},
		{name: "response payload", env: Envelope{Version: 1, Type: TypeResponse, GatewayRequestID: "gw_1", DeviceID: "dev_1"}},
		{name: "notification device", env: Envelope{Version: 1, Type: TypeNotification, Payload: json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/progress"}`)}},
		{name: "notification payload", env: Envelope{Version: 1, Type: TypeNotification, DeviceID: "dev_1"}},
		{name: "cancel correlation", env: Envelope{Version: 1, Type: TypeCancel, DeviceID: "dev_1"}},
		{name: "cancel device", env: Envelope{Version: 1, Type: TypeCancel, GatewayRequestID: "gw_1"}},
		{name: "error correlation", env: Envelope{Version: 1, Type: TypeError, DeviceID: "dev_1", Payload: json.RawMessage(`{"code":"failed"}`)}},
		{name: "error device", env: Envelope{Version: 1, Type: TypeError, GatewayRequestID: "gw_1", Payload: json.RawMessage(`{"code":"failed"}`)}},
		{name: "error payload", env: Envelope{Version: 1, Type: TypeError, GatewayRequestID: "gw_1", DeviceID: "dev_1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Encode(tt.env, Limits{MaxPayloadBytes: testMessageLimit}); err == nil {
				t.Fatal("Encode() error = nil, want missing-field rejection")
			}
		})
	}
}

func TestDecodeAppliesMessageSpecificValidation(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "hello device", raw: `{"version":1,"type":"hello","payload":{"bridge_version":"1.0.0"}}`},
		{name: "hello payload", raw: `{"version":1,"type":"hello","device_id":"dev_1"}`},
		{name: "heartbeat device", raw: `{"version":1,"type":"heartbeat"}`},
		{name: "status device", raw: `{"version":1,"type":"status","payload":{"mcp":"running"}}`},
		{name: "status payload", raw: `{"version":1,"type":"status","device_id":"dev_1"}`},
		{name: "request correlation", raw: `{"version":1,"type":"request","device_id":"dev_1","payload":{"jsonrpc":"2.0","id":1,"method":"tools/list"}}`},
		{name: "request device", raw: `{"version":1,"type":"request","gateway_request_id":"gw_1","payload":{"jsonrpc":"2.0","id":1,"method":"tools/list"}}`},
		{name: "request payload", raw: `{"version":1,"type":"request","gateway_request_id":"gw_1","device_id":"dev_1"}`},
		{name: "response correlation", raw: `{"version":1,"type":"response","device_id":"dev_1","payload":{"jsonrpc":"2.0","id":1,"result":{}}}`},
		{name: "response device", raw: `{"version":1,"type":"response","gateway_request_id":"gw_1","payload":{"jsonrpc":"2.0","id":1,"result":{}}}`},
		{name: "response payload", raw: `{"version":1,"type":"response","gateway_request_id":"gw_1","device_id":"dev_1"}`},
		{name: "notification device", raw: `{"version":1,"type":"notification","payload":{"jsonrpc":"2.0","method":"notifications/progress"}}`},
		{name: "notification payload", raw: `{"version":1,"type":"notification","device_id":"dev_1"}`},
		{name: "cancel correlation", raw: `{"version":1,"type":"cancel","device_id":"dev_1"}`},
		{name: "cancel device", raw: `{"version":1,"type":"cancel","gateway_request_id":"gw_1"}`},
		{name: "error correlation", raw: `{"version":1,"type":"error","device_id":"dev_1","payload":{"code":"failed"}}`},
		{name: "error device", raw: `{"version":1,"type":"error","gateway_request_id":"gw_1","payload":{"code":"failed"}}`},
		{name: "error payload", raw: `{"version":1,"type":"error","gateway_request_id":"gw_1","device_id":"dev_1"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Decode([]byte(tt.raw), Limits{MaxPayloadBytes: testMessageLimit}); err == nil {
				t.Fatal("Decode() error = nil, want message-specific validation error")
			}
		})
	}
}

func TestDecodeRejectsUnknownEnvelopeFields(t *testing.T) {
	raw := []byte(`{"version":1,"type":"heartbeat","device_id":"dev_1","future_field":true}`)
	if _, err := Decode(raw, Limits{MaxPayloadBytes: testMessageLimit}); err == nil {
		t.Fatal("Decode() error = nil, want unknown-field rejection")
	}
}

func TestMalformedJSONIsRejected(t *testing.T) {
	t.Run("malformed envelope", func(t *testing.T) {
		raw := []byte(`{"version":1,"type":"request","device_id":"dev_1"`)
		if _, err := Decode(raw, Limits{MaxPayloadBytes: testMessageLimit}); err == nil {
			t.Fatal("Decode() error = nil, want malformed JSON rejection")
		}
	})

	t.Run("trailing JSON value", func(t *testing.T) {
		raw := []byte(`{"version":1,"type":"heartbeat","device_id":"dev_1"} {}`)
		if _, err := Decode(raw, Limits{MaxPayloadBytes: testMessageLimit}); err == nil {
			t.Fatal("Decode() error = nil, want trailing JSON rejection")
		}
	})

	t.Run("malformed raw payload", func(t *testing.T) {
		env := Envelope{
			Version:          1,
			Type:             TypeRequest,
			GatewayRequestID: "gw_1",
			DeviceID:         "dev_1",
			Payload:          json.RawMessage(`{"jsonrpc":`),
		}
		if _, err := Encode(env, Limits{MaxPayloadBytes: testMessageLimit}); err == nil {
			t.Fatal("Encode() error = nil, want malformed payload rejection")
		}
	})
}

func TestByteLimitsAreEnforced(t *testing.T) {
	valid := Envelope{
		Version:          1,
		Type:             TypeRequest,
		GatewayRequestID: "gw_1",
		DeviceID:         "dev_1",
		Payload:          json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	}

	t.Run("encode", func(t *testing.T) {
		if _, err := Encode(valid, Limits{MaxPayloadBytes: 32}); err == nil {
			t.Fatal("Encode() error = nil, want oversized message rejection")
		}
	})

	t.Run("decode", func(t *testing.T) {
		raw := []byte(`{"version":1,"type":"heartbeat","device_id":"dev_1"}`)
		if len(raw) <= 32 {
			t.Fatalf("test fixture length = %d, want greater than 32", len(raw))
		}
		if _, err := Decode(raw, Limits{MaxPayloadBytes: 32}); err == nil {
			t.Fatal("Decode() error = nil, want oversized message rejection")
		}
	})
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	if len(got) == 0 || len(want) == 0 {
		if !bytes.Equal(got, want) {
			t.Fatalf("JSON bytes = %q, want %q", got, want)
		}
		return
	}

	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("got invalid JSON %q: %v", got, err)
	}
	var wantValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("test has invalid expected JSON %q: %v", want, err)
	}
	gotCanonical, err := json.Marshal(gotValue)
	if err != nil {
		t.Fatalf("marshal decoded JSON: %v", err)
	}
	wantCanonical, err := json.Marshal(wantValue)
	if err != nil {
		t.Fatalf("marshal expected JSON: %v", err)
	}
	if !bytes.Equal(gotCanonical, wantCanonical) {
		t.Fatalf("decoded payload = %s, want %s", gotCanonical, wantCanonical)
	}
}
