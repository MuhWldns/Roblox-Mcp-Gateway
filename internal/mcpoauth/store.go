package mcpoauth

import (
	"context"
	"time"
)

// Store persists connector OAuth clients, authorization codes, grants, and
// tokens. Implementations store only keyed digests — never plaintext
// secrets — and perform each mutation in a single transaction.
type Store interface {
	// RegisterClient idempotently registers a connector client by its public
	// client_id, updating the display name and redirect URIs on change.
	RegisterClient(ctx context.Context, client Client) (Client, error)

	// ClientByPublicID returns the registered client for a public client_id.
	ClientByPublicID(ctx context.Context, publicClientID string) (Client, error)

	// SaveAuthorizationCode persists a new single-use authorization code
	// digest together with its exact resource, client, and redirect binding.
	SaveAuthorizationCode(ctx context.Context, code AuthorizationCode, digest [32]byte) error

	// ConsumeAuthorizationCode atomically verifies the binding, rejects
	// consumed or expired codes, marks the code consumed, and returns it.
	// A binding mismatch never consumes the code.
	ConsumeAuthorizationCode(ctx context.Context, digest [32]byte, binding CodeBinding, now time.Time) (AuthorizationCode, error)

	// SaveGrant persists the durable user-to-client authorization, reusing
	// and updating the existing row when (user, client, device) repeats.
	SaveGrant(ctx context.Context, grant Grant) (Grant, error)

	// RevokeGrant revokes a grant together with every access and refresh
	// token issued under it, in one transaction. Revoking an unknown grant
	// fails with ErrGrantNotFound; revoking twice succeeds.
	RevokeGrant(ctx context.Context, grantID string, now time.Time) error

	// IssueTokens persists a new access token and the root of a new refresh
	// token family in one transaction. Either both rows persist or neither.
	IssueTokens(ctx context.Context, access AccessToken, accessDigest [32]byte, refresh RefreshToken, refreshDigest [32]byte) error

	// RotateRefreshToken atomically marks an active refresh token used and
	// issues its replacement access token and child refresh token in the same
	// family. Presenting an already-used or revoked refresh token revokes the
	// entire family, its grant, and the grant's access tokens.
	// The stored access and refresh rows inherit user, grant, family, and
	// parent identity from the consumed parent; incoming values are ignored.
	RotateRefreshToken(ctx context.Context, oldDigest [32]byte, access AccessToken, accessDigest [32]byte, refresh RefreshToken, refreshDigest [32]byte, now time.Time) (AccessToken, RefreshToken, error)

	// AccessTokenByDigest returns the token and its grant for a valid,
	// unexpired, unrevoked access token digest whose grant is still live.
	AccessTokenByDigest(ctx context.Context, digest [32]byte, now time.Time) (AccessTokenInfo, error)

	// RevokeAccessToken revokes one access token by digest. Unknown digests
	// succeed silently, matching RFC 7009.
	RevokeAccessToken(ctx context.Context, digest [32]byte, now time.Time) error

	// RevokeRefreshToken revokes a refresh token's whole family, its grant,
	// and the grant's access tokens in one transaction. Unknown digests
	// succeed silently, matching RFC 7009.
	RevokeRefreshToken(ctx context.Context, digest [32]byte, now time.Time) error
}
