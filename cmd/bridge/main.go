package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"robloxkit/internal/appconfig"
	"robloxkit/internal/bridgeapp"
	"robloxkit/internal/mcpprocess"
	"robloxkit/internal/statusui"
)

func main() {
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
