package mysqlstore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"robloxkit/internal/credential"
	"robloxkit/internal/mcpoauth"
)

const (
	oauthTestRedirect  = "https://chatgpt.com/aip/oauth/callback"
	oauthTestResource  = "https://gateway.example.com/mcp"
	oauthTestChallenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
)

var oauthTestPepper = []byte("oauth-store-test-pepper")

func oauthTestNow() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

func oauthUUID(t *testing.T) string {
	t.Helper()
	id, err := identityUUID()
	if err != nil {
		t.Fatalf("generate id: %v", err)
	}
	return id
}

func oauthTestCredential(t *testing.T) (string, [32]byte) {
	t.Helper()
	plain, digest, err := credential.Generate("rk13_", 32, oauthTestPepper)
	if err != nil {
		t.Fatalf("generate credential: %v", err)
	}
	return plain, digest
}

func oauthEnsureUser(t *testing.T, db *sql.DB) string {
	t.Helper()
	id := oauthUUID(t)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO users (id) VALUES (?)`, id); err != nil {
		t.Fatalf("insert test user: %v", err)
	}
	return id
}

func oauthEnsureDevice(t *testing.T, db *sql.DB, userID string) string {
	t.Helper()
	id := oauthUUID(t)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO devices (id, user_id, name) VALUES (?, ?, ?)`, id, userID, "Task 13 Test Device"); err != nil {
		t.Fatalf("insert test device: %v", err)
	}
	return id
}

func oauthEnsureStudioSession(t *testing.T, db *sql.DB, userID, deviceID string) string {
	t.Helper()
	id := oauthUUID(t)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO studio_sessions (id, user_id, device_id, studio_id, status, started_at) VALUES (?, ?, ?, ?, ?, ?)`, id, userID, deviceID, "studio-1", "active", oauthTestNow()); err != nil {
		t.Fatalf("insert test studio session: %v", err)
	}
	return id
}

type oauthFixture struct {
	db            *sql.DB
	store         *OAuthStore
	client        mcpoauth.Client
	userID        string
	deviceID      string
	otherDeviceID string
	studioID      string
	now           time.Time
}

func oauthTestFixture(t *testing.T) oauthFixture {
	t.Helper()
	db := identityTestDatabase(t)
	store := NewOAuthStore(db)
	client, err := store.RegisterClient(t.Context(), mcpoauth.Client{
		ClientID:     "https://chatgpt.com/aip/connector",
		ClientName:   "ChatGPT",
		RedirectURIs: []string{oauthTestRedirect},
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	userID := oauthEnsureUser(t, db)
	deviceID := oauthEnsureDevice(t, db, userID)
	otherDeviceID := oauthEnsureDevice(t, db, userID)
	studioID := oauthEnsureStudioSession(t, db, userID, deviceID)
	return oauthFixture{
		db:            db,
		store:         store,
		client:        client,
		userID:        userID,
		deviceID:      deviceID,
		otherDeviceID: otherDeviceID,
		studioID:      studioID,
		now:           oauthTestNow(),
	}
}

func (f oauthFixture) binding() mcpoauth.CodeBinding {
	return mcpoauth.CodeBinding{ClientID: f.client.ID, RedirectURI: oauthTestRedirect, Resource: oauthTestResource}
}

func (f oauthFixture) code(t *testing.T) (mcpoauth.AuthorizationCode, [32]byte) {
	t.Helper()
	_, digest := oauthTestCredential(t)
	code := mcpoauth.AuthorizationCode{
		ID:              oauthUUID(t),
		UserID:          f.userID,
		ClientID:        f.client.ID,
		RedirectURI:     oauthTestRedirect,
		CodeChallenge:   oauthTestChallenge,
		Scopes:          []string{mcpoauth.ScopeConnect, mcpoauth.ScopeStudioRead},
		DeviceID:        f.deviceID,
		StudioSessionID: f.studioID,
		Resource:        oauthTestResource,
		ExpiresAt:       f.now.Add(10 * time.Minute),
		CreatedAt:       f.now,
	}
	return code, digest
}

func (f oauthFixture) grant(t *testing.T, deviceID string) mcpoauth.Grant {
	t.Helper()
	return mcpoauth.Grant{
		ID:        oauthUUID(t),
		UserID:    f.userID,
		ClientID:  f.client.ID,
		DeviceID:  deviceID,
		Scopes:    []string{mcpoauth.ScopeConnect, mcpoauth.ScopeStudioRead},
		Resource:  oauthTestResource,
		CreatedAt: f.now,
	}
}

func oauthNewTokenPair(t *testing.T, grant mcpoauth.Grant, now time.Time) (mcpoauth.AccessToken, [32]byte, mcpoauth.RefreshToken, [32]byte) {
	t.Helper()
	_, accessDigest := oauthTestCredential(t)
	access := mcpoauth.AccessToken{
		ID:        oauthUUID(t),
		UserID:    grant.UserID,
		GrantID:   grant.ID,
		ExpiresAt: now.Add(time.Hour),
		CreatedAt: now,
	}
	_, refreshDigest := oauthTestCredential(t)
	refresh := mcpoauth.RefreshToken{
		ID:        oauthUUID(t),
		UserID:    grant.UserID,
		GrantID:   grant.ID,
		ExpiresAt: now.Add(30 * 24 * time.Hour),
		CreatedAt: now,
	}
	return access, accessDigest, refresh, refreshDigest
}

func TestOAuthStoreClientRegistrationRoundTrip(t *testing.T) {
	f := oauthTestFixture(t)
	ctx := t.Context()

	// re-registration with the same public client id updates, never duplicates
	updated, err := f.store.RegisterClient(ctx, mcpoauth.Client{
		ClientID:     f.client.ClientID,
		ClientName:   "ChatGPT (dev)",
		RedirectURIs: []string{oauthTestRedirect, "https://chatgpt.com/aip/oauth/callback-alt"},
	})
	if err != nil {
		t.Fatalf("re-register client: %v", err)
	}
	if updated.ID != f.client.ID {
		t.Fatalf("re-registration created a second client row: %q vs %q", updated.ID, f.client.ID)
	}
	if updated.ClientName != "ChatGPT (dev)" || len(updated.RedirectURIs) != 2 {
		t.Fatalf("re-registration did not update fields: %#v", updated)
	}
	if updated.CreatedAt.IsZero() {
		t.Fatal("client created_at missing")
	}

	var count int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_clients WHERE client_id = ?`, f.client.ClientID).Scan(&count); err != nil {
		t.Fatalf("count clients: %v", err)
	}
	if count != 1 {
		t.Fatalf("stored %d client rows, want 1", count)
	}

	got, err := f.store.ClientByPublicID(ctx, f.client.ClientID)
	if err != nil {
		t.Fatalf("lookup client: %v", err)
	}
	if got.ID != f.client.ID || !equalStrings(got.RedirectURIs, updated.RedirectURIs) {
		t.Fatalf("lookup mismatch: %#v", got)
	}

	if _, err := f.store.ClientByPublicID(ctx, "https://claude.ai/api/mcp/client"); !errors.Is(err, mcpoauth.ErrClientNotFound) {
		t.Fatalf("unknown client: want ErrClientNotFound, got %v", err)
	}

	// invalid registrations are rejected before touching the database
	if _, err := f.store.RegisterClient(ctx, mcpoauth.Client{ClientID: "https://chatgpt.com/aip/connector-no-redirects"}); !errors.Is(err, mcpoauth.ErrInvalidClient) {
		t.Fatalf("missing redirect uris: want ErrInvalidClient, got %v", err)
	}
	if _, err := f.store.RegisterClient(ctx, mcpoauth.Client{
		ClientID:     "https://chatgpt.com/aip/connector-insecure",
		RedirectURIs: []string{"http://chatgpt.com/cb"},
	}); !errors.Is(err, mcpoauth.ErrInvalidClient) {
		t.Fatalf("insecure redirect uri: want ErrInvalidClient, got %v", err)
	}
}

func TestOAuthStoreAuthorizationCodeSingleUse(t *testing.T) {
	f := oauthTestFixture(t)
	ctx := t.Context()

	code, digest := f.code(t)
	if err := f.store.SaveAuthorizationCode(ctx, code, digest); err != nil {
		t.Fatalf("save code: %v", err)
	}

	consumed, err := f.store.ConsumeAuthorizationCode(ctx, digest, f.binding(), f.now)
	if err != nil {
		t.Fatalf("consume code: %v", err)
	}
	if consumed.ID != code.ID || consumed.UserID != f.userID || consumed.ClientID != f.client.ID ||
		consumed.RedirectURI != oauthTestRedirect || consumed.CodeChallenge != oauthTestChallenge ||
		consumed.DeviceID != f.deviceID || consumed.StudioSessionID != f.studioID || consumed.Resource != oauthTestResource {
		t.Fatalf("consumed code mismatch: %#v", consumed)
	}
	if !equalStrings(consumed.Scopes, code.Scopes) {
		t.Fatalf("consumed scopes = %#v, want %#v", consumed.Scopes, code.Scopes)
	}
	if consumed.ConsumedAt == nil || consumed.ConsumedAt.IsZero() {
		t.Fatal("consumed code was not marked with a consumption timestamp")
	}

	// replay of a consumed code fails
	if _, err := f.store.ConsumeAuthorizationCode(ctx, digest, f.binding(), f.now); !errors.Is(err, mcpoauth.ErrCodeUsed) {
		t.Fatalf("code replay: want ErrCodeUsed, got %v", err)
	}

	_, unknownDigest := oauthTestCredential(t)
	if _, err := f.store.ConsumeAuthorizationCode(ctx, unknownDigest, f.binding(), f.now); !errors.Is(err, mcpoauth.ErrCodeNotFound) {
		t.Fatalf("unknown code: want ErrCodeNotFound, got %v", err)
	}

	// expired codes are rejected and never consumed
	_, expiredDigest := oauthTestCredential(t)
	expired := code
	expired.ID = oauthUUID(t)
	expired.ExpiresAt = f.now.Add(-time.Minute)
	if err := f.store.SaveAuthorizationCode(ctx, expired, expiredDigest); err != nil {
		t.Fatalf("save expired code: %v", err)
	}
	if _, err := f.store.ConsumeAuthorizationCode(ctx, expiredDigest, f.binding(), f.now); !errors.Is(err, mcpoauth.ErrCodeExpired) {
		t.Fatalf("expired code: want ErrCodeExpired, got %v", err)
	}
	if _, err := f.store.ConsumeAuthorizationCode(ctx, expiredDigest, f.binding(), f.now); !errors.Is(err, mcpoauth.ErrCodeExpired) {
		t.Fatalf("expired code was consumed on the first attempt: %v", err)
	}
}

func TestOAuthStoreAuthorizationCodeConsumptionIsAtomic(t *testing.T) {
	f := oauthTestFixture(t)
	binding := f.binding()
	code, digest := f.code(t)
	if err := f.store.SaveAuthorizationCode(t.Context(), code, digest); err != nil {
		t.Fatalf("save code: %v", err)
	}

	const workers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes, replays := 0, 0
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.store.ConsumeAuthorizationCode(context.Background(), digest, binding, f.now)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, mcpoauth.ErrCodeUsed):
				replays++
			default:
				t.Errorf("concurrent consume: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if successes != 1 || replays != workers-1 {
		t.Fatalf("concurrent consumption: successes=%d replays=%d, want 1 success and %d replays", successes, replays, workers-1)
	}
	var consumed int
	if err := f.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM oauth_authorization_codes WHERE consumed_at IS NOT NULL`).Scan(&consumed); err != nil {
		t.Fatalf("count consumed codes: %v", err)
	}
	if consumed != 1 {
		t.Fatalf("%d codes marked consumed, want exactly 1", consumed)
	}
}

func TestOAuthStoreAuthorizationCodeBinding(t *testing.T) {
	f := oauthTestFixture(t)
	ctx := t.Context()
	code, digest := f.code(t)
	if err := f.store.SaveAuthorizationCode(ctx, code, digest); err != nil {
		t.Fatalf("save code: %v", err)
	}

	wrongClient := f.binding()
	wrongClient.ClientID = oauthUUID(t)
	if _, err := f.store.ConsumeAuthorizationCode(ctx, digest, wrongClient, f.now); !errors.Is(err, mcpoauth.ErrCodeBinding) {
		t.Fatalf("wrong client: want ErrCodeBinding, got %v", err)
	}
	wrongRedirect := f.binding()
	wrongRedirect.RedirectURI = oauthTestRedirect + "?different=1"
	if _, err := f.store.ConsumeAuthorizationCode(ctx, digest, wrongRedirect, f.now); !errors.Is(err, mcpoauth.ErrCodeBinding) {
		t.Fatalf("wrong redirect: want ErrCodeBinding, got %v", err)
	}
	wrongResource := f.binding()
	wrongResource.Resource = "https://other.example.com/mcp"
	if _, err := f.store.ConsumeAuthorizationCode(ctx, digest, wrongResource, f.now); !errors.Is(err, mcpoauth.ErrCodeBinding) {
		t.Fatalf("wrong resource: want ErrCodeBinding, got %v", err)
	}

	// the failed binding attempts must not consume the code
	if _, err := f.store.ConsumeAuthorizationCode(ctx, digest, f.binding(), f.now); err != nil {
		t.Fatalf("consume after binding mismatches: %v", err)
	}
}

func TestOAuthStorePersistsDigestsOnly(t *testing.T) {
	f := oauthTestFixture(t)
	ctx := t.Context()

	plainCode, codeDigest := oauthTestCredential(t)
	code, _ := f.code(t)
	if err := f.store.SaveAuthorizationCode(ctx, code, codeDigest); err != nil {
		t.Fatalf("save code: %v", err)
	}
	grant, err := f.store.SaveGrant(ctx, f.grant(t, f.deviceID))
	if err != nil {
		t.Fatalf("save grant: %v", err)
	}
	plainAccess, accessDigest := oauthTestCredential(t)
	access := mcpoauth.AccessToken{
		ID:        oauthUUID(t),
		UserID:    grant.UserID,
		GrantID:   grant.ID,
		ExpiresAt: f.now.Add(time.Hour),
		CreatedAt: f.now,
	}
	plainRefresh, refreshDigest := oauthTestCredential(t)
	refresh := mcpoauth.RefreshToken{
		ID:        oauthUUID(t),
		UserID:    grant.UserID,
		GrantID:   grant.ID,
		ExpiresAt: f.now.Add(30 * 24 * time.Hour),
		CreatedAt: f.now,
	}
	if err := f.store.IssueTokens(ctx, access, accessDigest, refresh, refreshDigest); err != nil {
		t.Fatalf("issue tokens: %v", err)
	}

	// digest columns are BINARY(32) (reuses the committed migration assertion)
	assertBinaryDigest(t, f.db, "oauth_authorization_codes", "code_digest")
	assertBinaryDigest(t, f.db, "oauth_access_tokens", "token_digest")
	assertBinaryDigest(t, f.db, "oauth_refresh_tokens", "token_digest")

	stored := []struct {
		table  string
		column string
		id     string
		plain  string
		digest [32]byte
	}{
		{"oauth_authorization_codes", "code_digest", code.ID, plainCode, codeDigest},
		{"oauth_access_tokens", "token_digest", access.ID, plainAccess, accessDigest},
		{"oauth_refresh_tokens", "token_digest", refresh.ID, plainRefresh, refreshDigest},
	}
	for _, row := range stored {
		var raw []byte
		if err := f.db.QueryRowContext(ctx, "SELECT "+row.column+" FROM "+row.table+" WHERE id = ?", row.id).Scan(&raw); err != nil {
			t.Fatalf("read %s.%s: %v", row.table, row.column, err)
		}
		if len(raw) != 32 {
			t.Fatalf("%s.%s stores %d bytes, want 32", row.table, row.column, len(raw))
		}
		if !bytes.Equal(raw, row.digest[:]) {
			t.Fatalf("%s.%s does not store the keyed digest", row.table, row.column)
		}
		if bytes.Equal(raw, []byte(row.plain)) {
			t.Fatalf("%s.%s stores the plaintext secret", row.table, row.column)
		}
	}
}

func TestOAuthStoreGrantUpsertIsIdempotentPerDevice(t *testing.T) {
	f := oauthTestFixture(t)
	ctx := t.Context()

	grant, err := f.store.SaveGrant(ctx, f.grant(t, f.deviceID))
	if err != nil {
		t.Fatalf("save grant: %v", err)
	}
	if grant.ID == "" || grant.CreatedAt.IsZero() {
		t.Fatalf("grant missing id/created_at: %#v", grant)
	}

	// re-consent for the same user+client+device reuses the row
	narrowed := f.grant(t, f.deviceID)
	narrowed.ID = oauthUUID(t) // a fresh candidate id must not duplicate the grant
	narrowed.Scopes = []string{mcpoauth.ScopeConnect}
	narrowed.StudioSessionID = f.studioID
	updated, err := f.store.SaveGrant(ctx, narrowed)
	if err != nil {
		t.Fatalf("re-save grant: %v", err)
	}
	if updated.ID != grant.ID {
		t.Fatalf("re-consent created duplicate grant rows %q vs %q", grant.ID, updated.ID)
	}
	if !equalStrings(updated.Scopes, narrowed.Scopes) {
		t.Fatalf("scopes not updated: %#v", updated.Scopes)
	}
	if updated.StudioSessionID != f.studioID {
		t.Fatalf("studio session not updated: %#v", updated)
	}

	// the same user+client on another device is a separate grant
	other, err := f.store.SaveGrant(ctx, f.grant(t, f.otherDeviceID))
	if err != nil {
		t.Fatalf("save other grant: %v", err)
	}
	if other.ID == grant.ID {
		t.Fatal("grant for another device collapsed into the first grant")
	}

	var count int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_grants WHERE user_id = ?`, f.userID).Scan(&count); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if count != 2 {
		t.Fatalf("stored %d grants, want 2", count)
	}

	// revoking an unknown grant fails loudly
	if err := f.store.RevokeGrant(ctx, oauthUUID(t), f.now); !errors.Is(err, mcpoauth.ErrGrantNotFound) {
		t.Fatalf("revoke unknown grant: want ErrGrantNotFound, got %v", err)
	}
}

func TestOAuthStoreIssueTokensRollsBackOnFailure(t *testing.T) {
	f := oauthTestFixture(t)
	ctx := t.Context()
	grant, err := f.store.SaveGrant(ctx, f.grant(t, f.deviceID))
	if err != nil {
		t.Fatalf("save grant: %v", err)
	}

	access, accessDigest, refresh, refreshDigest := oauthNewTokenPair(t, grant, f.now)
	if err := f.store.IssueTokens(ctx, access, accessDigest, refresh, refreshDigest); err != nil {
		t.Fatalf("issue tokens: %v", err)
	}

	// a refresh digest collision must roll back the access token insert
	_, collidingAccessDigest := oauthTestCredential(t)
	collidingAccess := access
	collidingAccess.ID = oauthUUID(t)
	collidingRefresh := refresh
	collidingRefresh.ID = oauthUUID(t)
	if err := f.store.IssueTokens(ctx, collidingAccess, collidingAccessDigest, collidingRefresh, refreshDigest); err == nil {
		t.Fatal("duplicate refresh digest was accepted")
	}

	var accessCount, refreshCount int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_access_tokens`).Scan(&accessCount); err != nil {
		t.Fatalf("count access tokens: %v", err)
	}
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_refresh_tokens`).Scan(&refreshCount); err != nil {
		t.Fatalf("count refresh tokens: %v", err)
	}
	if accessCount != 1 || refreshCount != 1 {
		t.Fatalf("failed issuance was not rolled back: access=%d refresh=%d, want 1 and 1", accessCount, refreshCount)
	}
}

func TestOAuthStoreRefreshRotationFamilyReuseRevokesFamily(t *testing.T) {
	f := oauthTestFixture(t)
	ctx := t.Context()
	grant, err := f.store.SaveGrant(ctx, f.grant(t, f.deviceID))
	if err != nil {
		t.Fatalf("save grant: %v", err)
	}

	access1, access1Digest, refresh1, refresh1Digest := oauthNewTokenPair(t, grant, f.now)
	if err := f.store.IssueTokens(ctx, access1, access1Digest, refresh1, refresh1Digest); err != nil {
		t.Fatalf("issue tokens: %v", err)
	}

	// the first refresh token is the family root
	var familyID string
	if err := f.db.QueryRowContext(ctx, `SELECT family_id FROM oauth_refresh_tokens WHERE id = ?`, refresh1.ID).Scan(&familyID); err != nil {
		t.Fatalf("read family root: %v", err)
	}
	if familyID != refresh1.ID {
		t.Fatalf("family root = %q, want %q", familyID, refresh1.ID)
	}

	rotate := func(t *testing.T, oldDigest [32]byte) (mcpoauth.AccessToken, [32]byte, mcpoauth.RefreshToken, [32]byte, error) {
		t.Helper()
		access, accessDigest, refresh, refreshDigest := oauthNewTokenPair(t, grant, f.now)
		rotatedAccess, rotatedRefresh, err := f.store.RotateRefreshToken(ctx, oldDigest, access, accessDigest, refresh, refreshDigest, f.now)
		return rotatedAccess, accessDigest, rotatedRefresh, refreshDigest, err
	}

	_, access2Digest, refresh2, refresh2Digest, err := rotate(t, refresh1Digest)
	if err != nil {
		t.Fatalf("rotate family root: %v", err)
	}
	if refresh2.FamilyID != refresh1.ID {
		t.Fatalf("child family = %q, want %q", refresh2.FamilyID, refresh1.ID)
	}
	if refresh2.ParentID != refresh1.ID {
		t.Fatalf("child parent = %q, want %q", refresh2.ParentID, refresh1.ID)
	}
	if refresh2.GrantID != grant.ID || refresh2.UserID != f.userID {
		t.Fatalf("child lineage wrong: %#v", refresh2)
	}

	_, access3Digest, refresh3, refresh3Digest, err := rotate(t, refresh2Digest)
	if err != nil {
		t.Fatalf("rotate child: %v", err)
	}
	if refresh3.FamilyID != refresh1.ID || refresh3.ParentID != refresh2.ID {
		t.Fatalf("grandchild lineage wrong: %#v", refresh3)
	}

	// reuse of the rotated parent revokes the entire family
	reuseAccess, reuseAccessDigest, reuseRefresh, reuseRefreshDigest := oauthNewTokenPair(t, grant, f.now)
	if _, _, err := f.store.RotateRefreshToken(ctx, refresh1Digest, reuseAccess, reuseAccessDigest, reuseRefresh, reuseRefreshDigest, f.now); !errors.Is(err, mcpoauth.ErrTokenReused) {
		t.Fatalf("reuse: want ErrTokenReused, got %v", err)
	}

	// every family member is revoked
	var unrevoked, familySize int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_refresh_tokens WHERE family_id = ? AND revoked_at IS NULL`, refresh1.ID).Scan(&unrevoked); err != nil {
		t.Fatalf("count unrevoked family: %v", err)
	}
	if unrevoked != 0 {
		t.Fatalf("%d family tokens remain unrevoked after reuse", unrevoked)
	}
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_refresh_tokens WHERE family_id = ?`, refresh1.ID).Scan(&familySize); err != nil {
		t.Fatalf("count family: %v", err)
	}
	if familySize != 3 {
		t.Fatalf("family size = %d, want 3 (root plus two rotations)", familySize)
	}

	// the grant and all access tokens issued under it are revoked as containment
	for _, digest := range [][32]byte{access1Digest, access2Digest, access3Digest} {
		if _, err := f.store.AccessTokenByDigest(ctx, digest, f.now); !errors.Is(err, mcpoauth.ErrTokenRevoked) {
			t.Fatalf("access token survived family revocation: %v", err)
		}
	}
	var grantRevokedAt sql.NullTime
	if err := f.db.QueryRowContext(ctx, `SELECT revoked_at FROM oauth_grants WHERE id = ?`, grant.ID).Scan(&grantRevokedAt); err != nil {
		t.Fatalf("read grant revocation: %v", err)
	}
	if !grantRevokedAt.Valid {
		t.Fatal("grant survived family revocation")
	}

	// rotating any family member now fails as revoked
	if _, _, _, _, err := rotate(t, refresh3Digest); !errors.Is(err, mcpoauth.ErrTokenRevoked) {
		t.Fatalf("rotating a revoked family member: want ErrTokenRevoked, got %v", err)
	}
}

func TestOAuthStoreRefreshRotationEdgeCases(t *testing.T) {
	f := oauthTestFixture(t)
	ctx := t.Context()
	grant, err := f.store.SaveGrant(ctx, f.grant(t, f.deviceID))
	if err != nil {
		t.Fatalf("save grant: %v", err)
	}

	// unknown refresh token
	_, unknownDigest := oauthTestCredential(t)
	access, accessDigest, refresh, refreshDigest := oauthNewTokenPair(t, grant, f.now)
	if _, _, err := f.store.RotateRefreshToken(ctx, unknownDigest, access, accessDigest, refresh, refreshDigest, f.now); !errors.Is(err, mcpoauth.ErrTokenNotFound) {
		t.Fatalf("unknown refresh token: want ErrTokenNotFound, got %v", err)
	}

	// expired refresh tokens are rejected without consuming or revoking them
	expiredAccess, expiredAccessDigest, expiredRefresh, expiredRefreshDigest := oauthNewTokenPair(t, grant, f.now)
	expiredRefresh.ExpiresAt = f.now.Add(-time.Minute)
	if err := f.store.IssueTokens(ctx, expiredAccess, expiredAccessDigest, expiredRefresh, expiredRefreshDigest); err != nil {
		t.Fatalf("issue expired refresh: %v", err)
	}
	nextAccess, nextAccessDigest, nextRefresh, nextRefreshDigest := oauthNewTokenPair(t, grant, f.now)
	if _, _, err := f.store.RotateRefreshToken(ctx, expiredRefreshDigest, nextAccess, nextAccessDigest, nextRefresh, nextRefreshDigest, f.now); !errors.Is(err, mcpoauth.ErrTokenExpired) {
		t.Fatalf("expired refresh token: want ErrTokenExpired, got %v", err)
	}
	var usedAt, revokedAt sql.NullTime
	if err := f.db.QueryRowContext(ctx, `SELECT used_at, revoked_at FROM oauth_refresh_tokens WHERE id = ?`, expiredRefresh.ID).Scan(&usedAt, &revokedAt); err != nil {
		t.Fatalf("read expired refresh state: %v", err)
	}
	if usedAt.Valid || revokedAt.Valid {
		t.Fatal("expired rejection consumed or revoked the refresh token")
	}

	// a failed rotation rolls back completely: the parent stays unconsumed
	rolledAccess, rolledAccessDigest, rolledRefresh, rolledRefreshDigest := oauthNewTokenPair(t, grant, f.now)
	if err := f.store.IssueTokens(ctx, rolledAccess, rolledAccessDigest, rolledRefresh, rolledRefreshDigest); err != nil {
		t.Fatalf("issue rolled tokens: %v", err)
	}
	failAccess, failAccessDigest, failRefresh, _ := oauthNewTokenPair(t, grant, f.now)
	if _, _, err := f.store.RotateRefreshToken(ctx, rolledRefreshDigest, failAccess, failAccessDigest, failRefresh, rolledRefreshDigest, f.now); err == nil {
		t.Fatal("rotation with a colliding refresh digest succeeded")
	}
	var parentUsedAt sql.NullTime
	if err := f.db.QueryRowContext(ctx, `SELECT used_at FROM oauth_refresh_tokens WHERE id = ?`, rolledRefresh.ID).Scan(&parentUsedAt); err != nil {
		t.Fatalf("read parent used_at: %v", err)
	}
	if parentUsedAt.Valid {
		t.Fatal("failed rotation left the parent refresh token consumed")
	}
	var accessCount int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_access_tokens WHERE id = ?`, failAccess.ID).Scan(&accessCount); err != nil {
		t.Fatalf("count failed access token: %v", err)
	}
	if accessCount != 0 {
		t.Fatal("failed rotation persisted its access token")
	}

	// the same parent can still be rotated after the failed attempt
	okAccess, okAccessDigest, okRefresh, okRefreshDigest := oauthNewTokenPair(t, grant, f.now)
	okRotatedAccess, okRotatedRefresh, err := f.store.RotateRefreshToken(ctx, rolledRefreshDigest, okAccess, okAccessDigest, okRefresh, okRefreshDigest, f.now)
	if err != nil {
		t.Fatalf("rotate after rollback: %v", err)
	}
	if okRotatedAccess.ID != okAccess.ID || okRotatedAccess.GrantID != grant.ID || okRotatedAccess.UserID != f.userID {
		t.Fatalf("rotated access mismatch: %#v", okRotatedAccess)
	}
	if okRotatedRefresh.ParentID != rolledRefresh.ID || okRotatedRefresh.FamilyID != rolledRefresh.ID {
		t.Fatalf("rotation lineage wrong after rollback: %#v", okRotatedRefresh)
	}
}

func TestOAuthStoreAccessTokenValidation(t *testing.T) {
	f := oauthTestFixture(t)
	ctx := t.Context()
	grant, err := f.store.SaveGrant(ctx, f.grant(t, f.deviceID))
	if err != nil {
		t.Fatalf("save grant: %v", err)
	}

	access, accessDigest, refresh, refreshDigest := oauthNewTokenPair(t, grant, f.now)
	if err := f.store.IssueTokens(ctx, access, accessDigest, refresh, refreshDigest); err != nil {
		t.Fatalf("issue tokens: %v", err)
	}

	info, err := f.store.AccessTokenByDigest(ctx, accessDigest, f.now)
	if err != nil {
		t.Fatalf("validate access token: %v", err)
	}
	if info.Token.ID != access.ID || info.Token.UserID != f.userID || info.Token.GrantID != grant.ID {
		t.Fatalf("token mismatch: %#v", info.Token)
	}
	if info.Grant.ID != grant.ID || info.Grant.UserID != f.userID || info.Grant.DeviceID != f.deviceID || info.Grant.Resource != oauthTestResource {
		t.Fatalf("grant mismatch: %#v", info.Grant)
	}
	if !equalStrings(info.Grant.Scopes, grant.Scopes) {
		t.Fatalf("grant scopes = %#v, want %#v", info.Grant.Scopes, grant.Scopes)
	}

	_, unknownDigest := oauthTestCredential(t)
	if _, err := f.store.AccessTokenByDigest(ctx, unknownDigest, f.now); !errors.Is(err, mcpoauth.ErrTokenNotFound) {
		t.Fatalf("unknown token: want ErrTokenNotFound, got %v", err)
	}

	// an expired access token is rejected
	expiredAccess, expiredAccessDigest, expiredRefresh, expiredRefreshDigest := oauthNewTokenPair(t, grant, f.now)
	expiredAccess.ExpiresAt = f.now.Add(-time.Minute)
	if err := f.store.IssueTokens(ctx, expiredAccess, expiredAccessDigest, expiredRefresh, expiredRefreshDigest); err != nil {
		t.Fatalf("issue expired access: %v", err)
	}
	if _, err := f.store.AccessTokenByDigest(ctx, expiredAccessDigest, f.now); !errors.Is(err, mcpoauth.ErrTokenExpired) {
		t.Fatalf("expired token: want ErrTokenExpired, got %v", err)
	}

	// a revoked grant invalidates its unexpired, unrevoked access tokens
	if _, err := f.db.ExecContext(ctx, `UPDATE oauth_grants SET revoked_at = ? WHERE id = ?`, f.now, grant.ID); err != nil {
		t.Fatalf("revoke grant directly: %v", err)
	}
	if _, err := f.store.AccessTokenByDigest(ctx, accessDigest, f.now); !errors.Is(err, mcpoauth.ErrGrantRevoked) {
		t.Fatalf("revoked grant: want ErrGrantRevoked, got %v", err)
	}
}

func TestOAuthStoreRevocationSemantics(t *testing.T) {
	f := oauthTestFixture(t)
	ctx := t.Context()
	grant, err := f.store.SaveGrant(ctx, f.grant(t, f.deviceID))
	if err != nil {
		t.Fatalf("save grant: %v", err)
	}

	// access token revocation is targeted and idempotent for unknown tokens
	access1, access1Digest, refresh1, refresh1Digest := oauthNewTokenPair(t, grant, f.now)
	if err := f.store.IssueTokens(ctx, access1, access1Digest, refresh1, refresh1Digest); err != nil {
		t.Fatalf("issue tokens: %v", err)
	}
	if err := f.store.RevokeAccessToken(ctx, access1Digest, f.now); err != nil {
		t.Fatalf("revoke access token: %v", err)
	}
	if _, err := f.store.AccessTokenByDigest(ctx, access1Digest, f.now); !errors.Is(err, mcpoauth.ErrTokenRevoked) {
		t.Fatalf("revoked access token: want ErrTokenRevoked, got %v", err)
	}
	_, unknown := oauthTestCredential(t)
	if err := f.store.RevokeAccessToken(ctx, unknown, f.now); err != nil {
		t.Fatalf("revoking an unknown access token must succeed: %v", err)
	}

	// refresh token revocation revokes the family, the grant, and its access tokens
	access2, access2Digest, refresh2, refresh2Digest := oauthNewTokenPair(t, grant, f.now)
	if err := f.store.IssueTokens(ctx, access2, access2Digest, refresh2, refresh2Digest); err != nil {
		t.Fatalf("issue tokens: %v", err)
	}
	if err := f.store.RevokeRefreshToken(ctx, refresh2Digest, f.now); err != nil {
		t.Fatalf("revoke refresh token: %v", err)
	}
	var familyUnrevoked int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_refresh_tokens WHERE family_id = ? AND revoked_at IS NULL`, refresh2.ID).Scan(&familyUnrevoked); err != nil {
		t.Fatalf("count family: %v", err)
	}
	if familyUnrevoked != 0 {
		t.Fatalf("%d family tokens remain unrevoked", familyUnrevoked)
	}
	if _, err := f.store.AccessTokenByDigest(ctx, access2Digest, f.now); !errors.Is(err, mcpoauth.ErrTokenRevoked) {
		t.Fatalf("access token survived refresh revocation: want ErrTokenRevoked, got %v", err)
	}
	var grantRevokedAt sql.NullTime
	if err := f.db.QueryRowContext(ctx, `SELECT revoked_at FROM oauth_grants WHERE id = ?`, grant.ID).Scan(&grantRevokedAt); err != nil {
		t.Fatalf("read grant revocation: %v", err)
	}
	if !grantRevokedAt.Valid {
		t.Fatal("grant survived refresh token revocation")
	}
	nextAccess, nextAccessDigest, nextRefresh, nextRefreshDigest := oauthNewTokenPair(t, grant, f.now)
	if _, _, err := f.store.RotateRefreshToken(ctx, refresh2Digest, nextAccess, nextAccessDigest, nextRefresh, nextRefreshDigest, f.now); !errors.Is(err, mcpoauth.ErrTokenRevoked) {
		t.Fatalf("rotate a revoked refresh token: want ErrTokenRevoked, got %v", err)
	}
	if err := f.store.RevokeRefreshToken(ctx, unknown, f.now); err != nil {
		t.Fatalf("revoking an unknown refresh token must succeed: %v", err)
	}

	// grant revocation invalidates every token issued under the grant
	grant2, err := f.store.SaveGrant(ctx, f.grant(t, f.otherDeviceID))
	if err != nil {
		t.Fatalf("save second grant: %v", err)
	}
	access3, access3Digest, refresh3, refresh3Digest := oauthNewTokenPair(t, grant2, f.now)
	if err := f.store.IssueTokens(ctx, access3, access3Digest, refresh3, refresh3Digest); err != nil {
		t.Fatalf("issue tokens under second grant: %v", err)
	}
	if err := f.store.RevokeGrant(ctx, grant2.ID, f.now); err != nil {
		t.Fatalf("revoke grant: %v", err)
	}
	if _, err := f.store.AccessTokenByDigest(ctx, access3Digest, f.now); !errors.Is(err, mcpoauth.ErrTokenRevoked) {
		t.Fatalf("access token survived grant revocation: want ErrTokenRevoked, got %v", err)
	}
	var refreshUnrevoked int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM oauth_refresh_tokens WHERE grant_id = ? AND revoked_at IS NULL`, grant2.ID).Scan(&refreshUnrevoked); err != nil {
		t.Fatalf("count refresh tokens: %v", err)
	}
	if refreshUnrevoked != 0 {
		t.Fatalf("%d refresh tokens under the revoked grant remain unrevoked", refreshUnrevoked)
	}

	// grant revocation is idempotent
	if err := f.store.RevokeGrant(ctx, grant2.ID, f.now.Add(time.Minute)); err != nil {
		t.Fatalf("second grant revocation must succeed: %v", err)
	}
}
