//go:build !windows

package bridgeapp

import (
	"context"
	"errors"
	"io"
	"time"

	"robloxkit/internal/statusui"
)

// ServiceName mirrors the Windows service name so callers can reference the
// service identity on every platform.
const ServiceName = "RobloxBridge"

// errServiceUnsupported is the clear refusal for service mode outside
// Windows: there is no service control manager to register with, so service
// mode must fail loudly instead of silently falling back to another mode.
var errServiceUnsupported = errors.New("bridgeapp: windows service mode is only supported on windows")

// ServiceDeps mirrors the Windows service configuration; the fields exist so
// callers compile unchanged across platforms.
type ServiceDeps struct {
	Name string
	Run  func(ctx context.Context, sink func(statusui.Event) error) error
	Log  io.Writer
	Now  func() time.Time
}

// IsWindowsService always reports false outside Windows.
func IsWindowsService() bool { return false }

// RunService refuses service mode outside Windows with errServiceUnsupported.
func RunService(deps ServiceDeps) error {
	_ = deps
	return errServiceUnsupported
}
