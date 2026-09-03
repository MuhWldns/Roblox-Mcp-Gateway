package statusui

import (
	"fmt"
	"time"
)

// State is a real lifecycle state reported by the Bridge.
type State string

const (
	Initializing       State = "initializing"
	EnrollmentRequired State = "enrollment-required"
	Authenticating     State = "authenticating"
	Connecting         State = "connecting"
	MCPStarting        State = "MCP-starting"
	StudioDetecting    State = "Studio-detecting"
	Connected          State = "connected"
	Reconnecting       State = "reconnecting"
	Degraded           State = "degraded"
	Fatal              State = "fatal"
)

// Event contains the user-safe status data for one lifecycle event.
// InternalDiagnostic is input-only and must never be emitted by Renderer.
type Event struct {
	State              State
	Code               string
	SafeMessage        string
	RetryAfter         time.Duration
	DeviceName         string
	StudioCount        int
	InternalDiagnostic string
}

// Readiness records the dependencies that must both be usable before the
// Bridge may truthfully report Connected.
type Readiness struct {
	Gateway bool
	MCP     bool
}

// Machine validates Bridge lifecycle transitions.
type Machine struct {
	state State
}

// NewMachine returns a machine in the initializing state.
func NewMachine() *Machine {
	return &Machine{state: Initializing}
}

// Transition applies event when it is a legal transition from the current
// state. Connected can only be entered through MarkReady.
func (m *Machine) Transition(event Event) error {
	if m == nil {
		return fmt.Errorf("statusui: transition on nil machine")
	}
	if event.State == Connected {
		return fmt.Errorf("statusui: connected requires readiness")
	}
	if !knownState(event.State) {
		return fmt.Errorf("statusui: unknown state %q", event.State)
	}
	if !allowedTransition(m.state, event.State) {
		return fmt.Errorf("statusui: illegal transition from %q to %q", m.state, event.State)
	}
	m.state = event.State
	return nil
}

// MarkReady enters Connected only while Studio readiness is being detected and
// only after both the gateway and the local MCP are usable.
func (m *Machine) MarkReady(readiness Readiness) error {
	if m == nil {
		return fmt.Errorf("statusui: readiness on nil machine")
	}
	if m.state != StudioDetecting {
		return fmt.Errorf("statusui: readiness is invalid in state %q", m.state)
	}
	if !readiness.Gateway || !readiness.MCP {
		return fmt.Errorf("statusui: gateway and MCP must both be ready")
	}
	m.state = Connected
	return nil
}

func knownState(state State) bool {
	switch state {
	case Initializing, EnrollmentRequired, Authenticating, Connecting,
		MCPStarting, StudioDetecting, Connected, Reconnecting, Degraded, Fatal:
		return true
	default:
		return false
	}
}

func allowedTransition(from, to State) bool {
	if to == Fatal {
		return from != Fatal
	}

	switch from {
	case Initializing:
		return to == EnrollmentRequired || to == Authenticating || to == Connecting
	case EnrollmentRequired:
		return to == Authenticating
	case Authenticating:
		return to == Connecting
	case Connecting:
		return to == MCPStarting || to == Reconnecting || to == Degraded
	case MCPStarting:
		return to == StudioDetecting || to == Reconnecting || to == Degraded
	case StudioDetecting:
		return to == Reconnecting || to == Degraded
	case Connected:
		return to == Reconnecting || to == Degraded
	case Reconnecting:
		return to == Connecting || to == Degraded
	case Degraded:
		return to == Reconnecting || to == Connecting
	case Fatal:
		return false
	default:
		return false
	}
}
