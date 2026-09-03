package mcpoauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"github.com/ory/fosite/handler/oauth2"
	"github.com/ory/fosite/handler/pkce"

	"robloxkit/internal/audit"
	"robloxkit/internal/credential"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/session"
)

// Handler lifespans applied when the configuration leaves them unset.
const (
	DefaultAuthorizeCodeLifespan = 10 * time.Minute
	DefaultAccessTokenLifespan   = time.Hour
	DefaultRefreshTokenLifespan  = 30 * 24 * time.Hour
)

// Opaque credential prefixes. The strategy generates every code and token as
// an opaque secret and stores only its keyed digest.
const (
	authorizeCodePrefix = "mcc_"
	accessTokenPrefix   = "mca_"
	refreshTokenPrefix  = "mcr_"
	secretEntropy       = 32
)

// GrantAuditAction is the secret-free audit action recorded when a user
// approves a connector grant.
const GrantAuditAction = "connector.grant.approve"

var (
	// ErrInvalidConfig reports an incomplete provider configuration.
	ErrInvalidConfig = errors.New("mcpoauth: invalid provider configuration")
	// ErrMissingTokenFlow reports storage access outside a seeded token flow.
	ErrMissingTokenFlow = errors.New("mcpoauth: missing token flow context")
)

// Entitlements mirrors the frozen entitlement authorization surface: the
// connector gateway only serves users with an active trial or license.
type Entitlements interface {
	Authorize(ctx context.Context, subject entitlement.Subject) (entitlement.Decision, error)
}

// Config wires the connector authorization server to its stores and policy
// services. DB must reference the same database as Store: the consent flow
// persists the grant and its audit event in one transaction on it.
type Config struct {
	// Resource is the protected /mcp resource every code, grant, and token
	// binds exactly.
	Resource *url.URL

	// Store persists clients, codes, grants, and token digests.
	Store Store

	// DB backs the consent transaction and the adapter's read-only lookups.
	DB *sql.DB

	// Audits records the secret-free grant approval event.
	Audits *audit.Service

	// Entitlements gates consent on the active trial or license window.
	Entitlements Entitlements

	// Sessions validates the browser session on authorize and consent.
	Sessions *session.Service

	// Pepper keys every code and token digest.
	Pepper []byte

	// LoginPath receives unauthenticated authorize requests with the
	// original request URL as the "next" parameter.
	LoginPath string

	// Lifespans for issued credentials. Zero falls back to the defaults.
	AuthorizeCodeLifespan time.Duration
	AccessTokenLifespan   time.Duration
	RefreshTokenLifespan  time.Duration
}

// Provider is the connector-facing OAuth 2.1 authorization server. It composes
// the fosite authorization-code grant with mandatory PKCE S256, the refresh
// grant, and RFC 7009 revocation. Implicit, password, and client-credentials
// grants are deliberately not composed, and fosite debug output is off.
type Provider struct {
	config      Config
	store       Store
	db          *sql.DB
	audits      *audit.Service
	pepper      []byte
	resource    string
	fosite      fosite.OAuth2Provider
	codeLife    time.Duration
	accessLife  time.Duration
	refreshLife time.Duration
}

func NewProvider(cfg Config) (*Provider, error) {
	var invalid []string
	if cfg.Resource == nil {
		invalid = append(invalid, "resource is required")
	} else if err := ValidateResourceURL(cfg.Resource.String()); err != nil {
		invalid = append(invalid, fmt.Sprintf("resource: %v", err))
	}
	if cfg.Store == nil {
		invalid = append(invalid, "store is required")
	}
	if cfg.DB == nil {
		invalid = append(invalid, "database is required")
	}
	if cfg.Audits == nil {
		invalid = append(invalid, "audit service is required")
	}
	if cfg.Entitlements == nil {
		invalid = append(invalid, "entitlements service is required")
	}
	if cfg.Sessions == nil {
		invalid = append(invalid, "sessions service is required")
	}
	if len(cfg.Pepper) == 0 {
		invalid = append(invalid, "pepper is required")
	}
	if cfg.LoginPath == "" {
		invalid = append(invalid, "login path is required")
	}
	if !strings.HasPrefix(cfg.LoginPath, "/") {
		invalid = append(invalid, "login path must start with '/'")
	}
	if cfg.AuthorizeCodeLifespan < 0 || cfg.AccessTokenLifespan < 0 || cfg.RefreshTokenLifespan < 0 {
		invalid = append(invalid, "lifespans must not be negative")
	}
	if len(invalid) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalidConfig, strings.Join(invalid, "; "))
	}

	if cfg.AuthorizeCodeLifespan == 0 {
		cfg.AuthorizeCodeLifespan = DefaultAuthorizeCodeLifespan
	}
	if cfg.AccessTokenLifespan == 0 {
		cfg.AccessTokenLifespan = DefaultAccessTokenLifespan
	}
	if cfg.RefreshTokenLifespan == 0 {
		cfg.RefreshTokenLifespan = DefaultRefreshTokenLifespan
	}

	pepper := append([]byte(nil), cfg.Pepper...)
	adapter := &fositeStore{
		store:           cfg.Store,
		db:              cfg.DB,
		pepper:          pepper,
		accessLifespan:  cfg.AccessTokenLifespan,
		refreshLifespan: cfg.RefreshTokenLifespan,
		now:             func() time.Time { return time.Now().UTC() },
	}
	strategy := &opaqueStrategy{pepper: pepper}
	fositeConfig := &fosite.Config{
		AccessTokenLifespan:            cfg.AccessTokenLifespan,
		RefreshTokenLifespan:           cfg.RefreshTokenLifespan,
		AuthorizeCodeLifespan:          cfg.AuthorizeCodeLifespan,
		RefreshTokenScopes:             []string{}, // every exchange issues a refresh token
		ScopeStrategy:                  fosite.ExactScopeStrategy,
		EnforcePKCE:                    true,
		EnforcePKCEForPublicClients:    true,
		EnablePKCEPlainChallengeMethod: false,
		SendDebugMessagesToClients:     false,
		MinParameterEntropy:            fosite.MinParameterEntropy,
	}

	// Implicit, password, and client-credentials grants stay disabled by
	// never composing their factories.
	oauth2Provider := compose.Compose(
		fositeConfig,
		adapter,
		strategy,
		compose.OAuth2AuthorizeExplicitFactory,
		compose.OAuth2RefreshTokenGrantFactory,
		compose.OAuth2PKCEFactory,
		compose.OAuth2TokenRevocationFactory,
	)

	return &Provider{
		config:      cfg,
		store:       cfg.Store,
		db:          cfg.DB,
		audits:      cfg.Audits,
		pepper:      pepper,
		resource:    cfg.Resource.String(),
		fosite:      oauth2Provider,
		codeLife:    cfg.AuthorizeCodeLifespan,
		accessLife:  cfg.AccessTokenLifespan,
		refreshLife: cfg.RefreshTokenLifespan,
	}, nil
}

// Handler composes the three connector endpoints on a fresh mux.
func (p *Provider) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle(AuthorizePath, http.HandlerFunc(p.AuthorizeHTTP))
	mux.Handle(TokenPath, http.HandlerFunc(p.TokenHTTP))
	mux.Handle(RevocationPath, http.HandlerFunc(p.RevocationHTTP))
	return mux
}

// now returns the provider's evaluation instant.
func (p *Provider) now() time.Time {
	return time.Now().UTC()
}

// opaqueStrategy implements the fosite core strategy with opaque prefixed
// secrets whose keyed digests are persisted. The signature is the plaintext
// itself, so the storage adapter receives the full secret and digests it with
// the configured pepper.
type opaqueStrategy struct {
	pepper []byte
}

func (s *opaqueStrategy) AccessTokenSignature(_ context.Context, token string) string {
	return token
}

func (s *opaqueStrategy) RefreshTokenSignature(_ context.Context, token string) string {
	return token
}

func (s *opaqueStrategy) AuthorizeCodeSignature(_ context.Context, token string) string {
	return token
}

func (s *opaqueStrategy) GenerateAccessToken(ctx context.Context, _ fosite.Requester) (string, string, error) {
	return s.generate(ctx, accessTokenPrefix)
}

func (s *opaqueStrategy) GenerateRefreshToken(ctx context.Context, _ fosite.Requester) (string, string, error) {
	return s.generate(ctx, refreshTokenPrefix)
}

func (s *opaqueStrategy) GenerateAuthorizeCode(ctx context.Context, _ fosite.Requester) (string, string, error) {
	return s.generate(ctx, authorizeCodePrefix)
}

func (s *opaqueStrategy) generate(ctx context.Context, prefix string) (string, string, error) {
	plain, _, err := credential.Generate(prefix, secretEntropy, s.pepper)
	if err != nil {
		return "", "", fmt.Errorf("mcpoauth: generate %s secret: %w", prefix, err)
	}
	if flow := flowFromContext(ctx); flow != nil {
		switch prefix {
		case accessTokenPrefix:
			flow.access = plain
		case refreshTokenPrefix:
			flow.refresh = plain
		}
	}
	return plain, plain, nil
}

func (s *opaqueStrategy) ValidateAccessToken(_ context.Context, r fosite.Requester, token string) error {
	return validateOpaqueSecret(r, token, accessTokenPrefix, fosite.AccessToken)
}

func (s *opaqueStrategy) ValidateRefreshToken(_ context.Context, r fosite.Requester, token string) error {
	return validateOpaqueSecret(r, token, refreshTokenPrefix, fosite.RefreshToken)
}

func (s *opaqueStrategy) ValidateAuthorizeCode(_ context.Context, r fosite.Requester, token string) error {
	return validateOpaqueSecret(r, token, authorizeCodePrefix, fosite.AuthorizeCode)
}

// validateOpaqueSecret enforces the prefix and the hydrated session expiry.
// Storage lookups remain the authoritative revocation and binding checks.
func validateOpaqueSecret(r fosite.Requester, token, prefix string, kind fosite.TokenType) error {
	if !strings.HasPrefix(token, prefix) {
		return fosite.ErrInvalidTokenFormat.WithHintf("The token does not carry the %q prefix.", prefix)
	}
	session := r.GetSession()
	if session == nil {
		return nil
	}
	if exp := session.GetExpiresAt(kind); !exp.IsZero() && !time.Now().UTC().Before(exp) {
		return fosite.ErrTokenExpired.WithHintf("The %s expired at '%s'.", kind, exp)
	}
	return nil
}

// tokenFlow carries the presented binding and the generated secrets through
// one token endpoint request. The token handler seeds it into the context;
// the storage adapter is the only consumer.
type tokenFlow struct {
	presentedClientID    string
	presentedRedirectURI string
	presentedResource    string

	code       *AuthorizationCode
	codeClient string // public client id bound to the stored code
	access     string
	refresh    string
	issued     bool
}

type flowContextKey struct{}

func withFlow(ctx context.Context, flow *tokenFlow) context.Context {
	return context.WithValue(ctx, flowContextKey{}, flow)
}

func flowFromContext(ctx context.Context) *tokenFlow {
	flow, _ := ctx.Value(flowContextKey{}).(*tokenFlow)
	return flow
}

// fositeStore adapts the committed mcpoauth storage contract to fosite.
// Mutations always delegate to the transactional Store methods; the adapter
// adds only read-only lookups the Store interface does not expose.
type fositeStore struct {
	store           Store
	db              *sql.DB
	pepper          []byte
	accessLifespan  time.Duration
	refreshLifespan time.Duration
	now             func() time.Time
}

var (
	_ fosite.Storage                = (*fositeStore)(nil)
	_ oauth2.CoreStorage            = (*fositeStore)(nil)
	_ oauth2.TokenRevocationStorage = (*fositeStore)(nil)
	_ pkce.PKCERequestStorage       = (*fositeStore)(nil)
)

// mcpFositeClient projects a registered connector as a public OAuth client.
func mcpFositeClient(publicID string, redirects []string) *fosite.DefaultClient {
	return &fosite.DefaultClient{
		ID:            publicID,
		Public:        true,
		RedirectURIs:  redirects,
		GrantTypes:    []string{GrantTypeAuthorizationCode, GrantTypeRefreshToken},
		ResponseTypes: []string{ResponseTypeCode},
		Scopes:        SupportedScopes,
	}
}

// GetClient resolves the registered client by its public client_id.
func (s *fositeStore) GetClient(ctx context.Context, id string) (fosite.Client, error) {
	client, err := s.store.ClientByPublicID(ctx, id)
	if err != nil {
		return nil, err
	}
	wrapped := mcpFositeClient(client.ClientID, client.RedirectURIs)
	return wrapped, nil
}

// ClientAssertionJWTValid reports private_key_jwt client assertions as
// unsupported: the composed server authenticates public clients only and the
// default client authentication strategy rejects assertions before these
// methods can be consulted.
func (s *fositeStore) ClientAssertionJWTValid(context.Context, string) error {
	return errors.New("mcpoauth: client assertion JWTs are not supported")
}

// SetClientAssertionJWT mirrors ClientAssertionJWTValid.
func (s *fositeStore) SetClientAssertionJWT(context.Context, string, time.Time) error {
	return errors.New("mcpoauth: client assertion JWTs are not supported")
}

// CreateAuthorizeCodeSession is a no-op: the authorize endpoint persists the
// hashed code row itself because the row binds consent-owned fields — device,
// Studio session, narrowed scopes, and resource — that fosite's sanitized
// request cannot carry.
func (s *fositeStore) CreateAuthorizeCodeSession(context.Context, string, fosite.Requester) error {
	return nil
}

// GetAuthorizeCodeSession reconstructs the stored authorize request and
// enforces the exact presented binding: client, redirect URI, and resource.
// A mismatch surfaces as NotFound so the code is never consumed.
func (s *fositeStore) GetAuthorizeCodeSession(ctx context.Context, code string, session fosite.Session) (fosite.Requester, error) {
	flow := flowFromContext(ctx)
	if flow == nil {
		return nil, ErrMissingTokenFlow
	}
	row, err := mcpSelectCodeByDigest(ctx, s.db, credential.Digest(code, s.pepper))
	if errors.Is(err, ErrCodeNotFound) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	clientID, err := mcpSelectClientPublicID(ctx, s.db, row.ClientID)
	if err != nil {
		return nil, err
	}
	if flow.presentedClientID != clientID ||
		flow.presentedRedirectURI != row.RedirectURI ||
		flow.presentedResource != row.Resource {
		return nil, fosite.ErrNotFound
	}
	grant, err := mcpSelectGrantByOwner(ctx, s.db, row.UserID, row.ClientID, row.DeviceID)
	if errors.Is(err, ErrGrantNotFound) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if grant.RevokedAt != nil {
		return nil, fosite.ErrNotFound
	}
	flow.code = &row
	flow.codeClient = clientID
	requester := codeRequester(row, mcpFositeClient(clientID, nil), session)
	if row.ConsumedAt != nil {
		return requester, fosite.ErrInvalidatedAuthorizeCode
	}
	return requester, nil
}

// InvalidateAuthorizeCodeSession consumes the single-use code and issues the
// access and refresh token pair. Both Store operations are atomic: the code
// consumption is the exchange's linearization point and the token pair
// persists or fails together.
func (s *fositeStore) InvalidateAuthorizeCodeSession(ctx context.Context, code string) error {
	flow := flowFromContext(ctx)
	if flow == nil || flow.code == nil {
		return ErrMissingTokenFlow
	}
	now := s.now()
	binding := CodeBinding{
		ClientID:    flow.code.ClientID,
		RedirectURI: flow.presentedRedirectURI,
		Resource:    flow.presentedResource,
	}
	consumed, err := s.store.ConsumeAuthorizationCode(ctx, credential.Digest(code, s.pepper), binding, now)
	if err != nil {
		if errors.Is(err, ErrCodeUsed) {
			// A concurrent exchange consumed the code first: apply the
			// replay remedy and refuse this exchange.
			if rerr := s.revokeByRequestID(ctx, flow.code.ID, true); rerr != nil {
				return rerr
			}
			return fosite.ErrInvalidatedAuthorizeCode
		}
		return fmt.Errorf("mcpoauth: consume authorization code: %w", err)
	}
	if flow.access == "" || flow.refresh == "" {
		// The composed configuration always issues a paired token set; a
		// missing pair means the composition drifted. Fail closed.
		return errors.New("mcpoauth: token exchange without a generated token pair")
	}
	grant, err := mcpSelectGrantByOwner(ctx, s.db, consumed.UserID, consumed.ClientID, consumed.DeviceID)
	if err != nil {
		return err
	}
	if grant.RevokedAt != nil {
		return errors.New("mcpoauth: grant is no longer active")
	}
	accessID, err := mcpNewID()
	if err != nil {
		return err
	}
	refreshID, err := mcpNewID()
	if err != nil {
		return err
	}
	access := AccessToken{
		ID:        accessID,
		UserID:    consumed.UserID,
		GrantID:   grant.ID,
		ExpiresAt: now.Add(s.accessLifespan),
		CreatedAt: now,
	}
	refresh := RefreshToken{
		ID:        refreshID,
		UserID:    consumed.UserID,
		GrantID:   grant.ID,
		FamilyID:  refreshID, // family root
		ExpiresAt: now.Add(s.refreshLifespan),
		CreatedAt: now,
	}
	if err := s.store.IssueTokens(ctx,
		access, credential.Digest(flow.access, s.pepper),
		refresh, credential.Digest(flow.refresh, s.pepper)); err != nil {
		return fmt.Errorf("mcpoauth: issue tokens: %w", err)
	}
	flow.issued = true
	return nil
}

// codeRequester rebuilds the authorize request from the stored row.
func codeRequester(row AuthorizationCode, client fosite.Client, session fosite.Session) *fosite.Request {
	if session == nil {
		session = &fosite.DefaultSession{}
	}
	if def, ok := session.(*fosite.DefaultSession); ok {
		def.SetSubject(row.UserID)
	}
	session.SetExpiresAt(fosite.AuthorizeCode, row.ExpiresAt)
	form := url.Values{}
	form.Set("redirect_uri", row.RedirectURI)
	form.Set("code_challenge", row.CodeChallenge)
	form.Set("code_challenge_method", CodeChallengeMethodS256)
	form.Set("resource", row.Resource)
	return &fosite.Request{
		ID:             row.ID,
		RequestedAt:    row.CreatedAt,
		Client:         client,
		RequestedScope: fosite.Arguments(row.Scopes),
		GrantedScope:   fosite.Arguments(row.Scopes),
		Form:           form,
		Session:        session,
	}
}

// CreateAccessTokenSession and CreateRefreshTokenSession guard against
// issuance outside the completed Store transaction.
func (s *fositeStore) CreateAccessTokenSession(ctx context.Context, signature string, _ fosite.Requester) error {
	return assertIssued(ctx, signature, accessTokenPrefix)
}

func (s *fositeStore) CreateRefreshTokenSession(ctx context.Context, signature string, _ string, _ fosite.Requester) error {
	return assertIssued(ctx, signature, refreshTokenPrefix)
}

func assertIssued(ctx context.Context, signature, prefix string) error {
	flow := flowFromContext(ctx)
	if flow == nil || !flow.issued {
		return errors.New("mcpoauth: session creation outside an issued exchange")
	}
	if !strings.HasPrefix(signature, prefix) {
		return errors.New("mcpoauth: session signature does not match the issued token")
	}
	return nil
}

// GetAccessTokenSession validates the presented access token through the
// committed store contract. Every inactive state surfaces as NotFound so the
// revocation endpoint answers silently per RFC 7009.
func (s *fositeStore) GetAccessTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	info, err := s.store.AccessTokenByDigest(ctx, credential.Digest(signature, s.pepper), s.now())
	if err != nil {
		return nil, fosite.ErrNotFound
	}
	clientID, err := mcpSelectClientPublicID(ctx, s.db, info.Grant.ClientID)
	if err != nil {
		return nil, err
	}
	return tokenRequester(info.Token.ID, info.Grant, mcpFositeClient(clientID, nil), session), nil
}

// DeleteAccessTokenSession is a no-op: revocation marks rows, it never
// deletes audit-relevant state.
func (s *fositeStore) DeleteAccessTokenSession(context.Context, string) error {
	return nil
}

// GetRefreshTokenSession reconstructs the refresh session from the stored
// family row. In the refresh grant, a used or revoked token surfaces as
// ErrInactiveToken with the requester attached so fosite triggers family
// revocation on reuse. The revocation endpoint carries no token flow, so a
// used token remains revocable there — RFC 7009 revocation of a refresh
// token still invalidates its family and grant.
func (s *fositeStore) GetRefreshTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	row, err := mcpSelectRefreshByDigest(ctx, s.db, credential.Digest(signature, s.pepper))
	if errors.Is(err, ErrTokenNotFound) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	grant, err := mcpSelectGrantByID(ctx, s.db, row.GrantID, row.UserID)
	if errors.Is(err, ErrGrantNotFound) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if flow := flowFromContext(ctx); flow != nil && flow.presentedResource != "" && flow.presentedResource != grant.Resource {
		return nil, fosite.ErrNotFound
	}
	clientID, err := mcpSelectClientPublicID(ctx, s.db, grant.ClientID)
	if err != nil {
		return nil, err
	}
	requester := refreshTokenRequester(row, grant, mcpFositeClient(clientID, nil), session)
	if row.UsedAt != nil || row.RevokedAt != nil {
		if flowFromContext(ctx) != nil {
			return requester, fosite.ErrInactiveToken
		}
		return requester, nil
	}
	return requester, nil
}

// DeleteRefreshTokenSession is a no-op: family revocation marks rows.
func (s *fositeStore) DeleteRefreshTokenSession(context.Context, string) error {
	return nil
}

// RotateRefreshToken consumes the old refresh token and issues the rotated
// pair atomically through the committed store contract. Reuse races already
// revoked the family inside the store; report the inactive token.
func (s *fositeStore) RotateRefreshToken(ctx context.Context, _ string, signature string) error {
	flow := flowFromContext(ctx)
	if flow == nil {
		return ErrMissingTokenFlow
	}
	if flow.access == "" || flow.refresh == "" {
		return errors.New("mcpoauth: refresh without a generated token pair")
	}
	now := s.now()
	accessID, err := mcpNewID()
	if err != nil {
		return err
	}
	refreshID, err := mcpNewID()
	if err != nil {
		return err
	}
	access := AccessToken{ID: accessID, ExpiresAt: now.Add(s.accessLifespan), CreatedAt: now}
	refresh := RefreshToken{ID: refreshID, ExpiresAt: now.Add(s.refreshLifespan), CreatedAt: now}
	_, _, err = s.store.RotateRefreshToken(ctx,
		credential.Digest(signature, s.pepper),
		access, credential.Digest(flow.access, s.pepper),
		refresh, credential.Digest(flow.refresh, s.pepper),
		now)
	if err != nil {
		if errors.Is(err, ErrTokenReused) || errors.Is(err, ErrTokenRevoked) {
			return fosite.ErrInactiveToken
		}
		return fmt.Errorf("mcpoauth: rotate refresh token: %w", err)
	}
	flow.issued = true
	return nil
}

// refreshTokenRequester rebuilds the refresh session from the stored family
// row. The fosite request id is the family id so revocation by request id
// resolves the whole family.
func refreshTokenRequester(row RefreshToken, grant Grant, client fosite.Client, session fosite.Session) *fosite.Request {
	if session == nil {
		session = &fosite.DefaultSession{}
	}
	if def, ok := session.(*fosite.DefaultSession); ok {
		def.SetSubject(row.UserID)
	}
	session.SetExpiresAt(fosite.RefreshToken, row.ExpiresAt)
	return &fosite.Request{
		ID:             row.FamilyID,
		RequestedAt:    row.CreatedAt,
		Client:         client,
		RequestedScope: fosite.Arguments(grant.Scopes),
		GrantedScope:   fosite.Arguments(grant.Scopes),
		Form:           url.Values{},
		Session:        session,
	}
}

func tokenRequester(tokenID string, grant Grant, client fosite.Client, session fosite.Session) *fosite.Request {
	if session == nil {
		session = &fosite.DefaultSession{}
	}
	if def, ok := session.(*fosite.DefaultSession); ok {
		def.SetSubject(grant.UserID)
	}
	return &fosite.Request{
		ID:             tokenID,
		Client:         client,
		RequestedScope: fosite.Arguments(grant.Scopes),
		GrantedScope:   fosite.Arguments(grant.Scopes),
		Form:           url.Values{},
		Session:        session,
	}
}

// RevokeRefreshToken resolves the request id as a refresh family, else as the
// code whose grant backs it, else stays silent per RFC 7009.
func (s *fositeStore) RevokeRefreshToken(ctx context.Context, requestID string) error {
	return s.revokeByRequestID(ctx, requestID, false)
}

// RevokeAccessToken resolves the request id as one access token row, else as
// a refresh family, else as the code whose grant backs it.
func (s *fositeStore) RevokeAccessToken(ctx context.Context, requestID string) error {
	return s.revokeByRequestID(ctx, requestID, true)
}

func (s *fositeStore) revokeByRequestID(ctx context.Context, requestID string, includeAccess bool) error {
	if requestID == "" {
		return nil
	}
	now := s.now()
	if includeAccess {
		digest, err := mcpSelectAccessDigestByID(ctx, s.db, requestID)
		if err == nil {
			if err := s.store.RevokeAccessToken(ctx, digest, now); err != nil {
				return err
			}
			return nil
		}
		if !errors.Is(err, ErrTokenNotFound) {
			return err
		}
	}
	digest, err := mcpSelectRefreshDigestByFamily(ctx, s.db, requestID)
	if err == nil {
		if err := s.store.RevokeRefreshToken(ctx, digest, now); err != nil {
			return err
		}
		return nil
	}
	if !errors.Is(err, ErrTokenNotFound) {
		return err
	}
	code, err := mcpSelectCodeByID(ctx, s.db, requestID)
	if errors.Is(err, ErrCodeNotFound) {
		return nil // unknown request id revokes silently
	}
	if err != nil {
		return err
	}
	grant, err := mcpSelectGrantByOwner(ctx, s.db, code.UserID, code.ClientID, code.DeviceID)
	if errors.Is(err, ErrGrantNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.store.RevokeGrant(ctx, grant.ID, now)
}

// CreatePKCERequestSession is a no-op: the challenge is persisted with the
// code row, which is the only authoritative PKCE store.
func (s *fositeStore) CreatePKCERequestSession(context.Context, string, fosite.Requester) error {
	return nil
}

// GetPKCERequestSession rebuilds the PKCE request from the stored code row.
// Only S256 challenges can ever reach storage, so the method is fixed.
func (s *fositeStore) GetPKCERequestSession(ctx context.Context, code string, _ fosite.Session) (fosite.Requester, error) {
	row, err := mcpSelectCodeByDigest(ctx, s.db, credential.Digest(code, s.pepper))
	if errors.Is(err, ErrCodeNotFound) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	clientID, err := mcpSelectClientPublicID(ctx, s.db, row.ClientID)
	if err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("code_challenge", row.CodeChallenge)
	form.Set("code_challenge_method", CodeChallengeMethodS256)
	return &fosite.Request{
		ID:             row.ID,
		RequestedAt:    row.CreatedAt,
		Client:         mcpFositeClient(clientID, nil),
		RequestedScope: fosite.Arguments(row.Scopes),
		GrantedScope:   fosite.Arguments(row.Scopes),
		Form:           form,
		Session:        &fosite.DefaultSession{Subject: row.UserID},
	}, nil
}

// DeletePKCERequestSession is a no-op: the code row keeps the challenge until
// the single-use consumption governed by consumed_at.
func (s *fositeStore) DeletePKCERequestSession(context.Context, string) error {
	return nil
}

// mcpNewID generates a random RFC 4122 version 4 identifier.
func mcpNewID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("mcpoauth: generate id: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}

// mcpNewIDOrEmpty returns a fresh id, or the empty string when entropy fails
// at call sites that can fail closed on the empty value.
func mcpNewIDOrEmpty() string {
	id, err := mcpNewID()
	if err != nil {
		return ""
	}
	return id
}

// mcpQueryer abstracts *sql.DB and *sql.Tx for read helpers.
type mcpQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func mcpScanStrings(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, errors.New("mcpoauth: empty json string list")
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mcpoauth: decode json string list: %w", err)
	}
	return out, nil
}

func mcpJSONStrings(value []string) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: encode json string list: %w", err)
	}
	return raw, nil
}

func mcpSelectCodeByDigest(ctx context.Context, q mcpQueryer, digest [32]byte) (AuthorizationCode, error) {
	var (
		code       AuthorizationCode
		scopes     []byte
		deviceID   sql.NullString
		studioID   sql.NullString
		consumedAt sql.NullTime
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, user_id, client_id, redirect_uri, code_challenge, scopes, device_id, studio_session_id, resource, expires_at, consumed_at, created_at
		 FROM oauth_authorization_codes WHERE code_digest = ?`,
		digest[:]).Scan(&code.ID, &code.UserID, &code.ClientID, &code.RedirectURI, &code.CodeChallenge,
		&scopes, &deviceID, &studioID, &code.Resource, &code.ExpiresAt, &consumedAt, &code.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthorizationCode{}, ErrCodeNotFound
	}
	if err != nil {
		return AuthorizationCode{}, fmt.Errorf("mcpoauth: find authorization code: %w", err)
	}
	code.Scopes, err = mcpScanStrings(scopes)
	if err != nil {
		return AuthorizationCode{}, err
	}
	code.DeviceID = deviceID.String
	code.StudioSessionID = studioID.String
	if consumedAt.Valid {
		when := consumedAt.Time.UTC()
		code.ConsumedAt = &when
	}
	code.ExpiresAt = code.ExpiresAt.UTC()
	code.CreatedAt = code.CreatedAt.UTC()
	return code, nil
}

// mcpSelectCodeByID loads just the ownership triple of a code row, which is
// all the replay remedy needs to resolve its grant.
func mcpSelectCodeByID(ctx context.Context, q mcpQueryer, id string) (AuthorizationCode, error) {
	var (
		code     AuthorizationCode
		deviceID sql.NullString
	)
	err := q.QueryRowContext(ctx,
		`SELECT user_id, client_id, device_id FROM oauth_authorization_codes WHERE id = ?`,
		id).Scan(&code.UserID, &code.ClientID, &deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthorizationCode{}, ErrCodeNotFound
	}
	if err != nil {
		return AuthorizationCode{}, fmt.Errorf("mcpoauth: find authorization code by id: %w", err)
	}
	code.DeviceID = deviceID.String
	return code, nil
}

func mcpSelectRefreshByDigest(ctx context.Context, q mcpQueryer, digest [32]byte) (RefreshToken, error) {
	var (
		token   RefreshToken
		parent  sql.NullString
		usedAt  sql.NullTime
		revoked sql.NullTime
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, user_id, grant_id, family_id, parent_id, expires_at, used_at, revoked_at, created_at
		 FROM oauth_refresh_tokens WHERE token_digest = ?`,
		digest[:]).Scan(&token.ID, &token.UserID, &token.GrantID, &token.FamilyID, &parent,
		&token.ExpiresAt, &usedAt, &revoked, &token.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return RefreshToken{}, ErrTokenNotFound
	}
	if err != nil {
		return RefreshToken{}, fmt.Errorf("mcpoauth: find refresh token: %w", err)
	}
	token.ParentID = parent.String
	if usedAt.Valid {
		when := usedAt.Time.UTC()
		token.UsedAt = &when
	}
	if revoked.Valid {
		when := revoked.Time.UTC()
		token.RevokedAt = &when
	}
	token.ExpiresAt = token.ExpiresAt.UTC()
	token.CreatedAt = token.CreatedAt.UTC()
	return token, nil
}

func mcpScanGrant(row *sql.Row) (Grant, error) {
	var (
		grant   Grant
		scopes  []byte
		studio  sql.NullString
		revoked sql.NullTime
	)
	err := row.Scan(&grant.ID, &grant.UserID, &grant.ClientID, &grant.DeviceID, &studio, &scopes,
		&grant.Resource, &grant.CreatedAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return Grant{}, ErrGrantNotFound
	}
	if err != nil {
		return Grant{}, fmt.Errorf("mcpoauth: find grant: %w", err)
	}
	grant.StudioSessionID = studio.String
	var err2 error
	grant.Scopes, err2 = mcpScanStrings(scopes)
	if err2 != nil {
		return Grant{}, err2
	}
	if revoked.Valid {
		when := revoked.Time.UTC()
		grant.RevokedAt = &when
	}
	grant.CreatedAt = grant.CreatedAt.UTC()
	return grant, nil
}

func mcpSelectGrantByOwner(ctx context.Context, q mcpQueryer, userID, clientID, deviceID string) (Grant, error) {
	if userID == "" || clientID == "" || deviceID == "" {
		return Grant{}, ErrGrantNotFound
	}
	row := q.QueryRowContext(ctx,
		`SELECT id, user_id, client_id, device_id, studio_session_id, scopes, resource, created_at, revoked_at
		 FROM oauth_grants WHERE user_id = ? AND client_id = ? AND device_id = ?`,
		userID, clientID, deviceID)
	return mcpScanGrant(row)
}

func mcpSelectGrantByID(ctx context.Context, q mcpQueryer, grantID, userID string) (Grant, error) {
	if grantID == "" || userID == "" {
		return Grant{}, ErrGrantNotFound
	}
	row := q.QueryRowContext(ctx,
		`SELECT id, user_id, client_id, device_id, studio_session_id, scopes, resource, created_at, revoked_at
		 FROM oauth_grants WHERE id = ? AND user_id = ?`,
		grantID, userID)
	return mcpScanGrant(row)
}

func mcpSelectClientPublicID(ctx context.Context, q mcpQueryer, internalID string) (string, error) {
	var clientID string
	err := q.QueryRowContext(ctx, `SELECT client_id FROM oauth_clients WHERE id = ?`, internalID).Scan(&clientID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrClientNotFound
	}
	if err != nil {
		return "", fmt.Errorf("mcpoauth: find client: %w", err)
	}
	return clientID, nil
}

func mcpSelectAccessDigestByID(ctx context.Context, q mcpQueryer, id string) ([32]byte, error) {
	var digest []byte
	err := q.QueryRowContext(ctx, `SELECT token_digest FROM oauth_access_tokens WHERE id = ?`, id).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return [32]byte{}, ErrTokenNotFound
	}
	if err != nil {
		return [32]byte{}, fmt.Errorf("mcpoauth: find access token by id: %w", err)
	}
	if len(digest) != 32 {
		return [32]byte{}, errors.New("mcpoauth: access token digest has wrong length")
	}
	var out [32]byte
	copy(out[:], digest)
	return out, nil
}

func mcpSelectRefreshDigestByFamily(ctx context.Context, q mcpQueryer, familyID string) ([32]byte, error) {
	var digest []byte
	err := q.QueryRowContext(ctx,
		`SELECT token_digest FROM oauth_refresh_tokens WHERE family_id = ? LIMIT 1`, familyID).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return [32]byte{}, ErrTokenNotFound
	}
	if err != nil {
		return [32]byte{}, fmt.Errorf("mcpoauth: find refresh token by family: %w", err)
	}
	if len(digest) != 32 {
		return [32]byte{}, errors.New("mcpoauth: refresh token digest has wrong length")
	}
	var out [32]byte
	copy(out[:], digest)
	return out, nil
}

// mcpSaveGrantInTx upserts the durable user-to-client grant inside the
// caller's transaction, mirroring the committed store semantics: a repeated
// (user, client, device) triple updates scopes, resource, Studio session, and
// clears revocation.
func mcpSaveGrantInTx(ctx context.Context, tx *sql.Tx, grant Grant) (Grant, error) {
	if grant.ID == "" || grant.UserID == "" || grant.ClientID == "" || grant.DeviceID == "" {
		return Grant{}, errors.New("mcpoauth: grant id, user, client, and device are required")
	}
	scopes, err := mcpJSONStrings(grant.Scopes)
	if err != nil {
		return Grant{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO oauth_grants
		       (id, user_id, client_id, device_id, studio_session_id, scopes, resource, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE scopes = VALUES(scopes), resource = VALUES(resource),
		                          studio_session_id = VALUES(studio_session_id), revoked_at = NULL`,
		grant.ID, grant.UserID, grant.ClientID, grant.DeviceID, mcpNullableString(grant.StudioSessionID),
		scopes, grant.Resource, grant.CreatedAt.UTC()); err != nil {
		return Grant{}, fmt.Errorf("mcpoauth: upsert grant: %w", err)
	}
	return mcpScanGrant(tx.QueryRowContext(ctx,
		`SELECT id, user_id, client_id, device_id, studio_session_id, scopes, resource, created_at, revoked_at
		 FROM oauth_grants WHERE user_id = ? AND client_id = ? AND device_id = ?`,
		grant.UserID, grant.ClientID, grant.DeviceID))
}

func mcpNullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
