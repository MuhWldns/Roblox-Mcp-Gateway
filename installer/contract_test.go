// Package installer pins the generated Inno Setup contract in source form:
// the one REG_MULTI_SZ service-environment block (the only shape the service
// control manager appends to a service process environment), the bounded
// stop-wait-delete uninstall sequence, and the product-files-only scope.
// ISCC.exe is unavailable on every build machine by default, so these text
// assertions are the committed compile-time guard.
package installer

import (
	"os"
	"strings"
	"testing"
)

const issuerPath = "RobloxBridge.iss"

// environmentBlock extracts the ServiceEnvironment Result expression (the
// Inno 6 RegWriteMultiStringValue data: ONE String whose entries are joined
// by #0 — the official example form, with no dangling terminator).
func environmentBlock(source string) string {
	start := strings.Index(source, "function ServiceEnvironment: string;")
	if start < 0 {
		return ""
	}
	begin := strings.Index(source[start:], "Result :=")
	if begin < 0 {
		return ""
	}
	begin += start
	end := strings.Index(source[begin:], ";")
	if end < 0 {
		return ""
	}
	return source[begin : begin+end]
}

func issuerSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(issuerPath)
	if err != nil {
		t.Fatalf("read %s: %v", issuerPath, err)
	}
	return string(raw)
}

// The SCM populates a service process environment from ONE REG_MULTI_SZ
// value named "Environment" on the service key. Every required key must be
// present in that single block.
func TestInstallerWritesOneServiceEnvironmentMultiSZ(t *testing.T) {
	source := issuerSource(t)
	if strings.Contains(source, "array of string") {
		t.Fatal("RegWriteMultiStringValue takes a single String, not an array of string — build the REG_MULTI_SZ data as a #0-joined String (Inno 6 API)")
	}
	if !strings.Contains(source, "function ServiceEnvironment: string;") {
		t.Fatal("ServiceEnvironment must return a single String for RegWriteMultiStringValue")
	}
	if !strings.Contains(source, `'Environment', ServiceEnvironment()`) {
		t.Fatal("the multi-string value must be named Environment on the service key")
	}
	required := []string{
		"BRIDGE_MODE=service",
		"BRIDGE_GATEWAY_URL=",
		"BRIDGE_CREDENTIAL_PATH=",
		"BRIDGE_MCP_LAUNCHER=",
		"BRIDGE_CONNECT_TIMEOUT=",
		"BRIDGE_HEARTBEAT_INTERVAL=",
		"BRIDGE_RESPONSE_TIMEOUT=",
		"BRIDGE_QUEUE_LIMIT=",
		"BRIDGE_MAX_MESSAGE_BYTES=",
		"BRIDGE_SERVICE_LOG=",
	}
	for _, key := range required {
		if !strings.Contains(source, key) {
			t.Fatalf("the Environment multi-string block is missing %q", key)
		}
	}
	block := environmentBlock(source)
	if block == "" {
		t.Fatal("ServiceEnvironment must build the REG_MULTI_SZ data as one String expression")
	}
	if got := strings.Count(block, "+ #0 +"); got < 9 {
		t.Fatalf("the 10 environment entries must be joined by #0 in one String (found %d joins)", got)
	}
	// BRIDGE_DEVICE_ID is deliberately absent: it exists only after
	// enrollment and is appended by the documented post-enrollment step.
	if strings.Contains(block, "BRIDGE_DEVICE_ID") {
		t.Fatal("BRIDGE_DEVICE_ID must not be an installed Environment entry — it is appended post-enrollment")
	}
}

// The uninstall must stop the service, wait BOUNDED until the SCM reports
// Stopped, fail visibly on timeout, and delete the registration only
// afterwards — in that order. A service that does not exist (Win32 1060)
// must let the uninstall continue: the absence is carried out of the stop
// step, the delete is skipped for an absent service, and a delete racing to
// 1060 is tolerated as success.
func TestInstallerUninstallStopsWaitsFailsThenDeletes(t *testing.T) {
	source := issuerSource(t)
	if !strings.Contains(source, "function ServiceStopAndWait: Boolean;") {
		t.Fatal("ServiceStopAndWait must return the service-existence boolean so the uninstall can continue when the service is absent")
	}
	stopIdx := strings.Index(source, "'stop ' + ServiceName")
	if stopIdx < 0 {
		t.Fatal("the uninstall must stop the service")
	}
	waitIdx := strings.Index(source, "WaitForStatus")
	if waitIdx < 0 {
		t.Fatal("the uninstall must wait for Stopped through ServiceController.WaitForStatus")
	}
	timeoutIdx := strings.Index(source, "FromSeconds")
	if timeoutIdx < 0 {
		t.Fatal("the wait must be bounded")
	}
	failIdx := strings.Index(source, "uninstall aborted")
	if failIdx < 0 {
		t.Fatal("a stop timeout must visibly abort the uninstall")
	}
	deleteIdx := strings.Index(source, "'delete ' + ServiceName")
	if deleteIdx < 0 {
		t.Fatal("the uninstall must delete the service registration")
	}
	if !(stopIdx < waitIdx && waitIdx < failIdx && failIdx < deleteIdx) {
		t.Fatalf("uninstall order broken: stop@%d wait@%d abort@%d delete@%d", stopIdx, waitIdx, failIdx, deleteIdx)
	}
	// Absent-service branch: the delete runs only when the service existed,
	// and a delete that races to 1060 still succeeds.
	afterStop := source[stopIdx:]
	guardIdx := strings.Index(afterStop, "if ServiceExists then")
	if guardIdx < 0 {
		t.Fatal("the delete must be guarded by the carried service-existence boolean (absent service must uninstall successfully)")
	}
	if strings.Index(afterStop, "ResultCode <> 1060") < 0 {
		t.Fatal("sc delete returning 1060 (service already gone) must be tolerated as success")
	}
}

// The installer ships the Bridge binary and product files only — never the
// official Roblox MCP, Node, or Electron.
func TestInstallerShipsProductFilesOnly(t *testing.T) {
	source := issuerSource(t)
	for _, line := range strings.Split(source, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Source:") {
			continue
		}
		lower := strings.ToLower(trimmed)
		for _, forbidden := range []string{"node.exe", "electron", "node_modules", "mcp.exe"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("forbidden bundled file in [Files]: %s", trimmed)
			}
		}
	}
	if !strings.Contains(source, "official Roblox MCP") {
		t.Fatal("the scope guard must name the never-bundled official Roblox MCP")
	}
	if !strings.Contains(strings.ToLower(source), "never frees the server-side license slot") {
		t.Fatal("the uninstall must document that it never frees the server-side license slot")
	}
}
