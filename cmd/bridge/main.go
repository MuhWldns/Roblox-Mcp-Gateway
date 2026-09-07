package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"robloxkit/internal/appconfig"
	"robloxkit/internal/bridgeapp"
	"robloxkit/internal/bridgeconfig"
	"robloxkit/internal/mcpprocess"
	"robloxkit/internal/statusui"
)

const bridgeVersion = "0.1.0"

// Bridge run modes. The default double-click mode is smart: it runs from the
// saved configuration or completes the first-run wizard. The explicit env
// contract is unchanged: BRIDGE_MODE=remote selects the remote gateway mode,
// local stays local, and service mode is selected explicitly
// (BRIDGE_MODE=service) or automatically when the Windows service control
// manager launched the process; under the service control manager the
// detection is authoritative, because a non-service run mode could never
// report service status and would leave the SCM hanging.
const (
	bridgeModeLocal   = "local"
	bridgeModeRemote  = "remote"
	bridgeModeService = "service"
	bridgeModeEnroll  = "enroll"
	bridgeModeSmart   = ""
)

// serviceLogEnv overrides the structured service log location; the default
// lives under ProgramData so a LocalSystem service process can write it.
const serviceLogEnv = "BRIDGE_SERVICE_LOG"

func main() {
	switch resolveBridgeMode(os.Getenv, bridgeapp.IsWindowsService()) {
	case bridgeModeService:
		runService()
	case bridgeModeRemote:
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		runRemote(ctx)
	case bridgeModeEnroll:
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		runEnroll(ctx)
	default:
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		runSmart(ctx)
	}
}

// resolveBridgeMode keeps the explicit selection contract and adds the smart
// default: BRIDGE_MODE=service selects service mode, a process launched by
// the Windows service control manager always runs in service mode,
// BRIDGE_MODE=remote stays remote, local stays local, and anything else (or
// unset) runs the smart first-run flow.
func resolveBridgeMode(getenv func(string) string, inWindowsService bool) string {
	if inWindowsService {
		return bridgeModeService
	}
	switch strings.ToLower(strings.TrimSpace(getenv("BRIDGE_MODE"))) {
	case bridgeModeService:
		return bridgeModeService
	case bridgeModeEnroll:
		return bridgeModeEnroll
	case bridgeModeLocal:
		return bridgeModeLocal
	case bridgeModeRemote:
		return bridgeModeRemote
	default:
		return bridgeModeSmart
	}
}

func runLocal(ctx context.Context) {
	launcherPath := os.Getenv("BRIDGE_MCP_LAUNCHER")
	if launcherPath == "" {
		config, err := appconfig.LoadBridge(os.Getenv)
		if err != nil {
			log.Printf("startup failed: %v", err)
			os.Exit(1)
		}
		launcherPath = config.MCPLauncherPath
	}

	command, err := mcpprocess.NewLauncher(launcherPath).Resolve()
	if err != nil {
		log.Printf("startup failed: %v", err)
		os.Exit(1)
	}

	process := mcpprocess.NewProcess(command, mcpprocess.Options{})
	err = bridgeapp.RunLocal(ctx, bridgeapp.LocalDeps{
		Machine:     statusui.NewMachine(),
		Process:     process,
		Output:      os.Stdout,
		DeviceName:  hostname(),
		StudioReady: studioReadyAfterVerifiedMCPCall,
	})

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("bridge stopped: %v", err)
		os.Exit(1)
	}
}

// runRemote connects the Bridge to the authenticated gateway WSS hub. All
// configuration follows the appconfig conventions: BRIDGE_GATEWAY_URL,
// BRIDGE_CREDENTIAL_PATH, BRIDGE_MCP_LAUNCHER, and the optional bounds.
// BRIDGE_DEVICE_ID identifies the enrolled device whose credential the secure
// store holds.
func runRemote(ctx context.Context) {
	deps, err := newRemoteDeps(nil)
	if err != nil {
		log.Printf("startup failed: %v", err)
		os.Exit(1)
	}

	err = bridgeapp.RunRemote(ctx, deps)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("bridge stopped: %v", err)
		os.Exit(1)
	}
}

// runService runs the Bridge under the Windows service control manager. The
// service always drives the remote bridge run — the enrolled device talking
// to the gateway hub — and reports every status event to the structured
// local service log plus the supervisor. Startup failures exit non-zero
// before the service is registered, which the SCM reports as a failed start.
func runService() {
	logWriter, err := openServiceLog()
	if err != nil {
		log.Printf("startup failed: %v", err)
		os.Exit(1)
	}
	defer logWriter.Close()

	err = bridgeapp.RunService(bridgeapp.ServiceDeps{
		Name: bridgeapp.ServiceName,
		Run: func(ctx context.Context, sink func(statusui.Event) error) error {
			deps, err := newRemoteDeps(sink)
			if err != nil {
				return err
			}
			return bridgeapp.RunRemote(ctx, deps)
		},
		Log: logWriter,
	})
	if err != nil {
		log.Printf("service failed: %v", err)
		os.Exit(1)
	}
}

// runEnroll performs the interactive device enrollment against the gateway
// web API: it claims this device's identity, prints the verification URL and
// user code for the operator to approve, polls the code exchange, saves the
// returned credential under the current identity (interactive shell or
// service account — see docs/operations/windows-bridge.md), and prints the
// enrolled device id. The plaintext credential is never printed.
//
// Enrollment is a clean-install operation: it needs ONLY BRIDGE_GATEWAY_URL
// and BRIDGE_CREDENTIAL_PATH (appconfig.LoadEnroll) — no MCP launcher and no
// runtime bounds.
func runEnroll(ctx context.Context) {
	config, err := appconfig.LoadEnroll(os.Getenv)
	if err != nil {
		log.Printf("startup failed: %v", err)
		os.Exit(1)
	}
	if err := runEnrollFlow(ctx, config, os.Stdout, nil); err != nil {
		log.Printf("enrollment failed: %v", err)
		os.Exit(1)
	}
}

// runEnrollFlow drives the enrollment with an already-loaded minimal
// configuration. A nil httpClient keeps the default VERIFIED transport —
// production never relaxes TLS; tests inject a client that trusts their
// local fixture certificate.
func runEnrollFlow(ctx context.Context, config appconfig.Enroll, out io.Writer, httpClient *http.Client) error {
	origin, err := gatewayAPIOrigin(config.GatewayURL.String())
	if err != nil {
		return err
	}
	store, err := bridgeapp.NewFileCredentialStore(config.CredentialPath)
	if err != nil {
		return err
	}
	// The device id is the installation identity: once a credential exists
	// for it, re-running enroll would mint a second id, orphan the old
	// device row, and invite accidental rotations. Refuse instead — the
	// smart mode re-enrolls with the SAME id, and explicit rotation goes
	// through the dashboard.
	if _, err := store.Load(); err == nil {
		return errors.New("device credential already exists; delete it (or use the smart first-run flow) to re-enroll this device")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read device credential: %w", err)
	}
	// Reuse the wizard's device id when one was saved, so every mode on
	// this installation claims the same device. A fresh installation
	// generates a new id and persists it BEFORE the exchange, so a lost
	// token response cannot orphan the identity.
	configPath, err := bridgeconfig.DefaultPath()
	if err != nil {
		return fmt.Errorf("locate configuration: %w", err)
	}
	saved, err := bridgeconfig.Load(configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w (delete %s to restart setup)", err, configPath)
	}
	deviceID := saved.DeviceID
	if deviceID == "" {
		deviceID = newEnrollDeviceID()
	}
	if saved.GatewayURL == "" || saved.DeviceID == "" || saved.MCPLauncher == "" {
		saved = bridgeconfig.Config{
			Version:     bridgeconfig.CurrentVersion,
			GatewayURL:  saved.GatewayURL,
			DeviceID:    deviceID,
			MCPLauncher: saved.MCPLauncher,
		}
		if err := bridgeconfig.Save(configPath, saved); err != nil {
			return fmt.Errorf("save configuration: %w", err)
		}
	}
	return bridgeapp.RunEnroll(ctx, bridgeapp.EnrollConfig{
		APIBaseURL:    origin,
		DeviceID:      deviceID,
		DeviceName:    hostname() + " (RobloxBridge)",
		Hostname:      hostname(),
		Platform:      runtime.GOOS,
		BridgeVersion: bridgeVersion,
		Credential:    store,
		Output:        out,
		HTTPClient:    httpClient,
	})
}

// gatewayAPIOrigin derives the https web-API origin from the configured
// gateway URL: the WSS bridge endpoint and the API share one origin, so only
// the scheme changes and no TLS relaxation ever happens — a ws:// or http://
// gateway URL is refused.
func gatewayAPIOrigin(gatewayURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(gatewayURL))
	if err != nil {
		return "", fmt.Errorf("invalid BRIDGE_GATEWAY_URL %q", gatewayURL)
	}
	switch parsed.Scheme {
	case "wss":
		return "https://" + parsed.Host, nil
	case "https":
		return "https://" + parsed.Host, nil
	default:
		return "", fmt.Errorf("BRIDGE_GATEWAY_URL must use wss (or https); got %q", gatewayURL)
	}
}

// newEnrollDeviceID generates the RFC 4122 v4 device id the enrollment claims.
func newEnrollDeviceID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		log.Fatalf("generate device id: %v", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16])
}

// newRemoteDeps builds the authenticated gateway bridge dependencies shared
// by interactive remote mode and service mode. The sink is the statusui event
// consumer: nil renders the terminal states (interactive), the service
// passes its dual sink so the identical event stream reaches the structured
// local log and the service supervisor.
func newRemoteDeps(sink func(statusui.Event) error) (bridgeapp.RemoteDeps, error) {
	config, err := appconfig.LoadBridge(os.Getenv)
	if err != nil {
		return bridgeapp.RemoteDeps{}, err
	}
	deviceID := strings.TrimSpace(os.Getenv("BRIDGE_DEVICE_ID"))
	if deviceID == "" {
		return bridgeapp.RemoteDeps{}, errors.New("BRIDGE_DEVICE_ID is required in remote mode")
	}
	command, err := mcpprocess.NewLauncher(config.MCPLauncherPath).Resolve()
	if err != nil {
		return bridgeapp.RemoteDeps{}, err
	}
	store, err := bridgeapp.NewFileCredentialStore(config.CredentialPath)
	if err != nil {
		return bridgeapp.RemoteDeps{}, err
	}

	return bridgeapp.RemoteDeps{
		Machine: statusui.NewMachine(),
		NewProcess: func() mcpprocess.Process {
			return mcpprocess.NewProcess(command, mcpprocess.Options{})
		},
		Credential:      store,
		GatewayURL:      config.GatewayURL.String(),
		DeviceID:        deviceID,
		DeviceName:      hostname(),
		Output:          os.Stdout,
		EventSink:       sink,
		StudioReady:     studioReadyAfterVerifiedMCPCall,
		ConnectTimeout:  config.ConnectTimeout,
		ResponseTimeout: config.ResponseTimeout,
		QueueLimit:      config.QueueLimit,
		MaxMessageBytes: config.MaxMessageBytes,
		BridgeVersion:   bridgeVersion,
	}, nil
}

// defaultServiceLogPath resolves the structured service log location: the
// BRIDGE_SERVICE_LOG override, else the ProgramData default so a LocalSystem
// service process can write it.
func defaultServiceLogPath(getenv func(string) string) (string, error) {
	if path := strings.TrimSpace(getenv(serviceLogEnv)); path != "" {
		return path, nil
	}
	programData := strings.TrimSpace(getenv("ProgramData"))
	if programData == "" {
		return "", errors.New("BRIDGE_SERVICE_LOG is not set and ProgramData is unavailable")
	}
	return filepath.Join(programData, "RobloxBridge", "service.log"), nil
}

// openServiceLog opens the append-only structured service log. The installer
// creates the directory; the service account needs write access.
func openServiceLog() (*os.File, error) {
	path, err := defaultServiceLogPath(os.Getenv)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

// studioReadyAfterVerifiedMCPCall reports the single Studio session already
// proven by bridgeapp's initialize, tools/list, and successful safe read-only
// tools/call sequence. It is a post-handshake fact, not an OS discovery API.
func studioReadyAfterVerifiedMCPCall(context.Context) (int, error) {
	return 1, nil
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "local device"
	}
	return name
}
