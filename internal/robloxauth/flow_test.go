package robloxauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestRobloxFlowUsesPKCEStateNonceRedirectAndExactScopes(t *testing.T) {
	fixture := newProviderFixture(t, providerUser{Sub: "1516563360", PreferredUsername: "Builderman", Name: "Builder Man"})
	flow := fixture.flow(t, nil)

	authorize, transaction, err := flow.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	parsed, err := url.Parse(string(authorize))
	if err != nil {
		t.Fatalf("parse authorize URL: %v", err)
	}
	if parsed.Path != "/oauth/v1/authorize" {
		t.Fatalf("authorize path = %q", parsed.Path)
	}
	query := parsed.Query()
	assertQueryValue(t, query, "response_type", "code")
	assertQueryValue(t, query, "client_id", "client-id")
	assertQueryValue(t, query, "redirect_uri", "https://dashboard.example.test/auth/roblox/callback")
	assertQueryValue(t, query, "scope", "openid profile")
	assertQueryValue(t, query, "code_challenge_method", "S256")
	assertQueryValue(t, query, "state", transaction.State)
	assertQueryValue(t, query, "nonce", transaction.Nonce)
	if transaction.State == "" || transaction.Nonce == "" || transaction.CodeVerifier == "" {
		t.Fatal("transaction contains an empty state, nonce, or verifier")
	}
	if query.Get("code_challenge") != pkceChallenge(transaction.CodeVerifier) {
		t.Fatal("code_challenge is not the S256 digest of the verifier")
	}
	if strings.Contains(string(authorize), transaction.CodeVerifier) {
		t.Fatal("authorize URL discloses the PKCE verifier")
	}
	_, secondTransaction, err := flow.Begin(t.Context())
	if err != nil {
		t.Fatalf("second begin: %v", err)
	}
	if secondTransaction.State == transaction.State || secondTransaction.Nonce == transaction.Nonce || secondTransaction.CodeVerifier == transaction.CodeVerifier {
		t.Fatal("login transactions reused an opaque credential")
	}
	if len(transaction.State) < 43 || len(transaction.Nonce) < 43 || len(transaction.CodeVerifier) < 43 {
		t.Fatal("login transaction credentials contain less than 256 bits of encoded entropy")
	}
	fixture.setNonce(transaction.Nonce)

	identity, err := flow.Complete(t.Context(), Callback{Code: "provider-code", State: transaction.State, Binding: transaction.Binding})
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if identity.Subject != "1516563360" || identity.Username != "Builderman" || identity.DisplayName != "Builder Man" {
		t.Fatalf("identity = %#v", identity)
	}
	fixture.assertExchange(t, transaction.CodeVerifier)
}

func TestRobloxFlowRejectsReplayAndConcurrentCallbackReuse(t *testing.T) {
	fixture := newProviderFixture(t, providerUser{Sub: "1516563360", PreferredUsername: "Builderman"})
	flow := fixture.flow(t, nil)
	_, transaction, err := flow.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	fixture.setNonce(transaction.Nonce)

	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, err := flow.Complete(context.Background(), Callback{Code: "provider-code", State: transaction.State, Binding: transaction.Binding})
			results <- err
		}()
	}
	ready.Wait()
	close(start)

	successes := 0
	invalid := 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrInvalidTransaction):
			invalid++
		default:
			t.Fatalf("complete error = %v", err)
		}
	}
	if successes != 1 || invalid != 1 {
		t.Fatalf("successes=%d invalid=%d, want 1 each", successes, invalid)
	}
	if got := fixture.tokenCalls.Load(); got != 1 {
		t.Fatalf("token exchanges = %d, want 1", got)
	}
	if _, err := flow.Complete(t.Context(), Callback{Code: "provider-code", State: transaction.State, Binding: transaction.Binding}); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("replay error = %v, want ErrInvalidTransaction", err)
	}
}

func TestRobloxFlowRejectsWrongBrowserBindingAndConsumesTransaction(t *testing.T) {
	fixture := newProviderFixture(t, providerUser{Sub: "1516563360"})
	flow := fixture.flow(t, nil)
	_, transaction, err := flow.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if transaction.Binding == "" {
		t.Fatal("begin returned empty browser binding")
	}
	if _, err := flow.Complete(t.Context(), Callback{Code: "provider-code", State: transaction.State, Binding: "attacker-binding"}); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("wrong-binding error = %v, want ErrInvalidTransaction", err)
	}
	if _, err := flow.Complete(t.Context(), Callback{Code: "provider-code", State: transaction.State, Binding: transaction.Binding}); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("wrong-binding replay error = %v, want ErrInvalidTransaction", err)
	}
	if got := fixture.tokenCalls.Load(); got != 0 {
		t.Fatalf("token exchanges = %d, want 0", got)
	}
}

func TestRobloxFlowRejectsStaleCallbackWithoutCallingProvider(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	fixture := newProviderFixture(t, providerUser{Sub: "1516563360"})
	flow := fixture.flow(t, func() time.Time { return now })
	_, transaction, err := flow.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	now = now.Add(5 * time.Minute)

	if _, err := flow.Complete(t.Context(), Callback{Code: "provider-code", State: transaction.State, Binding: transaction.Binding}); !errors.Is(err, ErrExpiredTransaction) {
		t.Fatalf("complete error = %v, want ErrExpiredTransaction", err)
	}
	if got := fixture.tokenCalls.Load(); got != 0 {
		t.Fatalf("token exchanges = %d, want 0", got)
	}
	if _, err := flow.Complete(t.Context(), Callback{Code: "provider-code", State: transaction.State, Binding: transaction.Binding}); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("stale replay error = %v, want ErrInvalidTransaction", err)
	}
}

func TestRobloxFlowRejectsAdmissionAtCapacity(t *testing.T) {
	fixture := newProviderFixture(t, providerUser{Sub: "1516563360"})
	fixture.maxTransactions = 3
	flow := fixture.flow(t, nil)
	for range 3 {
		if _, _, err := flow.Begin(t.Context()); err != nil {
			t.Fatalf("begin within capacity: %v", err)
		}
	}
	if _, _, err := flow.Begin(t.Context()); !errors.Is(err, ErrTooManyTransactions) {
		t.Fatalf("over-capacity begin error = %v, want ErrTooManyTransactions", err)
	}
}

func TestRobloxFlowReclaimsExpiredTransactionsAtCapacity(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	fixture := newProviderFixture(t, providerUser{Sub: "1516563360"})
	fixture.maxTransactions = 2
	flow := fixture.flow(t, func() time.Time { return now })
	if _, _, err := flow.Begin(t.Context()); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	if _, _, err := flow.Begin(t.Context()); err != nil {
		t.Fatalf("second begin: %v", err)
	}
	now = now.Add(5 * time.Minute)
	if _, _, err := flow.Begin(t.Context()); err != nil {
		t.Fatalf("begin after expired entries reclaimed: %v", err)
	}
	flow.mu.Lock()
	remaining := len(flow.transactions)
	flow.mu.Unlock()
	if remaining != 1 {
		t.Fatalf("stored transactions = %d, want 1 after reclaiming expired entries", remaining)
	}
}

func TestRobloxFlowConsumesProviderDeniedTransaction(t *testing.T) {
	fixture := newProviderFixture(t, providerUser{Sub: "1516563360"})
	flow := fixture.flow(t, nil)
	_, transaction, err := flow.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := flow.Complete(t.Context(), Callback{State: transaction.State, Binding: transaction.Binding, Error: "access_denied"}); !errors.Is(err, ErrProviderDenied) {
		t.Fatalf("denied callback error = %v, want ErrProviderDenied", err)
	}
	if _, err := flow.Complete(t.Context(), Callback{Code: "provider-code", State: transaction.State, Binding: transaction.Binding}); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("denied callback replay error = %v, want ErrInvalidTransaction", err)
	}
	if got := fixture.tokenCalls.Load(); got != 0 {
		t.Fatalf("token exchanges = %d, want 0", got)
	}
}

func TestRobloxFlowRejectsUserInfoWithoutSubjectAndDoesNotLeakTokens(t *testing.T) {
	fixture := newProviderFixture(t, providerUser{PreferredUsername: "NoSubject"})
	flow := fixture.flow(t, nil)
	_, transaction, err := flow.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	_, err = flow.Complete(t.Context(), Callback{Code: "provider-code", State: transaction.State, Binding: transaction.Binding})
	if !errors.Is(err, ErrMissingSubject) {
		t.Fatalf("complete error = %v, want ErrMissingSubject", err)
	}
	for _, secret := range []string{fixture.accessToken, fixture.refreshToken} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error disclosed provider token %q", secret)
		}
	}
}

func TestRobloxFlowConsumesTransactionWhenTokenExchangeFails(t *testing.T) {
	fixture := newProviderFixture(t, providerUser{Sub: "1516563360"})
	fixture.failToken = true
	flow := fixture.flow(t, nil)
	_, transaction, err := flow.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := flow.Complete(t.Context(), Callback{Code: "bad-code", State: transaction.State, Binding: transaction.Binding}); err == nil {
		t.Fatal("complete succeeded after provider token failure")
	}
	if _, err := flow.Complete(t.Context(), Callback{Code: "provider-code", State: transaction.State, Binding: transaction.Binding}); !errors.Is(err, ErrInvalidTransaction) {
		t.Fatalf("retry error = %v, want ErrInvalidTransaction", err)
	}
}

func TestRobloxFlowValidatesIDToken(t *testing.T) {
	base := func(f *providerFixture, nonce string) idTokenSpec {
		return idTokenSpec{
			sub:        f.user.Sub,
			nonce:      nonce,
			issuer:     f.issuer,
			audience:   "client-id",
			expiresAt:  time.Now().Add(5 * time.Minute),
			issuedAt:   time.Now(),
			signingKey: f.signingKey,
			kid:        f.kid,
		}
	}
	cases := []struct {
		name   string
		mint   func(f *providerFixture, nonce string) string
		wantIs func(error) bool
	}{
		{
			name: "valid",
			mint: func(f *providerFixture, nonce string) string {
				return signIDToken(base(f, nonce))
			},
			wantIs: func(err error) bool { return err == nil },
		},
		{
			name: "nonce mismatch",
			mint: func(f *providerFixture, nonce string) string {
				spec := base(f, nonce)
				spec.nonce = nonce + "-tampered"
				return signIDToken(spec)
			},
			wantIs: func(err error) bool { return errors.Is(err, ErrIDTokenNonceMismatch) },
		},
		{
			name: "wrong issuer",
			mint: func(f *providerFixture, nonce string) string {
				spec := base(f, nonce)
				spec.issuer = "https://evil.example.test/"
				return signIDToken(spec)
			},
			wantIs: func(err error) bool { return errors.Is(err, ErrIDTokenWrongIssuer) },
		},
		{
			name: "wrong audience",
			mint: func(f *providerFixture, nonce string) string {
				spec := base(f, nonce)
				spec.audience = "other-client"
				return signIDToken(spec)
			},
			wantIs: func(err error) bool { return errors.Is(err, ErrIDTokenWrongAudience) },
		},
		{
			name: "expired",
			mint: func(f *providerFixture, nonce string) string {
				spec := base(f, nonce)
				spec.expiresAt = time.Now().Add(-time.Hour)
				spec.issuedAt = time.Now().Add(-2 * time.Hour)
				return signIDToken(spec)
			},
			wantIs: func(err error) bool { return errors.Is(err, jwt.ErrTokenExpired) },
		},
		{
			name: "bad signature",
			mint: func(f *providerFixture, nonce string) string {
				otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					return ""
				}
				spec := base(f, nonce)
				spec.signingKey = otherKey
				return signIDToken(spec)
			},
			wantIs: func(err error) bool { return errors.Is(err, jwt.ErrTokenSignatureInvalid) },
		},
		{
			name: "subject mismatch",
			mint: func(f *providerFixture, nonce string) string {
				spec := base(f, nonce)
				spec.sub = "different-sub"
				return signIDToken(spec)
			},
			wantIs: func(err error) bool { return errors.Is(err, ErrIDTokenSubjectMismatch) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newProviderFixture(t, providerUser{Sub: "1516563360", PreferredUsername: "Builderman", Name: "Builder Man"})
			flow := fixture.flow(t, nil)
			_, transaction, err := flow.Begin(t.Context())
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			fixture.setNonce(transaction.Nonce)
			tokenString := tc.mint(fixture, transaction.Nonce)
			if tokenString == "" {
				t.Fatal("mint produced an empty token")
			}
			fixture.setSigner(func() string { return tokenString })
			identity, err := flow.Complete(t.Context(), Callback{Code: "provider-code", State: transaction.State, Binding: transaction.Binding})
			if !tc.wantIs(err) {
				t.Fatalf("complete error = %v", err)
			}
			if err == nil && identity.Subject != "1516563360" {
				t.Fatalf("valid identity subject = %q", identity.Subject)
			}
		})
	}
}

type providerUser struct {
	Sub               string `json:"sub"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
}

type providerFixture struct {
	server          *httptest.Server
	accessToken     string
	refreshToken    string
	failToken       bool
	tokenCalls      atomic.Int32
	signingKey      *ecdsa.PrivateKey
	kid             string
	issuer          string
	maxTransactions int
	mu              sync.Mutex
	tokenForm       url.Values
	basicUser       string
	basicPass       string
	userAuth        string
	nonce           string
	idTokenSigner   func() string
	user            providerUser
}

func newProviderFixture(t *testing.T, user providerUser) *providerFixture {
	t.Helper()
	signingKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate id_token signing key: %v", err)
	}
	fixture := &providerFixture{
		accessToken:  "provider-access-secret",
		refreshToken: "provider-refresh-secret",
		signingKey:   signingKey,
		kid:          "test-es256-key",
		user:         user,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/v1/token", fixture.handleToken)
	mux.HandleFunc("/oauth/v1/userinfo", fixture.handleUserInfo)
	mux.HandleFunc("/oauth/v1/certs", fixture.handleJWKS)
	fixture.server = httptest.NewServer(mux)
	fixture.issuer = fixture.server.URL + "/"
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *providerFixture) flow(t *testing.T, now func() time.Time) *Flow {
	t.Helper()
	flow, err := NewFlow(Config{
		ClientID:        "client-id",
		ClientSecret:    "client-secret",
		RedirectURI:     "https://dashboard.example.test/auth/roblox/callback",
		ProviderBaseURL: f.server.URL,
		Issuer:          f.issuer,
		JWKSURI:         f.server.URL + "/oauth/v1/certs",
		HTTPClient:      f.server.Client(),
		TransactionTTL:  2 * time.Minute,
		MaxTransactions: f.maxTransactions,
		Now:             now,
	})
	if err != nil {
		t.Fatalf("new flow: %v", err)
	}
	return flow
}

func (f *providerFixture) setNonce(nonce string) {
	f.mu.Lock()
	f.nonce = nonce
	f.mu.Unlock()
}

func (f *providerFixture) setSigner(signer func() string) {
	f.mu.Lock()
	f.idTokenSigner = signer
	f.mu.Unlock()
}

func (f *providerFixture) handleToken(w http.ResponseWriter, r *http.Request) {
	f.tokenCalls.Add(1)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	user, pass, _ := r.BasicAuth()
	f.mu.Lock()
	f.tokenForm = r.PostForm
	f.basicUser = user
	f.basicPass = pass
	nonce := f.nonce
	signer := f.idTokenSigner
	f.mu.Unlock()
	if f.failToken {
		http.Error(w, f.accessToken, http.StatusUnauthorized)
		return
	}
	idToken := ""
	if signer != nil {
		idToken = signer()
	} else {
		idToken = signIDToken(idTokenSpec{
			sub:        f.user.Sub,
			nonce:      nonce,
			issuer:     f.issuer,
			audience:   "client-id",
			expiresAt:  time.Now().Add(5 * time.Minute),
			issuedAt:   time.Now(),
			signingKey: f.signingKey,
			kid:        f.kid,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  f.accessToken,
		"refresh_token": f.refreshToken,
		"id_token":      idToken,
		"token_type":    "Bearer",
		"expires_in":    300,
	})
}

func (f *providerFixture) handleJWKS(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pub := &f.signingKey.PublicKey
	xBytes := make([]byte, 32)
	yBytes := make([]byte, 32)
	pub.X.FillBytes(xBytes)
	pub.Y.FillBytes(yBytes)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keys": []map[string]any{{
			"kty": "EC", "use": "sig", "alg": "ES256", "kid": f.kid,
			"crv": "P-256",
			"x":   base64.RawURLEncoding.EncodeToString(xBytes),
			"y":   base64.RawURLEncoding.EncodeToString(yBytes),
		}},
	})
}

type idTokenSpec struct {
	sub        string
	nonce      string
	issuer     string
	audience   string
	expiresAt  time.Time
	issuedAt   time.Time
	signingKey *ecdsa.PrivateKey
	kid        string
}

func signIDToken(spec idTokenSpec) string {
	claims := &idTokenClaims{
		Nonce: spec.nonce,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    spec.issuer,
			Subject:   spec.sub,
			Audience:  jwt.ClaimStrings{spec.audience},
			ExpiresAt: jwt.NewNumericDate(spec.expiresAt),
			IssuedAt:  jwt.NewNumericDate(spec.issuedAt),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	token.Header["kid"] = spec.kid
	signed, err := token.SignedString(spec.signingKey)
	if err != nil {
		return ""
	}
	return signed
}

func (f *providerFixture) handleUserInfo(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.userAuth = r.Header.Get("Authorization")
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.user)
}

func (f *providerFixture) assertExchange(t *testing.T, verifier string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	assertQueryValue(t, f.tokenForm, "grant_type", "authorization_code")
	assertQueryValue(t, f.tokenForm, "code", "provider-code")
	assertQueryValue(t, f.tokenForm, "redirect_uri", "https://dashboard.example.test/auth/roblox/callback")
	assertQueryValue(t, f.tokenForm, "code_verifier", verifier)
	if f.basicUser != "client-id" || f.basicPass != "client-secret" {
		t.Fatalf("token endpoint basic auth = %q/%q", f.basicUser, f.basicPass)
	}
	if f.userAuth != "Bearer "+f.accessToken {
		t.Fatalf("userinfo authorization = %q", f.userAuth)
	}
}

func assertQueryValue(t *testing.T, values url.Values, key, want string) {
	t.Helper()
	if got := values.Get(key); got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}
