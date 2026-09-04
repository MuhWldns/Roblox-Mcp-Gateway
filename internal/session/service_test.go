package session

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu       sync.Mutex
	sessions map[[32]byte]Session
}

func (m *memoryStore) Insert(_ context.Context, sess Session, digest [32]byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[digest] = sess
	return nil
}

func (m *memoryStore) ValidateAndTouch(_ context.Context, digest [32]byte, when time.Time) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[digest]
	if !ok {
		return Session{}, ErrNotFound
	}
	if sess.RevokedAt != nil {
		return Session{}, ErrRevoked
	}
	if !when.Before(sess.ExpiresAt) {
		return Session{}, ErrExpired
	}
	sess.LastSeenAt = when
	m.sessions[digest] = sess
	return sess, nil
}

func (m *memoryStore) Rotate(_ context.Context, oldDigest [32]byte, next Session, nextDigest [32]byte, when time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.sessions[oldDigest]
	if !ok {
		return ErrNotFound
	}
	if old.RevokedAt != nil || !when.Before(old.ExpiresAt) {
		return ErrRevoked
	}
	old.RevokedAt = &when
	m.sessions[oldDigest] = old
	m.sessions[nextDigest] = next
	return nil
}

func (m *memoryStore) Revoke(_ context.Context, id string, when time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for digest, sess := range m.sessions {
		if sess.ID == id {
			sess.RevokedAt = &when
			m.sessions[digest] = sess
		}
	}
	return nil
}

func (m *memoryStore) RevokeAll(_ context.Context, userID string, when time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for digest, sess := range m.sessions {
		if sess.UserID == userID {
			sess.RevokedAt = &when
			m.sessions[digest] = sess
		}
	}
	return nil
}

func fixedService(now *time.Time) (*Service, *memoryStore) {
	store := &memoryStore{sessions: make(map[[32]byte]Session)}
	service := &Service{
		Store:    store,
		Pepper:   []byte("pepper"),
		Lifetime: time.Hour,
		Now:      func() time.Time { return *now },
	}
	return service, store
}

func TestSessionLifecycleExpiryRotationAndRevokeAll(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	service, _ := fixedService(&now)
	plain, created, err := service.Create(context.Background(), "user1")
	if err != nil {
		t.Fatal(err)
	}
	if created.UserID != "user1" || created.ExpiresAt != now.Add(time.Hour) {
		t.Fatalf("bad session: %#v", created)
	}
	if _, err := service.Validate(context.Background(), plain); err != nil {
		t.Fatal(err)
	}

	rotated, _, err := service.Rotate(context.Background(), plain)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Validate(context.Background(), plain); err == nil {
		t.Fatal("old token still valid")
	}
	if _, err := service.Validate(context.Background(), rotated); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Hour + time.Nanosecond)
	if _, err := service.Validate(context.Background(), rotated); err == nil {
		t.Fatal("expired token valid")
	}
	now = now.Add(-time.Minute)
	if err := service.RevokeAll(context.Background(), "user1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Validate(context.Background(), rotated); err == nil {
		t.Fatal("revoked token valid")
	}
}

func TestConcurrentRotationOnlyOneSucceeds(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	service, _ := fixedService(&now)
	plain, _, err := service.Create(context.Background(), "user1")
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _, rotateErr := service.Rotate(context.Background(), plain)
			results <- rotateErr
		}()
	}
	wait.Wait()
	close(results)

	successes := 0
	for rotateErr := range results {
		if rotateErr == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("rotation successes=%d, want 1", successes)
	}
}

func TestSessionCookieMetadata(t *testing.T) {
	cookie := Cookie("opaque", 3600)
	if cookie.Name != "__Host-robloxkit_session" || cookie.Value != "opaque" || cookie.Path != "/" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Domain != "" {
		t.Fatalf("unexpected cookie metadata: %#v", cookie)
	}
}

type controlledStore struct {
	*memoryStore
	validateStarted chan struct{}
	validateRelease chan struct{}
}

func (s *controlledStore) ValidateAndTouch(ctx context.Context, digest [32]byte, when time.Time) (Session, error) {
	if s.validateStarted != nil {
		close(s.validateStarted)
		<-s.validateRelease
	}
	return s.memoryStore.ValidateAndTouch(ctx, digest, when)
}

func TestValidateAndRevokeAllLinearization(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	base := &memoryStore{sessions: make(map[[32]byte]Session)}
	service := &Service{Store: base, Pepper: []byte("pepper"), Lifetime: time.Hour, Now: func() time.Time { return now }}
	plain, _, err := service.Create(context.Background(), "user1")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.RevokeAll(context.Background(), "user1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Validate(context.Background(), plain); !errors.Is(err, ErrRevoked) {
		t.Fatalf("validate after completed revoke-all = %v, want ErrRevoked", err)
	}

	base = &memoryStore{sessions: make(map[[32]byte]Session)}
	controlled := &controlledStore{memoryStore: base, validateStarted: make(chan struct{}), validateRelease: make(chan struct{})}
	service.Store = controlled
	plain, _, err = service.Create(context.Background(), "user1")
	if err != nil {
		t.Fatal(err)
	}
	validation := make(chan error, 1)
	go func() { _, validateErr := service.Validate(context.Background(), plain); validation <- validateErr }()
	<-controlled.validateStarted
	if err := service.RevokeAll(context.Background(), "user1"); err != nil {
		t.Fatal(err)
	}
	close(controlled.validateRelease)
	if err := <-validation; !errors.Is(err, ErrRevoked) {
		t.Fatalf("validate after revoke-all linearized first = %v, want ErrRevoked", err)
	}
}
