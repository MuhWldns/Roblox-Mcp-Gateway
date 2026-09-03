// Package device implements the browser-gated Bridge download and the
// one-time device enrollment flow that binds a Bridge installation to the
// internal user who approved it.
package device

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"robloxkit/internal/credential"
	"robloxkit/internal/entitlement"
)

const (
	// userCodePrefix marks the short, human-typable enrollment code.
	userCodePrefix = "rkuc_"
	// userCodeBytes sets the enrollment code entropy.
	userCodeBytes = 6
	// deviceCredentialPrefix marks Bridge device credentials.
	deviceCredentialPrefix = "rkd_"
	// deviceCredentialBytes sets the device credential entropy.
	deviceCredentialBytes = 32

	// DefaultPendingTTL bounds how long an unapproved enrollment stays alive.
	DefaultPendingTTL = 10 * time.Minute
	// DefaultCodeTTL bounds how long an approved enrollment code may be
	// exchanged for a device credential.
	DefaultCodeTTL = 10 * time.Minute
	// DefaultMaxPending bounds unapproved enrollments held in memory.
	DefaultMaxPending = 10000

	// robloxProvider is the only provider the enrollment flow binds today.
	robloxProvider = "roblox"
)

var (
	// ErrInvalidClaim indicates a Bridge device claim failed validation.
	ErrInvalidClaim = errors.New("device: invalid enrollment claim")
	// ErrApprovalOwnerRequired indicates an approval arrived without a
	// session-owned internal user.
	ErrApprovalOwnerRequired = errors.New("device: approval requires a session user")
	// ErrEnrollmentNotFound indicates the enrollment code is unknown or spent.
	ErrEnrollmentNotFound = errors.New("device: enrollment not found")
	// ErrEnrollmentExpired indicates the enrollment window elapsed.
	ErrEnrollmentExpired = errors.New("device: enrollment expired")
	// ErrEnrollmentPending indicates the device has not been approved yet.
	ErrEnrollmentPending = errors.New("device: enrollment not approved yet")
	// ErrTooManyPending indicates the pending enrollment buffer is full.
	ErrTooManyPending = errors.New("device: too many pending enrollments")
	// ErrCodeNotFound indicates no enrollment code row matches the digest.
	ErrCodeNotFound = errors.New("device: enrollment code not found")
	// ErrCodeConsumed indicates the enrollment code was already used.
	ErrCodeConsumed = errors.New("device: enrollment code already consumed")
	// ErrCodeExpired indicates the enrollment code row elapsed.
	ErrCodeExpired = errors.New("device: enrollment code expired")
)

// DeviceClaim is the self-asserted Bridge installation identity presented at
// enrollment. The device id is a random installation identifier; hostname is
// display metadata only and never part of the device identity.
type DeviceClaim struct {
	DeviceID      string `json:"device_id"`
	Name          string `json:"name"`
	Hostname      string `json:"hostname"`
	Platform      string `json:"platform"`
	BridgeVersion string `json:"bridge_version"`
}

// UserCode is the short pairing code the Bridge shows to its operator.
type UserCode string

// VerificationURL is the dashboard URL the Bridge directs its operator to.
type VerificationURL string

// DeviceCredential is the opaque Bridge credential minted by a successful
// exchange. The token is handed to the Bridge exactly once and is never
// exposed to the browser.
type DeviceCredential struct {
	Token    string `json:"device_credential"`
	DeviceID string `json:"device_id"`
	UserID   string `json:"-"`
}

// EnrollmentCode is the persisted, digest-keyed pairing code an approver
// creates for one device enrollment.
type EnrollmentCode struct {
	ID         string
	UserID     string
	CodeDigest [32]byte
	ExpiresAt  time.Time
}

// EnrollmentRecord identifies the owner bound to a consumed enrollment code.
type EnrollmentRecord struct {
	ID              string
	UserID          string
	IdentityID      string
	ProviderSubject string
}

// PendingEnrollment is the claim a session user reviews before approving.
type PendingEnrollment struct {
	DeviceID      string    `json:"device_id"`
	Hostname      string    `json:"hostname"`
	Platform      string    `json:"platform"`
	BridgeVersion string    `json:"bridge_version"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// RobloxIdentity is the browser-visible identity metadata of an internal user.
type RobloxIdentity struct {
	IdentityID  string
	Subject     string
	DisplayName string
}

// EnrollmentStore persists enrollment codes. Implementations must consume
// codes atomically; the consume is the single-use linearization point.
type EnrollmentStore interface {
	InsertEnrollmentCode(ctx context.Context, code EnrollmentCode) error
	ConsumeEnrollmentCode(ctx context.Context, digest [32]byte, now time.Time) (EnrollmentRecord, error)
}

// FirstDeviceBinder starts the one-time trial and registers the device and
// its credential atomically. entitlement.Service satisfies it.
type FirstDeviceBinder interface {
	BindFirstDevice(ctx context.Context, in entitlement.FirstDeviceBinding) (entitlement.Entitlement, entitlement.Binding, error)
}

type pendingEntry struct {
	claim      DeviceClaim
	approved   bool
	approvedBy string
	expiresAt  time.Time
}

// Enrollment orchestrates the one-time device enrollment: Begin mints a
// pairing code for an unauthenticated Bridge, Approve binds that code to the
// approving session user, and Exchange swaps the code for a device credential
// while starting the first-device trial atomically.
type Enrollment struct {
	store  EnrollmentStore
	binder FirstDeviceBinder
	pepper []byte
	now    func() time.Time

	// VerificationBaseURL is the dashboard origin embedded in the URL the
	// Bridge displays, for example https://app.example.com.
	VerificationBaseURL string
	// PendingTTL bounds the unapproved enrollment lifetime.
	PendingTTL time.Duration
	// CodeTTL bounds the approved enrollment code lifetime.
	CodeTTL time.Duration
	// MaxPending bounds unapproved enrollments held in memory.
	MaxPending int

	mu      sync.Mutex
	pending map[string]*pendingEntry
}

// NewEnrollment constructs the enrollment flow over a code store, the frozen
// first-device binder, and a credential pepper. The pepper is copied.
func NewEnrollment(store EnrollmentStore, binder FirstDeviceBinder, pepper []byte, now func() time.Time) (*Enrollment, error) {
	if store == nil {
		return nil, errors.New("device: nil enrollment store")
	}
	if binder == nil {
		return nil, errors.New("device: nil first device binder")
	}
	if len(pepper) == 0 {
		return nil, errors.New("device: empty credential pepper")
	}
	if now == nil {
		return nil, errors.New("device: nil clock")
	}
	return &Enrollment{
		store:      store,
		binder:     binder,
		pepper:     append([]byte(nil), pepper...),
		now:        now,
		PendingTTL: DefaultPendingTTL,
		CodeTTL:    DefaultCodeTTL,
		MaxPending: DefaultMaxPending,
		pending:    make(map[string]*pendingEntry),
	}, nil
}

// Begin registers a Bridge claim and returns the pairing code plus the
// dashboard URL its operator must open. No trial state is touched here.
func (e *Enrollment) Begin(ctx context.Context, claim DeviceClaim) (UserCode, VerificationURL, error) {
	if ctx == nil {
		return "", "", errors.New("device: nil context")
	}
	if err := validateClaim(claim); err != nil {
		return "", "", err
	}
	now := e.now().UTC()
	plain, digest, err := credential.Generate(userCodePrefix, userCodeBytes, e.pepper)
	if err != nil {
		return "", "", fmt.Errorf("device: generate enrollment code: %w", err)
	}
	key := codeKey(digest)

	e.mu.Lock()
	e.evictExpiredLocked(now)
	if len(e.pending) >= e.MaxPending {
		e.mu.Unlock()
		return "", "", ErrTooManyPending
	}
	e.pending[key] = &pendingEntry{claim: claim, expiresAt: now.Add(e.PendingTTL)}
	e.mu.Unlock()

	return UserCode(plain), VerificationURL(verificationURL(e.VerificationBaseURL, plain)), nil
}

// Lookup returns the claim a session user should review before approving.
func (e *Enrollment) Lookup(ctx context.Context, userCode string) (PendingEnrollment, error) {
	if ctx == nil {
		return PendingEnrollment{}, errors.New("device: nil context")
	}
	if userCode == "" {
		return PendingEnrollment{}, ErrEnrollmentNotFound
	}
	now := e.now().UTC()
	key := codeKey(credential.Digest(userCode, e.pepper))

	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.pending[key]
	if !ok {
		return PendingEnrollment{}, ErrEnrollmentNotFound
	}
	if !now.Before(entry.expiresAt) {
		delete(e.pending, key)
		return PendingEnrollment{}, ErrEnrollmentExpired
	}
	return PendingEnrollment{
		DeviceID:      entry.claim.DeviceID,
		Hostname:      entry.claim.Hostname,
		Platform:      entry.claim.Platform,
		BridgeVersion: entry.claim.BridgeVersion,
		ExpiresAt:     entry.expiresAt,
	}, nil
}

// Approve binds a pending enrollment to the approving session user by
// persisting its digest-keyed code. Codes are single-use: an approved code
// can never be approved again.
func (e *Enrollment) Approve(ctx context.Context, userID, userCode string) error {
	if ctx == nil {
		return errors.New("device: nil context")
	}
	if userID == "" {
		return ErrApprovalOwnerRequired
	}
	if userCode == "" {
		return ErrEnrollmentNotFound
	}
	now := e.now().UTC()
	digest := credential.Digest(userCode, e.pepper)
	key := codeKey(digest)

	e.mu.Lock()
	defer e.mu.Unlock()
	entry, ok := e.pending[key]
	if !ok {
		return ErrEnrollmentNotFound
	}
	if !now.Before(entry.expiresAt) {
		delete(e.pending, key)
		return ErrEnrollmentExpired
	}
	if entry.approved {
		return ErrEnrollmentNotFound
	}
	codeID, err := newEnrollmentID()
	if err != nil {
		return fmt.Errorf("device: generate enrollment id: %w", err)
	}
	if err := e.store.InsertEnrollmentCode(ctx, EnrollmentCode{
		ID:         codeID,
		UserID:     userID,
		CodeDigest: digest,
		ExpiresAt:  now.Add(e.CodeTTL),
	}); err != nil {
		return fmt.Errorf("device: persist enrollment code: %w", err)
	}
	entry.approved = true
	entry.approvedBy = userID
	return nil
}

// Exchange swaps an approved enrollment code for a device credential. The
// first successful exchange starts the one-time trial and registers the
// device plus its credential atomically through the entitlement binder; any
// failure rolls the whole binding back without consuming trial eligibility.
// The code itself is single-use and consumed under a row lock.
func (e *Enrollment) Exchange(ctx context.Context, deviceCode string) (DeviceCredential, error) {
	if ctx == nil {
		return DeviceCredential{}, errors.New("device: nil context")
	}
	if deviceCode == "" {
		return DeviceCredential{}, ErrEnrollmentNotFound
	}
	now := e.now().UTC()
	digest := credential.Digest(deviceCode, e.pepper)
	key := codeKey(digest)

	e.mu.Lock()
	entry, ok := e.pending[key]
	if !ok {
		e.mu.Unlock()
		return DeviceCredential{}, ErrEnrollmentNotFound
	}
	if !now.Before(entry.expiresAt) {
		delete(e.pending, key)
		e.mu.Unlock()
		return DeviceCredential{}, ErrEnrollmentExpired
	}
	if !entry.approved {
		e.mu.Unlock()
		return DeviceCredential{}, ErrEnrollmentPending
	}
	claim := entry.claim
	approvedBy := entry.approvedBy
	e.mu.Unlock()

	record, err := e.store.ConsumeEnrollmentCode(ctx, digest, now)
	if err != nil {
		switch {
		case errors.Is(err, ErrCodeExpired):
			return DeviceCredential{}, ErrEnrollmentExpired
		case errors.Is(err, ErrCodeNotFound):
			return DeviceCredential{}, ErrEnrollmentNotFound
		default:
			return DeviceCredential{}, err
		}
	}
	if record.UserID != approvedBy {
		return DeviceCredential{}, errors.New("device: enrollment owner mismatch")
	}

	token, credentialDigest, err := credential.Generate(deviceCredentialPrefix, deviceCredentialBytes, e.pepper)
	if err != nil {
		return DeviceCredential{}, fmt.Errorf("device: generate device credential: %w", err)
	}
	if _, _, err := e.binder.BindFirstDevice(ctx, entitlement.FirstDeviceBinding{
		UserID:           record.UserID,
		IdentityID:       record.IdentityID,
		Provider:         robloxProvider,
		ProviderSubject:  record.ProviderSubject,
		DeviceID:         claim.DeviceID,
		CredentialDigest: credentialDigest,
		AuditCorrelation: record.ID,
	}); err != nil {
		return DeviceCredential{}, err
	}

	e.mu.Lock()
	delete(e.pending, key)
	e.mu.Unlock()
	return DeviceCredential{Token: token, DeviceID: claim.DeviceID, UserID: record.UserID}, nil
}

func (e *Enrollment) evictExpiredLocked(now time.Time) {
	for key, entry := range e.pending {
		if !now.Before(entry.expiresAt) {
			delete(e.pending, key)
		}
	}
}

func codeKey(digest [32]byte) string {
	return hex.EncodeToString(digest[:])
}

func verificationURL(base, userCode string) string {
	trimmed := strings.TrimRight(base, "/")
	if trimmed == "" {
		trimmed = "/enroll"
	} else {
		trimmed += "/enroll"
	}
	return trimmed + "?code=" + url.QueryEscape(userCode)
}

func validateClaim(claim DeviceClaim) error {
	deviceID := strings.TrimSpace(claim.DeviceID)
	if deviceID == "" || len(deviceID) > 64 {
		return ErrInvalidClaim
	}
	for _, r := range deviceID {
		if !isClaimChar(r) {
			return ErrInvalidClaim
		}
	}
	if len(claim.Hostname) > 255 || len(claim.Name) > 255 || len(claim.Platform) > 64 || len(claim.BridgeVersion) > 64 {
		return ErrInvalidClaim
	}
	return nil
}

func isClaimChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '-' || r == '_' || r == '.':
		return true
	default:
		return false
	}
}

func newEnrollmentID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
