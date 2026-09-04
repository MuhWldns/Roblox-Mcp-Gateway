// Package routing resolves connector requests to concrete online Studio
// targets. It is a pure policy package: Resolve performs no I/O and decides
// only from the grant's target, the request's optional explicit Studio, and
// the caller-provided snapshot of online Studios.
package routing

import (
	"errors"
	"fmt"
)

// Targeting errors. Resolve returns these sentinels, wrapped with the
// offending identifiers, so callers classify outcomes with errors.Is.
var (
	// ErrDeviceOffline reports that the grant's device has no online
	// Studio to deliver to.
	ErrDeviceOffline = errors.New("routing: target device is not online")

	// ErrAmbiguousStudio reports that several online Studios match and no
	// explicit target names one.
	ErrAmbiguousStudio = errors.New("routing: multiple online Studios require an explicit target")

	// ErrCrossDeviceStudio reports that the requested Studio is online on
	// a different device than the grant's.
	ErrCrossDeviceStudio = errors.New("routing: Studio belongs to a different device")

	// ErrStudioNotAllowed reports that the grant is bound to a different
	// Studio than the requested one.
	ErrStudioNotAllowed = errors.New("routing: Studio is not allowed by the grant")

	// ErrStudioOffline reports that the requested Studio is not online.
	ErrStudioOffline = errors.New("routing: Studio is not online")
)

// GrantTarget is the targeting slice of a connector grant: exactly one
// device and, optionally, the single Studio the grant is bound to. A bound
// Studio is both the only Studio the grant allows and the default target
// when the request does not name one.
type GrantTarget struct {
	DeviceID string
	StudioID string
}

// RequestTarget is what an incoming request asks for. StudioID is the
// optional explicit Studio the caller wants; empty means no preference.
type RequestTarget struct {
	StudioID string
}

// Studio is one online Studio session as reported by its device.
type Studio struct {
	StudioID string
	DeviceID string
}

// ResolvedTarget is the chosen delivery target for one request.
type ResolvedTarget struct {
	DeviceID string
	StudioID string
}

// Resolve picks the Studio a request must be delivered to. Precedence,
// from strongest to weakest:
//
//  1. An explicit request Studio, when the grant allows it, is online, and
//     is on the grant's device.
//  2. Otherwise the Studio the grant is bound to, which must be online on
//     the grant's device.
//  3. Otherwise the sole online Studio of the grant's device.
//
// Anything else fails: several candidates without a preference are
// ambiguous, a Studio on another device is never used, and offline devices
// and Studios are rejected. A request for a Studio the grant does not
// allow is denied before any online state is consulted.
func Resolve(grant GrantTarget, request RequestTarget, online []Studio) (ResolvedTarget, error) {
	if grant.DeviceID == "" {
		return ResolvedTarget{}, fmt.Errorf("%w: grant has no device", ErrDeviceOffline)
	}

	// Only Studios reporting on the grant's device are candidates.
	device := make([]Studio, 0, len(online))
	for _, s := range online {
		if s.DeviceID == grant.DeviceID {
			device = append(device, s)
		}
	}

	want := request.StudioID
	if want == "" {
		want = grant.StudioID
	}
	if want != "" {
		if request.StudioID != "" && grant.StudioID != "" && request.StudioID != grant.StudioID {
			return ResolvedTarget{}, fmt.Errorf("%w: grant allows only Studio %q",
				ErrStudioNotAllowed, grant.StudioID)
		}
		if _, ok := findStudio(device, want); ok {
			return ResolvedTarget{DeviceID: grant.DeviceID, StudioID: want}, nil
		}
		if _, ok := findStudio(online, want); ok {
			return ResolvedTarget{}, fmt.Errorf("%w: Studio %q is not on device %q",
				ErrCrossDeviceStudio, want, grant.DeviceID)
		}
		return ResolvedTarget{}, fmt.Errorf("%w: Studio %q", ErrStudioOffline, want)
	}

	switch len(device) {
	case 1:
		return ResolvedTarget{DeviceID: grant.DeviceID, StudioID: device[0].StudioID}, nil
	case 0:
		return ResolvedTarget{}, ErrDeviceOffline
	default:
		return ResolvedTarget{}, fmt.Errorf("%w: %d Studios online on device %q",
			ErrAmbiguousStudio, len(device), grant.DeviceID)
	}
}

// findStudio returns the online Studio with the given ID.
func findStudio(studios []Studio, id string) (Studio, bool) {
	for _, s := range studios {
		if s.StudioID == id {
			return s, true
		}
	}
	return Studio{}, false
}
