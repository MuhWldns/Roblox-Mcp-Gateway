package main

// The smart first-run flow: double-clicking RobloxBridge.exe (no
// BRIDGE_MODE set) either runs with the previously saved configuration or
// walks the operator through a one-time setup wizard in the terminal. The
// wizard detects the official Roblox MCP launcher, performs device
// enrollment against the gateway, opens the approval page in the browser
// automatically, and persists the non-secret configuration for every later
// run. The device credential itself never lands in the config file; it
// stays in the DPAPI-protected credential store.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"robloxkit/internal/bridgeapp"
	"robloxkit/internal/bridgeconfig"
	"robloxkit/internal/mcpprocess"
	"robloxkit/internal/statusui"
)

// defaultGatewayURL is the production gateway the wizard pairs with. The
// BRIDGE_GATEWAY_URL environment variable overrides it for testing and
// staging; every other BRIDGE_* variable stays reserved for the explicit
// modes.
const defaultGatewayURL = "wss://mcp.rbxskuy.web.id/bridge"

// launcherCandidates are the well-known install locations of the official
// Roblox Studio MCP launcher, probed in order. The list covers the standard
// per-user install; anything else the operator types in manually.
var launcherCandidates = []string{
	`%LOCALAPPDATA%\Roblox\mcp.bat`,
	`%LOCALAPPDATA%\Roblox\RobloxStudioMCP.bat`,
	`%APPDATA%\Roblox\mcp.bat`,
}

// firstRunDeps isolates the environment side effects the wizard performs so
// tests can fake them: browser opening and terminal input. Every field is
// required; runSmart wires the real implementations.
type firstRunDeps struct {
	stdin       io.Reader
	stdout      io.Writer
	openBrowser func(rawURL string) error
	// httpClient is nil in production, which selects the default fully
	// verified transport. Tests inject a client trusting their fixture
	// certificate; production never relaxes TLS.
	httpClient *http.Client
}

// runSmart is the default double-click mode: load the saved configuration,
// complete the wizard when it is missing or incomplete, then hand over to
// the remote bridge run with the runtime bounds left at the bridgeapp
// defaults.
func runSmart(ctx context.Context) {
	if err := runSmartFlow(ctx, firstRunDeps{
		stdin:       os.Stdin,
		stdout:      os.Stdout,
		openBrowser: openInDefaultBrowser,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "RobloxBridge failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Press Enter to close this window…")
		fmt.Scanln()
		os.Exit(1)
	}
}

// runSmartFlow executes the first-run decision tree. Terminal failures are
// returned, never swallowed: a wrong launcher path or a rejected enrollment
// must stop the run, not silently degrade.
func runSmartFlow(ctx context.Context, deps firstRunDeps) error {
	dir, err := bridgeconfig.Dir()
	if err != nil {
		return fmt.Errorf("locate configuration directory: %w", err)
	}
	configPath := filepath.Join(dir, "config.json")
	config, err := bridgeconfig.Load(configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w (delete %s to restart setup)", err, configPath)
	}
	credentialPath := filepath.Join(dir, "device.credential")

	if config.GatewayURL == "" || config.DeviceID == "" || config.MCPLauncher == "" {
		config, err = runWizard(ctx, deps, configPath, credentialPath, config)
		if err != nil {
			return err
		}
	} else {
		fmt.Fprintf(deps.stdout, "RobloxBridge — device %s\n", config.DeviceID)
	}

	store, err := bridgeapp.NewFileCredentialStore(credentialPath)
	if err != nil {
		return fmt.Errorf("open credential store: %w", err)
	}
	// A missing credential file means this identity lost its enrollment (a
	// deleted file or a new Windows user): re-run enrollment against the
	// SAME device id so the dashboard keeps recognizing the machine.
	if _, loadErr := store.Load(); loadErr != nil {
		if !errors.Is(loadErr, os.ErrNotExist) {
			return fmt.Errorf("read device credential: %w", loadErr)
		}
		fmt.Fprintln(deps.stdout, "Device credential is missing; starting re-enrollment for this device.")
		if err := runEnrollment(ctx, deps, config.GatewayURL, config.DeviceID, credentialPath); err != nil {
			return err
		}
	}

	command, err := mcpprocess.NewLauncher(config.MCPLauncher).Resolve()
	if err != nil {
		return fmt.Errorf("resolve MCP launcher %q: %w", config.MCPLauncher, err)
	}

	// Zero bounds select the bridgeapp defaults (10s timeouts, queue 64,
	// 1 MiB envelopes); the wizard contract intentionally has no knobs.
	return bridgeapp.RunRemote(ctx, bridgeapp.RemoteDeps{
		Machine: statusui.NewMachine(),
		NewProcess: func() mcpprocess.Process {
			return mcpprocess.NewProcess(command, mcpprocess.Options{})
		},
		Credential:    store,
		GatewayURL:    config.GatewayURL,
		DeviceID:      config.DeviceID,
		DeviceName:    hostname(),
		Output:        deps.stdout,
		StudioReady:   studioReadyAfterVerifiedMCPCall,
		BridgeVersion: bridgeVersion,
	})
}

// runWizard completes a partial configuration: detect (or ask for) the MCP
// launcher, pick the gateway URL, enroll the device, and save the result.
// Previously saved fields are reused when valid, so a wizard re-run after a
// failed enrollment never discards operator input.
func runWizard(ctx context.Context, deps firstRunDeps, configPath, credentialPath string, saved bridgeconfig.Config) (bridgeconfig.Config, error) {
	fmt.Fprintln(deps.stdout, "RobloxBridge first-time setup")
	fmt.Fprintln(deps.stdout, "=============================")

	gatewayURL := saved.GatewayURL
	if gatewayURL == "" {
		gatewayURL = strings.TrimSpace(os.Getenv("BRIDGE_GATEWAY_URL"))
	}
	if gatewayURL == "" {
		gatewayURL = defaultGatewayURL
	}
	if _, err := gatewayAPIOrigin(gatewayURL); err != nil {
		return bridgeconfig.Config{}, fmt.Errorf("gateway URL: %w", err)
	}

	launcher := saved.MCPLauncher
	if launcher == "" {
		detected := detectLauncher()
		if detected != "" {
			launcher = detected
			fmt.Fprintf(deps.stdout, "Roblox MCP launcher found: %s\n", launcher)
		} else {
			fmt.Fprintln(deps.stdout, "The official Roblox Studio MCP launcher was not found automatically.")
			fmt.Fprintln(deps.stdout, "Enter the full path to the launcher (for example C:\\Users\\me\\AppData\\Local\\Roblox\\mcp.bat):")
			line, err := promptLine(deps.stdin, deps.stdout)
			if err != nil {
				return bridgeconfig.Config{}, fmt.Errorf("read launcher path: %w", err)
			}
			launcher = line
		}
	}
	// Validate before saving: Resolve canonicalizes and requires a real,
	// local, regular file, so a typo can never be persisted as "configured".
	if _, err := mcpprocess.NewLauncher(launcher).Resolve(); err != nil {
		return bridgeconfig.Config{}, fmt.Errorf("MCP launcher %q: %w", launcher, err)
	}

	deviceID := saved.DeviceID
	if deviceID == "" {
		deviceID = newEnrollDeviceID()
	}
	// Persist the installation identity BEFORE the exchange. If the process
	// dies between the gateway commit and the token response (or the token
	// response is lost), the next run reuses this device id and the server
	// re-claim path mints a fresh credential for the same device.
	pending := bridgeconfig.Config{
		Version:     bridgeconfig.CurrentVersion,
		GatewayURL:  gatewayURL,
		DeviceID:    deviceID,
		MCPLauncher: launcher,
	}
	if err := bridgeconfig.Save(configPath, pending); err != nil {
		return bridgeconfig.Config{}, fmt.Errorf("save configuration: %w", err)
	}
	if err := runEnrollment(ctx, deps, gatewayURL, deviceID, credentialPath); err != nil {
		return bridgeconfig.Config{}, err
	}
	fmt.Fprintln(deps.stdout, "Setup complete. This PC is now linked to your RobloxKit account.")
	return pending, nil
}

// runEnrollment drives one enrollment pass through the shared runEnrollFlow,
// adding the browser hook. It reuses the committed enrollment pipeline —
// begin, approve, exchange, DPAPI save — unchanged.
func runEnrollment(ctx context.Context, deps firstRunDeps, gatewayURL, deviceID, credentialPath string) error {
	origin, err := gatewayAPIOrigin(gatewayURL)
	if err != nil {
		return err
	}
	store, err := bridgeapp.NewFileCredentialStore(credentialPath)
	if err != nil {
		return err
	}
	// The DPAPI store writes the blob directly; the smart flow owns the
	// directory. The installer provisions its own ProgramData layout, so
	// this only affects wizard-managed per-user storage.
	if err := os.MkdirAll(filepath.Dir(credentialPath), 0o755); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	enrollConfig := bridgeapp.EnrollConfig{
		APIBaseURL:    origin,
		DeviceID:      deviceID,
		HTTPClient:    deps.httpClient,
		DeviceName:    hostname() + " (RobloxBridge)",
		Hostname:      hostname(),
		Platform:      runtime.GOOS,
		BridgeVersion: bridgeVersion,
		Credential:    store,
		Output:        deps.stdout,
		OnVerificationURL: func(rawURL string) {
			fmt.Fprintln(deps.stdout, "Opening your browser for approval…")
			if err := deps.openBrowser(rawURL); err != nil {
				fmt.Fprintf(deps.stdout, "If it did not open, open this URL manually: %s\n", rawURL)
			}
		},
	}
	return bridgeapp.RunEnroll(ctx, enrollConfig)
}

// detectLauncher probes the well-known launcher locations and returns the
// first existing file, or "" when none match.
func detectLauncher() string {
	for _, candidate := range launcherCandidates {
		expanded, err := expandEnvPath(candidate)
		if err != nil {
			continue
		}
		if info, err := os.Stat(expanded); err == nil && info.Mode().IsRegular() {
			return expanded
		}
	}
	return ""
}

// expandEnvPath resolves a %VAR% Windows path template. Go's os.Expand only
// understands $VAR, so the percent form is parsed here: an unterminated
// reference or an unset variable fails the candidate instead of producing a
// literal-percent path that can never exist.
func expandEnvPath(path string) (string, error) {
	if !strings.Contains(path, "%") {
		return path, nil
	}
	var builder strings.Builder
	for {
		open := strings.IndexByte(path, '%')
		if open < 0 {
			builder.WriteString(path)
			break
		}
		builder.WriteString(path[:open])
		rest := path[open+1:]
		close := strings.IndexByte(rest, '%')
		if close < 0 {
			return "", fmt.Errorf("unterminated %%VAR%% in path %q", path)
		}
		name := rest[:close]
		value := os.Getenv(name)
		if value == "" {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		builder.WriteString(value)
		path = rest[close+1:]
	}
	return builder.String(), nil
}

// promptLine reads one trimmed non-empty line from the wizard input.
func promptLine(stdin io.Reader, stdout io.Writer) (string, error) {
	reader := bufio.NewReader(stdin)
	for {
		fmt.Fprint(stdout, "> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimSpace(line)
		if line != "" {
			return line, nil
		}
		fmt.Fprintln(stdout, "Please enter a value.")
	}
}

// openInDefaultBrowser opens the approval page with the shell association on
// Windows and the well-known openers elsewhere. It never relaxes anything:
// the URL arrives verbatim from the gateway begin response.
func openInDefaultBrowser(rawURL string) error {
	if runtime.GOOS == "windows" {
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", rawURL).Start()
	}
	for _, opener := range []string{"xdg-open", "open"} {
		if path, err := exec.LookPath(opener); err == nil {
			return exec.Command(path, rawURL).Start()
		}
	}
	return errors.New("no browser opener available")
}
