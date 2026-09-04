package mcpgateway

import (
	"strings"
	"testing"

	"robloxkit/internal/mcpoauth"
)

func TestPolicyRequiredScopeMapsOfficialTools(t *testing.T) {
	tests := []struct {
		tool  string
		scope string
	}{
		{"get_instance_tree", mcpoauth.ScopeStudioRead},
		{"get_instance_properties", mcpoauth.ScopeStudioRead},
		{"get_script_content", mcpoauth.ScopeStudioRead},
		{"query_instances", mcpoauth.ScopeStudioRead},
		{"get_studio_state", mcpoauth.ScopeStudioRead},
		{"set_instance_properties", mcpoauth.ScopeStudioEdit},
		{"set_script_content", mcpoauth.ScopeStudioEdit},
		{"create_script", mcpoauth.ScopeStudioEdit},
		{"insert_instance", mcpoauth.ScopeStudioEdit},
		{"delete_instance", mcpoauth.ScopeStudioEdit},
		{"run_playtest", mcpoauth.ScopeStudioPlay},
		{"stop_playtest", mcpoauth.ScopeStudioPlay},
		{"send_playtest_input", mcpoauth.ScopeStudioInput},
		{"insert_asset", mcpoauth.ScopeStudioAsset},
		{"upload_asset", mcpoauth.ScopeStudioAsset},
		{"execute_lua", mcpoauth.ScopeStudioExec},
	}
	policy := Policy{}
	for _, tt := range tests {
		scope, allowed := policy.RequiredScope(tt.tool)
		if !allowed {
			t.Errorf("RequiredScope(%q) = allowed=false, want scope %q", tt.tool, tt.scope)
			continue
		}
		if scope != tt.scope {
			t.Errorf("RequiredScope(%q) = scope %q, want %q", tt.tool, scope, tt.scope)
		}
	}
}

func TestPolicyRequiredScopeNormalizesToolNames(t *testing.T) {
	policy := Policy{}
	canonical, allowed := policy.RequiredScope("get_instance_tree")
	if !allowed {
		t.Fatal("canonical tool name must be mapped")
	}
	// Normalization folds case and whitespace variants of the canonical
	// snake_case names; it never invents new names, so camelCase guesses
	// such as "GetInstanceTree" stay default-deny.
	for _, variant := range []string{"  get_instance_tree  ", "GET_INSTANCE_TREE", "Get_Instance_Tree"} {
		scope, ok := policy.RequiredScope(variant)
		if !ok {
			t.Errorf("RequiredScope(%q) = allowed=false, want %q", variant, canonical)
			continue
		}
		if scope != canonical {
			t.Errorf("RequiredScope(%q) = %q, want %q", variant, scope, canonical)
		}
	}
	if _, ok := policy.RequiredScope("GetInstanceTree"); ok {
		t.Error("RequiredScope(GetInstanceTree) must stay default-deny; normalization never invents names")
	}
}

func TestPolicyRequiredScopeDeniesUnknownTools(t *testing.T) {
	policy := Policy{}
	for _, name := range []string{"", "shell", "read_file", "arbitrary_os_command", "get_instance_tree_v0"} {
		if scope, allowed := policy.RequiredScope(name); allowed {
			t.Errorf("RequiredScope(%q) = scope %q, allowed=true; want default-deny", name, scope)
		}
	}
}

func TestPolicyScopeMappingIsVersioned(t *testing.T) {
	if ToolScopeVersion < 1 {
		t.Fatalf("ToolScopeVersion = %d, want at least 1", ToolScopeVersion)
	}
}

func TestPolicyCoversEveryEnforcedStudioScope(t *testing.T) {
	enforced := map[string][]string{}
	for tool, scope := range officialToolScopes {
		enforced[scope] = append(enforced[scope], tool)
	}
	for _, scope := range mcpoauth.SupportedScopes {
		if scope == mcpoauth.ScopeConnect {
			continue
		}
		if len(enforced[scope]) == 0 {
			t.Errorf("scope %q has no mapped tool names", scope)
		}
	}
}

func TestPolicyScopeAllowedReportsGrantMembership(t *testing.T) {
	tests := []struct {
		name   string
		scopes []string
		want   bool
	}{
		{"member", []string{mcpoauth.ScopeConnect, mcpoauth.ScopeStudioRead}, true},
		{"absent", []string{mcpoauth.ScopeConnect}, false},
		{"empty grant", nil, false},
	}
	for _, tt := range tests {
		if got := scopeAllowed(tt.scopes, mcpoauth.ScopeStudioRead); got != tt.want {
			t.Errorf("%s: scopeAllowed(%v, studio:read) = %v, want %v", tt.name, tt.scopes, got, tt.want)
		}
	}
}

func TestPolicyUnknownToolNameIsSanitized(t *testing.T) {
	// The denial message never reflects internal state; it names only the
	// client's own tool identifier, truncated to a bounded length.
	long := strings.Repeat("x", 400)
	msg := unknownToolMessage(long)
	if len(msg) > 160 {
		t.Fatalf("unknown-tool message length %d leaks the raw name", len(msg))
	}
	if !strings.Contains(msg, "unknown tool") {
		t.Fatalf("unknown-tool message %q lost its meaning", msg)
	}
}
