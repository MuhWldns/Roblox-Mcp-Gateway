package device_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		DeviceID:        deviceID,
		Hostname:        "DESKTOP-ABC123",
		Platform:        "windows",
		BridgeVersion:   "1.4.2",
		FingerprintHash: fmt.Sprintf("%064x", []byte(deviceID)),
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

	if _, _, err := stack.enrollment.Begin(t.Context(), device.DeviceClaim{Hostname: "no-id", FingerprintHash: strings.Repeat("a", 64)}); !errors.Is(err, device.ErrInvalidClaim) {
		t.Fatalf("empty device id error = %v, want ErrInvalidClaim", err)
	}
	if _, _, err := stack.enrollment.Begin(t.Context(), device.DeviceClaim{DeviceID: "device-no-fp"}); !errors.Is(err, device.ErrInvalidClaim) {
		t.Fatalf("missing fingerprint error = %v, want ErrInvalidClaim", err)
	}
	if _, _, err := stack.enrollment.Begin(t.Context(), device.DeviceClaim{DeviceID: "device-short-fp", FingerprintHash: "abcd"}); !errors.Is(err, device.ErrInvalidClaim) {
		t.Fatalf("short fingerprint error = %v, want ErrInvalidClaim", err)
	}
	if _, _, err := stack.enrollment.Begin(t.Context(), device.DeviceClaim{DeviceID: "device-bad-hex", FingerprintHash: strings.Repeat("z", 64)}); !errors.Is(err, device.ErrInvalidClaim) {
		t.Fatalf("invalid hex fingerprint error = %v, want ErrInvalidClaim", err)
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
func TestExchangePersistsDeviceClaimMetadata(t *testing.T) {
	stack := newEnrollmentStack(t)
	user := stack.user(t, "1516563360")

	claim := device.DeviceClaim{
		DeviceID:        "device-meta-1",
		Name:            "Custom Device Name",
		Hostname:        "WORKSTATION-X",
		Platform:        "windows",
		BridgeVersion:   "1.5.0",
		FingerprintHash: fmt.Sprintf("%064x", []byte("device-meta-1")),
	}
	code := stack.beginAndApprove(t, user, claim)
	if _, err := stack.enrollment.Exchange(t.Context(), code); err != nil {
		t.Fatalf("exchange: %v", err)
	}

	var name, hostname, platform, bridgeVersion sql.NullString
	err := stack.db.QueryRowContext(t.Context(),
		"SELECT name, hostname, platform, bridge_version FROM devices WHERE id = ?", "device-meta-1",
	).Scan(&name, &hostname, &platform, &bridgeVersion)
	if err != nil {
		t.Fatalf("query device metadata: %v", err)
	}
	if name.String != "Custom Device Name" {
		t.Errorf("name = %q, want 'Custom Device Name'", name.String)
	}
	if hostname.String != "WORKSTATION-X" {
		t.Errorf("hostname = %q, want 'WORKSTATION-X'", hostname.String)
	}
	if platform.String != "windows" {
		t.Errorf("platform = %q, want 'windows'", platform.String)
	}
	if bridgeVersion.String != "1.5.0" {
		t.Errorf("bridge_version = %q, want '1.5.0'", bridgeVersion.String)
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

func TestCrossAccountFingerprintCollisionRejectionAndLookupTransition(t *testing.T) {
	stack := newEnrollmentStack(t)
	user1 := stack.user(t, "user-subject-1")
	user2 := stack.user(t, "user-subject-2")

	fpHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// Account 1 enrolls first.
	claim1 := desktopClaim("dev-account-1")
	claim1.FingerprintHash = fpHash

	userCode1, _, err := stack.enrollment.Begin(t.Context(), claim1)
	if err != nil {
		t.Fatalf("account 1 begin: %v", err)
	}
	pending1, err := stack.enrollment.Lookup(t.Context(), string(userCode1))
	if err != nil {
		t.Fatalf("account 1 initial lookup: %v", err)
	}
	if pending1.Status != "pending" {
		t.Fatalf("account 1 initial status = %q, want 'pending'", pending1.Status)
	}
	if err := stack.enrollment.Approve(t.Context(), user1.ID, string(userCode1)); err != nil {
		t.Fatalf("account 1 approve: %v", err)
	}
	pending1Approved, err := stack.enrollment.Lookup(t.Context(), string(userCode1))
	if err != nil {
		t.Fatalf("account 1 approved lookup: %v", err)
	}
	if pending1Approved.Status != "approved" {
		t.Fatalf("account 1 approved status = %q, want 'approved'", pending1Approved.Status)
	}
	cred1, err := stack.enrollment.Exchange(t.Context(), string(userCode1))
	if err != nil {
		t.Fatalf("account 1 exchange: %v", err)
	}
	if cred1.Token == "" || cred1.DeviceID != "dev-account-1" {
		t.Fatalf("account 1 credential invalid: %+v", cred1)
	}

	// Account 2 attempts to enroll with the exact same device fingerprint.
	claim2 := desktopClaim("dev-account-2")
	claim2.FingerprintHash = fpHash

	userCode2, _, err := stack.enrollment.Begin(t.Context(), claim2)
	if err != nil {
		t.Fatalf("account 2 begin: %v", err)
	}
	pending2, err := stack.enrollment.Lookup(t.Context(), string(userCode2))
	if err != nil {
		t.Fatalf("account 2 initial lookup: %v", err)
	}
	if pending2.Status != "pending" {
		t.Fatalf("account 2 initial status = %q, want 'pending'", pending2.Status)
	}
	if err := stack.enrollment.Approve(t.Context(), user2.ID, string(userCode2)); err != nil {
		t.Fatalf("account 2 approve: %v", err)
	}
	pending2Approved, err := stack.enrollment.Lookup(t.Context(), string(userCode2))
	if err != nil {
		t.Fatalf("account 2 approved lookup: %v", err)
	}
	if pending2Approved.Status != "approved" {
		t.Fatalf("account 2 approved status = %q, want 'approved'", pending2Approved.Status)
	}

	// Account 2 exchange must fail with ErrDeviceAlreadyUsed.
	_, err = stack.enrollment.Exchange(t.Context(), string(userCode2))
	if !errors.Is(err, entitlement.ErrDeviceAlreadyUsed) {
		t.Fatalf("account 2 exchange error = %v, want ErrDeviceAlreadyUsed", err)
	}

	// Lookup must now transition to "license_required".
	pending2LicenseRequired, err := stack.enrollment.Lookup(t.Context(), string(userCode2))
	if err != nil {
		t.Fatalf("account 2 lookup after collision: %v", err)
	}
	if pending2LicenseRequired.Status != "license_required" {
		t.Fatalf("account 2 status after collision = %q, want 'license_required'", pending2LicenseRequired.Status)
	}

	// Verify no trial, trial identity, device, or credential row exists for account 2.
	if got := stack.countRows(t, "SELECT COUNT(*) FROM trial_entitlements WHERE user_id = ?", user2.ID); got != 0 {
		t.Fatalf("account 2 trial_entitlements count = %d, want 0", got)
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM trial_entitlement_identities WHERE user_id = ?", user2.ID); got != 0 {
		t.Fatalf("account 2 trial_entitlement_identities count = %d, want 0", got)
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM devices WHERE user_id = ?", user2.ID); got != 0 {
		t.Fatalf("account 2 devices count = %d, want 0", got)
	}
	if got := stack.countRows(t, "SELECT COUNT(*) FROM device_credentials WHERE user_id = ?", user2.ID); got != 0 {
		t.Fatalf("account 2 device_credentials count = %d, want 0", got)
	}
}

func TestExchangeHandlerReturnsGeneric403OnDeviceAlreadyUsed(t *testing.T) {
	stack := newEnrollmentStack(t)
	user1 := stack.user(t, "handler-user-1")
	user2 := stack.user(t, "handler-user-2")

	fpHash := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	// Account 1 succeeds.
	claim1 := desktopClaim("handler-dev-1")
	claim1.FingerprintHash = fpHash
	code1 := stack.beginAndApprove(t, user1, claim1)
	if _, err := stack.enrollment.Exchange(t.Context(), code1); err != nil {
		t.Fatalf("account 1 exchange: %v", err)
	}

	// Account 2 attempts exchange with the same fingerprint via HTTP handler.
	claim2 := desktopClaim("handler-dev-2")
	claim2.FingerprintHash = fpHash
	code2 := stack.beginAndApprove(t, user2, claim2)

	handler := &device.EnrollmentExchangeHandler{Enrollment: stack.enrollment}
	body, _ := json.Marshal(map[string]string{"device_code": code2})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/device/enrollment/exchange", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("handler status = %d, want 403", rec.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode handler response: %v", err)
	}
	expectedMsg := "You don’t have a license. Please contact support to get a license."
	if resp["error"] != expectedMsg {
		t.Fatalf("error message = %q, want %q", resp["error"], expectedMsg)
	}

	// Response must omit technical / internal error terms.
	bodyStr := rec.Body.String()
	for _, technicalTerm := range []string{"ErrDeviceAlreadyUsed", "fingerprint", "collision", "trial", "SQL", "mysql", "devices"} {
		if strings.Contains(strings.ToLower(bodyStr), strings.ToLower(technicalTerm)) {
			t.Fatalf("handler response leaked technical term %q: %s", technicalTerm, bodyStr)
		}
	}
}
