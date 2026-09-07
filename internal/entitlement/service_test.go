package entitlement_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"robloxkit/internal/audit"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/mysqlstore"
)

// fixedClock pins policy evaluation to a deterministic instant.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

func digest(plain string) [32]byte { return sha256.Sum256([]byte(plain)) }

func trialBase() time.Time { return time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC) }

// newStack wires the full service -> store -> audit chain against a fresh,
// migrated test database.
func newStack(t *testing.T, clock entitlement.Clock) (*entitlement.Service, *mysqlstore.EntitlementStore, *sql.DB) {
	t.Helper()
	db := entitlementTestDatabase(t)
	auditService := audit.NewService(mysqlstore.NewAuditStore(db))
	store := mysqlstore.NewEntitlementStore(db, clock, auditService)
	return entitlement.NewService(store, clock), store, db
}

func firstBinding(userID, identityID, subject, deviceID string, cred [32]byte) entitlement.FirstDeviceBinding {
	return entitlement.FirstDeviceBinding{
		UserID:           userID,
		IdentityID:       identityID,
		Provider:         "roblox",
		ProviderSubject:  subject,
		DeviceID:         deviceID,
		CredentialDigest: cred,
	}
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func TestFirstBindingStartsExactlyOneFourteenDayTrial(t *testing.T) {
	clock := fixedClock{trialBase()}
	svc, _, _ := newStack(t, clock)
	ent, _, err := svc.BindFirstDevice(t.Context(), firstBinding("user_1", "identity_1", "1516563360", "device_1", digest("credential")))
	if err != nil {
		t.Fatalf("BindFirstDevice: %v", err)
	}
	if !ent.StartedAt.Equal(clock.Now().UTC()) {
		t.Fatalf("started = %v, want %v", ent.StartedAt, clock.Now().UTC())
	}
	if want := clock.Now().Add(14 * 24 * time.Hour); !ent.EndsAt.Equal(want) {
		t.Fatalf("ends = %v, want %v", ent.EndsAt, want)
	}
}

func TestTrialSecondBindingDoesNotCreateSecondTrial(t *testing.T) {
	clock := fixedClock{trialBase()}
	svc, _, db := newStack(t, clock)
	if _, _, err := svc.BindFirstDevice(t.Context(), firstBinding("u1", "i1", "100", "d1", digest("c1"))); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	_, _, err := svc.BindFirstDevice(t.Context(), firstBinding("u1", "i1", "100", "d2", digest("c2")))
	if !errors.Is(err, entitlement.ErrTrialAlreadyUsed) {
		t.Fatalf("second bind error = %v, want ErrTrialAlreadyUsed", err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM trial_entitlements"); got != 1 {
		t.Fatalf("trial rows = %d, want 1", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM trial_entitlement_identities"); got != 1 {
		t.Fatalf("trial identity rows = %d, want 1", got)
	}
}

func TestTrialHistoricalIdentityIsIneligibleAcrossAccounts(t *testing.T) {
	clock := fixedClock{trialBase()}
	svc, _, db := newStack(t, clock)
	if _, _, err := svc.BindFirstDevice(t.Context(), firstBinding("uA", "ia", "777", "dA", digest("ca"))); err != nil {
		t.Fatalf("first account bind: %v", err)
	}
	_, _, err := svc.BindFirstDevice(t.Context(), firstBinding("uB", "ib", "777", "dB", digest("cb")))
	if !errors.Is(err, entitlement.ErrTrialAlreadyUsed) {
		t.Fatalf("historical identity error = %v, want ErrTrialAlreadyUsed", err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM trial_entitlement_identities WHERE provider_subject = '777'"); got != 1 {
		t.Fatalf("historical trial identity rows = %d, want 1", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM trial_entitlements WHERE user_id = 'uB'"); got != 0 {
		t.Fatalf("second account got %d trials, want 0", got)
	}
}

func TestTrialFailedBindingConsumesNoEligibility(t *testing.T) {
	clock := fixedClock{trialBase()}
	svc, _, db := newStack(t, clock)
	if _, _, err := svc.BindFirstDevice(t.Context(), firstBinding("uA", "ia", "888", "dA", digest("shared"))); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	// Reuse the digest to force a late constraint failure after the trial and
	// identity rows are already inserted; the transaction must roll back cleanly.
	_, _, err := svc.BindFirstDevice(t.Context(), firstBinding("uB", "ib", "999", "dB", digest("shared")))
	if err == nil {
		t.Fatal("duplicate-digest bind unexpectedly succeeded")
	}
	if errors.Is(err, entitlement.ErrTrialAlreadyUsed) {
		t.Fatalf("duplicate-digest failure mislabeled as trial reuse: %v", err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM trial_entitlement_identities WHERE provider_subject = '999'"); got != 0 {
		t.Fatalf("failed bind consumed eligibility: %d identity rows", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM trial_entitlements WHERE user_id = 'uB'"); got != 0 {
		t.Fatalf("failed bind consumed eligibility: %d trial rows", got)
	}
	// The same subject remains eligible on retry.
	if _, _, err := svc.BindFirstDevice(t.Context(), firstBinding("uB", "ib", "999", "dB", digest("fresh"))); err != nil {
		t.Fatalf("retry after failed bind: %v", err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM trial_entitlement_identities WHERE provider_subject = '999'"); got != 1 {
		t.Fatalf("post-retry identity rows = %d, want 1", got)
	}
}

func TestTrialLoginDownloadAndAuthorizeDoNotStartTrial(t *testing.T) {
	clock := fixedClock{trialBase()}
	svc, _, db := newStack(t, clock)
	dec, err := svc.Authorize(t.Context(), entitlement.Subject{UserID: "u1", Provider: "roblox", ProviderSubject: "111"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if dec.Active {
		t.Fatal("fresh subject unexpectedly active")
	}
	if !dec.Permits(entitlement.ActionDownload) || !dec.Permits(entitlement.ActionDashboard) {
		t.Fatal("download/dashboard must be permitted without any trial")
	}
	if dec.Permits(entitlement.ActionEnroll) || dec.Permits(entitlement.ActionMCP) || dec.Permits(entitlement.ActionWSS) {
		t.Fatal("enroll/WSS/MCP must be denied without an active trial")
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM trial_entitlements"); got != 0 {
		t.Fatalf("authorize created %d trials, want 0", got)
	}
}

func TestTrialExpiryBlocksRuntimeButPermitsDashboardDownload(t *testing.T) {
	clock := fixedClock{trialBase()}
	svc, store, _ := newStack(t, clock)
	if _, _, err := svc.BindFirstDevice(t.Context(), firstBinding("u1", "i1", "222", "d1", digest("c1"))); err != nil {
		t.Fatalf("bind: %v", err)
	}
	active, err := svc.Authorize(t.Context(), entitlement.Subject{UserID: "u1", Provider: "roblox", ProviderSubject: "222"})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if !active.Active || !active.Permits(entitlement.ActionEnroll) || !active.Permits(entitlement.ActionMCP) || !active.Permits(entitlement.ActionWSS) {
		t.Fatalf("active decision = %+v, want active runtime", active)
	}

	late := fixedClock{trialBase().Add(14*24*time.Hour + time.Second)}
	expired, err := entitlement.NewService(store, late).Authorize(t.Context(), entitlement.Subject{UserID: "u1", Provider: "roblox", ProviderSubject: "222"})
	if err != nil {
		t.Fatalf("Authorize(expired): %v", err)
	}
	if expired.Active || !expired.Expired() {
		t.Fatalf("expired decision = %+v, want expired", expired)
	}
	if expired.Permits(entitlement.ActionEnroll) || expired.Permits(entitlement.ActionWSS) || expired.Permits(entitlement.ActionMCP) {
		t.Fatal("expired trial must block enrollment/WSS/MCP")
	}
	if !expired.Permits(entitlement.ActionDashboard) || !expired.Permits(entitlement.ActionDownload) {
		t.Fatal("expired trial must still permit dashboard/download")
	}
}

func TestTrialRevokeReinstallTransferRecoveryDoNotReset(t *testing.T) {
	clock := fixedClock{trialBase()}
	svc, store, db := newStack(t, clock)
	ent, _, err := svc.BindFirstDevice(t.Context(), firstBinding("u1", "i1", "333", "d1", digest("c1")))
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	assertTrialWindow := func() {
		t.Helper()
		var started, ends time.Time
		if err := db.QueryRowContext(t.Context(), "SELECT started_at, ends_at FROM trial_entitlements WHERE user_id = 'u1'").Scan(&started, &ends); err != nil {
			t.Fatalf("read trial: %v", err)
		}
		if !started.Equal(ent.StartedAt) || !ends.Equal(ent.EndsAt) {
			t.Fatalf("trial window changed: started %v (want %v) ends %v (want %v)", started, ent.StartedAt, ends, ent.EndsAt)
		}
	}

	if err := store.RevokeDevice(t.Context(), "d1", trialBase().Add(time.Hour)); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	assertTrialWindow()

	if _, _, err := svc.BindFirstDevice(t.Context(), firstBinding("u1", "i1", "333", "d9", digest("reinstall"))); !errors.Is(err, entitlement.ErrTrialAlreadyUsed) {
		t.Fatalf("reinstall error = %v, want ErrTrialAlreadyUsed", err)
	}
	assertTrialWindow()

	lic, err := store.CreateLicense(t.Context(), "u1", "", 1)
	if err != nil {
		t.Fatalf("CreateLicense: %v", err)
	}
	if _, err := store.BindDeviceSlot(t.Context(), lic.ID, "d1"); err != nil {
		t.Fatalf("BindDeviceSlot: %v", err)
	}
	if err := store.CreateDevice(t.Context(), "u1", "d2", "d2"); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	if err := svc.TransferDevice(t.Context(), entitlement.AdminActor{UserID: "admin1"}, lic.ID, "d1", "d2", "admin transfer"); err != nil {
		t.Fatalf("TransferDevice: %v", err)
	}
	assertTrialWindow()

	if err := svc.RecoverIdentity(t.Context(), entitlement.AdminActor{UserID: "admin1"}, "u1", "i2", "recovery", "evidence-1"); err != nil {
		t.Fatalf("RecoverIdentity: %v", err)
	}
	assertTrialWindow()
}

func TestRecoveryRevokesAllCredentialsAndPreservesTrial(t *testing.T) {
	clock := fixedClock{trialBase()}
	svc, _, db := newStack(t, clock)
	ent, _, err := svc.BindFirstDevice(t.Context(), firstBinding("u1", "i1", "444", "d1", digest("c1")))
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	// Add a second credential so "all credentials" is meaningful.
	extraDigest := digest("c2")
	if _, err := db.ExecContext(t.Context(), "INSERT INTO device_credentials (id, user_id, device_id, credential_digest, expires_at, revoked_at) VALUES (?, ?, ?, ?, NULL, NULL)", "cred-extra-1", "u1", "d1", extraDigest[:]); err != nil {
		t.Fatalf("insert second credential: %v", err)
	}
	if err := svc.RecoverIdentity(t.Context(), entitlement.AdminActor{UserID: "admin1"}, "u1", "i2", "stolen account", "evidence-9"); err != nil {
		t.Fatalf("RecoverIdentity: %v", err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM device_credentials WHERE user_id = 'u1' AND revoked_at IS NULL"); got != 0 {
		t.Fatalf("%d unrevoked credentials remain, want 0", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM account_recovery_cases WHERE user_id = 'u1'"); got != 1 {
		t.Fatalf("recovery cases = %d, want 1", got)
	}
	var started, ends time.Time
	if err := db.QueryRowContext(t.Context(), "SELECT started_at, ends_at FROM trial_entitlements WHERE user_id = 'u1'").Scan(&started, &ends); err != nil {
		t.Fatalf("read trial: %v", err)
	}
	if !started.Equal(ent.StartedAt) || !ends.Equal(ent.EndsAt) {
		t.Fatalf("recovery reset trial: started %v ends %v", started, ends)
	}
}

func TestReclaimSameDeviceMintsNewCredentialAndKeepsTrialWindow(t *testing.T) {
	clock := fixedClock{trialBase()}
	svc, _, db := newStack(t, clock)
	ent, _, err := svc.BindFirstDevice(t.Context(), firstBinding("u1", "i1", "555", "d1", digest("c1")))
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	later := fixedClock{trialBase().Add(3 * 24 * time.Hour)}
	rebound, binding, err := entitlement.NewService(storeOver(db, fixedClock{trialBase()}), later).BindFirstDevice(t.Context(), firstBinding("u1", "i1", "555", "d1", digest("c2")))
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if !rebound.StartedAt.Equal(ent.StartedAt) || !rebound.EndsAt.Equal(ent.EndsAt) {
		t.Fatalf("re-claim restarted trial window: %+v, want %+v", rebound, ent)
	}
	if binding.Status != "active" || binding.DeviceID != "d1" {
		t.Fatalf("re-claim binding = %+v", binding)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM trial_entitlements"); got != 1 {
		t.Fatalf("trial rows after re-claim = %d, want 1", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM device_credentials WHERE device_id = 'd1' AND revoked_at IS NULL"); got != 1 {
		t.Fatalf("active credentials after re-claim = %d, want exactly 1", got)
	}
	var activeDigest []byte
	if err := db.QueryRowContext(t.Context(), "SELECT credential_digest FROM device_credentials WHERE device_id = 'd1' AND revoked_at IS NULL").Scan(&activeDigest); err != nil {
		t.Fatalf("read active credential: %v", err)
	}
	if want := digest("c2"); !bytes.Equal(activeDigest, want[:]) {
		t.Fatal("re-claim did not activate the fresh credential digest")
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM audit_logs WHERE action = 'device.reclaim'"); got != 1 {
		t.Fatalf("device.reclaim audit rows = %d, want 1", got)
	}
}

func TestReclaimRejectsForeignDeviceAndRevokedDevice(t *testing.T) {
	clock := fixedClock{trialBase()}
	svc, _, db := newStack(t, clock)
	if _, _, err := svc.BindFirstDevice(t.Context(), firstBinding("uA", "ia", "601", "dA", digest("ca"))); err != nil {
		t.Fatalf("first bind uA: %v", err)
	}
	if _, _, err := svc.BindFirstDevice(t.Context(), firstBinding("uB", "ib", "602", "dB", digest("cb"))); err != nil {
		t.Fatalf("first bind uB: %v", err)
	}
	// uA tries to re-claim uB's device: refused, and uB's credential state
	// must be untouched.
	if _, _, err := svc.BindFirstDevice(t.Context(), firstBinding("uA", "ia", "601", "dB", digest("steal"))); !errors.Is(err, entitlement.ErrDeviceOwnedByOther) {
		t.Fatalf("foreign re-claim error = %v, want ErrDeviceOwnedByOther", err)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM device_credentials WHERE device_id = 'dB' AND revoked_at IS NULL"); got != 1 {
		t.Fatalf("foreign re-claim altered credentials: %d active, want 1", got)
	}
	// A revoked device is never silently reactivated by a re-claim.
	store := storeOver(db, fixedClock{trialBase()})
	if err := store.RevokeDevice(t.Context(), "dA", trialBase().Add(time.Hour)); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	if _, _, err := entitlement.NewService(store, fixedClock{trialBase().Add(2 * time.Hour)}).BindFirstDevice(t.Context(), firstBinding("uA", "ia", "601", "dA", digest("again"))); err == nil {
		t.Fatal("revoked re-claim unexpectedly succeeded")
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM device_credentials WHERE device_id = 'dA' AND revoked_at IS NULL"); got != 1 {
		t.Fatalf("revoked re-claim minted credential: %d active, want 1 (pre-revoke)", got)
	}
}

func TestReclaimAfterLostTokenResponseSucceeds(t *testing.T) {
	clock := fixedClock{trialBase()}
	svc, _, db := newStack(t, clock)
	if _, _, err := svc.BindFirstDevice(t.Context(), firstBinding("u1", "i1", "610", "d1", digest("lost"))); err != nil {
		t.Fatalf("first bind: %v", err)
	}
	// Simulate the bridge retrying enrollment with the same device id after
	// the first token response never arrived.
	rebound, _, err := entitlement.NewService(storeOver(db, fixedClock{trialBase()}), fixedClock{trialBase().Add(time.Minute)}).BindFirstDevice(t.Context(), firstBinding("u1", "i1", "610", "d1", digest("retry")))
	if err != nil {
		t.Fatalf("retry bind: %v", err)
	}
	if rebound.ID == "" {
		t.Fatal("retry returned empty entitlement id")
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM trial_entitlements WHERE user_id = 'u1'"); got != 1 {
		t.Fatalf("trial rows after retry = %d, want 1", got)
	}
	if got := countRows(t, db, "SELECT COUNT(*) FROM device_credentials WHERE device_id = 'd1' AND revoked_at IS NULL"); got != 1 {
		t.Fatalf("active credentials after retry = %d, want 1", got)
	}
}

// storeOver rebinds a new store+audit stack onto an existing test database,
// mirroring how a later process would reopen the same persistence.
func storeOver(db *sql.DB, clock entitlement.Clock) *mysqlstore.EntitlementStore {
	return mysqlstore.NewEntitlementStore(db, clock, audit.NewService(mysqlstore.NewAuditStore(db)))
}

// entitlementTestDatabase provisions a fresh, migrated schema per test.
func entitlementTestDatabase(t *testing.T) *sql.DB {
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
	t.Cleanup(func() { _ = admin.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("ping MYSQL_TEST_DSN: %v", err)
	}
	dbName := fmt.Sprintf("robloxkit_entitlement_test_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+dbName+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		t.Fatalf("create temporary database: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+dbName+"`")
	})
	target := *base
	target.DBName = dbName
	target.ParseTime = true
	target.Loc = time.UTC
	db, err := sql.Open("mysql", target.FormatDSN())
	if err != nil {
		t.Fatalf("open temporary database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := mysqlstore.Migrate(ctx, db, "up"); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return db
}
