package statusui

import "testing"

func TestConnectedRequiresRealReadiness(t *testing.T) {
	m := NewMachine()

	requireTransitionError(t, m, Event{State: Connected})
	requireTransition(t, m, Event{State: Connecting})
	requireTransition(t, m, Event{State: MCPStarting})
	requireTransition(t, m, Event{State: StudioDetecting})

	if err := m.MarkReady(Readiness{Gateway: true, MCP: false}); err == nil {
		t.Fatal("MarkReady() succeeded while the local MCP was not ready")
	}
	if err := m.MarkReady(Readiness{Gateway: false, MCP: true}); err == nil {
		t.Fatal("MarkReady() succeeded while the gateway was not ready")
	}
	if err := m.MarkReady(Readiness{Gateway: true, MCP: true}); err != nil {
		t.Fatalf("MarkReady() with gateway and MCP ready returned error: %v", err)
	}

	// Reconnecting is legal only from a genuinely connected machine. This
	// proves that successful readiness moved the machine to Connected.
	requireTransition(t, m, Event{State: Reconnecting})
}

func TestReadinessCanOnlyCompleteStudioDetection(t *testing.T) {
	m := NewMachine()
	if err := m.MarkReady(Readiness{Gateway: true, MCP: true}); err == nil {
		t.Fatal("MarkReady() succeeded before the machine reached StudioDetecting")
	}
}

func TestMachineAllowsEnrollmentStartupAndReconnectPaths(t *testing.T) {
	m := NewMachine()

	for _, event := range []Event{
		{State: EnrollmentRequired},
		{State: Authenticating},
		{State: Connecting},
		{State: MCPStarting},
		{State: StudioDetecting},
	} {
		requireTransition(t, m, event)
	}
	if err := m.MarkReady(Readiness{Gateway: true, MCP: true}); err != nil {
		t.Fatalf("initial MarkReady() returned error: %v", err)
	}

	for _, event := range []Event{
		{State: Reconnecting, RetryAfter: 4},
		{State: Connecting},
		{State: MCPStarting},
		{State: StudioDetecting},
	} {
		requireTransition(t, m, event)
	}
	if err := m.MarkReady(Readiness{Gateway: true, MCP: true}); err != nil {
		t.Fatalf("reconnect MarkReady() returned error: %v", err)
	}
}

func TestMachineAllowsDegradedRecoveryAndFatalTermination(t *testing.T) {
	m := connectedMachine(t)

	requireTransition(t, m, Event{
		State:       Degraded,
		Code:        "STUDIO_SESSION_UNAVAILABLE",
		SafeMessage: "No Roblox Studio session is available.",
	})
	requireTransition(t, m, Event{State: Reconnecting})
	requireTransition(t, m, Event{State: Fatal, Code: "GATEWAY_UNAVAILABLE"})
	requireTransitionError(t, m, Event{State: Reconnecting})
}

func TestMachineRejectsIllegalTransitions(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T) *Machine
		event Event
	}{
		{
			name:  "initializing skips connection",
			setup: func(*testing.T) *Machine { return NewMachine() },
			event: Event{State: MCPStarting},
		},
		{
			name:  "initializing reconnects",
			setup: func(*testing.T) *Machine { return NewMachine() },
			event: Event{State: Reconnecting},
		},
		{
			name: "connecting skips MCP startup",
			setup: func(t *testing.T) *Machine {
				m := NewMachine()
				requireTransition(t, m, Event{State: Connecting})
				return m
			},
			event: Event{State: StudioDetecting},
		},
		{
			name: "Studio detection moves backwards",
			setup: func(t *testing.T) *Machine {
				m := NewMachine()
				requireTransition(t, m, Event{State: Connecting})
				requireTransition(t, m, Event{State: MCPStarting})
				requireTransition(t, m, Event{State: StudioDetecting})
				return m
			},
			event: Event{State: Authenticating},
		},
		{
			name:  "connected moves back to MCP startup",
			setup: connectedMachine,
			event: Event{State: MCPStarting},
		},
		{
			name:  "unknown target state",
			setup: func(*testing.T) *Machine { return NewMachine() },
			event: Event{State: State("not-a-real-state")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requireTransitionError(t, tt.setup(t), tt.event)
		})
	}
}

func connectedMachine(t *testing.T) *Machine {
	t.Helper()
	m := NewMachine()
	requireTransition(t, m, Event{State: Connecting})
	requireTransition(t, m, Event{State: MCPStarting})
	requireTransition(t, m, Event{State: StudioDetecting})
	if err := m.MarkReady(Readiness{Gateway: true, MCP: true}); err != nil {
		t.Fatalf("MarkReady() returned error: %v", err)
	}
	return m
}

func requireTransition(t *testing.T, m *Machine, event Event) {
	t.Helper()
	if err := m.Transition(event); err != nil {
		t.Fatalf("Transition(%q) returned error: %v", event.State, err)
	}
}

func requireTransitionError(t *testing.T, m *Machine, event Event) {
	t.Helper()
	if err := m.Transition(event); err == nil {
		t.Fatalf("Transition(%q) succeeded, want an illegal-transition error", event.State)
	}
}
