package routing

import (
	"errors"
	"testing"
)

func makeGrant(device, studio string) GrantTarget {
	return GrantTarget{DeviceID: device, StudioID: studio}
}

func makeRequest(studio string) RequestTarget {
	return RequestTarget{StudioID: studio}
}

// studiosOnline builds the online snapshot for one device.
func studiosOnline(device string, ids ...string) []Studio {
	list := make([]Studio, 0, len(ids))
	for _, id := range ids {
		list = append(list, Studio{StudioID: id, DeviceID: device})
	}
	return list
}

func mustResolve(t *testing.T, g GrantTarget, r RequestTarget, online []Studio, wantDevice, wantStudio string) {
	t.Helper()
	got, err := Resolve(g, r, online)
	if err != nil {
		t.Fatalf("Resolve(%+v, %+v) failed: %v", g, r, err)
	}
	if got.DeviceID != wantDevice || got.StudioID != wantStudio {
		t.Fatalf("Resolve(%+v, %+v) = %+v, want device %q studio %q", g, r, got, wantDevice, wantStudio)
	}
}

func mustReject(t *testing.T, g GrantTarget, r RequestTarget, online []Studio, want error) {
	t.Helper()
	got, err := Resolve(g, r, online)
	if !errors.Is(err, want) {
		t.Fatalf("Resolve(%+v, %+v) error = %v, want %v", g, r, err, want)
	}
	if got != (ResolvedTarget{}) {
		t.Fatalf("Resolve(%+v, %+v) returned target %+v alongside the error", g, r, got)
	}
}

func TestResolveHonorsExplicitAllowedStudio(t *testing.T) {
	// The grant is bound to one Studio and the request names exactly it.
	mustResolve(t, makeGrant("d1", "s1"), makeRequest("s1"), studiosOnline("d1", "s1"), "d1", "s1")

	// An unscoped grant allows any Studio on its device.
	mustResolve(t, makeGrant("d1", ""), makeRequest("s2"), studiosOnline("d1", "s1", "s2"), "d1", "s2")

	// A grant bound to one Studio rejects a different explicit Studio even
	// when it is online on the granted device.
	mustReject(t, makeGrant("d1", "s1"), makeRequest("s2"), studiosOnline("d1", "s1", "s2"), ErrStudioNotAllowed)

	// The grant denial takes precedence over cross-device reporting.
	mustReject(t, makeGrant("d1", "s1"), makeRequest("s2"),
		studiosOnline("d2", "s2"), ErrStudioNotAllowed)
}

func TestResolveUsesGrantStudioAsDefault(t *testing.T) {
	// Two online Studios would be ambiguous, but the Studio the grant is
	// bound to is the default target and wins.
	mustResolve(t, makeGrant("d1", "s1"), makeRequest(""), studiosOnline("d1", "s1", "s2"), "d1", "s1")

	// The default Studio must be online on the granted device.
	mustReject(t, makeGrant("d1", "s1"), makeRequest(""), studiosOnline("d1", "s2"), ErrStudioOffline)

	// A default Studio online on another device is cross-device.
	mustReject(t, makeGrant("d1", "s1"), makeRequest(""),
		[]Studio{{StudioID: "s1", DeviceID: "d2"}, {StudioID: "s2", DeviceID: "d1"}},
		ErrCrossDeviceStudio)
}

func TestResolveSoleOnlineStudio(t *testing.T) {
	mustResolve(t, makeGrant("d1", ""), makeRequest(""), studiosOnline("d1", "s2"), "d1", "s2")

	// Studios on other devices do not participate in the sole-candidate rule.
	mustResolve(t, makeGrant("d1", ""), makeRequest(""),
		[]Studio{{StudioID: "s9", DeviceID: "d2"}, {StudioID: "s2", DeviceID: "d1"}},
		"d1", "s2")
}

func TestResolveAmbiguousWithoutExplicitTarget(t *testing.T) {
	mustReject(t, makeGrant("d1", ""), makeRequest(""), studiosOnline("d1", "s1", "s2"), ErrAmbiguousStudio)

	// Only the granted device's Studios count toward ambiguity.
	mustReject(t, makeGrant("d1", ""), makeRequest(""),
		[]Studio{
			{StudioID: "s1", DeviceID: "d1"},
			{StudioID: "s2", DeviceID: "d1"},
			{StudioID: "s3", DeviceID: "d2"},
		},
		ErrAmbiguousStudio)
}

func TestResolveCrossDeviceStudioRejected(t *testing.T) {
	// An explicit Studio online on a different device is never delivered to.
	mustReject(t, makeGrant("d1", ""), makeRequest("s1"), studiosOnline("d2", "s1"), ErrCrossDeviceStudio)
}

func TestResolveOfflineDeviceRejected(t *testing.T) {
	// Nothing online at all.
	mustReject(t, makeGrant("d1", ""), makeRequest(""), nil, ErrDeviceOffline)

	// Only other devices online.
	mustReject(t, makeGrant("d1", ""), makeRequest(""), studiosOnline("d2", "s1"), ErrDeviceOffline)

	// An explicitly requested Studio that is not online anywhere.
	mustReject(t, makeGrant("d1", ""), makeRequest("s1"), nil, ErrStudioOffline)

	// A grant without a device has nothing online.
	mustReject(t, makeGrant("", ""), makeRequest(""), studiosOnline("d1", "s1"), ErrDeviceOffline)
}
