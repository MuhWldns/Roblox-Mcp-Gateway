package statusui

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Renderer writes concise, plain-text-safe terminal status. It intentionally
// ignores Event.InternalDiagnostic because diagnostic details belong in the
// bounded local log, not the terminal.
type Renderer struct{}

// Render writes the user-facing representation of event.
func (Renderer) Render(w io.Writer, event Event) error {
	if w == nil {
		return fmt.Errorf("statusui: nil writer")
	}

	switch event.State {
	case Initializing:
		return writeString(w, "[1/5] Loading device configuration ...\n")
	case EnrollmentRequired:
		return writeString(w, "ENROLLMENT REQUIRED\nComplete device enrollment to continue.\n")
	case Authenticating:
		return writeString(w, "[2/5] Authenticating licensed device ...\n")
	case Connecting:
		return writeString(w, "[3/5] Connecting to gateway ...\n")
	case MCPStarting:
		return writeString(w, "[4/5] Starting official Roblox MCP ...\n")
	case StudioDetecting:
		return writeString(w, "[5/5] Detecting Studio sessions ...\n")
	case Connected:
		return renderConnected(w, event)
	case Reconnecting:
		return renderReconnecting(w, event)
	case Degraded:
		return renderProblem(w, "SYSTEM DEGRADED", event)
	case Fatal:
		return renderProblem(w, "SYSTEM ERROR", event)
	default:
		return fmt.Errorf("statusui: cannot render unknown state %q", event.State)
	}
}

func renderConnected(w io.Writer, event Event) error {
	studio := fmt.Sprintf("%d sessions connected", event.StudioCount)
	if event.StudioCount == 1 {
		studio = "1 session connected"
	}

	_, err := fmt.Fprintf(w, "SYSTEM CONNECTED\nDevice : %s\nGateway: Connected\nMCP    : Running\nStudio : %s\n\nPress Ctrl+C to stop.\n", sanitizeTerminalField(event.DeviceName), studio)
	return err
}

func renderReconnecting(w io.Writer, event Event) error {
	_, err := fmt.Fprintf(w, "CONNECTION LOST\nRetrying gateway connection in %s ...\n", event.RetryAfter)
	return err
}

func renderProblem(w io.Writer, heading string, event Event) error {
	if _, err := fmt.Fprintf(w, "%s\nCode   : %s\nMessage: %s\n", heading, sanitizeTerminalField(event.Code), sanitizeTerminalField(event.SafeMessage)); err != nil {
		return err
	}
	if event.State != Fatal {
		_, err := fmt.Fprintf(w, "Action : Open Roblox Studio, then retry the connection.\n")
		return err
	}
	_, err := fmt.Fprintf(w, "Action : %s\n", fatalAction(event.Code))
	return err
}

// sanitizeTerminalField is the final output boundary for caller-provided
// fields. Printable runes preserve normal safe text; control runes (including
// newlines and ANSI escape introducers) are omitted so fields cannot inject
// terminal commands or additional status lines.
func sanitizeTerminalField(value string) string {
	var sanitized strings.Builder
	sanitized.Grow(len(value))
	for _, r := range value {
		if unicode.IsPrint(r) {
			sanitized.WriteRune(r)
		}
	}
	return sanitized.String()
}

func fatalAction(code string) string {
	switch code {
	case "MCP_PROCESS_UNAVAILABLE":
		return "Install/repair the official Roblox MCP, then restart Bridge."
	default:
		return "Resolve the reported error, then restart Bridge."
	}
}

func writeString(w io.Writer, text string) error {
	_, err := io.WriteString(w, text)
	return err
}
