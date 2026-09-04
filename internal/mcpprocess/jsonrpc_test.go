package mcpprocess

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateRequestAcceptsCallsAndNotifications(t *testing.T) {
	tests := []struct {
		name  string
		frame string
	}{
		{name: "numeric request ID", frame: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`},
		{name: "string request ID", frame: `{"jsonrpc":"2.0","id":"request-1","method":"tools/list","params":[]}`},
		{name: "notification", frame: `{"jsonrpc":"2.0","method":"notifications/initialized"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateRequest(json.RawMessage(tt.frame)); err != nil {
				t.Fatalf("validateRequest() error = %v", err)
			}
		})
	}
}

func TestValidateRequestRejectsInvalidJSONRPC(t *testing.T) {
	tests := []struct {
		name  string
		frame string
	}{
		{name: "malformed JSON", frame: `{"jsonrpc":"2.0"`},
		{name: "trailing JSON", frame: `{"jsonrpc":"2.0","method":"ping"}{}`},
		{name: "non-object", frame: `[]`},
		{name: "missing version", frame: `{"id":1,"method":"initialize"}`},
		{name: "wrong version", frame: `{"jsonrpc":"1.0","id":1,"method":"initialize"}`},
		{name: "missing method", frame: `{"jsonrpc":"2.0","id":1,"params":{}}`},
		{name: "empty method", frame: `{"jsonrpc":"2.0","id":1,"method":""}`},
		{name: "null ID", frame: `{"jsonrpc":"2.0","id":null,"method":"initialize"}`},
		{name: "boolean ID", frame: `{"jsonrpc":"2.0","id":true,"method":"initialize"}`},
		{name: "object ID", frame: `{"jsonrpc":"2.0","id":{},"method":"initialize"}`},
		{name: "fractional numeric ID", frame: `{"jsonrpc":"2.0","id":1.5,"method":"initialize"}`},
		{name: "scalar params", frame: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":"bad"}`},
		{name: "response fields on request", frame: `{"jsonrpc":"2.0","id":1,"method":"initialize","result":{}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateRequest(json.RawMessage(tt.frame)); err == nil {
				t.Fatal("validateRequest() error = nil, want strict validation failure")
			}
		})
	}
}

func TestValidateResponseAcceptsCorrelatedResultAndError(t *testing.T) {
	tests := []struct {
		name       string
		frame      string
		expectedID string
	}{
		{
			name:       "result response",
			frame:      `{"jsonrpc":"2.0","id":7,"result":{"protocolVersion":"test"}}`,
			expectedID: `7`,
		},
		{
			name:       "error response",
			frame:      `{"jsonrpc":"2.0","id":"call-a","error":{"code":-32601,"message":"Method not found","data":{"method":"missing"}}}`,
			expectedID: `"call-a"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateResponse(json.RawMessage(tt.frame), json.RawMessage(tt.expectedID)); err != nil {
				t.Fatalf("validateResponse() error = %v", err)
			}
		})
	}
}

func TestValidateResponseRejectsInvalidJSONRPC(t *testing.T) {
	tests := []struct {
		name       string
		frame      string
		expectedID string
		wantError  string
	}{
		{name: "malformed JSON", frame: `{"jsonrpc":"2.0"`, expectedID: `1`, wantError: "json"},
		{name: "trailing JSON", frame: `{"jsonrpc":"2.0","id":1,"result":{}}[]`, expectedID: `1`, wantError: "json"},
		{name: "non-object", frame: `[]`, expectedID: `1`, wantError: "object"},
		{name: "wrong version", frame: `{"jsonrpc":"1.0","id":1,"result":{}}`, expectedID: `1`, wantError: "2.0"},
		{name: "missing ID", frame: `{"jsonrpc":"2.0","result":{}}`, expectedID: `1`, wantError: "id"},
		{name: "null ID", frame: `{"jsonrpc":"2.0","id":null,"result":{}}`, expectedID: `1`, wantError: "id"},
		{name: "invalid ID type", frame: `{"jsonrpc":"2.0","id":true,"result":{}}`, expectedID: `1`, wantError: "id"},
		{name: "fractional ID", frame: `{"jsonrpc":"2.0","id":1.5,"result":{}}`, expectedID: `1.5`, wantError: "id"},
		{name: "missing result and error", frame: `{"jsonrpc":"2.0","id":1}`, expectedID: `1`, wantError: "result"},
		{name: "both result and error", frame: `{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-1,"message":"bad"}}`, expectedID: `1`, wantError: "exactly one"},
		{name: "non-object error", frame: `{"jsonrpc":"2.0","id":1,"error":"bad"}`, expectedID: `1`, wantError: "error"},
		{name: "missing error code", frame: `{"jsonrpc":"2.0","id":1,"error":{"message":"bad"}}`, expectedID: `1`, wantError: "code"},
		{name: "fractional error code", frame: `{"jsonrpc":"2.0","id":1,"error":{"code":-1.5,"message":"bad"}}`, expectedID: `1`, wantError: "code"},
		{name: "missing error message", frame: `{"jsonrpc":"2.0","id":1,"error":{"code":-1}}`, expectedID: `1`, wantError: "message"},
		{name: "non-string error message", frame: `{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":false}}`, expectedID: `1`, wantError: "message"},
		{name: "request field on response", frame: `{"jsonrpc":"2.0","id":1,"method":"initialize","result":{}}`, expectedID: `1`, wantError: "method"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateResponse(json.RawMessage(tt.frame), json.RawMessage(tt.expectedID))
			if err == nil {
				t.Fatal("validateResponse() error = nil, want strict validation failure")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantError)) {
				t.Fatalf("validateResponse() error = %q, want it to mention %q", err, tt.wantError)
			}
		})
	}
}

func TestValidateResponseCorrelatesIDsByTypeAndValue(t *testing.T) {
	tests := []struct {
		name       string
		frame      string
		expectedID string
	}{
		{name: "different integer", frame: `{"jsonrpc":"2.0","id":2,"result":{}}`, expectedID: `1`},
		{name: "string versus number", frame: `{"jsonrpc":"2.0","id":"1","result":{}}`, expectedID: `1`},
		{name: "different string", frame: `{"jsonrpc":"2.0","id":"request-b","result":{}}`, expectedID: `"request-a"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateResponse(json.RawMessage(tt.frame), json.RawMessage(tt.expectedID))
			if err == nil {
				t.Fatal("validateResponse() error = nil, want correlation failure")
			}
			if !strings.Contains(strings.ToLower(err.Error()), "id") {
				t.Fatalf("validateResponse() error = %q, want ID correlation message", err)
			}
		})
	}
}

func TestValidateResponseRejectsInvalidExpectedCorrelationID(t *testing.T) {
	tests := []string{``, `null`, `true`, `1.5`, `{}`}
	frame := json.RawMessage(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	for _, expectedID := range tests {
		t.Run(expectedID, func(t *testing.T) {
			if err := validateResponse(frame, json.RawMessage(expectedID)); err == nil {
				t.Fatal("validateResponse() error = nil, want invalid expected ID rejection")
			}
		})
	}
}
