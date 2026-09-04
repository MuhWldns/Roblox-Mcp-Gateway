package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"robloxkit/internal/appconfig"
	"robloxkit/internal/bridgeapp"
	"robloxkit/internal/mcpprocess"
	"robloxkit/internal/statusui"
)

const bridgeVersion = "0.1.0"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if isRemoteMode() {
		runRemote(ctx)
		return
	}
	runLocal(ctx)
}

// isRemoteMode selects the remote gateway mode; the default stays the local
// Studio-only mode.
func isRemoteMode() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("BRIDGE_MODE")), "remote")
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
		StudioReady: studioReadinessUnavailable,
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
	config, err := appconfig.LoadBridge(os.Getenv)
	if err != nil {
		log.Printf("startup failed: %v", err)
		os.Exit(1)
	}
	deviceID := strings.TrimSpace(os.Getenv("BRIDGE_DEVICE_ID"))
	if deviceID == "" {
		log.Printf("startup failed: BRIDGE_DEVICE_ID is required in remote mode")
		os.Exit(1)
	}
	command, err := mcpprocess.NewLauncher(config.MCPLauncherPath).Resolve()
	if err != nil {
		log.Printf("startup failed: %v", err)
		os.Exit(1)
	}
	store, err := bridgeapp.NewFileCredentialStore(config.CredentialPath)
	if err != nil {
		log.Printf("startup failed: %v", err)
		os.Exit(1)
	}

	err = bridgeapp.RunRemote(ctx, bridgeapp.RemoteDeps{
		Machine: statusui.NewMachine(),
		NewProcess: func() mcpprocess.Process {
			return mcpprocess.NewProcess(command, mcpprocess.Options{})
		},
		Credential:      store,
		GatewayURL:      config.GatewayURL.String(),
		DeviceID:        deviceID,
		DeviceName:      hostname(),
		Output:          os.Stdout,
		StudioReady:     studioReadinessUnavailable,
		ConnectTimeout:  config.ConnectTimeout,
		ResponseTimeout: config.ResponseTimeout,
		QueueLimit:      config.QueueLimit,
		MaxMessageBytes: config.MaxMessageBytes,
		BridgeVersion:   bridgeVersion,
	})

	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		log.Printf("bridge stopped: %v", err)
		os.Exit(1)
	}
}

// studioReadinessUnavailable is deliberately fail-closed until the official
// local Studio readiness API is available. A nil callback must never imply a
// live Studio session and allow SYSTEM CONNECTED to be rendered.
func studioReadinessUnavailable(context.Context) (int, error) {
	return 0, errors.New("local Roblox Studio readiness probe is unavailable")
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "local device"
	}
	return name
}
