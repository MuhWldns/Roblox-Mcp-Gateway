package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"robloxkit/internal/appconfig"
)

func getenvWith(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// The committed selection contract is untouched: BRIDGE_MODE=remote selects
// the remote gateway mode, anything else (or unset) stays local. Service mode
// is selected explicitly with BRIDGE_MODE=service or by the Windows service
// control manager launch detection, which is authoritative while running as
// the service — a non-service run mode could never report service status.
func TestResolveBridgeMode(t *testing.T) {
	cases := []struct {
		name       string
		bridgeMode string
		inService  bool
		want       string
	}{
		{"unset stays local", "", false, bridgeModeLocal},
		{"blank stays local", "   ", false, bridgeModeLocal},
		{"explicit local", "local", false, bridgeModeLocal},
		{"local is case-insensitive", "LOCAL", false, bridgeModeLocal},
		{"remote selects remote", "remote", false, bridgeModeRemote},
		{"remote is case-insensitive", " Remote ", false, bridgeModeRemote},
		{"service selects service", "service", false, bridgeModeService},
		{"service is case-insensitive", "SERVICE", false, bridgeModeService},
		{"unknown value stays local", "cluster", false, bridgeModeLocal},
		{"enroll selects enroll", "enroll", false, bridgeModeEnroll},
		{"enroll is case-insensitive", "ENROLL", false, bridgeModeEnroll},
		{"SCM launch wins over enroll", "enroll", true, bridgeModeService},
		{"SCM launch wins over unset", "", true, bridgeModeService},
		{"SCM launch wins over remote", "remote", true, bridgeModeService},
		{"SCM launch wins over local", "local", true, bridgeModeService},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveBridgeMode(getenvWith(map[string]string{"BRIDGE_MODE": tc.bridgeMode}), tc.inService)
			if got != tc.want {
				t.Fatalf("resolveBridgeMode(%q, %v) = %q, want %q", tc.bridgeMode, tc.inService, got, tc.want)
			}
		})
	}
}

func TestServiceLogPathSelection(t *testing.T) {
	got, err := defaultServiceLogPath(getenvWith(map[string]string{"BRIDGE_SERVICE_LOG": "", "ProgramData": `C:\ProgramData`}))
	if err != nil {
		t.Fatalf("default service log path: %v", err)
	}
	if !strings.EqualFold(got, `C:\ProgramData\RobloxBridge\service.log`) {
		t.Fatalf("default service log path = %q", got)
	}
	got, err = defaultServiceLogPath(getenvWith(map[string]string{"BRIDGE_SERVICE_LOG": `D:\logs\bridge.log`}))
	if err != nil {
		t.Fatalf("service log override: %v", err)
	}
	if got != `D:\logs\bridge.log` {
		t.Fatalf("service log override = %q", got)
	}
	_, err = defaultServiceLogPath(getenvWith(nil))
	if err == nil {
		t.Fatal("missing ProgramData and override must fail")
	}
}

// gatewayAPIOrigin derives the https API origin from the configured gateway
// URL. Only the scheme changes — ws:// and http:// gateway URLs are refused,
// so enrollment can never run against a relaxed-TLS origin.
func TestGatewayAPIOrigin(t *testing.T) {
	cases := []struct {
		name    string
		given   string
		want    string
		wantErr bool
	}{
		{"wss becomes https", "wss://gateway.example.com/bridge", "https://gateway.example.com", false},
		{"https stays https", "https://gateway.example.com", "https://gateway.example.com", false},
		{"https with path keeps origin", "https://gateway.example.com/api", "https://gateway.example.com", false},
		{"ws is refused", "ws://gateway.example.com/bridge", "", true},
		{"http is refused", "http://gateway.example.com", "", true},
		{"garbage is refused", "not a url", "", true},
		{"empty is refused", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gatewayAPIOrigin(tc.given)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("gatewayAPIOrigin(%q) = %q, want error", tc.given, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("gatewayAPIOrigin(%q): %v", tc.given, err)
			}
			if got != tc.want {
				t.Fatalf("gatewayAPIOrigin(%q) = %q, want %q", tc.given, got, tc.want)
			}
		})
	}
}

// newEnrollDeviceID must produce a 36-character RFC 4122 v4 shape — the
// devices.id column bound.
func TestNewEnrollDeviceID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		id := newEnrollDeviceID()
		if len(id) != 36 {
			t.Fatalf("device id length = %d, want 36: %q", len(id), id)
		}
		if seen[id] {
			t.Fatalf("device id collision: %s", id)
		}
		seen[id] = true
	}
}

// The documented clean-install enrollment invocation supplies ONLY
// BRIDGE_GATEWAY_URL and BRIDGE_CREDENTIAL_PATH — no launcher and no runtime
// bounds — and must complete the full begin/approve/exchange/save flow
// against the gateway API over verified https. The plaintext credential is
// saved to the credential file and the device id is printed, but the
// credential itself is never printed.
func TestRunEnrollMinimalConfigOnlyRequiresGatewayAndCredential(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/device/enrollment/begin":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user_code":        "rkuc_minimal",
				"verification_url": "https://gateway.example/enroll?code=rkuc_minimal",
				"expires_in":       600,
			})
		case "/api/v1/device/enrollment/exchange":
			var body struct {
				DeviceCode string `json:"device_code"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.DeviceCode != "rkuc_minimal" {
				http.Error(w, "wrong code", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"device_credential": "rkd_minimal_secret",
				"device_id":         "device-minimal-1",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	credentialPath := filepath.Join(t.TempDir(), "device.credential")
	config, err := appconfig.LoadEnroll(getenvWith(map[string]string{
		// ONLY the two documented variables — no BRIDGE_MCP_LAUNCHER, no
		// BRIDGE_CONNECT_TIMEOUT, none of the runtime bounds.
		"BRIDGE_GATEWAY_URL":     "wss" + strings.TrimPrefix(srv.URL, "https") + "/bridge",
		"BRIDGE_CREDENTIAL_PATH": credentialPath,
	}))
	if err != nil {
		t.Fatalf("LoadEnroll() with only the documented variables: %v", err)
	}

	var out bytes.Buffer
	// srv.Client trusts only this fixture's certificate while still performing
	// normal chain and hostname verification; the positive path never disables
	// TLS verification.
	if err := runEnrollFlow(t.Context(), config, &out, srv.Client()); err != nil {
		t.Fatalf("runEnrollFlow() with only the documented variables: %v", err)
	}

	printed := out.String()
	if !strings.Contains(printed, "rkuc_minimal") {
		t.Fatalf("the user code must be printed for operator approval, got %q", printed)
	}
	if !strings.Contains(printed, "device-minimal-1") {
		t.Fatalf("the enrolled device id must be printed, got %q", printed)
	}
	if strings.Contains(printed, "rkd_minimal_secret") {
		t.Fatal("the plaintext credential must never be printed")
	}
	saved, err := os.ReadFile(credentialPath)
	if err != nil || len(saved) == 0 {
		t.Fatalf("the credential must be saved under BRIDGE_CREDENTIAL_PATH: read err %v, %d bytes", err, len(saved))
	}

	// Production passes nil, which must select the standard verified client.
	// The same self-signed fixture must therefore fail before any credential is
	// persisted at this separate path.
	untrustedCredentialPath := filepath.Join(t.TempDir(), "untrusted-device.credential")
	untrustedConfig := config
	untrustedConfig.CredentialPath = untrustedCredentialPath
	var untrustedOut bytes.Buffer
	err = runEnrollFlow(t.Context(), untrustedConfig, &untrustedOut, nil)
	if err == nil {
		t.Fatal("nil HTTP client must reject the fixture's self-signed certificate")
	}
	var unknownAuthority x509.UnknownAuthorityError
	if !errors.As(err, &unknownAuthority) {
		t.Fatalf("nil HTTP client error = %v, want certificate verification failure", err)
	}
	if _, statErr := os.Stat(untrustedCredentialPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed verified-TLS enrollment must not create credential path; stat err = %v", statErr)
	}
}

// StudioReady runs only after bridgeapp has completed initialize, tools/list,
// and a successful safe read-only tools/call against the official MCP. That
// verified handshake proves exactly one Studio session for this release.
func TestStudioReadyAfterVerifiedMCPCallReportsOneStudio(t *testing.T) {
	count, err := studioReadyAfterVerifiedMCPCall(t.Context())
	if err != nil {
		t.Fatalf("studioReadyAfterVerifiedMCPCall() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("studioReadyAfterVerifiedMCPCall() = %d, want exactly 1", count)
	}
}
