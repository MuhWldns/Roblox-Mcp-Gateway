package robloxauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	defaultIssuer  = "https://apis.roblox.com/oauth/"
	defaultJWKSURI = "https://apis.roblox.com/oauth/v1/certs"
	jwksCacheTTL   = 5 * time.Minute
)

var (
	ErrIDTokenMissing         = errors.New("robloxauth: id_token missing")
	ErrIDTokenNonceMismatch   = errors.New("robloxauth: id_token nonce mismatch")
	ErrIDTokenWrongIssuer     = errors.New("robloxauth: id_token issuer mismatch")
	ErrIDTokenWrongAudience   = errors.New("robloxauth: id_token audience mismatch")
	ErrIDTokenMissingSubject  = errors.New("robloxauth: id_token missing subject")
	ErrIDTokenSubjectMismatch = errors.New("robloxauth: id_token subject mismatch")
	ErrJWKSUnavailable        = errors.New("robloxauth: JWKS unavailable")
	ErrJWKSNoMatchingKey      = errors.New("robloxauth: JWKS lacks a signing key for the token")
)

// idTokenClaims is the subset of the provider's signed id_token consumed for
// binding. Nonce is provider-supplied and checked for exact equality with the
// transaction nonce.
type idTokenClaims struct {
	Nonce string `json:"nonce"`
	jwt.RegisteredClaims
}

type jsonWebKey struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type jwksDocument struct {
	Keys []jsonWebKey `json:"keys"`
}

// jwksCache holds the provider's signing keys for a short TTL and resolves the
// verification key selected by a token's kid header. The configured JWKS
// endpoint is the only trusted source of keys.
type jwksCache struct {
	uri    string
	client *http.Client
	now    func() time.Time

	mu    sync.Mutex
	doc   *jwksDocument
	until time.Time
}

func newJWKSCache(uri string, client *http.Client, now func() time.Time) *jwksCache {
	return &jwksCache{uri: uri, client: client, now: now}
}

// verify cryptographically validates an id_token and returns its subject.
// Signature verification is ES256-only against the configured JWKS; exp is
// required and iat must not be in the future. Issuer, audience, nonce, and
// subject presence are binding-correctness checks performed here.
func (c *jwksCache) verify(ctx context.Context, idToken, issuer, audience, nonce string) (string, error) {
	if strings.TrimSpace(idToken) == "" {
		return "", ErrIDTokenMissing
	}
	doc, err := c.document(ctx)
	if err != nil {
		return "", err
	}
	claims := &idTokenClaims{}
	_, err = jwt.ParseWithClaims(idToken, claims, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		key, ok := selectJWK(doc.Keys, kid)
		if !ok {
			return nil, ErrJWKSNoMatchingKey
		}
		pub, err := key.publicKey()
		if err != nil {
			return nil, err
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil {
		return "", fmt.Errorf("robloxauth: id_token: %w", err)
	}
	if claims.Issuer != issuer {
		return "", ErrIDTokenWrongIssuer
	}
	audienceMatch := false
	for _, aud := range claims.Audience {
		if aud == audience {
			audienceMatch = true
			break
		}
	}
	if !audienceMatch {
		return "", ErrIDTokenWrongAudience
	}
	if claims.Nonce != nonce {
		return "", ErrIDTokenNonceMismatch
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return "", ErrIDTokenMissingSubject
	}
	return claims.Subject, nil
}

// document returns the cached key set, refreshing it at most once per TTL and
// bounding the response size.
func (c *jwksCache) document(ctx context.Context) (*jwksDocument, error) {
	c.mu.Lock()
	if c.doc != nil && c.now().Before(c.until) {
		defer c.mu.Unlock()
		return c.doc, nil
	}
	c.mu.Unlock()

	requestContext, cancel := context.WithTimeout(ctx, providerRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestContext, http.MethodGet, c.uri, nil)
	if err != nil {
		return nil, ErrJWKSUnavailable
	}
	req.Header.Set("Accept", "application/json")
	response, err := c.client.Do(req)
	if err != nil {
		return nil, ErrJWKSUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxProviderResponse))
		return nil, ErrJWKSUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxProviderResponse+1))
	if err != nil {
		return nil, ErrJWKSUnavailable
	}
	if len(body) > maxProviderResponse {
		return nil, ErrJWKSUnavailable
	}
	var doc jwksDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, ErrJWKSUnavailable
	}
	if len(doc.Keys) == 0 {
		return nil, ErrJWKSUnavailable
	}

	c.mu.Lock()
	c.doc = &doc
	c.until = c.now().Add(jwksCacheTTL)
	c.mu.Unlock()
	return &doc, nil
}

func selectJWK(keys []jsonWebKey, kid string) (jsonWebKey, bool) {
	for _, key := range keys {
		if key.Kid == kid {
			return key, true
		}
	}
	return jsonWebKey{}, false
}

func (k jsonWebKey) publicKey() (*ecdsa.PublicKey, error) {
	if k.Kty != "EC" || k.Crv != "P-256" {
		return nil, ErrJWKSUnavailable
	}
	if k.Alg != "" && k.Alg != "ES256" {
		return nil, ErrJWKSUnavailable
	}
	if k.X == "" || k.Y == "" {
		return nil, ErrJWKSUnavailable
	}
	xBytes, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, ErrJWKSUnavailable
	}
	yBytes, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil {
		return nil, ErrJWKSUnavailable
	}
	if len(xBytes) != 32 || len(yBytes) != 32 {
		return nil, ErrJWKSUnavailable
	}
	return &ecdsa.PublicKey{
		Curve: elliptic.P256(),
		X:     new(big.Int).SetBytes(xBytes),
		Y:     new(big.Int).SetBytes(yBytes),
	}, nil
}
