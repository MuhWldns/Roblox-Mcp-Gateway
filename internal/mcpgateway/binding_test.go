package mcpgateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// The binding-source tests pin the entitlement contract across the hub dial
// and the per-call MCP reauthorization: an active trial covers the enrolled
// credential-owned active device without any paid slot binding, while
// license-only access keeps requiring an active slot binding.

// TestActiveTrialWithoutPaidBindingRelaysToolCall proves case (a): a trial-only
// device (first enrollment created trial + device + credential, never a
// binding) dials the hub and completes an MCP initialize → tools/call relay.
func TestActiveTrialWithoutPaidBindingRelaysToolCall(t *testing.T) {
	fx := newGatewayFixture(t, func(spec *gatewaySpec) {
		spec.licenseStatus = "" // no paid license, no slot binding
		spec.trialEndsAt = 13 * 24 * time.Hour
	})
	fx.connectDevice(t, nil)
	session := fx.openSession(t)

	envelope := session.call("tools/call", `{"name":"get_instance_tree","arguments":{}}`)
	if _, hasErr := envelope["error"]; hasErr {
		t.Fatalf("trial-only tools/call must relay, got error: %v", envelope["error"])
	}
}

// TestExpiredTrialWithLicensedBindingRelaysToolCall proves case (b): the paid
// license path keeps serving the device after the trial window closes.
func TestExpiredTrialWithLicensedBindingRelaysToolCall(t *testing.T) {
	fx := newGatewayFixture(t, func(spec *gatewaySpec) {
		spec.licenseStatus = "active"
		spec.trialEndsAt = -24 * time.Hour
	})
	fx.connectDevice(t, nil)
	session := fx.openSession(t)

	envelope := session.call("tools/call", `{"name":"get_instance_tree","arguments":{}}`)
	if _, hasErr := envelope["error"]; hasErr {
		t.Fatalf("licensed tools/call must relay, got error: %v", envelope["error"])
	}
}

// TestLicenseOnlyBindingRemovalDeniedPerCall proves case (d): license-only
// access loses MCP the moment its slot binding goes away (the transfer shape)
// — the per-call reauthorization denies the very next relayed request.
func TestLicenseOnlyBindingRemovalDeniedPerCall(t *testing.T) {
	fx := newGatewayFixture(t, func(spec *gatewaySpec) {
		spec.licenseStatus = "active"
		spec.trialEndsAt = -24 * time.Hour
	})
	fx.connectDevice(t, nil)
	session := fx.openSession(t)

	// Sanity: the call relays while the binding is present.
	envelope := session.call("tools/call", `{"name":"get_instance_tree","arguments":{}}`)
	if _, hasErr := envelope["error"]; hasErr {
		t.Fatalf("pre-removal tools/call must relay, got error: %v", envelope["error"])
	}

	// Move the binding away (the admin-transfer shape).
	if _, err := fx.db.Exec(`DELETE FROM license_device_bindings WHERE user_id = ? AND device_id = ?`,
		fx.userID, fx.deviceID); err != nil {
		t.Fatalf("remove binding: %v", err)
	}

	session.nextID++
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"get_instance_tree","arguments":{}}}`, session.nextID)
	resp := fx.post(t, fx.token, session.sessionID, body, "")
	denied := resp.StatusCode != http.StatusOK
	if !denied {
		for _, message := range parseSSE(t, resp) {
			var probe map[string]json.RawMessage
			if err := json.Unmarshal(message, &probe); err == nil {
				if _, hasErr := probe["error"]; hasErr {
					denied = true
				}
			}
		}
	}
	if !denied {
		t.Fatalf("license-only access without its binding must be denied per call (status %d)", resp.StatusCode)
	}
}
