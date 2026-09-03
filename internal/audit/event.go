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
