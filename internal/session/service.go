// Package session implements revocable opaque browser sessions.
package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"time"

	"robloxkit/internal/credential"
)

var (
	ErrNotFound = errors.New("session: not found")
	ErrExpired  = errors.New("session: expired")
	ErrRevoked  = errors.New("session: revoked")
	ErrInvalid  = errors.New("session: invalid")
)

type Session struct {
	ID         string
	UserID     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time
}

type Clock interface {
	Now() time.Time
}

type Store interface {
	Insert(context.Context, Session, [32]byte) error
	ValidateAndTouch(context.Context, [32]byte, time.Time) (Session, error)
	Rotate(context.Context, [32]byte, Session, [32]byte, time.Time) error
	Revoke(context.Context, string, time.Time) error
	RevokeAll(context.Context, string, time.Time) error
}

type Service struct {
	Store    Store
	Pepper   []byte
	Lifetime time.Duration
	Now      func() time.Time
	Clock    Clock
}

const (
	tokenPrefix = "rks_"
	tokenBytes  = 32
	CookieName  = "__Host-robloxkit_session"
)

// NewService constructs a session service. The pepper is copied so callers
// may safely reuse or clear their input buffer after construction.
func NewService(store Store, pepper []byte, lifetime time.Duration) *Service {
	return &Service{
		Store:    store,
		Pepper:   append([]byte(nil), pepper...),
		Lifetime: lifetime,
	}
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now().UTC()
	}
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) validateConfig() error {
	if s.Store == nil {
		return errors.New("session: nil store")
	}
	if len(s.Pepper) == 0 {
		return errors.New("session: empty pepper")
	}
	if s.Lifetime <= 0 {
		return errors.New("session: non-positive lifetime")
	}
	return nil
}

func (s *Service) Create(ctx context.Context, userID string) (string, Session, error) {
	if ctx == nil {
		return "", Session{}, errors.New("session: nil context")
	}
	if err := s.validateConfig(); err != nil {
		return "", Session{}, err
	}
	if userID == "" {
		return "", Session{}, errors.New("session: empty user id")
	}

	plain, digest, err := credential.Generate(tokenPrefix, tokenBytes, s.Pepper)
	if err != nil {
		return "", Session{}, fmt.Errorf("session: generate token: %w", err)
	}
	now := s.now()
	id, err := newID()
	if err != nil {
		return "", Session{}, fmt.Errorf("session: generate id: %w", err)
	}

	sess := Session{
		ID:         id,
		UserID:     userID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(s.Lifetime),
	}
	if err := s.Store.Insert(ctx, sess, digest); err != nil {
		return "", Session{}, fmt.Errorf("session: insert: %w", err)
	}
	return plain, sess, nil
}

func (s *Service) Validate(ctx context.Context, plain string) (Session, error) {
	if ctx == nil {
		return Session{}, errors.New("session: nil context")
	}
	if err := s.validateConfig(); err != nil {
		return Session{}, err
	}
	if plain == "" {
		return Session{}, ErrInvalid
	}
	return s.Store.ValidateAndTouch(ctx, credential.Digest(plain, s.Pepper), s.now())
}

func (s *Service) Rotate(ctx context.Context, plain string) (string, Session, error) {
	if ctx == nil {
		return "", Session{}, errors.New("session: nil context")
	}
	if err := s.validateConfig(); err != nil {
		return "", Session{}, err
	}
	if plain == "" {
		return "", Session{}, ErrInvalid
	}

	now := s.now()
	oldDigest := credential.Digest(plain, s.Pepper)
	old, err := s.Store.ValidateAndTouch(ctx, oldDigest, now)
	if err != nil {
		return "", Session{}, err
	}
	newPlain, newDigest, err := credential.Generate(tokenPrefix, tokenBytes, s.Pepper)
	if err != nil {
		return "", Session{}, fmt.Errorf("session: generate token: %w", err)
	}
	id, err := newID()
	if err != nil {
		return "", Session{}, fmt.Errorf("session: generate id: %w", err)
	}
	next := Session{
		ID:         id,
		UserID:     old.UserID,
		CreatedAt:  now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(s.Lifetime),
	}
	if err := s.Store.Rotate(ctx, oldDigest, next, newDigest, now); err != nil {
		return "", Session{}, err
	}
	return newPlain, next, nil
}

func (s *Service) RevokeAll(ctx context.Context, userID string) error {
	if ctx == nil {
		return errors.New("session: nil context")
	}
	if err := s.validateConfig(); err != nil {
		return err
	}
	if userID == "" {
		return errors.New("session: empty user id")
	}
	return s.Store.RevokeAll(ctx, userID, s.now())
}

// Cookie returns the required session cookie metadata. Domain is intentionally
// omitted so the __Host- prefix remains valid.
func Cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
