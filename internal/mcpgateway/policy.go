package mcpgateway

import (
	"fmt"
	"strings"

	"robloxkit/internal/mcpoauth"
)

// ToolScopeVersion pins the normalized tool-to-scope mapping. Bump it when
// the official Roblox Studio MCP catalog changes; the external ChatGPT and
// Claude gate re-validates the mapping against the real Studio MCP before
// deployment.
const ToolScopeVersion = 1

// officialToolScopes maps normalized official Roblox Studio MCP tool names
// to the connector scope a grant must carry to see and call them. Names are
// the official snake_case identifiers; anything absent is default-deny.
var officialToolScopes = map[string]string{
	// Read-only inspection.
	"get_instance_tree":       mcpoauth.ScopeStudioRead,
	"get_instance_properties": mcpoauth.ScopeStudioRead,
	"get_selected_instances":  mcpoauth.ScopeStudioRead,
	"get_script_content":      mcpoauth.ScopeStudioRead,
	"get_studio_state":        mcpoauth.ScopeStudioRead,
	"query_instances":         mcpoauth.ScopeStudioRead,

	// Place and script editing.
	"set_instance_properties": mcpoauth.ScopeStudioEdit,
	"set_script_content":      mcpoauth.ScopeStudioEdit,
	"create_script":           mcpoauth.ScopeStudioEdit,
	"insert_instance":         mcpoauth.ScopeStudioEdit,
	"delete_instance":         mcpoauth.ScopeStudioEdit,
	"rename_instance":         mcpoauth.ScopeStudioEdit,

	// Playtest lifecycle.
	"run_playtest":       mcpoauth.ScopeStudioPlay,
	"stop_playtest":      mcpoauth.ScopeStudioPlay,
	"get_playtest_state": mcpoauth.ScopeStudioPlay,

	// Simulated input during playtests.
	"send_playtest_input": mcpoauth.ScopeStudioInput,
	"send_input":          mcpoauth.ScopeStudioInput,

	// Asset pipeline operations.
	"insert_asset": mcpoauth.ScopeStudioAsset,
	"upload_asset": mcpoauth.ScopeStudioAsset,
	"get_asset":    mcpoauth.ScopeStudioAsset,

	// Script execution.
	"execute_lua": mcpoauth.ScopeStudioExec,
	"run_script":  mcpoauth.ScopeStudioExec,
}

// Policy answers which scope a tool requires. The zero value is a complete
// policy; the mapping is the versioned table above.
type Policy struct{}

// RequiredScope maps a tool name to the scope a grant must carry. Names are
// normalized (trimmed, lowercased) before lookup; any name without a
// mapping returns allowed=false and is default-deny.
func (p Policy) RequiredScope(toolName string) (scope string, allowed bool) {
	scope, allowed = officialToolScopes[normalizeToolName(toolName)]
	return scope, allowed
}

// normalizeToolName folds case and whitespace variants of the canonical
// snake_case official names. It never invents names: unmapped tools stay
// denied.
func normalizeToolName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// scopeAllowed reports whether the granted scope set contains want.
func scopeAllowed(granted []string, want string) bool {
	for _, scope := range granted {
		if scope == want {
			return true
		}
	}
	return false
}

// unknownToolMessage builds the client-safe denial for an unmapped tool.
// Only the client's own (bounded) identifier is ever echoed back.
func unknownToolMessage(toolName string) string {
	trimmed := normalizeToolName(toolName)
	if trimmed == "" {
		return "unknown tool"
	}
	if len(trimmed) > 96 {
		trimmed = trimmed[:96]
	}
	return fmt.Sprintf("unknown tool %q", trimmed)
}
