package mysqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"robloxkit/internal/mcpoauth"
)

const oauthMaxCodeChallengeLength = 128

// OAuthStore persists connector OAuth clients, authorization codes, grants,
// and token digests. Plaintext codes and tokens never reach this store: only
// keyed digests are written, and every mutation runs in a single transaction.
type OAuthStore struct {
	DB *sql.DB
}

// OAuthStore implements the mcpoauth storage contract.
var _ mcpoauth.Store = (*OAuthStore)(nil)

func NewOAuthStore(db *sql.DB) *OAuthStore {
	return &OAuthStore{DB: db}
}

func (s *OAuthStore) check(ctx context.Context) error {
	if ctx == nil {
		return errors.New("mysqlstore: nil context")
	}
	if s == nil || s.DB == nil {
		return errors.New("mysqlstore: nil database")
	}
	return nil
}

// oauthRowQueryer abstracts *sql.DB and *sql.Tx for row-selection helpers.
type oauthRowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func oauthJSONStrings(value []string) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("mysqlstore: encode json string list: %w", err)
	}
	return raw, nil
}

func oauthScanStrings(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("mysqlstore: empty json string list")
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mysqlstore: decode json string list: %w", err)
	}
	return out, nil
}

func (s *OAuthStore) RegisterClient(ctx context.Context, client mcpoauth.Client) (mcpoauth.Client, error) {
	if err := s.check(ctx); err != nil {
		return mcpoauth.Client{}, err
	}
	if err := mcpoauth.ValidateClient(client); err != nil {
		return mcpoauth.Client{}, err
	}
	// The internal row id is generated here; a caller-supplied id is only a
	// candidate that the upsert discards when the public client_id repeats.
	candidateID := client.ID
	if candidateID == "" {
		var genErr error
		candidateID, genErr = identityUUID()
		if genErr != nil {
			return mcpoauth.Client{}, fmt.Errorf("mysqlstore: generate client id: %w", genErr)
		}
	}
	createdAt := client.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	redirects, err := oauthJSONStrings(client.RedirectURIs)
	if err != nil {
		return mcpoauth.Client{}, err
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return mcpoauth.Client{}, fmt.Errorf("mysqlstore: begin client upsert: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO oauth_clients (id, client_id, client_name, redirect_uris, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE client_name = VALUES(client_name), redirect_uris = VALUES(redirect_uris)`,
		candidateID, client.ClientID, client.ClientName, redirects, createdAt.UTC()); err != nil {
		return mcpoauth.Client{}, fmt.Errorf("mysqlstore: upsert client: %w", err)
	}
	stored, err := oauthSelectClient(ctx, tx, client.ClientID)
	if err != nil {
		return mcpoauth.Client{}, err
	}
	if err := tx.Commit(); err != nil {
		return mcpoauth.Client{}, fmt.Errorf("mysqlstore: commit client upsert: %w", err)
	}
	return stored, nil
}

func oauthSelectClient(ctx context.Context, q oauthRowQueryer, publicClientID string) (mcpoauth.Client, error) {
	var client mcpoauth.Client
	var redirects []byte
	err := q.QueryRowContext(ctx,
		`SELECT id, client_id, client_name, redirect_uris, created_at FROM oauth_clients WHERE client_id = ?`,
		publicClientID).Scan(&client.ID, &client.ClientID, &client.ClientName, &redirects, &client.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return mcpoauth.Client{}, mcpoauth.ErrClientNotFound
	}
	if err != nil {
		return mcpoauth.Client{}, fmt.Errorf("mysqlstore: find client: %w", err)
	}
	client.RedirectURIs, err = oauthScanStrings(redirects)
	if err != nil {
		return mcpoauth.Client{}, err
	}
	client.CreatedAt = client.CreatedAt.UTC()
	return client, nil
}

func (s *OAuthStore) ClientByPublicID(ctx context.Context, publicClientID string) (mcpoauth.Client, error) {
	if err := s.check(ctx); err != nil {
		return mcpoauth.Client{}, err
	}
	return oauthSelectClient(ctx, s.DB, publicClientID)
}

func (s *OAuthStore) SaveAuthorizationCode(ctx context.Context, code mcpoauth.AuthorizationCode, digest [32]byte) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if code.ID == "" {
		return errors.New("mysqlstore: authorization code id is required")
	}
	if code.UserID == "" {
		return errors.New("mysqlstore: authorization code user is required")
	}
	if code.ClientID == "" {
		return errors.New("mysqlstore: authorization code client is required")
	}
	if code.CodeChallenge == "" {
		return errors.New("mysqlstore: authorization code PKCE challenge is required")
	}
	if len(code.CodeChallenge) > oauthMaxCodeChallengeLength {
		return fmt.Errorf("mysqlstore: PKCE challenge exceeds %d characters", oauthMaxCodeChallengeLength)
	}
	if err := mcpoauth.ValidateRedirectURI(code.RedirectURI); err != nil {
		return fmt.Errorf("mysqlstore: authorization code redirect URI: %w", err)
	}
	if err := mcpoauth.ValidateResourceURL(code.Resource); err != nil {
		return fmt.Errorf("mysqlstore: authorization code resource: %w", err)
	}
	if err := mcpoauth.ValidateScopes(code.Scopes); err != nil {
		return err
	}
	if code.ExpiresAt.IsZero() {
		return errors.New("mysqlstore: authorization code expiry is required")
	}
	scopes, err := oauthJSONStrings(code.Scopes)
	if err != nil {
		return err
	}
	createdAt := code.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO oauth_authorization_codes
		       (id, user_id, client_id, redirect_uri, code_challenge, scopes, device_id, studio_session_id, resource, expires_at, created_at, code_digest)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		code.ID, code.UserID, code.ClientID, code.RedirectURI, code.CodeChallenge, scopes,
		nullableString(code.DeviceID), nullableString(code.StudioSessionID), code.Resource,
		code.ExpiresAt.UTC(), createdAt.UTC(), digest[:]); err != nil {
		return fmt.Errorf("mysqlstore: insert authorization code: %w", err)
	}
	return nil
}

// ConsumeAuthorizationCode is the single-use linearization point: it locks
// the code row, rejects consumed or expired codes, verifies the exact
// resource, client, and redirect binding, and marks the code consumed in one
// transaction. Binding mismatches never consume the code.
func (s *OAuthStore) ConsumeAuthorizationCode(ctx context.Context, digest [32]byte, binding mcpoauth.CodeBinding, now time.Time) (mcpoauth.AuthorizationCode, error) {
	if err := s.check(ctx); err != nil {
		return mcpoauth.AuthorizationCode{}, err
	}
	if now.IsZero() {
		return mcpoauth.AuthorizationCode{}, errors.New("mysqlstore: consumption time is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return mcpoauth.AuthorizationCode{}, fmt.Errorf("mysqlstore: begin code consumption: %w", err)
	}
	defer tx.Rollback()

	var (
		code       mcpoauth.AuthorizationCode
		scopes     []byte
		deviceID   sql.NullString
		studioID   sql.NullString
		consumedAt sql.NullTime
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, user_id, client_id, redirect_uri, code_challenge, scopes, device_id, studio_session_id, resource, expires_at, consumed_at, created_at
		 FROM oauth_authorization_codes WHERE code_digest = ? FOR UPDATE`,
		digest[:]).Scan(&code.ID, &code.UserID, &code.ClientID, &code.RedirectURI, &code.CodeChallenge,
		&scopes, &deviceID, &studioID, &code.Resource, &code.ExpiresAt, &consumedAt, &code.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return mcpoauth.AuthorizationCode{}, mcpoauth.ErrCodeNotFound
	}
	if err != nil {
		return mcpoauth.AuthorizationCode{}, fmt.Errorf("mysqlstore: find authorization code: %w", err)
	}
	code.Scopes, err = oauthScanStrings(scopes)
	if err != nil {
		return mcpoauth.AuthorizationCode{}, err
	}
	code.DeviceID = deviceID.String
	code.StudioSessionID = studioID.String
	if consumedAt.Valid {
		return mcpoauth.AuthorizationCode{}, mcpoauth.ErrCodeUsed
	}
	if !now.Before(code.ExpiresAt) {
		return mcpoauth.AuthorizationCode{}, mcpoauth.ErrCodeExpired
	}
	if binding.ClientID != code.ClientID || binding.RedirectURI != code.RedirectURI || binding.Resource != code.Resource {
		return mcpoauth.AuthorizationCode{}, mcpoauth.ErrCodeBinding
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE oauth_authorization_codes SET consumed_at = ? WHERE id = ? AND consumed_at IS NULL`,
		now.UTC(), code.ID)
	if err != nil {
		return mcpoauth.AuthorizationCode{}, fmt.Errorf("mysqlstore: consume authorization code: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return mcpoauth.AuthorizationCode{}, fmt.Errorf("mysqlstore: consume authorization code: %w", err)
	}
	if affected != 1 {
		return mcpoauth.AuthorizationCode{}, mcpoauth.ErrCodeUsed
	}
	if err := tx.Commit(); err != nil {
		return mcpoauth.AuthorizationCode{}, fmt.Errorf("mysqlstore: commit code consumption: %w", err)
	}
	consumedWhen := now.UTC()
	code.ConsumedAt = &consumedWhen
	code.ExpiresAt = code.ExpiresAt.UTC()
	code.CreatedAt = code.CreatedAt.UTC()
	return code, nil
}

func (s *OAuthStore) SaveGrant(ctx context.Context, grant mcpoauth.Grant) (mcpoauth.Grant, error) {
	if err := s.check(ctx); err != nil {
		return mcpoauth.Grant{}, err
	}
	if grant.ID == "" {
		return mcpoauth.Grant{}, errors.New("mysqlstore: grant id is required")
	}
	if grant.UserID == "" {
		return mcpoauth.Grant{}, errors.New("mysqlstore: grant user is required")
	}
	if grant.ClientID == "" {
		return mcpoauth.Grant{}, errors.New("mysqlstore: grant client is required")
	}
	if grant.DeviceID == "" {
		return mcpoauth.Grant{}, errors.New("mysqlstore: grant device is required")
	}
	if err := mcpoauth.ValidateResourceURL(grant.Resource); err != nil {
		return mcpoauth.Grant{}, fmt.Errorf("mysqlstore: grant resource: %w", err)
	}
	if err := mcpoauth.ValidateScopes(grant.Scopes); err != nil {
		return mcpoauth.Grant{}, err
	}
	scopes, err := oauthJSONStrings(grant.Scopes)
	if err != nil {
		return mcpoauth.Grant{}, err
	}
	createdAt := grant.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return mcpoauth.Grant{}, fmt.Errorf("mysqlstore: begin grant upsert: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO oauth_grants
		       (id, user_id, client_id, device_id, studio_session_id, scopes, resource, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE scopes = VALUES(scopes), resource = VALUES(resource),
		                          studio_session_id = VALUES(studio_session_id), revoked_at = NULL`,
		grant.ID, grant.UserID, grant.ClientID, grant.DeviceID, nullableString(grant.StudioSessionID),
		scopes, grant.Resource, createdAt.UTC()); err != nil {
		return mcpoauth.Grant{}, fmt.Errorf("mysqlstore: upsert grant: %w", err)
	}
	stored, err := oauthSelectGrant(ctx, tx, grant.UserID, grant.ClientID, grant.DeviceID)
	if err != nil {
		return mcpoauth.Grant{}, err
	}
	if err := tx.Commit(); err != nil {
		return mcpoauth.Grant{}, fmt.Errorf("mysqlstore: commit grant upsert: %w", err)
	}
	return stored, nil
}

func oauthSelectGrant(ctx context.Context, q oauthRowQueryer, userID, clientID, deviceID string) (mcpoauth.Grant, error) {
	var (
		grant   mcpoauth.Grant
		scopes  []byte
		studio  sql.NullString
		revoked sql.NullTime
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, user_id, client_id, device_id, studio_session_id, scopes, resource, created_at, revoked_at
		 FROM oauth_grants WHERE user_id = ? AND client_id = ? AND device_id = ?`,
		userID, clientID, deviceID).Scan(&grant.ID, &grant.UserID, &grant.ClientID, &grant.DeviceID,
		&studio, &scopes, &grant.Resource, &grant.CreatedAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return mcpoauth.Grant{}, mcpoauth.ErrGrantNotFound
	}
	if err != nil {
		return mcpoauth.Grant{}, fmt.Errorf("mysqlstore: find grant: %w", err)
	}
	grant.StudioSessionID = studio.String
	grant.Scopes, err = oauthScanStrings(scopes)
	if err != nil {
		return mcpoauth.Grant{}, err
	}
	if revoked.Valid {
		revokedWhen := revoked.Time.UTC()
		grant.RevokedAt = &revokedWhen
	}
	grant.CreatedAt = grant.CreatedAt.UTC()
	return grant, nil
}

func (s *OAuthStore) RevokeGrant(ctx context.Context, grantID string, now time.Time) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if grantID == "" {
		return errors.New("mysqlstore: grant id is required")
	}
	if now.IsZero() {
		return errors.New("mysqlstore: revocation time is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysqlstore: begin grant revocation: %w", err)
	}
	defer tx.Rollback()
	var userID string
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM oauth_grants WHERE id = ? FOR UPDATE`, grantID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return mcpoauth.ErrGrantNotFound
	}
	if err != nil {
		return fmt.Errorf("mysqlstore: find grant for revocation: %w", err)
	}
	when := now.UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_grants SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND user_id = ?`,
		when, grantID, userID); err != nil {
		return fmt.Errorf("mysqlstore: revoke grant: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_access_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE grant_id = ? AND user_id = ?`,
		when, grantID, userID); err != nil {
		return fmt.Errorf("mysqlstore: revoke grant access tokens: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_refresh_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE grant_id = ? AND user_id = ?`,
		when, grantID, userID); err != nil {
		return fmt.Errorf("mysqlstore: revoke grant refresh tokens: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysqlstore: commit grant revocation: %w", err)
	}
	return nil
}

func (s *OAuthStore) IssueTokens(ctx context.Context, access mcpoauth.AccessToken, accessDigest [32]byte, refresh mcpoauth.RefreshToken, refreshDigest [32]byte) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if err := oauthValidateTokenPair(access, refresh); err != nil {
		return err
	}
	if refresh.FamilyID == "" {
		refresh.FamilyID = refresh.ID // family root
	}
	now := time.Now().UTC()
	if access.CreatedAt.IsZero() {
		access.CreatedAt = now
	}
	if refresh.CreatedAt.IsZero() {
		refresh.CreatedAt = now
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysqlstore: begin token issuance: %w", err)
	}
	defer tx.Rollback()
	if err := oauthInsertAccessToken(ctx, tx, access, accessDigest); err != nil {
		return err
	}
	if err := oauthInsertRefreshToken(ctx, tx, refresh, refreshDigest); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysqlstore: commit token issuance: %w", err)
	}
	return nil
}

// RotateRefreshToken consumes an active refresh token and issues its
// replacements in one transaction. Presenting a rotated or revoked refresh
// token revokes the entire family, its grant, and the grant's access tokens
// before the error is reported.
func (s *OAuthStore) RotateRefreshToken(ctx context.Context, oldDigest [32]byte, access mcpoauth.AccessToken, accessDigest [32]byte, refresh mcpoauth.RefreshToken, refreshDigest [32]byte, now time.Time) (mcpoauth.AccessToken, mcpoauth.RefreshToken, error) {
	if err := s.check(ctx); err != nil {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, err
	}
	if access.ID == "" || refresh.ID == "" {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, errors.New("mysqlstore: token ids are required")
	}
	if access.ExpiresAt.IsZero() || refresh.ExpiresAt.IsZero() {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, errors.New("mysqlstore: token expiry is required")
	}
	if now.IsZero() {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, errors.New("mysqlstore: rotation time is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, fmt.Errorf("mysqlstore: begin refresh rotation: %w", err)
	}
	defer tx.Rollback()

	parent, usedAt, revokedAt, err := oauthSelectRefresh(ctx, tx, oldDigest[:])
	if err != nil {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, err
	}
	if usedAt.Valid || revokedAt.Valid {
		if err := oauthRevokeFamilyTx(ctx, tx, parent.FamilyID, parent.GrantID, parent.UserID, now); err != nil {
			return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, err
		}
		if err := tx.Commit(); err != nil {
			return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, fmt.Errorf("mysqlstore: commit family revocation: %w", err)
		}
		if usedAt.Valid {
			return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, mcpoauth.ErrTokenReused
		}
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, mcpoauth.ErrTokenRevoked
	}
	if !now.Before(parent.ExpiresAt) {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, mcpoauth.ErrTokenExpired
	}
	result, err := tx.ExecContext(ctx,
		`UPDATE oauth_refresh_tokens SET used_at = ? WHERE id = ? AND user_id = ? AND used_at IS NULL`,
		now.UTC(), parent.ID, parent.UserID)
	if err != nil {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, fmt.Errorf("mysqlstore: consume refresh token: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, fmt.Errorf("mysqlstore: consume refresh token: %w", err)
	}
	if affected != 1 {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, mcpoauth.ErrTokenReused
	}

	access.UserID = parent.UserID
	access.GrantID = parent.GrantID
	refresh.UserID = parent.UserID
	refresh.GrantID = parent.GrantID
	refresh.FamilyID = parent.FamilyID
	refresh.ParentID = parent.ID
	when := now.UTC()
	if access.CreatedAt.IsZero() {
		access.CreatedAt = when
	}
	if refresh.CreatedAt.IsZero() {
		refresh.CreatedAt = when
	}
	if err := oauthInsertAccessToken(ctx, tx, access, accessDigest); err != nil {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, err
	}
	if err := oauthInsertRefreshToken(ctx, tx, refresh, refreshDigest); err != nil {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return mcpoauth.AccessToken{}, mcpoauth.RefreshToken{}, fmt.Errorf("mysqlstore: commit refresh rotation: %w", err)
	}
	access.ExpiresAt = access.ExpiresAt.UTC()
	access.CreatedAt = access.CreatedAt.UTC()
	refresh.ExpiresAt = refresh.ExpiresAt.UTC()
	refresh.CreatedAt = refresh.CreatedAt.UTC()
	return access, refresh, nil
}

func oauthSelectRefresh(ctx context.Context, q oauthRowQueryer, digest []byte) (mcpoauth.RefreshToken, sql.NullTime, sql.NullTime, error) {
	var (
		token   mcpoauth.RefreshToken
		parent  sql.NullString
		usedAt  sql.NullTime
		revoked sql.NullTime
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, user_id, grant_id, family_id, parent_id, expires_at, used_at, revoked_at, created_at
		 FROM oauth_refresh_tokens WHERE token_digest = ? FOR UPDATE`,
		digest).Scan(&token.ID, &token.UserID, &token.GrantID, &token.FamilyID, &parent,
		&token.ExpiresAt, &usedAt, &revoked, &token.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return mcpoauth.RefreshToken{}, sql.NullTime{}, sql.NullTime{}, mcpoauth.ErrTokenNotFound
	}
	if err != nil {
		return mcpoauth.RefreshToken{}, sql.NullTime{}, sql.NullTime{}, fmt.Errorf("mysqlstore: find refresh token: %w", err)
	}
	token.ParentID = parent.String
	token.ExpiresAt = token.ExpiresAt.UTC()
	token.CreatedAt = token.CreatedAt.UTC()
	return token, usedAt, revoked, nil
}

// oauthRevokeFamilyTx revokes a refresh-token family, the grant that issued
// it, and all access tokens under that grant. It runs inside the caller's
// transaction.
func oauthRevokeFamilyTx(ctx context.Context, tx *sql.Tx, familyID, grantID, userID string, now time.Time) error {
	when := now.UTC()
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_refresh_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE family_id = ? AND user_id = ?`,
		when, familyID, userID); err != nil {
		return fmt.Errorf("mysqlstore: revoke refresh family: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_access_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE grant_id = ? AND user_id = ?`,
		when, grantID, userID); err != nil {
		return fmt.Errorf("mysqlstore: revoke grant access tokens: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE oauth_grants SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ? AND user_id = ?`,
		when, grantID, userID); err != nil {
		return fmt.Errorf("mysqlstore: revoke grant: %w", err)
	}
	return nil
}

func oauthInsertAccessToken(ctx context.Context, tx *sql.Tx, access mcpoauth.AccessToken, digest [32]byte) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO oauth_access_tokens (id, user_id, grant_id, token_digest, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		access.ID, access.UserID, access.GrantID, digest[:], access.ExpiresAt.UTC(), access.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("mysqlstore: insert access token: %w", err)
	}
	return nil
}

func oauthInsertRefreshToken(ctx context.Context, tx *sql.Tx, refresh mcpoauth.RefreshToken, digest [32]byte) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO oauth_refresh_tokens (id, user_id, grant_id, family_id, parent_id, token_digest, expires_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		refresh.ID, refresh.UserID, refresh.GrantID, refresh.FamilyID, nullableString(refresh.ParentID),
		digest[:], refresh.ExpiresAt.UTC(), refresh.CreatedAt.UTC()); err != nil {
		return fmt.Errorf("mysqlstore: insert refresh token: %w", err)
	}
	return nil
}

func oauthValidateTokenPair(access mcpoauth.AccessToken, refresh mcpoauth.RefreshToken) error {
	if access.ID == "" || refresh.ID == "" {
		return errors.New("mysqlstore: token ids are required")
	}
	if access.UserID == "" || refresh.UserID == "" {
		return errors.New("mysqlstore: token user is required")
	}
	if access.UserID != refresh.UserID {
		return errors.New("mysqlstore: access and refresh tokens must share a user")
	}
	if access.GrantID == "" || refresh.GrantID == "" {
		return errors.New("mysqlstore: token grant is required")
	}
	if access.GrantID != refresh.GrantID {
		return errors.New("mysqlstore: access and refresh tokens must share a grant")
	}
	if access.ExpiresAt.IsZero() || refresh.ExpiresAt.IsZero() {
		return errors.New("mysqlstore: token expiry is required")
	}
	return nil
}

func (s *OAuthStore) AccessTokenByDigest(ctx context.Context, digest [32]byte, now time.Time) (mcpoauth.AccessTokenInfo, error) {
	if err := s.check(ctx); err != nil {
		return mcpoauth.AccessTokenInfo{}, err
	}
	if now.IsZero() {
		return mcpoauth.AccessTokenInfo{}, errors.New("mysqlstore: validation time is required")
	}
	var (
		info         mcpoauth.AccessTokenInfo
		scopes       []byte
		deviceID     sql.NullString
		studioID     sql.NullString
		tokenRevoked sql.NullTime
		grantRevoked sql.NullTime
	)
	err := s.DB.QueryRowContext(ctx,
		`SELECT a.id, a.user_id, a.grant_id, a.expires_at, a.revoked_at, a.created_at,
		        g.id, g.user_id, g.client_id, g.device_id, g.studio_session_id, g.scopes, g.resource, g.created_at, g.revoked_at
		 FROM oauth_access_tokens a
		 JOIN oauth_grants g ON g.id = a.grant_id AND g.user_id = a.user_id
		 WHERE a.token_digest = ?`,
		digest[:]).Scan(&info.Token.ID, &info.Token.UserID, &info.Token.GrantID, &info.Token.ExpiresAt,
		&tokenRevoked, &info.Token.CreatedAt, &info.Grant.ID, &info.Grant.UserID, &info.Grant.ClientID,
		&deviceID, &studioID, &scopes, &info.Grant.Resource, &info.Grant.CreatedAt, &grantRevoked)
	if errors.Is(err, sql.ErrNoRows) {
		return mcpoauth.AccessTokenInfo{}, mcpoauth.ErrTokenNotFound
	}
	if err != nil {
		return mcpoauth.AccessTokenInfo{}, fmt.Errorf("mysqlstore: find access token: %w", err)
	}
	info.Grant.DeviceID = deviceID.String
	info.Grant.StudioSessionID = studioID.String
	info.Grant.Scopes, err = oauthScanStrings(scopes)
	if err != nil {
		return mcpoauth.AccessTokenInfo{}, err
	}
	if tokenRevoked.Valid {
		return mcpoauth.AccessTokenInfo{}, mcpoauth.ErrTokenRevoked
	}
	if !now.Before(info.Token.ExpiresAt) {
		return mcpoauth.AccessTokenInfo{}, mcpoauth.ErrTokenExpired
	}
	if grantRevoked.Valid {
		return mcpoauth.AccessTokenInfo{}, mcpoauth.ErrGrantRevoked
	}
	info.Token.ExpiresAt = info.Token.ExpiresAt.UTC()
	info.Token.CreatedAt = info.Token.CreatedAt.UTC()
	info.Grant.CreatedAt = info.Grant.CreatedAt.UTC()
	return info, nil
}

func (s *OAuthStore) RevokeAccessToken(ctx context.Context, digest [32]byte, now time.Time) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx,
		`UPDATE oauth_access_tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE token_digest = ?`,
		now.UTC(), digest[:]); err != nil {
		return fmt.Errorf("mysqlstore: revoke access token: %w", err)
	}
	return nil
}

func (s *OAuthStore) RevokeRefreshToken(ctx context.Context, digest [32]byte, now time.Time) error {
	if err := s.check(ctx); err != nil {
		return err
	}
	if now.IsZero() {
		return errors.New("mysqlstore: revocation time is required")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysqlstore: begin refresh revocation: %w", err)
	}
	defer tx.Rollback()
	token, _, _, err := oauthSelectRefresh(ctx, tx, digest[:])
	if errors.Is(err, mcpoauth.ErrTokenNotFound) {
		return nil // RFC 7009: unknown tokens revoke silently
	}
	if err != nil {
		return err
	}
	if err := oauthRevokeFamilyTx(ctx, tx, token.FamilyID, token.GrantID, token.UserID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysqlstore: commit refresh revocation: %w", err)
	}
	return nil
}
