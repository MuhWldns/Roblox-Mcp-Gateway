package robloxauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidTransaction  = errors.New("robloxauth: invalid login transaction")
	ErrExpiredTransaction  = errors.New("robloxauth: expired login transaction")
	ErrProviderDenied      = errors.New("robloxauth: provider denied login")
	ErrMissingSubject      = errors.New("robloxauth: userinfo missing subject")
	ErrTooManyTransactions = errors.New("robloxauth: too many login transactions")
)

const (
	defaultTransactionTTL  = 5 * time.Minute
	defaultMaxTransactions = 1000
	randomCredentialBytes  = 32
)

type AuthorizeURL string

type LoginTransaction struct {
	State        string
	Nonce        string
	CodeVerifier string
	Binding      string
	ExpiresAt    time.Time
}

type Callback struct {
	Code    string
	State   string
	Binding string
	Error   string
}

type RobloxIdentity struct {
	Subject     string
	Username    string
	DisplayName string
}

type User struct {
	ID            string
	IdentityID    string
	RobloxSubject string
	DisplayName   string
}

type Config struct {
	ClientID        string
	ClientSecret    string
	RedirectURI     string
	ProviderBaseURL string
	Issuer          string
	JWKSURI         string
	HTTPClient      *http.Client
	TransactionTTL  time.Duration
	MaxTransactions int
	Now             func() time.Time
}

type Flow struct {
	client          providerClient
	issuer          string
	jwks            *jwksCache
	transactionTTL  time.Duration
	maxTransactions int
	now             func() time.Time
	mu              sync.Mutex
	transactions    map[string]LoginTransaction
}

func NewFlow(config Config) (*Flow, error) {
	if config.ClientID == "" {
		return nil, errors.New("robloxauth: empty client id")
	}
	if config.RedirectURI == "" {
		return nil, errors.New("robloxauth: empty redirect URI")
	}
	redirect, err := url.Parse(config.RedirectURI)
	if err != nil || !redirect.IsAbs() {
		return nil, errors.New("robloxauth: invalid redirect URI")
	}
	base := config.ProviderBaseURL
	if base == "" {
		base = defaultProviderBaseURL
	}
	baseURL, err := url.Parse(base)
	if err != nil || !baseURL.IsAbs() {
		return nil, errors.New("robloxauth: invalid provider base URL")
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	ttl := config.TransactionTTL
	if ttl == 0 {
		ttl = defaultTransactionTTL
	}
	if ttl < 0 {
		return nil, errors.New("robloxauth: negative transaction lifetime")
	}
	maxTransactions := config.MaxTransactions
	if maxTransactions < 0 {
		return nil, errors.New("robloxauth: negative transaction limit")
	}
	if maxTransactions == 0 {
		maxTransactions = defaultMaxTransactions
	}
	issuer := config.Issuer
	if issuer == "" {
		issuer = defaultIssuer
	}
	jwksURI := config.JWKSURI
	if jwksURI == "" {
		jwksURI = defaultJWKSURI
	}
	if _, err := url.Parse(jwksURI); err != nil {
		return nil, errors.New("robloxauth: invalid JWKS URI")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Flow{
		client: providerClient{
			httpClient: httpClient, baseURL: baseURL, clientID: config.ClientID,
			clientSecret: config.ClientSecret, redirectURI: config.RedirectURI,
		},
		issuer:          issuer,
		jwks:            newJWKSCache(jwksURI, httpClient, now),
		transactionTTL:  ttl,
		maxTransactions: maxTransactions,
		now:             now,
		transactions:    make(map[string]LoginTransaction),
	}, nil
}

func (f *Flow) Begin(ctx context.Context) (AuthorizeURL, LoginTransaction, error) {
	if ctx == nil {
		return "", LoginTransaction{}, errors.New("robloxauth: nil context")
	}
	if err := ctx.Err(); err != nil {
		return "", LoginTransaction{}, err
	}
	state, err := randomCredential()
	if err != nil {
		return "", LoginTransaction{}, fmt.Errorf("robloxauth: generate state: %w", err)
	}
	nonce, err := randomCredential()
	if err != nil {
		return "", LoginTransaction{}, fmt.Errorf("robloxauth: generate nonce: %w", err)
	}
	verifier, err := randomCredential()
	if err != nil {
		return "", LoginTransaction{}, fmt.Errorf("robloxauth: generate verifier: %w", err)
	}
	binding, err := randomCredential()
	if err != nil {
		return "", LoginTransaction{}, fmt.Errorf("robloxauth: generate browser binding: %w", err)
	}
	transaction := LoginTransaction{
		State: state, Nonce: nonce, CodeVerifier: verifier, Binding: binding,
		ExpiresAt: f.now().UTC().Add(f.transactionTTL),
	}
	f.mu.Lock()
	if len(f.transactions) >= f.maxTransactions {
		now := f.now().UTC()
		for storedState, stored := range f.transactions {
			if !now.Before(stored.ExpiresAt) {
				delete(f.transactions, storedState)
			}
		}
	}
	if len(f.transactions) >= f.maxTransactions {
		f.mu.Unlock()
		return "", LoginTransaction{}, ErrTooManyTransactions
	}
	f.transactions[state] = transaction
	f.mu.Unlock()

	query := url.Values{
		"response_type":         {"code"},
		"client_id":             {f.client.clientID},
		"redirect_uri":          {f.client.redirectURI},
		"scope":                 {"openid profile"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	return AuthorizeURL(f.client.endpoint("/oauth/v1/authorize") + "?" + query.Encode()), transaction, nil
}

func (f *Flow) Complete(ctx context.Context, callback Callback) (RobloxIdentity, error) {
	if ctx == nil {
		return RobloxIdentity{}, errors.New("robloxauth: nil context")
	}
	if callback.State == "" {
		return RobloxIdentity{}, ErrInvalidTransaction
	}

	f.mu.Lock()
	transaction, ok := f.transactions[callback.State]
	if ok {
		delete(f.transactions, callback.State)
	}
	f.mu.Unlock()
	if !ok {
		return RobloxIdentity{}, ErrInvalidTransaction
	}
	if callback.Binding == "" || callback.Binding != transaction.Binding {
		return RobloxIdentity{}, ErrInvalidTransaction
	}
	if !f.now().UTC().Before(transaction.ExpiresAt) {
		return RobloxIdentity{}, ErrExpiredTransaction
	}
	if callback.Error != "" {
		return RobloxIdentity{}, ErrProviderDenied
	}
	if callback.Code == "" {
		return RobloxIdentity{}, ErrInvalidTransaction
	}

	tokens, err := f.client.exchange(ctx, callback.Code, transaction.CodeVerifier)
	if err != nil {
		return RobloxIdentity{}, err
	}
	info, err := f.client.userInfo(ctx, tokens.AccessToken)
	if err != nil {
		return RobloxIdentity{}, err
	}
	if strings.TrimSpace(info.Subject) == "" {
		return RobloxIdentity{}, ErrMissingSubject
	}
	idSubject, err := f.jwks.verify(ctx, tokens.IDToken, f.issuer, f.client.clientID, transaction.Nonce)
	if err != nil {
		return RobloxIdentity{}, err
	}
	if idSubject != info.Subject {
		return RobloxIdentity{}, ErrIDTokenSubjectMismatch
	}
	username := info.Username
	if username == "" {
		username = info.LegacyUsername
	}
	return RobloxIdentity{Subject: info.Subject, Username: username, DisplayName: info.DisplayName}, nil
}

func randomCredential() (string, error) {
	raw := make([]byte, randomCredentialBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func pkceChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
