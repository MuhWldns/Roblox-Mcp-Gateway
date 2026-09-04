//go:build !windows

package bridgeapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"robloxkit/internal/statusui"
)

// The non-Windows build carries a service stub: RunService must refuse
// clearly instead of pretending to run under a service control manager, and
// the SCM detection helper must always report false. This test executes on
// non-Windows platforms; on Windows the equivalent cross-compile proof lives
// in service_windows_test.go.

func TestServiceNonWindowsUnsupported(t *testing.T) {
	err := RunService(ServiceDeps{
		Name: "RobloxBridge",
		Run: func(context.Context, func(statusui.Event) error) error {
			return nil
		},
		Log: nil,
		Now: func() time.Time { return time.Now().UTC() },
	})
	if err == nil {
		t.Fatal("RunService on non-Windows must fail, got nil")
	}
	if !errors.Is(err, errServiceUnsupported) {
		t.Fatalf("RunService error %v is not errServiceUnsupported", err)
	}
	if IsWindowsService() {
		t.Fatal("IsWindowsService must be false on non-Windows")
	}
}
