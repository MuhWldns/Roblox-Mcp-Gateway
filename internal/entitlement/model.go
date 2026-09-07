// Package entitlement enforces the one-time free trial and device-slot policy.
package entitlement

import (
	"errors"
	"time"
)

// TrialWindow is the fixed duration of the one-time free trial.
const TrialWindow = 14 * 24 * time.Hour

var (
	// ErrNotFound indicates the requested resource does not exist.
	ErrNotFound = errors.New("entitlement: not found")
	// ErrTrialAlreadyUsed indicates the Roblox subject, across all internal
	// accounts, has already consumed its one historical free trial.
	ErrTrialAlreadyUsed = errors.New("entitlement: trial already used")
	// ErrNoSlot indicates a license has no free device slot.
	ErrNoSlot = errors.New("entitlement: no free device slot")
	// ErrBindingNotFound indicates a license-device binding is missing.
	ErrBindingNotFound = errors.New("entitlement: device binding not found")
	// ErrInvalidExtension indicates a trial extension is not later than the
	// current expiry.
	ErrInvalidExtension = errors.New("entitlement: extension must be later than current expiry")
	// ErrDeviceOwnedByOther indicates the device id is claimed by another
	// internal account; a re-claim by the wrong owner is rejected.
	ErrDeviceOwnedByOther = errors.New("entitlement: device owned by another user")
)

// Clock supplies deterministically controlled time for policy evaluation.
type Clock interface {
	Now() time.Time
}

// FirstDeviceBinding is the atomic first-enrollment request.
type FirstDeviceBinding struct {
	UserID           string
	IdentityID       string
	Provider         string
	ProviderSubject  string
	DeviceID         string
	CredentialDigest [32]byte
	AuditCorrelation string
}

// Subject identifies the requesting principal during authorization.
type Subject struct {
	UserID          string
	Provider        string
	ProviderSubject string
}

// Action names a gated capability.
type Action string

const (
	ActionEnroll    Action = "enroll"
	ActionWSS       Action = "wss"
	ActionMCP       Action = "mcp"
	ActionDashboard Action = "dashboard"
	ActionDownload  Action = "download"
)

// Decision is the outcome of Authorize for a subject. Active reports the
// overall window (trial or license); TrialActive and LicenseActive expose the
// source so binding-gated surfaces can apply the contract: an active trial
// covers the enrolled credential-owned device without any paid slot binding,
// while license-only access is bound to its license's device slots.
type Decision struct {
	Active        bool
	TrialActive   bool
	LicenseActive bool
	Entitlement   Entitlement
}

// Expired reports whether the subject lacks an in-window entitlement.
func (d Decision) Expired() bool { return !d.Active }

// Permits reports whether the subject may take the given action. Dashboard and
// download remain available regardless of trial state; enrollment, WSS, and
// MCP require an active entitlement window.
func (d Decision) Permits(action Action) bool {
	switch action {
	case ActionDashboard, ActionDownload:
		return true
	case ActionEnroll, ActionWSS, ActionMCP:
		return d.Active
	default:
		return false
	}
}

// Entitlement is a time-bounded policy window (the free trial).
type Entitlement struct {
	ID              string
	UserID          string
	StartedAt       time.Time
	EndsAt          time.Time
	ExtensionReason string
	ExtendedBy      string
}

// Expired reports whether the window is closed at now.
func (e Entitlement) Expired(now time.Time) bool { return !now.Before(e.EndsAt) }

// Binding records a license/device slot binding or a first-device enrollment.
type Binding struct {
	ID          string
	UserID      string
	LicenseID   string
	DeviceID    string
	SlotOrdinal int
	Status      string
}

// License is a paid policy grant carrying a bounded number of device slots.
type License struct {
	ID               string
	UserID           string
	RobloxIdentityID string
	DeviceSlots      int
	Status           string
}

// AdminActor identifies the administrator performing a privileged mutation.
type AdminActor struct {
	UserID string
}
