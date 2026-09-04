package mcpgateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"robloxkit/internal/audit"
	"robloxkit/internal/credential"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/mcpoauth"
)

// Audit action and reason codes. Every policy and rate denial records one
// synchronous secret-free event carrying only these safe identifiers —
// never tokens, digests, or internal error text.
const (
	auditActionDenied = "mcp.request_denied"

	auditReasonMissingBearer     = "missing_bearer"
	auditReasonInvalidToken      = "invalid_token"
	auditReasonWrongResource     = "wrong_resource"
	auditReasonEntitlement       = "entitlement_inactive"
	auditReasonOrigin            = "origin_rejected"
	auditReasonRateLimited       = "rate_limited"
	auditReasonUnknownTool       = "unknown_tool"
	auditReasonInsufficientScope = "insufficient_scope"
	auditReasonSessionInvalid    = "session_invalid"
	auditTargetConnectorGrant    = "connector_grant"
)

// Application JSON-RPC error codes in the server-reserved range, plus the
// standard invalid-params code. Every client-visible failure carries one of
// these codes with a fixed sanitized message; internal detail never leaves
// the gateway.
const (
	codeScopeDenied       = -32000
	codeReauthRequired    = -32001
	codeTargetUnavailable = -32002
	codeTimeout           = -32003
	codeCancelled         = -32004
	codeBusy              = -32005
	codeInvalidParams     = -32602 // matches jsonrpc.CodeInvalidParams
	codeInternalError     = -32603 // matches jsonrpc.CodeInternalError
)

// TokenInfo.Extra keys threading the authenticated principal from the HTTP
// admission layer into session construction. The bearer token itself is
// never retained: sessions keep only its keyed digest.
const (
	extraGrantID         = "grant_id"
	extraDeviceID        = "device_id"
	extraStudioSessionID = "studio_session_id"
	extraTokenDigest     = "token_digest" // hex-encoded keyed digest
)

// errTokenValidation reports an infrastructure failure to the SDK bearer
// middleware; its text is the entire sanitized client response.
var errTokenValidation = errors.New("token validation failed")

// errSessionInvalid reports a per-call re-authorization failure. It is
// internal: tools.go maps it to a sanitized JSON-RPC error and audits it.
var errSessionInvalid = errors.New("mcpgateway: session no longer authorized")

// Principal is the re-authorization result carried into relayed calls.
type Principal struct {
	Token mcpoauth.AccessToken
	Grant mcpoauth.Grant
}

// authenticator validates bearer tokens for /mcp admission through the
// committed OAuth store contract.
type authenticator struct {
	oauth    mcpoauth.Store
	pepper   []byte
	resource string
	audit    *audit.Service
	now      func() time.Time
}

// verify is the SDK TokenVerifier: it resolves the presented opaque token
// into user, grant, resource, and scopes. Every rejection path writes its
// sanitized reason to the append-only audit log before answering.
func (a *authenticator) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	digest := credential.Digest(token, a.pepper)
	info, err := a.oauth.AccessTokenByDigest(ctx, digest, a.now())
	switch {
	case errors.Is(err, mcpoauth.ErrTokenNotFound),
		errors.Is(err, mcpoauth.ErrTokenRevoked),
		errors.Is(err, mcpoauth.ErrTokenExpired),
		errors.Is(err, mcpoauth.ErrGrantRevoked),
		errors.Is(err, mcpoauth.ErrGrantNotFound):
		recordDenial(ctx, a.audit, auditReasonInvalidToken, "", "")
		// The error text reaches the client body verbatim, so it stays
		// exactly the sanitized sentinel.
		return nil, auth.ErrInvalidToken
	case err != nil:
		return nil, errTokenValidation
	case info.Grant.Resource != a.resource:
		recordDenial(ctx, a.audit, auditReasonWrongResource, info.Grant.UserID, info.Grant.ID)
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{
		Scopes:     append([]string(nil), info.Grant.Scopes...),
		Expiration: info.Token.ExpiresAt,
		UserID:     info.Grant.UserID,
		Extra: map[string]any{
			extraGrantID:         info.Grant.ID,
			extraDeviceID:        info.Grant.DeviceID,
			extraStudioSessionID: info.Grant.StudioSessionID,
			extraTokenDigest:     hex.EncodeToString(digest[:]),
		},
	}, nil
}

// reauthorize re-runs the full authorization pipeline for the session's
// token digest on every relayed call: token and grant state, resource
// binding, identity, device ownership, entitlement, and — for license-only
// access — the paid slot binding. The digest is the only retained token
// material, mirroring the hub's live revalidation pattern. An active trial
// covers the enrolled credential-owned device without any paid binding; the
// license-only path stays bound to its license's device slots.
func (g *Gateway) reauthorize(ctx context.Context, digest [32]byte) (Principal, error) {
	info, err := g.cfg.OAuth.AccessTokenByDigest(ctx, digest, g.cfg.Now())
	if err != nil {
		return Principal{}, fmt.Errorf("%w: token: %v", errSessionInvalid, err)
	}
	if info.Grant.Resource != g.cfg.Resource {
		return Principal{}, fmt.Errorf("%w: resource binding", errSessionInvalid)
	}
	identity, err := g.cfg.Store.UserIdentity(ctx, info.Grant.UserID)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: identity: %v", errSessionInvalid, err)
	}
	owned, err := g.cfg.Store.DeviceOwnedAndActive(ctx, info.Grant.UserID, info.Grant.DeviceID)
	if err != nil || !owned {
		return Principal{}, fmt.Errorf("%w: device ownership", errSessionInvalid)
	}
	decision, err := g.cfg.Entitlements.Authorize(ctx, entitlement.Subject{
		UserID:          info.Grant.UserID,
		Provider:        identity.Provider,
		ProviderSubject: identity.ProviderSubject,
	})
	if err != nil {
		return Principal{}, fmt.Errorf("%w: entitlement: %v", errSessionInvalid, err)
	}
	if !decision.Permits(entitlement.ActionMCP) {
		return Principal{}, fmt.Errorf("%w: entitlement window closed", errSessionInvalid)
	}
	// License-only access is bound to the paid device slots; an active trial
	// covers the enrolled credential-owned active device without a binding.
	if !decision.TrialActive {
		bound, err := g.cfg.Store.HasActiveDeviceBinding(ctx, info.Grant.UserID, info.Grant.DeviceID)
		if err != nil || !bound {
			return Principal{}, fmt.Errorf("%w: device binding", errSessionInvalid)
		}
	}
	return Principal{Token: info.Token, Grant: info.Grant}, nil
}

// entitle evaluates the entitlement window for an admitted request. It
// runs on every /mcp HTTP request, before rate limiting.
func (g *Gateway) entitle(ctx context.Context, userID string) error {
	identity, err := g.cfg.Store.UserIdentity(ctx, userID)
	if err != nil {
		return fmt.Errorf("mcpgateway: identity: %w", err)
	}
	decision, err := g.cfg.Entitlements.Authorize(ctx, entitlement.Subject{
		UserID: userID, Provider: identity.Provider, ProviderSubject: identity.ProviderSubject,
	})
	if err != nil {
		return fmt.Errorf("mcpgateway: entitlement: %w", err)
	}
	if !decision.Permits(entitlement.ActionMCP) {
		return errors.New("mcpgateway: entitlement window closed")
	}
	return nil
}

// recordDenial appends one synchronous secret-free denial event. Failures
// never block the security response itself.
func recordDenial(ctx context.Context, service *audit.Service, reason, userID, grantID string) {
	if service == nil {
		return
	}
	kind := audit.ActorSystem
	if userID != "" {
		kind = audit.ActorUser
	}
	_ = service.Record(ctx, audit.Event{
		Actor:         audit.Actor{UserID: userID, Kind: kind},
		Action:        auditActionDenied,
		CorrelationID: correlationFrom(ctx),
		Reason:        reason,
		UserID:        userID,
		TargetType:    auditTargetConnectorGrant,
		TargetID:      grantID,
	})
}

// correlationKeyType carries the per-request audit correlation id.
type correlationKeyType struct{}

var correlationKey correlationKeyType

var correlationCounter atomic.Int64

// withCorrelation seeds the request with its audit correlation id.
func withCorrelation(ctx context.Context) context.Context {
	value, err := randomHex(16)
	if err != nil {
		value = fmt.Sprintf("mcp-%d", correlationCounter.Add(1))
	}
	return context.WithValue(ctx, correlationKey, value)
}

// correlationFrom returns the ambient correlation id, or a fresh one when
// the context carries none (for example session-scoped JSON-RPC denials).
func correlationFrom(ctx context.Context) string {
	if ctx != nil {
		if value, ok := ctx.Value(correlationKey).(string); ok && value != "" {
			return value
		}
	}
	value, err := randomHex(16)
	if err != nil {
		return fmt.Sprintf("mcp-%d", correlationCounter.Add(1))
	}
	return value
}

func randomHex(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// decodeDigest parses the hex-encoded keyed token digest carried in the
// admission TokenInfo.
func decodeDigest(encoded string) ([32]byte, error) {
	var digest [32]byte
	raw, err := hex.DecodeString(encoded)
	if err != nil {
		return digest, err
	}
	if len(raw) != len(digest) {
		return digest, errors.New("mcpgateway: token digest has wrong length")
	}
	copy(digest[:], raw)
	return digest, nil
}

// hasBearerToken mirrors the SDK's bearer parse so both layers accept and
// reject the same Authorization headers.
func hasBearerToken(header string) bool {
	fields := strings.Fields(header)
	return len(fields) == 2 && strings.EqualFold(fields[0], "Bearer") && fields[1] != ""
}
