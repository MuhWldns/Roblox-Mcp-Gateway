// Package bridgehub owns authenticated Bridge WebSocket connections, their
// bounded outbound writer queues, and the live device registry. It is the
// single server-side entry point for the /bridge endpoint.
package bridgehub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"robloxkit/internal/credential"
	"robloxkit/internal/entitlement"
)

const (
	// robloxProvider is the only supported identity provider.
	robloxProvider = "roblox"
	// maxCredentialLength bounds the presented bearer token length.
	maxCredentialLength = 512
)

// ErrUnauthorized is the generic, client-safe authentication failure. Every
// rejection reason is wrapped into it; details never reach the wire.
var ErrUnauthorized = errors.New("bridgehub: invalid device credential")

// Store-level sentinels used by Store implementations.
var (
	// ErrCredentialNotFound indicates no credential row matches a digest.
	ErrCredentialNotFound = errors.New("bridgehub: device credential not found")
	// ErrIdentityNotFound indicates the user has no active provider identity.
	ErrIdentityNotFound = errors.New("bridgehub: active identity not found")
)

// Device identifies one authenticated Bridge connection. It carries only
// internal identifiers; the plaintext credential is never retained.
type Device struct {
	UserID           string
	DeviceID         string
	Provider         string
	ProviderSubject  string
	CredentialDigest [32]byte
}

// DeviceCredential mirrors one device_credentials row.
type DeviceCredential struct {
	ID        string
	UserID    string
	DeviceID  string
	Digest    [32]byte
	ExpiresAt time.Time // zero means no expiry
	RevokedAt time.Time // zero means not revoked
}

// Identity is the active provider identity bound to a user.
type Identity struct {
	Provider        string
	ProviderSubject string
}

// Store reads device credentials, device state, bindings, and identities.
// Implementations must be safe for concurrent use; the hub never writes.
type Store interface {
	LookupDeviceCredential(ctx context.Context, digest [32]byte) (DeviceCredential, error)
	DeviceOwnedAndActive(ctx context.Context, userID, deviceID string) (bool, error)
	HasActiveDeviceBinding(ctx context.Context, userID, deviceID string) (bool, error)
	UserIdentity(ctx context.Context, userID string) (Identity, error)
}

// Authenticator validates a presented device credential against the store,
// device ownership, the active license binding, and the frozen entitlement
// policy before a WebSocket upgrade is offered.
type Authenticator struct {
	store        Store
	entitlements *entitlement.Service
	pepper       []byte
	now          func() time.Time
}

// NewAuthenticator builds the authenticator. A nil now defaults to time.Now.
func NewAuthenticator(store Store, entitlements *entitlement.Service, pepper []byte, now func() time.Time) *Authenticator {
	if now == nil {
		now = time.Now
	}
	return &Authenticator{store: store, entitlements: entitlements, pepper: pepper, now: now}
}

// Authenticate parses an Authorization header value and validates the
// presented credential. All failures are wrapped ErrUnauthorized.
func (a *Authenticator) Authenticate(ctx context.Context, authorizationHeader string) (Device, error) {
	token, ok := parseBearerToken(authorizationHeader)
	if !ok {
		return Device{}, fmt.Errorf("%w: malformed bearer credential", ErrUnauthorized)
	}
	digest := credential.Digest(token, a.pepper)
	return a.AuthenticateDigest(ctx, digest)
}

// AuthenticateDigest validates an already-keyed credential digest. It is also
// the mid-connection revalidation path, so live connections never keep the
// plaintext credential in memory.
func (a *Authenticator) AuthenticateDigest(ctx context.Context, digest [32]byte) (Device, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	reject := func(reason string) (Device, error) {
		return Device{}, fmt.Errorf("%w: %s", ErrUnauthorized, reason)
	}

	record, err := a.store.LookupDeviceCredential(ctx, digest)
	if err != nil {
		return reject("credential not accepted")
	}
	// Constant-time confirmation of the unique-index match. The SQL lookup
	// already narrows to the keyed digest; this defends the comparison itself.
	if !credential.Equal(record.Digest, digest) {
		return reject("credential digest mismatch")
	}
	if !record.RevokedAt.IsZero() {
		return reject("credential revoked")
	}
	if !record.ExpiresAt.IsZero() && !a.now().Before(record.ExpiresAt) {
		return reject("credential expired")
	}
	owned, err := a.store.DeviceOwnedAndActive(ctx, record.UserID, record.DeviceID)
	if err != nil || !owned {
		return reject("device is not active for owner")
	}
	bound, err := a.store.HasActiveDeviceBinding(ctx, record.UserID, record.DeviceID)
	if err != nil || !bound {
		return reject("device has no active license binding")
	}
	identity, err := a.store.UserIdentity(ctx, record.UserID)
	if err != nil {
		return reject("no active identity")
	}
	decision, err := a.entitlements.Authorize(ctx, entitlement.Subject{
		UserID:          record.UserID,
		Provider:        identity.Provider,
		ProviderSubject: identity.ProviderSubject,
	})
	if err != nil {
		return reject("entitlement check failed")
	}
	if !decision.Permits(entitlement.ActionWSS) {
		return reject("entitlement does not permit wss")
	}
	return Device{
		UserID:           record.UserID,
		DeviceID:         record.DeviceID,
		Provider:         identity.Provider,
		ProviderSubject:  identity.ProviderSubject,
		CredentialDigest: digest,
	}, nil
}

// parseBearerToken extracts the opaque credential from an Authorization
// header value. Scheme matching is case-insensitive per RFC 7235.
func parseBearerToken(header string) (string, bool) {
	scheme, rest, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token := strings.TrimSpace(rest)
	if token == "" || len(token) > maxCredentialLength {
		return "", false
	}
	if strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

// SQLStore implements Store against MySQL. It performs read-only lookups with
// no writes; revocation and rotation happen in the device packages.
type SQLStore struct {
	DB *sql.DB
}

// NewSQLStore builds the store over a verified connection pool.
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{DB: db}
}

func (s *SQLStore) check(ctx context.Context) error {
	if ctx == nil {
		return errors.New("bridgehub: nil context")
	}
	if s == nil || s.DB == nil {
		return errors.New("bridgehub: nil database")
	}
	return nil
}

// LookupDeviceCredential returns the credential row whose keyed digest matches.
func (s *SQLStore) LookupDeviceCredential(ctx context.Context, digest [32]byte) (DeviceCredential, error) {
	if err := s.check(ctx); err != nil {
		return DeviceCredential{}, err
	}
	var out DeviceCredential
	var stored []byte
	var expires, revoked sql.NullTime
	row := s.DB.QueryRowContext(ctx,
		`SELECT id, user_id, device_id, credential_digest, expires_at, revoked_at
		 FROM device_credentials WHERE credential_digest = ?`, digest[:])
	err := row.Scan(&out.ID, &out.UserID, &out.DeviceID, &stored, &expires, &revoked)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeviceCredential{}, ErrCredentialNotFound
		}
		return DeviceCredential{}, fmt.Errorf("bridgehub: lookup device credential: %w", err)
	}
	if len(stored) != len(out.Digest) {
		return DeviceCredential{}, ErrCredentialNotFound
	}
	copy(out.Digest[:], stored)
	if expires.Valid {
		out.ExpiresAt = expires.Time.UTC()
	}
	if revoked.Valid {
		out.RevokedAt = revoked.Time.UTC()
	}
	return out, nil
}

// DeviceOwnedAndActive reports whether the device exists, is owned by the user,
// and is active.
func (s *SQLStore) DeviceOwnedAndActive(ctx context.Context, userID, deviceID string) (bool, error) {
	if err := s.check(ctx); err != nil {
		return false, err
	}
	var one int
	err := s.DB.QueryRowContext(ctx,
		`SELECT 1 FROM devices WHERE id = ? AND user_id = ? AND status = 'active'`,
		deviceID, userID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("bridgehub: lookup device: %w", err)
	}
	return true, nil
}

// HasActiveDeviceBinding reports whether the device occupies an active,
// non-revoked license slot.
func (s *SQLStore) HasActiveDeviceBinding(ctx context.Context, userID, deviceID string) (bool, error) {
	if err := s.check(ctx); err != nil {
		return false, err
	}
	var one int
	err := s.DB.QueryRowContext(ctx,
		`SELECT 1 FROM license_device_bindings
		 WHERE device_id = ? AND user_id = ? AND status = 'active' AND revoked_at IS NULL`,
		deviceID, userID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("bridgehub: lookup device binding: %w", err)
	}
	return true, nil
}

// UserIdentity returns the user's active Roblox identity for entitlement
// subjects.
func (s *SQLStore) UserIdentity(ctx context.Context, userID string) (Identity, error) {
	if err := s.check(ctx); err != nil {
		return Identity{}, err
	}
	var identity Identity
	err := s.DB.QueryRowContext(ctx,
		`SELECT provider, provider_subject FROM user_identities
		 WHERE user_id = ? AND provider = ? AND status = 'active' LIMIT 1`,
		userID, robloxProvider).Scan(&identity.Provider, &identity.ProviderSubject)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Identity{}, ErrIdentityNotFound
		}
		return Identity{}, fmt.Errorf("bridgehub: lookup identity: %w", err)
	}
	return identity, nil
}
