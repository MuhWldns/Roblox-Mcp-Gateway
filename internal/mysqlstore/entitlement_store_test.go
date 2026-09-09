package mysqlstore

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"robloxkit/internal/audit"
	"robloxkit/internal/entitlement"
)

// trialClock pins store operations to a deterministic instant.
type trialClock struct{ t time.Time }

func (c trialClock) Now() time.Time { return c.t }

func trialTestBase() time.Time { return time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC) }

func testDigest(plain string) [32]byte { return sha256.Sum256([]byte(plain)) }

func newEntitlementTestStack(t *testing.T) (*EntitlementStore, *sql.DB) {
	t.Helper()
	db := identityTestDatabase(t)
	store := NewEntitlementStore(db, trialClock{trialTestBase()}, audit.NewService(NewAuditStore(db)))
	return store, db
}

func rowCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func TestConcurrentLastSlotActivationHasExactlyOneWinner(t *testing.T) {
	store, _ := newEntitlementTestStack(t)
	lic, err := store.CreateLicense(t.Context(), "user-1", "", 1)
	if err != nil {
		t.Fatalf("CreateLicense: %v", err)
	}
	const workers = 50
	for i := 0; i < workers; i++ {
		deviceID := fmt.Sprintf("device-%02d", i)
		if err := store.CreateDevice(t.Context(), "user-1", deviceID, deviceID); err != nil {
			t.Fatalf("CreateDevice(%s): %v", deviceID, err)
		}
	}
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		successes int
		noSlot    int
		other     []error
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.BindDeviceSlot(t.Context(), lic.ID, fmt.Sprintf("device-%02d", i))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, entitlement.ErrNoSlot):
				noSlot++
			default:
				other = append(other, err)
			}
		}(i)
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("concurrent activations succeeded %d times, want exactly 1", successes)
	}
	if noSlot != workers-1 {
		t.Fatalf("ErrNoSlot count = %d, want %d", noSlot, workers-1)
	}
	if len(other) != 0 {
		t.Fatalf("unexpected binding errors: %v", other)
	}
}

func TestRevokedDeviceRetainsItsSlot(t *testing.T) {
	store, db := newEntitlementTestStack(t)
	lic, err := store.CreateLicense(t.Context(), "user-1", "", 1)
	if err != nil {
		t.Fatalf("CreateLicense: %v", err)
	}
	for _, deviceID := range []string{"d1", "d2"} {
		if err := store.CreateDevice(t.Context(), "user-1", deviceID, deviceID); err != nil {
			t.Fatalf("CreateDevice(%s): %v", deviceID, err)
		}
	}
	bound, err := store.BindDeviceSlot(t.Context(), lic.ID, "d1")
	if err != nil {
		t.Fatalf("BindDeviceSlot(d1): %v", err)
	}
	if err := store.RevokeDevice(t.Context(), "d1", trialTestBase().Add(time.Hour)); err != nil {
		t.Fatalf("RevokeDevice(d1): %v", err)
	}
	_, err = store.BindDeviceSlot(t.Context(), lic.ID, "d2")
	if !errors.Is(err, entitlement.ErrNoSlot) {
		t.Fatalf("second bind error = %v, want ErrNoSlot", err)
	}
	var status string
	var ordinal int
	if err := db.QueryRowContext(t.Context(),
		`SELECT slot_ordinal, status FROM license_device_bindings WHERE id = ?`, bound.ID,
	).Scan(&ordinal, &status); err != nil {
		t.Fatalf("read revoked binding: %v", err)
	}
	if ordinal != bound.SlotOrdinal {
		t.Fatalf("revoked binding ordinal = %d, want retained %d", ordinal, bound.SlotOrdinal)
	}
	if status != "revoked" {
		t.Fatalf("revoked binding status = %q, want revoked", status)
	}
}

func TestTransferDeviceMovesBindingAndRecordsRequest(t *testing.T) {
	store, db := newEntitlementTestStack(t)
	lic, err := store.CreateLicense(t.Context(), "user-1", "", 1)
	if err != nil {
		t.Fatalf("CreateLicense: %v", err)
	}
	for _, deviceID := range []string{"d1", "d2"} {
		if err := store.CreateDevice(t.Context(), "user-1", deviceID, deviceID); err != nil {
			t.Fatalf("CreateDevice(%s): %v", deviceID, err)
		}
	}
	bound, err := store.BindDeviceSlot(t.Context(), lic.ID, "d1")
	if err != nil {
		t.Fatalf("BindDeviceSlot(d1): %v", err)
	}
	if err := store.TransferDevice(t.Context(), trialTestBase().Add(2*time.Hour), entitlement.AdminActor{UserID: "admin-1"}, lic.ID, "d1", "d2", "hardware swap"); err != nil {
		t.Fatalf("TransferDevice: %v", err)
	}
	var (
		deviceID    string
		slotOrdinal int
	)
	if err := db.QueryRowContext(t.Context(),
		`SELECT device_id, slot_ordinal FROM license_device_bindings WHERE id = ?`, bound.ID,
	).Scan(&deviceID, &slotOrdinal); err != nil {
		t.Fatalf("read moved binding: %v", err)
	}
	if deviceID != "d2" {
		t.Fatalf("binding device_id = %q, want d2", deviceID)
	}
	if slotOrdinal != bound.SlotOrdinal {
		t.Fatalf("binding ordinal = %d, want unchanged %d", slotOrdinal, bound.SlotOrdinal)
	}
	if got := rowCount(t, db, `SELECT COUNT(*) FROM license_device_bindings WHERE license_id = ? AND device_id = 'd1'`, lic.ID); got != 0 {
		t.Fatalf("old device binding rows = %d, want 0", got)
	}
	var status string
	if err := db.QueryRowContext(t.Context(),
		`SELECT status FROM license_transfer_requests WHERE license_id = ? AND old_device_id = 'd1' AND new_device_id = 'd2'`, lic.ID,
	).Scan(&status); err != nil {
		t.Fatalf("read transfer request: %v", err)
	}
	if status != "completed" {
		t.Fatalf("transfer request status = %q, want completed", status)
	}
	if got := rowCount(t, db, `SELECT COUNT(*) FROM admin_actions WHERE action = 'license.transfer_device' AND target_id = ?`, lic.ID); got != 1 {
		t.Fatalf("transfer admin audit rows = %d, want 1", got)
	}
}

func TestExtendTrialOnlyLengthensExpiryAndAudits(t *testing.T) {
	store, db := newEntitlementTestStack(t)
	base := trialTestBase()
	ent, _, err := store.BindFirstDevice(t.Context(), base, entitlement.FirstDeviceBinding{
		UserID:           "user-1",
		IdentityID:       "identity-1",
		Provider:         "roblox",
		ProviderSubject:  "1516563360",
		DeviceID:         "d1",
		FingerprintHash:  "1111111111111111111111111111111111111111111111111111111111111111",
		CredentialDigest: testDigest("credential"),
		AuditCorrelation: "corr-extend",
	})
	if err != nil {
		t.Fatalf("BindFirstDevice: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO users (id) VALUES ('admin-1')`); err != nil {
		t.Fatalf("insert admin user: %v", err)
	}
	later := base.Add(20 * 24 * time.Hour)
	if err := store.ExtendTrial(t.Context(), entitlement.AdminActor{UserID: "admin-1"}, ent.ID, later, "goodwill extension"); err != nil {
		t.Fatalf("ExtendTrial: %v", err)
	}
	var (
		ends       time.Time
		reason     sql.NullString
		extendedBy sql.NullString
	)
	if err := db.QueryRowContext(t.Context(),
		`SELECT ends_at, extension_reason, extended_by FROM trial_entitlements WHERE id = ?`, ent.ID,
	).Scan(&ends, &reason, &extendedBy); err != nil {
		t.Fatalf("read extended trial: %v", err)
	}
	if !ends.Equal(later) {
		t.Fatalf("ends_at = %v, want %v", ends, later)
	}
	if reason.String != "goodwill extension" || extendedBy.String != "admin-1" {
		t.Fatalf("extension reason = %q extended_by = %q", reason.String, extendedBy.String)
	}
	if got := rowCount(t, db, `SELECT COUNT(*) FROM admin_actions WHERE action = 'trial.extend' AND target_id = ?`, ent.ID); got != 1 {
		t.Fatalf("extension admin audit rows = %d, want 1", got)
	}
	for _, candidate := range []time.Time{later, later.Add(-24 * time.Hour)} {
		if err := store.ExtendTrial(t.Context(), entitlement.AdminActor{UserID: "admin-1"}, ent.ID, candidate, "repeat extension"); !errors.Is(err, entitlement.ErrInvalidExtension) {
			t.Fatalf("ExtendTrial(%s) error = %v, want ErrInvalidExtension", candidate.Format(time.RFC3339), err)
		}
	}
	var endsAfter time.Time
	if err := db.QueryRowContext(t.Context(),
		`SELECT ends_at FROM trial_entitlements WHERE id = ?`, ent.ID,
	).Scan(&endsAfter); err != nil {
		t.Fatalf("re-read trial: %v", err)
	}
	if !endsAfter.Equal(later) {
		t.Fatalf("failed extensions changed ends_at to %v, want %v", endsAfter, later)
	}
}

func TestRecoveryRevokesCredentialsAndSessions(t *testing.T) {
	store, db := newEntitlementTestStack(t)
	base := trialTestBase()
	ent, _, err := store.BindFirstDevice(t.Context(), base, entitlement.FirstDeviceBinding{
		UserID:           "user-1",
		IdentityID:       "identity-1",
		Provider:         "roblox",
		ProviderSubject:  "1516563360",
		DeviceID:         "d1",
		FingerprintHash:  "2222222222222222222222222222222222222222222222222222222222222222",
		CredentialDigest: testDigest("credential"),
	})
	if err != nil {
		t.Fatalf("BindFirstDevice: %v", err)
	}
	extra := testDigest("extra-credential")
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO device_credentials (id, user_id, device_id, credential_digest) VALUES ('cred-extra-1', 'user-1', 'd1', ?)`, extra[:],
	); err != nil {
		t.Fatalf("insert extra credential: %v", err)
	}
	token := testDigest("session-token")
	if _, err := db.ExecContext(t.Context(),
		`INSERT INTO web_sessions (id, user_id, token_digest, expires_at) VALUES ('sess-1', 'user-1', ?, ?)`, token[:], base.Add(24*time.Hour),
	); err != nil {
		t.Fatalf("insert web session: %v", err)
	}
	when := base.Add(time.Hour)
	if err := store.RecoverIdentity(t.Context(), when, entitlement.AdminActor{UserID: "admin-1"}, "user-1", "identity-2", "stolen account", "evidence-9"); err != nil {
		t.Fatalf("RecoverIdentity: %v", err)
	}
	if got := rowCount(t, db, `SELECT COUNT(*) FROM device_credentials WHERE user_id = 'user-1' AND revoked_at IS NULL`); got != 0 {
		t.Fatalf("unrevoked credentials = %d, want 0", got)
	}
	if got := rowCount(t, db, `SELECT COUNT(*) FROM web_sessions WHERE user_id = 'user-1' AND revoked_at IS NULL`); got != 0 {
		t.Fatalf("unrevoked sessions = %d, want 0", got)
	}
	var revokedAt time.Time
	if err := db.QueryRowContext(t.Context(), `SELECT revoked_at FROM web_sessions WHERE id = 'sess-1'`).Scan(&revokedAt); err != nil {
		t.Fatalf("read revoked session: %v", err)
	}
	if !revokedAt.Equal(when) {
		t.Fatalf("session revoked_at = %v, want %v", revokedAt, when)
	}
	var caseStatus string
	if err := db.QueryRowContext(t.Context(),
		`SELECT status FROM account_recovery_cases WHERE user_id = 'user-1'`,
	).Scan(&caseStatus); err != nil {
		t.Fatalf("read recovery case: %v", err)
	}
	if caseStatus != "completed" {
		t.Fatalf("recovery case status = %q, want completed", caseStatus)
	}
	var started, ends time.Time
	if err := db.QueryRowContext(t.Context(),
		`SELECT started_at, ends_at FROM trial_entitlements WHERE user_id = 'user-1'`,
	).Scan(&started, &ends); err != nil {
		t.Fatalf("read trial after recovery: %v", err)
	}
	if !started.Equal(ent.StartedAt) || !ends.Equal(ent.EndsAt) {
		t.Fatalf("recovery reset trial: started %v ends %v", started, ends)
	}
}

func TestAuditRowsNeverContainCredentialDigest(t *testing.T) {
	store, db := newEntitlementTestStack(t)
	cred := testDigest("audit-secret-credential")
	if _, _, err := store.BindFirstDevice(t.Context(), trialTestBase(), entitlement.FirstDeviceBinding{
		UserID:           "user-1",
		IdentityID:       "identity-1",
		Provider:         "roblox",
		ProviderSubject:  "1516563360",
		DeviceID:         "d1",
		FingerprintHash:  "3333333333333333333333333333333333333333333333333333333333333333",
		CredentialDigest: cred,
		AuditCorrelation: "corr-audit",
	}); err != nil {
		t.Fatalf("BindFirstDevice: %v", err)
	}
	hexDigest := hex.EncodeToString(cred[:])
	if got := rowCount(t, db, `SELECT COUNT(*) FROM audit_logs`); got == 0 {
		t.Fatal("BindFirstDevice wrote no audit rows")
	}
	var blob strings.Builder
	rows, err := db.QueryContext(t.Context(),
		`SELECT CONCAT_WS('|', COALESCE(id,''), COALESCE(user_id,''), COALESCE(actor_user_id,''), action, correlation_id, COALESCE(reason,''), COALESCE(target_type,''), COALESCE(target_id,''), COALESCE(CAST(metadata AS CHAR),'')) FROM audit_logs`,
	)
	if err != nil {
		t.Fatalf("query audit logs: %v", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan audit log row: %v", err)
		}
		blob.WriteString(line)
		blob.WriteByte('|')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate audit logs: %v", err)
	}
	rows.Close()
	rows, err = db.QueryContext(t.Context(),
		`SELECT CONCAT_WS('|', COALESCE(id,''), COALESCE(actor_user_id,''), action, correlation_id, COALESCE(reason,''), COALESCE(target_type,''), COALESCE(target_id,''), COALESCE(CAST(before_state AS CHAR),''), COALESCE(CAST(after_state AS CHAR),'')) FROM admin_actions`,
	)
	if err != nil {
		t.Fatalf("query admin actions: %v", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan admin action row: %v", err)
		}
		blob.WriteString(line)
		blob.WriteByte('|')
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate admin actions: %v", err)
	}
	rows.Close()
	if strings.Contains(blob.String(), hexDigest) {
		t.Fatalf("audit rows leaked credential digest %q", hexDigest)
	}
	var stored []byte
	if err := db.QueryRowContext(t.Context(),
		`SELECT credential_digest FROM device_credentials WHERE user_id = 'user-1'`,
	).Scan(&stored); err != nil {
		t.Fatalf("read credential digest: %v", err)
	}
	if !bytes.Equal(stored, cred[:]) {
		t.Fatal("device_credentials does not store the credential digest")
	}
}

func TestBindFirstDeviceRejectsCrossAccountDeviceReuse(t *testing.T) {
	store, db := newEntitlementTestStack(t)
	fpHash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	// First user succeeds.
	cred1 := testDigest("cred-user-1")
	_, _, err := store.BindFirstDevice(t.Context(), trialTestBase(), entitlement.FirstDeviceBinding{
		UserID:           "user-1",
		IdentityID:       "identity-1",
		Provider:         "roblox",
		ProviderSubject:  "11111",
		DeviceID:         "dev-1",
		FingerprintHash:  fpHash,
		CredentialDigest: cred1,
		AuditCorrelation: "corr-1",
	})
	if err != nil {
		t.Fatalf("first user BindFirstDevice: %v", err)
	}

	// Second user with same device fingerprint is rejected with ErrDeviceAlreadyUsed.
	cred2 := testDigest("cred-user-2")
	_, _, err = store.BindFirstDevice(t.Context(), trialTestBase(), entitlement.FirstDeviceBinding{
		UserID:           "user-2",
		IdentityID:       "identity-2",
		Provider:         "roblox",
		ProviderSubject:  "22222",
		DeviceID:         "dev-2",
		FingerprintHash:  fpHash,
		CredentialDigest: cred2,
		AuditCorrelation: "corr-2",
	})
	if !errors.Is(err, entitlement.ErrDeviceAlreadyUsed) {
		t.Fatalf("second user error = %v, want ErrDeviceAlreadyUsed", err)
	}

	// Verify no rows created for user-2.
	if got := rowCount(t, db, "SELECT COUNT(*) FROM trial_entitlements WHERE user_id = 'user-2'"); got != 0 {
		t.Fatalf("trial_entitlements for user-2 count = %d, want 0", got)
	}
	if got := rowCount(t, db, "SELECT COUNT(*) FROM trial_entitlement_identities WHERE user_id = 'user-2'"); got != 0 {
		t.Fatalf("trial_entitlement_identities for user-2 count = %d, want 0", got)
	}
	if got := rowCount(t, db, "SELECT COUNT(*) FROM devices WHERE user_id = 'user-2'"); got != 0 {
		t.Fatalf("devices for user-2 count = %d, want 0", got)
	}
	if got := rowCount(t, db, "SELECT COUNT(*) FROM device_credentials WHERE user_id = 'user-2'"); got != 0 {
		t.Fatalf("device_credentials for user-2 count = %d, want 0", got)
	}
}

func TestBindFirstDevicePermitsSameAccountReclaimWithFingerprint(t *testing.T) {
	store, db := newEntitlementTestStack(t)
	fpHash := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	cred1 := testDigest("cred-reclaim-1")
	_, _, err := store.BindFirstDevice(t.Context(), trialTestBase(), entitlement.FirstDeviceBinding{
		UserID:           "user-reclaim",
		IdentityID:       "identity-reclaim",
		Provider:         "roblox",
		ProviderSubject:  "33333",
		DeviceID:         "dev-reclaim",
		FingerprintHash:  fpHash,
		CredentialDigest: cred1,
		AuditCorrelation: "corr-reclaim-1",
	})
	if err != nil {
		t.Fatalf("initial BindFirstDevice: %v", err)
	}

	cred2 := testDigest("cred-reclaim-2")
	_, _, err = store.BindFirstDevice(t.Context(), trialTestBase().Add(time.Hour), entitlement.FirstDeviceBinding{
		UserID:           "user-reclaim",
		IdentityID:       "identity-reclaim",
		Provider:         "roblox",
		ProviderSubject:  "33333",
		DeviceID:         "dev-reclaim",
		FingerprintHash:  fpHash,
		CredentialDigest: cred2,
		AuditCorrelation: "corr-reclaim-2",
	})
	if err != nil {
		t.Fatalf("reclaim BindFirstDevice: %v", err)
	}

	if got := rowCount(t, db, "SELECT COUNT(*) FROM devices WHERE user_id = 'user-reclaim'"); got != 1 {
		t.Fatalf("device count = %d, want 1", got)
	}
	if got := rowCount(t, db, "SELECT COUNT(*) FROM device_credentials WHERE user_id = 'user-reclaim' AND revoked_at IS NULL"); got != 1 {
		t.Fatalf("active credential count = %d, want 1", got)
	}
}

func TestBindFirstDeviceLegacyDeviceReclaimBackfillsFingerprintWithoutRestartingTrial(t *testing.T) {
	store, db := newEntitlementTestStack(t)
	base := trialTestBase()

	// Seed initial legacy device row with NULL fingerprint_hash and an initial trial
	userID := "user-legacy"
	identityID := "identity-legacy"
	deviceID := "dev-legacy"
	robloxSubject := "44444"

	cred1 := testDigest("cred-legacy-1")
	initialEnt, _, err := store.BindFirstDevice(t.Context(), base, entitlement.FirstDeviceBinding{
		UserID:           userID,
		IdentityID:       identityID,
		Provider:         "roblox",
		ProviderSubject:  robloxSubject,
		DeviceID:         deviceID,
		FingerprintHash:  "", // legacy enrollment had no fingerprint
		CredentialDigest: cred1,
		AuditCorrelation: "corr-legacy-1",
	})
	if err != nil {
		t.Fatalf("initial legacy BindFirstDevice: %v", err)
	}

	// Verify device row initially has NULL fingerprint_hash
	var initialFp sql.NullString
	if err := db.QueryRowContext(t.Context(), "SELECT fingerprint_hash FROM devices WHERE id = ?", deviceID).Scan(&initialFp); err != nil {
		t.Fatalf("read legacy device fingerprint: %v", err)
	}
	if initialFp.Valid {
		t.Fatalf("initial legacy device fingerprint = %q, want NULL", initialFp.String)
	}

	// Now same owner reclaims the device using a new Bridge release that supplies a fingerprint.
	fpHash := "4444444444444444444444444444444444444444444444444444444444444444"
	reclaimTime := base.Add(2 * time.Hour)
	cred2 := testDigest("cred-legacy-2")
	reclaimedEnt, binding, err := store.BindFirstDevice(t.Context(), reclaimTime, entitlement.FirstDeviceBinding{
		UserID:           userID,
		IdentityID:       identityID,
		Provider:         "roblox",
		ProviderSubject:  robloxSubject,
		DeviceID:         deviceID,
		FingerprintHash:  fpHash,
		CredentialDigest: cred2,
		AuditCorrelation: "corr-legacy-2",
	})
	if err != nil {
		t.Fatalf("reclaim legacy device with fingerprint: %v", err)
	}

	// The trial window MUST remain unchanged (started_at and ends_at match initialEnt).
	if !reclaimedEnt.StartedAt.Equal(initialEnt.StartedAt) || !reclaimedEnt.EndsAt.Equal(initialEnt.EndsAt) {
		t.Fatalf("trial window modified on reclaim: before (%v - %v), after (%v - %v)",
			initialEnt.StartedAt, initialEnt.EndsAt, reclaimedEnt.StartedAt, reclaimedEnt.EndsAt)
	}
	if binding.Status != "active" {
		t.Fatalf("binding status = %q, want 'active'", binding.Status)
	}

	// Verify device row now has the backfilled fingerprint_hash
	var storedFp []byte
	if err := db.QueryRowContext(t.Context(), "SELECT fingerprint_hash FROM devices WHERE id = ?", deviceID).Scan(&storedFp); err != nil {
		t.Fatalf("read backfilled fingerprint: %v", err)
	}
	if hex.EncodeToString(storedFp) != fpHash {
		t.Fatalf("stored fingerprint = %x, want %s", storedFp, fpHash)
	}

	// Verify exactly 1 trial row, 1 device row, and old credential revoked
	if got := rowCount(t, db, "SELECT COUNT(*) FROM trial_entitlements WHERE user_id = ?", userID); got != 1 {
		t.Fatalf("trial_entitlements count = %d, want 1", got)
	}
	if got := rowCount(t, db, "SELECT COUNT(*) FROM devices WHERE user_id = ?", userID); got != 1 {
		t.Fatalf("devices count = %d, want 1", got)
	}
	if got := rowCount(t, db, "SELECT COUNT(*) FROM device_credentials WHERE user_id = ? AND revoked_at IS NULL", userID); got != 1 {
		t.Fatalf("active credentials count = %d, want 1", got)
	}
	if got := rowCount(t, db, "SELECT COUNT(*) FROM device_credentials WHERE user_id = ? AND revoked_at IS NOT NULL", userID); got != 1 {
		t.Fatalf("revoked credentials count = %d, want 1", got)
	}

	// A second account attempting to use this newly backfilled fingerprint must be rejected with ErrDeviceAlreadyUsed.
	cred3 := testDigest("cred-attacker")
	_, _, err = store.BindFirstDevice(t.Context(), reclaimTime.Add(time.Hour), entitlement.FirstDeviceBinding{
		UserID:           "user-other",
		IdentityID:       "identity-other",
		Provider:         "roblox",
		ProviderSubject:  "55555",
		DeviceID:         "dev-other",
		FingerprintHash:  fpHash,
		CredentialDigest: cred3,
		AuditCorrelation: "corr-other",
	})
	if !errors.Is(err, entitlement.ErrDeviceAlreadyUsed) {
		t.Fatalf("cross-account attempt using backfilled fingerprint error = %v, want ErrDeviceAlreadyUsed", err)
	}
}
