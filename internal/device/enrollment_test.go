package device_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"robloxkit/internal/audit"
	"robloxkit/internal/device"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/mysqlstore"
	"robloxkit/internal/robloxauth"
)

const testPepper = "device-test-pepper"

// mutableClock lets tests advance time deterministically.
type mutableClock struct{ now time.Time }

func (c *mutableClock) Now() time.Time { return c.now }

func enrollmentTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	rawDSN := os.Getenv("MYSQL_TEST_DSN")
	if rawDSN == "" {
		t.Skip("MYSQL_TEST_DSN is not configured")
	}
	base, err := mysql.ParseDSN(rawDSN)
	if err != nil {
		t.Fatalf("parse MYSQL_TEST_DSN: %v", err)
	}
	adminConfig := *base
	adminConfig.DBName = ""
	admin, err := sql.Open("mysql", adminConfig.FormatDSN())
	if err != nil {
		t.Fatalf("open admin database: %v", err)
	}
	t.Cleanup(func() {
		if err := admin.Close(); err != nil {
			t.Errorf("close admin database: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping MYSQL_TEST_DSN: %v", err)
	}
	dbName := fmt.Sprintf("robloxkit_device_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+dbName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create temporary database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+dbName+"`"); err != nil {
			t.Errorf("drop temporary database: %v", err)
		}
	})
	target := *base
	target.DBName = dbName
	target.ParseTime = true
	target.Loc = time.UTC
	db, err := sql.Open("mysql", target.FormatDSN())
	if err != nil {
		t.Fatalf("open temporary database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close temporary database: %v", err)
		}
	})
	if _, err := mysqlstore.Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}

// enrollmentStack wires enrollment, entitlement, audit, and MySQL stores.
type enrollmentStack struct {
	db          *sql.DB
	clock       *mutableClock
	enrollment  *device.Enrollment
	entitlement *entitlement.Service
}

func newEnrollmentStack(t *testing.T) *enrollmentStack {
	t.Helper()
	db := enrollmentTestDatabase(t)
	clock := &mutableClock{now: time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC)}
	auditSvc := audit.NewService(mysqlstore.NewAuditStore(db))
	entSvc := entitlement.NewService(mysqlstore.NewEntitlementStore(db, clock, auditSvc), clock)
	store := mysqlstore.NewDeviceStore(db)
	enrollment, err := device.NewEnrollment(store, entSvc, []byte(testPepper), clock.Now)
	if err != nil {
		t.Fatalf("construct enrollment: %v", err)
	}
	enrollment.VerificationBaseURL = "https://app.example.com"
	return &enrollmentStack{db: db, clock: clock, enrollment: enrollment, entitlement: entSvc}
}

func (s *enrollmentStack) user(t *testing.T, subject string) robloxauth.User {
	t.Helper()
	user, err := mysqlstore.NewIdentityStore(s.db).UpsertRobloxIdentity(t.Context(), robloxauth.RobloxIdentity{
		Subject: subject, Username: "builder", DisplayName: "Builder " + subject,
	})
	if err != nil {
		t.Fatalf("upsert identity %q: %v", subject, err)
	}
	return user
}

func (s *enrollmentStack) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func desktopClaim(deviceID string) device.DeviceClaim {
	return device.DeviceClaim{
		DeviceID:      deviceID,
		Hostname:      "DESKTOP-ABC123",
		Platform:      "windows",
		BridgeVersion: "1.4.2",
	}
}

// beginAndApprove drives Begin -> Approve and returns the pairing code. The
// bridge reuses the same opaque code as the device code for Exchange.
func (s *enrollmentStack) beginAndApprove(t *testing.T, user robloxauth.User, claim device.DeviceClaim) string {
	t.Helper()
	userCode, _, err := s.enrollment.Begin(t.Context(), claim)
	if err != nil {
		t.Fatalf("begin enrollment: %v", err)
	}
	if err := s.enrollment.Approve(t.Context(), user.ID, string(userCode)); err != nil {
		t.Fatalf("approve enrollment: %v", err)
	}
	return string(userCode)
}

func TestBeginReturnsUserCodeAndVerificationURL(t *testing.T) {
	stack := newEnrollmentStack(t)

	userCode, verificationURL, err := stack.enrollment.Begin(t.Context(), desktopClaim("device-alpha"))
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if !strings.HasPrefix(string(userCode), "rkuc_") {
		t.Fatalf("user code = %q, want rkuc_ prefix", userCode)
	}
	if !strings.HasPrefix(string(verificationURL), "https://app.example.com/enroll?code=") {
		t.Fatalf("verification URL = %q", verificationURL)
	}
	if !strings.Contains(string(verificationURL), string(userCode)) {
		t.Fatalf("verification URL %q does not embed user code %q", verificationURL, userCode)
	}

	pending, err := stack.enrollment.Lookup(t.Context(), string(userCode))
	if err != nil {
		t.Fatalf("lookup pending: %v", err)
	}
	if pending.Hostname != "DESKTOP-ABC123" || pending.DeviceID != "device-alpha" {
		t.Fatalf("pending claim = %+v", pending)
	}
	if pending.BridgeVersion != "1.4.2" || pending.Platform != "windows" {
		t.Fatalf("pending claim versions = %+v", pending)
	}
	if !pending.ExpiresAt.After(stack.clock.now) {
		t.Fatalf("pending expiry %v is not in the future", pending.ExpiresAt)
	}
}

func TestBeginRejectsInvalidClaims(t *testing.T) {
	stack := newEnrollmentStack(t)

	if _, _, err := stack.enrollment.Begin(t.Context(), device.DeviceClaim{Hostname: "no-id"}); !errors.Is(err, device.ErrInvalidClaim) {
		t.Fatalf("empty device id error = %v, want ErrInvalidClaim", err)
	}
}

func TestApproveRequiresSessionUser(t *testing.T) {
	stack := newEnrollmentStack(t)
	userCode, _, err := stack.enrollment.Begin(t.Context(), desktopClaim("device-alpha"))
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	if err := stack.enrollment.Approve(t.Context(), "", string(userCode)); !errors.Is(err, device.ErrApprovalOwnerRequired) {
		t.Fatalf("ownerless approve error = %v, want ErrApprovalOwnerRequired", err)
	}
	if err := stack.enrollment.Approve(t.Context(), "", ""); !errors.Is(err, device.ErrApprovalOwnerRequired) {
		t.Fatalf("empty approve error = %v, want ErrApprovalOwnerRequired", err)
	}
}

func TestApproveRejectsUnknownCode(t *testing.T) {
	stack := newEnrollmentStack(t)

	if err := stack.enrollment.Approve(t.Context(), "user-1", "rkuc_missing"); !errors.Is(err, device.ErrEnrollmentNotFound) {
		t.Fatalf("unknown code error = %v, want ErrEnrollmentNotFound", err)
	}
}

func TestUserCodesAreSingleUse(t *testing.T) {
	stack := newEnrollmentStack(t)
	user := stack.user(t, "1516563360")

	userCode := stack.beginAndApprove(t, user, desktopClaim("device-alpha"))

	if err := stack.enrollment.Approve(t.Context(), user.ID, userCode); !errors.Is(err, device.ErrEnrollmentNotFound) {
		t.Fatalf("replay approve error = %v, want ErrEnrollmentNotFound", err)
	}
}

func TestPendingEnrollmentExpires(t *testing.T) {
	stack := newEnrollmentStack(t)

	// Both enrollments start before the clock advances.
	lookupCode, _, err := stack.enrollment.Begin(t.Context(), desktopClaim("device-lookup"))
	if err != nil {
		t.Fatalf("begin lookup: %v", err)
	}
	approveCode, _, err := stack.enrollment.Begin(t.Context(), desktopClaim("device-approve"))
	if err != nil {
		t.Fatalf("begin approve: %v", err)
	}
	stack.clock.now = stack.clock.now.Add(stack.enrollment.PendingTTL + time.Second)

	// Lookup surfaces expiry for its own pending enrollment.
	if _, err := stack.enrollment.Lookup(t.Context(), string(lookupCode)); !errors.Is(err, device.ErrEnrollmentExpired) {
		t.Fatalf("expired lookup error = %v, want ErrEnrollmentExpired", err)
	}

	// Approve surfaces expiry for its own pending enrollment.
	if err := stack.enrollment.Approve(t.Context(), "user-1", string(approveCode)); !errors.Is(err, device.ErrEnrollmentExpired) {
		t.Fatalf("expired approve error = %v, want ErrEnrollmentExpired", err)
	}
}

func TestExchangeUnknownOrExpiredDeviceCodeFails(t *testing.T) {
	stack := newEnrollmentStack(t)
	user := stack.user(t, "1516563360")

	if _, err := stack.enrollment.Exchange(t.Context(), "rkuc_unknown"); !errors.Is(err, device.ErrEnrollmentNotFound) {
		t.Fatalf("unknown exchange error = %v, want ErrEnrollmentNotFound", err)
	}

	deviceCode := stack.beginAndApprove(t, user, desktopClaim("device-alpha"))
	stack.clock.now = stack.clock.now.Add(stack.enrollment.CodeTTL + time.Second)
	if _, err := stack.enrollment.Exchange(t.Context(), deviceCode); !errors.Is(err, device.ErrEnrollmentExpired) {
		t.Fatalf("expired exchange error = %v, want ErrEnrollmentExpired", err)
	}
}

func TestExchangeStartsExactlyOneTrialAtomically(t *testing.T) {
	stack := newEnrollmentStack(t)
	user := stack.user(t, "1516563360")

	userCode, _, err := stack.enrollment.Begin(t.Context(), desktopClaim("device-alpha"))
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	deviceCode := string(userCode)
	if got := stack.countRows(t, "SELECT COUNT(*) FROM trial_entitlements"); got != 0 {
		t.Fatalf("begin created %d trial rows, want 0", got)
	}
	if err := stack.enrollment.Approve(t.Context(), user.ID, deviceCode); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM trial_entitlements"); got != 0 {
		t.Fatalf("approve created %d trial rows, want 0", got)
	}

	credential, err := stack.enrollment.Exchange(t.Context(), deviceCode)
	if !strings.HasPrefix(credential.Token, "rkd_") {
		t.Fatalf("device credential = %q, want rkd_ prefix", credential.Token)
	}
	if credential.DeviceID != "device-alpha" {
		t.Fatalf("credential device id = %q, want device-alpha", credential.DeviceID)
	}

	if got := stack.countRows(t, "SELECT COUNT(*) FROM trial_entitlements"); got != 1 {
		t.Fatalf("trial rows after exchange = %d, want 1", got)
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM trial_entitlement_identities"); got != 1 {
		t.Fatalf("trial identity rows after exchange = %d, want 1", got)
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM device_credentials WHERE device_id = ?", "device-alpha"); got != 1 {
		t.Fatalf("device credential rows after exchange = %d, want 1", got)
	}

	decision, err := stack.entitlement.Authorize(t.Context(), entitlement.Subject{UserID: user.ID, Provider: "roblox", ProviderSubject: user.RobloxSubject})
	if err != nil {
		t.Fatalf("authorize after exchange: %v", err)
	}
	if !decision.Active {
		t.Fatalf("decision after exchange = %+v, want active trial", decision)
	}

	// The code is spent; replays must fail and never mint a second trial.
	if _, err := stack.enrollment.Exchange(t.Context(), deviceCode); err == nil {
		t.Fatal("device code replay unexpectedly succeeded")
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM trial_entitlements"); got != 1 {
		t.Fatalf("trial rows after replay = %d, want 1", got)
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM device_credentials"); got != 1 {
		t.Fatalf("credential rows after replay = %d, want 1", got)
	}
}

func TestExhaustedTrialSlotLeavesNoNewRows(t *testing.T) {
	stack := newEnrollmentStack(t)
	user := stack.user(t, "1516563360")

	firstCode := stack.beginAndApprove(t, user, desktopClaim("device-one"))
	if _, err := stack.enrollment.Exchange(t.Context(), firstCode); err != nil {
		t.Fatalf("first exchange: %v", err)
	}

	secondCode := stack.beginAndApprove(t, user, desktopClaim("device-two"))
	if _, err := stack.enrollment.Exchange(t.Context(), secondCode); !errors.Is(err, entitlement.ErrTrialAlreadyUsed) {
		t.Fatalf("second exchange error = %v, want ErrTrialAlreadyUsed", err)
	}

	if got := stack.countRows(t, "SELECT COUNT(*) FROM trial_entitlements"); got != 1 {
		t.Fatalf("trial rows = %d, want 1", got)
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM trial_entitlement_identities"); got != 1 {
		t.Fatalf("trial identity rows = %d, want 1", got)
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM device_credentials"); got != 1 {
		t.Fatalf("credential rows = %d, want 1", got)
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM devices WHERE id = ?", "device-two"); got != 0 {
		t.Fatalf("failed exchange persisted device rows = %d, want 0", got)
	}
}

func TestFailedExchangePreservesExistingTrialWindow(t *testing.T) {
	stack := newEnrollmentStack(t)
	user := stack.user(t, "1516563360")

	firstCode := stack.beginAndApprove(t, user, desktopClaim("device-one"))
	if _, err := stack.enrollment.Exchange(t.Context(), firstCode); err != nil {
		t.Fatalf("first exchange: %v", err)
	}

	var startedAt, endsAt time.Time
	if err := stack.db.QueryRowContext(t.Context(),
		"SELECT started_at, ends_at FROM trial_entitlements WHERE user_id = ?", user.ID,
	).Scan(&startedAt, &endsAt); err != nil {
		t.Fatalf("read trial window: %v", err)
	}

	// A second device for the same owner has no free slot: the exchange must
	// fail without touching the existing trial window.
	secondCode := stack.beginAndApprove(t, user, desktopClaim("device-two"))
	if _, err := stack.enrollment.Exchange(t.Context(), secondCode); !errors.Is(err, entitlement.ErrTrialAlreadyUsed) {
		t.Fatalf("second exchange error = %v, want ErrTrialAlreadyUsed", err)
	}
	var afterStartedAt, afterEndsAt time.Time
	if err := stack.db.QueryRowContext(t.Context(),
		"SELECT started_at, ends_at FROM trial_entitlements WHERE user_id = ?", user.ID,
	).Scan(&afterStartedAt, &afterEndsAt); err != nil {
		t.Fatalf("read trial window after failure: %v", err)
	}
	if !afterStartedAt.Equal(startedAt) || !afterEndsAt.Equal(endsAt) {
		t.Fatalf("trial window changed: before %v-%v after %v-%v", startedAt, endsAt, afterStartedAt, afterEndsAt)
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM trial_entitlements"); got != 1 {
		t.Fatalf("trial rows = %d, want 1", got)
	}
}

func TestEnrollmentConstructorRejectsInvalidInputs(t *testing.T) {
	db := enrollmentTestDatabase(t)
	clock := &mutableClock{now: time.Now().UTC()}
	auditSvc := audit.NewService(mysqlstore.NewAuditStore(db))
	entSvc := entitlement.NewService(mysqlstore.NewEntitlementStore(db, clock, auditSvc), clock)
	store := mysqlstore.NewDeviceStore(db)

	if _, err := device.NewEnrollment(nil, entSvc, []byte("pepper"), clock.Now); err == nil {
		t.Fatal("constructor accepted nil store")
	}
	if _, err := device.NewEnrollment(store, nil, []byte("pepper"), clock.Now); err == nil {
		t.Fatal("constructor accepted nil binder")
	}
	if _, err := device.NewEnrollment(store, entSvc, nil, clock.Now); err == nil {
		t.Fatal("constructor accepted nil pepper")
	}
	if _, err := device.NewEnrollment(store, entSvc, []byte("pepper"), nil); err == nil {
		t.Fatal("constructor accepted nil clock")
	}
}
