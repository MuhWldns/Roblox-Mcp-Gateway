// Package mcpoauth implements the connector-facing OAuth 2.1 authorization
// server for the remote MCP gateway: published discovery metadata documents,
// connector client registration, and the persistence contract for
// authorization codes, grants, and opaque hashed tokens.
//
// The package never stores or receives Roblox provider tokens, and plaintext
// codes and tokens never reach storage: callers digest every secret with
// internal/credential before handing it to a Store.
package mcpoauth

import (
	"errors"
	"fmt"
	"net/url"
	"time"
)

// Connector scopes published by discovery, granted at consent, and enforced
// per tools/call.
const (
	ScopeConnect     = "mcp:connect"
	ScopeStudioRead  = "studio:read"
	ScopeStudioEdit  = "studio:edit"
	ScopeStudioExec  = "studio:execute"
	ScopeStudioPlay  = "studio:playtest"
	ScopeStudioAsset = "studio:asset"
	ScopeStudioInput = "studio:input"
)

// SupportedScopes lists every scope the gateway can grant, in display order.
var SupportedScopes = []string{
	ScopeConnect,
	ScopeStudioRead,
	ScopeStudioEdit,
	ScopeStudioExec,
	ScopeStudioPlay,
	ScopeStudioAsset,
	ScopeStudioInput,
}

// Storage and fetch errors. Implementations return these sentinels (directly
// or wrapped) so callers can classify outcomes without string matching.
var (
	// ErrInvalidClient reports a rejected connector client registration.
	ErrInvalidClient = errors.New("mcpoauth: invalid client registration")
	// ErrInvalidClientID reports a client_id that is not an absolute https URL.
	ErrInvalidClientID = errors.New("mcpoauth: client id must be an absolute https URL")
	// ErrInvalidMetadata reports a malformed Client ID Metadata Document.
	ErrInvalidMetadata = errors.New("mcpoauth: invalid client metadata document")
	// ErrUnsafeFetch reports a fetch-policy violation: private address,
	// non-HTTPS redirect, redirect limit, content type, or size limit.
	ErrUnsafeFetch = errors.New("mcpoauth: unsafe client metadata fetch")

	// ErrClientNotFound reports an unknown public client_id.
	ErrClientNotFound = errors.New("mcpoauth: client not found")

	// ErrCodeNotFound reports an unknown authorization code digest.
	ErrCodeNotFound = errors.New("mcpoauth: authorization code not found")
	// ErrCodeUsed reports replay of a consumed single-use authorization code.
	ErrCodeUsed = errors.New("mcpoauth: authorization code already used")
	// ErrCodeExpired reports an authorization code past its expiry.
	ErrCodeExpired = errors.New("mcpoauth: authorization code expired")
	// ErrCodeBinding reports a resource, client, or redirect URI mismatch at
	// code consumption.
	ErrCodeBinding = errors.New("mcpoauth: authorization code binding mismatch")

	// ErrGrantNotFound reports an unknown grant identifier.
	ErrGrantNotFound = errors.New("mcpoauth: grant not found")
	// ErrGrantRevoked reports an access token presented under a revoked grant.
	ErrGrantRevoked = errors.New("mcpoauth: grant revoked")

	// ErrTokenNotFound reports an unknown token digest.
	ErrTokenNotFound = errors.New("mcpoauth: token not found")
	// ErrTokenExpired reports a token past its expiry.
	ErrTokenExpired = errors.New("mcpoauth: token expired")
	// ErrTokenRevoked reports a revoked token.
	ErrTokenRevoked = errors.New("mcpoauth: token revoked")
	// ErrTokenReused reports reuse of an already-rotated refresh token, which
	// revokes its entire family.
	ErrTokenReused = errors.New("mcpoauth: refresh token reuse detected")
)

// Client is a registered connector OAuth client such as ChatGPT or Claude.
// ID is the internal row identifier; ClientID is the public client_id used
// in authorization requests.
type Client struct {
	ID           string
	ClientID     string
	ClientName   string
	RedirectURIs []string
	CreatedAt    time.Time
}

// CodeBinding is the exact-match triple a presented authorization code must
// carry at consumption: the internal client, the exact redirect URI, and
// the resource the code was issued for.
type CodeBinding struct {
	ClientID    string
	RedirectURI string
	Resource    string
}

// AuthorizationCode is a single-use, hashed authorization code bound to one
// user, client, redirect URI, PKCE challenge, scopes, device, optional
// Studio session, and resource.
type AuthorizationCode struct {
	ID              string
	UserID          string
	ClientID        string
	RedirectURI     string
	CodeChallenge   string
	Scopes          []string
	DeviceID        string
	StudioSessionID string
	Resource        string
	ExpiresAt       time.Time
	ConsumedAt      *time.Time
	CreatedAt       time.Time
}

// Grant is the durable user-to-connector authorization: user, client, exact
// scopes, resource, target device, and optional Studio session.
type Grant struct {
	ID              string
	UserID          string
	ClientID        string
	DeviceID        string
	StudioSessionID string
	Scopes          []string
	Resource        string
	RevokedAt       *time.Time
	CreatedAt       time.Time
}

// AccessToken references one hashed opaque bearer token issued under a grant.
type AccessToken struct {
	ID        string
	UserID    string
	GrantID   string
	ExpiresAt time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

// RefreshToken references one hashed opaque refresh token inside a rotation
// family. FamilyID identifies the family and equals the root token's id;
// ParentID is the token this one rotated from, empty for the family root.
type RefreshToken struct {
	ID        string
	UserID    string
	GrantID   string
	FamilyID  string
	ParentID  string
	ExpiresAt time.Time
	UsedAt    *time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

// AccessTokenInfo pairs a validated access token with the grant that issued
// it, carrying the scopes and resource every /mcp request is checked against.
type AccessTokenInfo struct {
	Token AccessToken
	Grant Grant
}

// Column and identifier limits matching the MySQL schema.
const (
	maxClientIDLength      = 255
	maxClientNameLength    = 255
	maxRedirectURILength   = 2048
	maxResourceURLLength   = 2048
	maxCodeChallengeLength = 128
	maxScopeTokenLength    = 255
)

// ValidateClient verifies a connector client registration: a non-empty
// client_id, an optional bounded name, and at least one absolute HTTPS
// redirect URI.
func ValidateClient(client Client) error {
	if client.ClientID == "" {
		return fmt.Errorf("%w: client_id is required", ErrInvalidClient)
	}
	if len(client.ClientID) > maxClientIDLength {
		return fmt.Errorf("%w: client_id exceeds %d characters", ErrInvalidClient, maxClientIDLength)
	}
	if len(client.ClientName) > maxClientNameLength {
		return fmt.Errorf("%w: client_name exceeds %d characters", ErrInvalidClient, maxClientNameLength)
	}
	return ValidateRedirectURIs(client.RedirectURIs)
}

// ValidateRedirectURIs verifies that uris contains at least one absolute
// HTTPS URL without userinfo or fragment.
func ValidateRedirectURIs(uris []string) error {
	if len(uris) == 0 {
		return fmt.Errorf("%w: at least one redirect URI is required", ErrInvalidClient)
	}
	for _, uri := range uris {
		if err := validateHTTPSURL(uri, "redirect uri", maxRedirectURILength, true); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidClient, err)
		}
	}
	return nil
}

// ValidateRedirectURI verifies that uri is an absolute HTTPS URL without
// userinfo or fragment. Unlike ValidateRedirectURIs it reports plain errors
// for callers validating a single presented value such as an authorization
// code's redirect binding.
func ValidateRedirectURI(uri string) error {
	return validateHTTPSURL(uri, "redirect uri", maxRedirectURILength, true)
}

// ValidateResourceURL verifies a resource indicator: an absolute HTTPS URL
// without query, fragment, or userinfo.
func ValidateResourceURL(resource string) error {
	return validateHTTPSURL(resource, "resource", maxResourceURLLength, false)
}

// ValidateScopes verifies a non-empty list of distinct RFC 6749 scope tokens.
func ValidateScopes(scopes []string) error {
	if len(scopes) == 0 {
		return errors.New("mcpoauth: at least one scope is required")
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if !validateScopeToken(scope) {
			return fmt.Errorf("mcpoauth: invalid scope %q", scope)
		}
		if _, duplicate := seen[scope]; duplicate {
			return fmt.Errorf("mcpoauth: duplicate scope %q", scope)
		}
		seen[scope] = struct{}{}
	}
	return nil
}

// validateScopeToken implements the RFC 6749 scope-token charset: printable
// ASCII excluding space, double quote, and backslash.
func validateScopeToken(scope string) bool {
	if scope == "" || len(scope) > maxScopeTokenLength {
		return false
	}
	for _, r := range scope {
		if r == '"' || r == '\\' || r <= 0x20 || r >= 0x7f {
			return false
		}
	}
	return true
}

// validateHTTPSURL checks that raw is an absolute HTTPS URL of bounded
// length without userinfo or fragment; allowQuery controls the query rule.
func validateHTTPSURL(raw, name string, maxLength int, allowQuery bool) error {
	if raw == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(raw) > maxLength {
		return fmt.Errorf("%s exceeds %d characters", name, maxLength)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL: %v", name, err)
	}
	if parsed.Scheme != "https" || !parsed.IsAbs() || parsed.Host == "" {
		return fmt.Errorf("%s must be an absolute https URL", name)
	}
	if parsed.User != nil {
		return fmt.Errorf("%s must not carry credentials", name)
	}
	if !allowQuery && parsed.RawQuery != "" {
		return fmt.Errorf("%s must not carry a query", name)
	}
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return fmt.Errorf("%s must not carry a fragment", name)
	}
	return nil
}
