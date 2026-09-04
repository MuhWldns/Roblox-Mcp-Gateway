package mysqlstore

import (
	"errors"
	"sync"
	"testing"
	"time"

	"robloxkit/internal/credential"
	"robloxkit/internal/device"
	"robloxkit/internal/robloxauth"
)

func TestDeviceStoreInsertAndConsumeEnrollmentCode(t *testing.T) {
	db := identityTestDatabase(t)
	store := NewDeviceStore(db)

	user, err := NewIdentityStore(db).UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{
		Subject: "1516563360", Username: "Builderman", DisplayName: "Builder Man",
	})
	if err != nil {
		t.Fatalf("upsert identity: %v", err)
	}

	_, codeDigest, err := credential.Generate("rkuc_", 6, []byte("pepper"))
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	expires := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	if err := store.InsertEnrollmentCode(t.Context(), device.EnrollmentCode{
		ID: "enroll-1", UserID: user.ID, CodeDigest: codeDigest, ExpiresAt: expires,
	}); err != nil {
		t.Fatalf("insert enrollment code: %v", err)
	}

	var persisted []byte
	if err := db.QueryRowContext(t.Context(), "SELECT code_digest FROM device_enrollment_codes WHERE id = ?", "enroll-1").Scan(&persisted); err != nil {
		t.Fatalf("read stored digest: %v", err)
	}
	if len(persisted) != 32 || string(persisted) != string(codeDigest[:]) {
		t.Fatalf("stored digest mismatch: %x", persisted)
	}

	now := expires.Add(-time.Minute)
	record, err := store.ConsumeEnrollmentCode(t.Context(), codeDigest, now)
	if err != nil {
		t.Fatalf("consume enrollment code: %v", err)
	}
	if record.ID != "enroll-1" || record.UserID != user.ID {
		t.Fatalf("consumed record = %+v", record)
	}
	if record.IdentityID != user.IdentityID || record.ProviderSubject != "1516563360" {
		t.Fatalf("consumed identity = %+v", record)
	}

	var consumedAt *time.Time
	if err := db.QueryRowContext(t.Context(), "SELECT consumed_at FROM device_enrollment_codes WHERE id = ?", "enroll-1").Scan(&consumedAt); err != nil {
		t.Fatalf("read consumed_at: %v", err)
	}
	if consumedAt == nil || !consumedAt.Equal(now) {
		t.Fatalf("consumed_at = %v, want %v", consumedAt, now)
	}
}

func TestDeviceStoreConsumeIsSingleUse(t *testing.T) {
	db := identityTestDatabase(t)
	store := NewDeviceStore(db)
	user, err := NewIdentityStore(db).UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{Subject: "42"})
	if err != nil {
		t.Fatalf("upsert identity: %v", err)
	}
	_, digest, err := credential.Generate("rkuc_", 6, []byte("pepper"))
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	if err := store.InsertEnrollmentCode(t.Context(), device.EnrollmentCode{ID: "enroll-1", UserID: user.ID, CodeDigest: digest, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}); err != nil {
		t.Fatalf("insert enrollment code: %v", err)
	}

	if _, err := store.ConsumeEnrollmentCode(t.Context(), digest, time.Now().UTC()); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if _, err := store.ConsumeEnrollmentCode(t.Context(), digest, time.Now().UTC()); !errors.Is(err, device.ErrCodeConsumed) {
		t.Fatalf("replay consume error = %v, want ErrCodeConsumed", err)
	}
}

func TestDeviceStoreConsumeUnknownAndExpiredCodes(t *testing.T) {
	db := identityTestDatabase(t)
	store := NewDeviceStore(db)
	user, err := NewIdentityStore(db).UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{Subject: "42"})
	if err != nil {
		t.Fatalf("upsert identity: %v", err)
	}

	_, unknown, err := credential.Generate("rkuc_", 6, []byte("pepper"))
	if err != nil {
		t.Fatalf("generate unknown code: %v", err)
	}
	if _, err := store.ConsumeEnrollmentCode(t.Context(), unknown, time.Now().UTC()); !errors.Is(err, device.ErrCodeNotFound) {
		t.Fatalf("unknown consume error = %v, want ErrCodeNotFound", err)
	}

	_, expired, err := credential.Generate("rkuc_", 6, []byte("pepper"))
	if err != nil {
		t.Fatalf("generate expired code: %v", err)
	}
	past := time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)
	if err := store.InsertEnrollmentCode(t.Context(), device.EnrollmentCode{ID: "enroll-old", UserID: user.ID, CodeDigest: expired, ExpiresAt: past.Add(time.Minute)}); err != nil {
		t.Fatalf("insert expired code: %v", err)
	}
	if _, err := store.ConsumeEnrollmentCode(t.Context(), expired, past.Add(2*time.Minute)); !errors.Is(err, device.ErrCodeExpired) {
		t.Fatalf("expired consume error = %v, want ErrCodeExpired", err)
	}
}

func TestDeviceStoreConsumeRequiresRobloxIdentity(t *testing.T) {
	db := identityTestDatabase(t)
	store := NewDeviceStore(db)
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, "INSERT INTO users (id) VALUES (?)", "user-no-identity"); err != nil {
		t.Fatalf("insert bare user: %v", err)
	}
	_, digest, err := credential.Generate("rkuc_", 6, []byte("pepper"))
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	if err := store.InsertEnrollmentCode(ctx, device.EnrollmentCode{ID: "enroll-1", UserID: "user-no-identity", CodeDigest: digest, ExpiresAt: time.Now().UTC().Add(time.Minute)}); err != nil {
		t.Fatalf("insert enrollment code: %v", err)
	}
	if _, err := store.ConsumeEnrollmentCode(ctx, digest, time.Now().UTC()); err == nil {
		t.Fatal("consume succeeded for user without Roblox identity")
	}
}

func TestDeviceStoreConsumeEnrollmentCodeExactlyOnceConcurrently(t *testing.T) {
	db := identityTestDatabase(t)
	store := NewDeviceStore(db)
	user, err := NewIdentityStore(db).UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{Subject: "42"})
	if err != nil {
		t.Fatalf("upsert identity: %v", err)
	}
	_, digest, err := credential.Generate("rkuc_", 6, []byte("pepper"))
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	if err := store.InsertEnrollmentCode(t.Context(), device.EnrollmentCode{ID: "enroll-1", UserID: user.ID, CodeDigest: digest, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}); err != nil {
		t.Fatalf("insert enrollment code: %v", err)
	}

	const attempts = 8
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ConsumeEnrollmentCode(t.Context(), digest, time.Now().UTC())
			results <- err
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent consumes succeeded %d times, want exactly 1", successes)
	}
}

func TestDeviceStoreRobloxIdentity(t *testing.T) {
	db := identityTestDatabase(t)
	store := NewDeviceStore(db)
	user, err := NewIdentityStore(db).UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{
		Subject: "1516563360", Username: "Builderman", DisplayName: "Builder Man",
	})
	if err != nil {
		t.Fatalf("upsert identity: %v", err)
	}

	identity, err := store.RobloxIdentity(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("read roblox identity: %v", err)
	}
	if identity.IdentityID != user.IdentityID || identity.Subject != "1516563360" || identity.DisplayName != "Builder Man" {
		t.Fatalf("identity = %+v", identity)
	}

	if _, err := store.RobloxIdentity(t.Context(), "user-missing"); err == nil {
		t.Fatal("unknown user identity lookup succeeded")
	}
}
