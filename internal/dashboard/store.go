// Package dashboard declares the browser dashboard's store contract: the
// row shapes the dashboard reads render and the self-service mutations it
// applies. It is a pure leaf package — internal/mysqlstore implements the
// interface and internal/httpserver consumes it, so no implementation
// package ever imports a composition package.
package dashboard

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound reports that a dashboard read or mutation named an object the
// session user does not own — or that does not exist at all. The two cases
// are deliberately indistinguishable so responses leak nothing.
var ErrNotFound = errors.New("dashboard: object not found")

// DeviceRow is one owned device as persisted. Live presence is not part of
// the row: it comes from the Bridge registry at read time.
type DeviceRow struct {
	ID        string
	Name      string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// StudioRow is one Studio session as persisted.
type StudioRow struct {
	ID        string
	DeviceID  string
	StudioID  string
	Status    string
	StartedAt time.Time
	EndedAt   *time.Time
}

// ConnectorRow is one connector authorization as persisted. It never carries
// token values, only their metadata.
type ConnectorRow struct {
	ID              string
	ClientID        string
	ClientName      string
	DeviceID        string
	StudioSessionID string
	Scopes          []string
	Resource        string
	CreatedAt       time.Time
	RevokedAt       *time.Time
}

// LicenseRow is the active paid-license state of one user.
type LicenseRow struct {
	Status         string
	DeviceSlots    int
	ActiveBindings int
}

// Store reads the session user's dashboard state and applies the self-service
// mutations. Every method is scoped to the owning user: rows belonging to
// any other user are invisible, and mutations naming them fail with
// ErrNotFound. Mutations apply their state change and their audit event in
// one transaction; the correlation argument seeds the audit trail (the
// request id in production).
type Store interface {
	// Devices lists the user's devices.
	Devices(ctx context.Context, userID string) ([]DeviceRow, error)
	// Studios lists the user's Studio sessions.
	Studios(ctx context.Context, userID string) ([]StudioRow, error)
	// Connectors lists the user's connector grants with client names.
	Connectors(ctx context.Context, userID string) ([]ConnectorRow, error)
	// License returns the active license row, or nil when none exists.
	License(ctx context.Context, userID string) (*LicenseRow, error)
	// StudioSessionsActive counts the user's live Studio sessions.
	StudioSessionsActive(ctx context.Context, userID string) (int, error)

	// RenameDevice renames an owned device and audits the change.
	RenameDevice(ctx context.Context, correlation, userID, deviceID, name string) error
	// RevokeDevice revokes an owned device and its credentials, keeps the
	// license slot occupied, and audits the transition. Revoking twice
	// succeeds without a second audit event.
	RevokeDevice(ctx context.Context, correlation string, now time.Time, userID, deviceID string) error
	// SetConnectorTarget repoints an owned, unrevoked connector grant at an
	// owned device and an optional owned Studio session, and audits the
	// change.
	SetConnectorTarget(ctx context.Context, correlation, userID, grantID, deviceID, studioSessionID string) error
	// RevokeConnector revokes an owned connector grant together with every
	// access and refresh token under it, and audits the transition.
	// Revoking twice succeeds without a second audit event.
	RevokeConnector(ctx context.Context, correlation string, now time.Time, userID, grantID string) error
}
