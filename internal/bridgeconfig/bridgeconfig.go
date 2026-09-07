// Package bridgeconfig persists the RobloxBridge first-run configuration:
// the non-secret values the wizard learns once and every later run reuses.
// The device credential itself never lives here; it stays in the DPAPI-
// encrypted credential file managed by internal/bridgeapp.
package bridgeconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CurrentVersion is the schema version written by Save. Load refuses files
// from a newer schema so an old binary never misreads a new layout.
const CurrentVersion = 1

// appDirName is the folder created under %LOCALAPPDATA%.
const appDirName = "RobloxBridge"

// ErrCorrupt reports a config file that exists but cannot be parsed as the
// known schema. The operator deletes the file to restart first-run setup.
var ErrCorrupt = errors.New("bridgeconfig: config file is corrupt")

// Config is the persisted first-run state. Every field is non-secret: the
// device credential is stored separately, encrypted with DPAPI.
type Config struct {
	Version     int    `json:"version"`
	GatewayURL  string `json:"gateway_url"`
	DeviceID    string `json:"device_id"`
	MCPLauncher string `json:"mcp_launcher"`
}

// Dir returns the per-user configuration directory under %LOCALAPPDATA%.
// LOCALAPPDATA is deliberate, not a preference: the DPAPI credential blob
// beside this file is bound to one machine, so a Roaming profile would sync
// a config that references an unreadable credential onto every other machine.
func Dir() (string, error) {
	base := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if base == "" {
		return "", errors.New("bridgeconfig: LOCALAPPDATA is not set")
	}
	return filepath.Join(base, appDirName), nil
}

// DefaultPath returns the config file path inside Dir.
func DefaultPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the config file. A missing file yields the zero Config and nil
// error — absent configuration is the normal first-run state, not a failure.
// Any other read error, and a file that does not parse as the known schema,
// returns an error the caller must surface to the operator.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("bridgeconfig: read config: %w", err)
	}
	var config Config
	if err := json.Unmarshal(raw, &config); err != nil {
		return Config{}, fmt.Errorf("%w: %s", ErrCorrupt, path)
	}
	if config.Version > CurrentVersion {
		return Config{}, fmt.Errorf("%w: schema version %d is newer than this binary supports", ErrCorrupt, config.Version)
	}
	return config, nil
}

// Save atomically writes the config: the payload goes to a sibling temp file
// first, then a rename replaces the target, so a crash mid-write can never
// leave a torn config behind.
func Save(path string, config Config) error {
	if config.Version != CurrentVersion {
		return fmt.Errorf("bridgeconfig: refusing to save schema version %d", config.Version)
	}
	payload, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("bridgeconfig: encode config: %w", err)
	}
	payload = append(payload, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("bridgeconfig: create config directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, "config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("bridgeconfig: create temp config: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath) // no-op once the rename below succeeded
	if _, err := temp.Write(payload); err != nil {
		temp.Close()
		return fmt.Errorf("bridgeconfig: write temp config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("bridgeconfig: close temp config: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("bridgeconfig: replace config: %w", err)
	}
	return nil
}
