package entitlement

import (
	"context"
	"errors"
	"time"
)

// Store persists trials, licenses, bindings, and their audit trail. Every
// clock-dependent decision receives an explicit now so callers can inject a
// deterministic time source.
type Store interface {
	BindFirstDevice(context.Context, time.Time, FirstDeviceBinding) (Entitlement, Binding, error)
	Authorize(context.Context, time.Time, Subject) (Decision, error)
	TransferDevice(context.Context, time.Time, AdminActor, string, string, string, string) error
	RecoverIdentity(context.Context, time.Time, AdminActor, string, string, string, string) error
	ExtendTrial(context.Context, AdminActor, string, time.Time, string) error
}

// Service applies clock-dependent business rules on top of a Store.
type Service struct {
	store Store
	clock Clock
}

// NewService constructs the entitlement service over a persistence store.
func NewService(store Store, clock Clock) *Service {
	return &Service{store: store, clock: clock}
}

func (s *Service) now() time.Time {
	if s == nil || s.clock == nil {
		return time.Now().UTC()
	}
	return s.clock.Now().UTC()
}

// BindFirstDevice starts the one-time trial and registers the first device and
// its credential atomically. The clock pins the trial window.
func (s *Service) BindFirstDevice(ctx context.Context, in FirstDeviceBinding) (Entitlement, Binding, error) {
	if ctx == nil {
		return Entitlement{}, Binding{}, errors.New("entitlement: nil context")
	}
	if s == nil || s.store == nil {
		return Entitlement{}, Binding{}, errors.New("entitlement: nil store")
	}
	if in.UserID == "" || in.Provider == "" || in.ProviderSubject == "" || in.DeviceID == "" {
		return Entitlement{}, Binding{}, errors.New("entitlement: invalid first device binding")
	}
	if in.CredentialDigest == [32]byte{} {
		return Entitlement{}, Binding{}, errors.New("entitlement: empty credential digest")
	}
	return s.store.BindFirstDevice(ctx, s.now(), in)
}

// Authorize evaluates the subject's entitlement window and returns a Decision.
func (s *Service) Authorize(ctx context.Context, subject Subject) (Decision, error) {
	if ctx == nil {
		return Decision{}, errors.New("entitlement: nil context")
	}
	if s == nil || s.store == nil {
		return Decision{}, errors.New("entitlement: nil store")
	}
	return s.store.Authorize(ctx, s.now(), subject)
}

// TransferDevice moves an active license slot from one device to another.
func (s *Service) TransferDevice(ctx context.Context, actor AdminActor, licenseID, oldDeviceID, newDeviceID, reason string) error {
	if ctx == nil {
		return errors.New("entitlement: nil context")
	}
	if s == nil || s.store == nil {
		return errors.New("entitlement: nil store")
	}
	if reason == "" {
		return errors.New("entitlement: transfer requires a reason")
	}
	return s.store.TransferDevice(ctx, s.now(), actor, licenseID, oldDeviceID, newDeviceID, reason)
}

// RecoverIdentity revokes all credentials and sessions for a user and records
// the recovery case. The trial window is never touched.
func (s *Service) RecoverIdentity(ctx context.Context, actor AdminActor, userID, newIdentityID, reason, evidenceRef string) error {
	if ctx == nil {
		return errors.New("entitlement: nil context")
	}
	if s == nil || s.store == nil {
		return errors.New("entitlement: nil store")
	}
	if reason == "" || evidenceRef == "" {
		return errors.New("entitlement: recovery requires reason and evidence")
	}
	return s.store.RecoverIdentity(ctx, s.now(), actor, userID, newIdentityID, reason, evidenceRef)
}

// ExtendTrial lengthens the existing entitlement's expiry only.
func (s *Service) ExtendTrial(ctx context.Context, actor AdminActor, entitlementID string, newEndsAt time.Time, reason string) error {
	if ctx == nil {
		return errors.New("entitlement: nil context")
	}
	if s == nil || s.store == nil {
		return errors.New("entitlement: nil store")
	}
	if reason == "" {
		return errors.New("entitlement: extension requires a reason")
	}
	if newEndsAt.IsZero() {
		return errors.New("entitlement: extension requires a new expiry")
	}
	return s.store.ExtendTrial(ctx, actor, entitlementID, newEndsAt, reason)
}
