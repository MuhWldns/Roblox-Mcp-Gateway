package mcpgateway

import "errors"

// Correlation errors returned by the pending registry. They are plain
// sentinels: callers classify outcomes with errors.Is, never by string.
var (
	// ErrUnknownCorrelation reports that no pending request matches the
	// gateway ID — the request already completed, expired, or was
	// cancelled, so a late or duplicate response is rejected.
	ErrUnknownCorrelation = errors.New("mcpgateway: unknown correlation")

	// ErrTooManyPending reports that the registry already holds its
	// maximum number of pending requests.
	ErrTooManyPending = errors.New("mcpgateway: pending request limit exceeded")

	// ErrInvalidRequest reports an unusable registration or association:
	// an empty session, JSON-RPC id, or device identifier.
	ErrInvalidRequest = errors.New("mcpgateway: invalid correlation request")

	// ErrDeadlineExceeded is delivered as the Result when a request's
	// deadline passes before a response arrives.
	ErrDeadlineExceeded = errors.New("mcpgateway: request deadline exceeded")

	// ErrCancelled is delivered as the Result when the requesting session
	// ends before a response arrives.
	ErrCancelled = errors.New("mcpgateway: request cancelled")

	// ErrDeviceFailed is delivered as the Result when the target device
	// fails, and is the default FailDevice cause when the caller passes
	// no cause.
	ErrDeviceFailed = errors.New("mcpgateway: target device failed")
)
