package statusui

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
)

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

func TestRendererPrintsCompleteConnectedStatusWithoutDependingOnANSI(t *testing.T) {
	output := renderEvent(t, Event{
		State:       Connected,
		DeviceName:  "DESKTOP-ABC",
		StudioCount: 1,
	})
	plain := ansiEscape.ReplaceAllString(output, "")

	for _, want := range []string{
		"SYSTEM CONNECTED",
		"Device : DESKTOP-ABC",
		"Gateway: Connected",
		"MCP    : Running",
		"Studio : 1 session connected",
		"Press Ctrl+C to stop.",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("plain connected output missing %q\noutput:\n%s", want, plain)
		}
	}
}

func TestRendererPrintsTruthfulStartupPhrases(t *testing.T) {
	tests := []struct {
		name  string
		event Event
		want  string
	}{
		{name: "initializing", event: Event{State: Initializing}, want: "Loading device configuration"},
		{name: "enrollment required", event: Event{State: EnrollmentRequired}, want: "ENROLLMENT REQUIRED"},
		{name: "authenticating", event: Event{State: Authenticating}, want: "Authenticating licensed device"},
		{name: "connecting", event: Event{State: Connecting}, want: "Connecting to gateway"},
		{name: "MCP starting", event: Event{State: MCPStarting}, want: "Starting official Roblox MCP"},
		{name: "Studio detecting", event: Event{State: StudioDetecting}, want: "Detecting Studio sessions"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plain := renderPlainEvent(t, tt.event)
			if !strings.Contains(plain, tt.want) {
				t.Errorf("output for %q missing %q\noutput:\n%s", tt.event.State, tt.want, plain)
			}
		})
	}
}

func TestRendererPrintsReconnectPhraseAndRetryDelay(t *testing.T) {
	plain := renderPlainEvent(t, Event{
		State:      Reconnecting,
		RetryAfter: 4 * time.Second,
	})

	if !strings.Contains(plain, "CONNECTION LOST") {
		t.Errorf("reconnect output missing CONNECTION LOST\noutput:\n%s", plain)
	}
	if !strings.Contains(plain, "Retrying gateway connection in 4s") {
		t.Errorf("reconnect output missing literal 4s retry delay\noutput:\n%s", plain)
	}
}

func TestRendererPrintsDegradedSafeError(t *testing.T) {
	plain := renderPlainEvent(t, Event{
		State:       Degraded,
		Code:        "STUDIO_SESSION_UNAVAILABLE",
		SafeMessage: "No Roblox Studio session is available.",
	})

	for _, want := range []string{
		"SYSTEM DEGRADED",
		"Code   : STUDIO_SESSION_UNAVAILABLE",
		"Message: No Roblox Studio session is available.",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("degraded output missing %q\noutput:\n%s", want, plain)
		}
	}
}

func TestRendererPrintsActionableDegradedRecovery(t *testing.T) {
	plain := renderPlainEvent(t, Event{
		State:       Degraded,
		Code:        "STUDIO_SESSION_UNAVAILABLE",
		SafeMessage: "No Roblox Studio session is available.",
	})

	if !strings.Contains(plain, "Action : Open Roblox Studio, then retry the connection.") {
		t.Errorf("degraded output missing actionable recovery line\noutput:\n%s", plain)
	}
}

func TestRendererSanitizesCallerFieldsAtOutputBoundary(t *testing.T) {
	const hostile = "BAD\nCODE\x1b[31m\x00"
	output := renderEvent(t, Event{
		State:       Connected,
		DeviceName:  hostile,
		StudioCount: 1,
	})
	if strings.Contains(output, hostile) {
		t.Fatalf("connected output emitted hostile device name raw: %q", output)
	}

	output = renderEvent(t, Event{
		State:       Degraded,
		Code:        hostile,
		SafeMessage: hostile,
	})
	if strings.Contains(output, hostile) {
		t.Fatalf("degraded output emitted hostile caller fields raw: %q", output)
	}
	if strings.IndexFunc(strings.ReplaceAll(output, "\n", ""), unicode.IsControl) >= 0 {
		t.Fatalf("renderer output contains injected control characters: %q", output)
	}
}

func TestRendererPrintsCompleteFatalErrorWithSafeCode(t *testing.T) {
	plain := renderPlainEvent(t, Event{
		State:       Fatal,
		Code:        "MCP_PROCESS_UNAVAILABLE",
		SafeMessage: "Official Roblox MCP could not be started.",
	})

	for _, want := range []string{
		"SYSTEM ERROR",
		"Code   : MCP_PROCESS_UNAVAILABLE",
		"Message: Official Roblox MCP could not be started.",
		"Action : Install/repair the official Roblox MCP, then restart Bridge.",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("fatal output missing %q\noutput:\n%s", want, plain)
		}
	}
}

func TestRendererNeverDisclosesInternalDiagnosticOrSecrets(t *testing.T) {
	const internalDiagnostic = "stack trace: panic at relay.go:91; Authorization: Bearer bridge-secret-token; mysql://root:database-password@db/private; raw JSON-RPC params: enrollment-code-7291"

	events := []Event{
		{State: Connected, DeviceName: "DESKTOP-ABC", StudioCount: 1, InternalDiagnostic: internalDiagnostic},
		{State: Reconnecting, RetryAfter: 4 * time.Second, InternalDiagnostic: internalDiagnostic},
		{State: Degraded, Code: "STUDIO_SESSION_UNAVAILABLE", SafeMessage: "No Studio session is available.", InternalDiagnostic: internalDiagnostic},
		{State: Fatal, Code: "MCP_PROCESS_UNAVAILABLE", SafeMessage: "Official Roblox MCP could not be started.", InternalDiagnostic: internalDiagnostic},
	}

	for _, event := range events {
		t.Run(string(event.State), func(t *testing.T) {
			output := renderEvent(t, event)
			for _, forbidden := range []string{
				"stack trace",
				"relay.go:91",
				"bridge-secret-token",
				"database-password",
				"raw JSON-RPC",
				"enrollment-code-7291",
			} {
				if strings.Contains(output, forbidden) {
					t.Errorf("terminal output disclosed %q\noutput:\n%s", forbidden, output)
				}
			}
		})
	}
}

func renderPlainEvent(t *testing.T, event Event) string {
	t.Helper()
	return ansiEscape.ReplaceAllString(renderEvent(t, event), "")
}

func renderEvent(t *testing.T, event Event) string {
	t.Helper()
	var output bytes.Buffer
	if err := (Renderer{}).Render(&output, event); err != nil {
		t.Fatalf("Render(%q) returned error: %v", event.State, err)
	}
	return output.String()
}
