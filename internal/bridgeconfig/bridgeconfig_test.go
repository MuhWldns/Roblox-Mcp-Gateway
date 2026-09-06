package bridgeconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMissingFileIsFirstRun(t *testing.T) {
	config, err := Load(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("Load() on absent file: %v", err)
	}
	if config != (Config{}) {
		t.Fatalf("Load() on absent file = %+v, want zero Config", config)
	}
}

func TestSaveThenLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	want := Config{
		Version:     CurrentVersion,
		GatewayURL:  "wss://mcp.rbxskuy.web.id/bridge",
		DeviceID:    "0e468609-6e77-4f9a-86a8-1c1f3a1a7a01",
		MCPLauncher: `C:\Users\SULASMI\AppData\Local\Roblox\mcp.bat`,
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestSaveIsAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := Save(path, Config{Version: CurrentVersion}); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(): %v", err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temp file survived Save: %s", entry.Name())
		}
	}
}

func TestSaveRefusesForeignSchemaVersion(t *testing.T) {
	err := Save(filepath.Join(t.TempDir(), "config.json"), Config{Version: 99})
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("Save() with version 99 error = %v, want refusal", err)
	}
}

func TestLoadRejectsCorruptPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	_, err := Load(path)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load() corrupt payload error = %v, want ErrCorrupt", err)
	}
}

func TestLoadRejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	payload := `{"version":2,"gateway_url":"wss://future.example/bridge"}`
	if err := os.WriteFile(path, []byte(payload), 0o600); err != nil {
		t.Fatalf("seed newer schema: %v", err)
	}
	_, err := Load(path)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Load() newer schema error = %v, want ErrCorrupt", err)
	}
}

func TestDirRequiresLOCALAPPDATA(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	if _, err := Dir(); err == nil {
		t.Fatal("Dir() without LOCALAPPDATA must fail")
	}

	t.Setenv("LOCALAPPDATA", t.TempDir())
	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir(): %v", err)
	}
	if !strings.HasSuffix(filepath.ToSlash(dir), "/RobloxBridge") {
		t.Fatalf("Dir() = %q, want RobloxBridge suffix", dir)
	}
}
