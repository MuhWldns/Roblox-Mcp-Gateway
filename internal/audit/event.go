// Package audit defines immutable, secret-free audit records.
package audit

import "time"

// Actor is the principal that performed an audited action. UserID is the
// internal user id; Kind is "user", "admin", or "system".
type Actor struct {
	UserID string
	Kind   string
}

// Actor kinds carried by audit events. They select the persistence target:
// admin events go to admin_actions, user and system events to audit_logs.
const (
	ActorUser   = "user"
	ActorAdmin  = "admin"
	ActorSystem = "system"
)

// Event is an append-only audit record. It may carry only safe identifiers and
// state descriptions: never tokens, credential digests, or plaintext secrets.
type Event struct {
	Actor         Actor
	Action        string
	CorrelationID string
	Reason        string
	UserID        string
	TargetType    string
	TargetID      string
	Before        map[string]string
	After         map[string]string
	CreatedAt     time.Time
}

// Usage is one append-only metering record for a relayed gateway request.
// It carries only safe identifiers and counters: never tokens, credential
// digests, request payloads, or plaintext secrets. The persistence store
// keys idempotency on the caller-supplied gateway request id.
type Usage struct {
	// UserID is the internal user the usage belongs to (required).
	UserID string
	// DeviceID and StudioSessionID locate the relay target; empty means
	// the row stores NULL for that reference.
	DeviceID        string
	StudioSessionID string
	// Operation names the relayed operation (for example "tools/call").
	Operation string
	// Outcome is the relay outcome (for example "success" or "error").
	Outcome string
	// Units counts what the request consumed; one call is one unit.
	Units int64
	// Metadata carries optional safe key/value annotations, such as the
	// invoked tool name. Values pass through the same redaction as audit
	// events before persistence.
	Metadata map[string]string
}
