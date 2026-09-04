package mcpoauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type registrationStore struct {
	Store
	clients map[string]Client
	calls   int
	err     error
}

func newRegistrationStore() *registrationStore {
	return &registrationStore{clients: make(map[string]Client)}
}

func (s *registrationStore) RegisterClient(_ context.Context, client Client) (Client, error) {
	s.calls++
	if s.err != nil {
		return Client{}, s.err
	}
	client.ID = "stored-client-id"
	s.clients[client.ClientID] = client
	return client, nil
}

func (s *registrationStore) ClientByPublicID(_ context.Context, publicClientID string) (Client, error) {
	client, ok := s.clients[publicClientID]
	if !ok {
		return Client{}, ErrClientNotFound
	}
	return client, nil
}


func doRegistration(t *testing.T, handler http.Handler, method, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, RegistrationPath, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func TestProviderHandlerRegistersAndResolvesPublicClient(t *testing.T) {
	store := newRegistrationStore()
	handler := (&Provider{store: store}).Handler()
	body := `{
		"client_name":"Claude Desktop",
		"redirect_uris":["https://connector.example/callback?flow=mcp","https://connector.example/alternate"],
		"grant_types":["refresh_token","authorization_code"],
		"response_types":["code"],
		"token_endpoint_auth_method":"none",
		"scope":"studio:read mcp:connect"
	}`

	response := doRegistration(t, handler, http.MethodPost, "application/json; charset=utf-8", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("registration status = %d, want 201 (body %s)", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	var registered registrationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode registration response: %v", err)
	}
	if registered.ClientName != "Claude Desktop" {
		t.Fatalf("client_name = %q", registered.ClientName)
	}
	if !reflect.DeepEqual(registered.RedirectURIs, []string{"https://connector.example/callback?flow=mcp", "https://connector.example/alternate"}) {
		t.Fatalf("redirect_uris = %#v", registered.RedirectURIs)
	}
	if !reflect.DeepEqual(registered.GrantTypes, []string{GrantTypeAuthorizationCode, GrantTypeRefreshToken}) {
		t.Fatalf("grant_types = %#v", registered.GrantTypes)
	}
	if !reflect.DeepEqual(registered.ResponseTypes, []string{ResponseTypeCode}) {
		t.Fatalf("response_types = %#v", registered.ResponseTypes)
	}
	if registered.TokenEndpointAuthMethod != TokenEndpointAuthNone {
		t.Fatalf("token_endpoint_auth_method = %q", registered.TokenEndpointAuthMethod)
	}
	if registered.Scope != ScopeConnect+" "+ScopeStudioRead {
		t.Fatalf("scope = %q, want canonical supported order", registered.Scope)
	}
	const clientIDPrefix = "mcp_client_"
	if !strings.HasPrefix(registered.ClientID, clientIDPrefix) {
		t.Fatalf("client_id = %q, want opaque server-generated prefix", registered.ClientID)
	}
	entropy, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(registered.ClientID, clientIDPrefix))
	if err != nil || len(entropy) != registrationClientIDEntropy {
		t.Fatalf("client_id entropy is not %d random bytes", registrationClientIDEntropy)
	}

	persisted, err := store.ClientByPublicID(t.Context(), registered.ClientID)
	if err != nil {
		t.Fatalf("resolve registered client: %v", err)
	}
	if persisted.ClientID != registered.ClientID || persisted.ClientName != registered.ClientName || !reflect.DeepEqual(persisted.RedirectURIs, registered.RedirectURIs) {
		t.Fatalf("persisted client = %#v, response = %#v", persisted, registered)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode response fields: %v", err)
	}
	if _, exists := fields["client_secret"]; exists {
		t.Fatal("public registration response issued a client_secret")
	}
	if _, exists := fields["client_secret_expires_at"]; exists {
		t.Fatal("public registration response issued client secret metadata")
	}
}

func TestRegistrationDefaultsCanonicalPublicMetadata(t *testing.T) {
	store := newRegistrationStore()
	response := doRegistration(t, (&Provider{store: store}).Handler(), http.MethodPost, "application/json", `{
		"client_name":"Minimal Connector",
		"redirect_uris":["https://connector.example/callback"]
	}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("registration status = %d, want 201 (body %s)", response.Code, response.Body.String())
	}
	var registered registrationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &registered); err != nil {
		t.Fatalf("decode registration response: %v", err)
	}
	if !reflect.DeepEqual(registered.GrantTypes, []string{GrantTypeAuthorizationCode}) {
		t.Fatalf("default grant_types = %#v", registered.GrantTypes)
	}
	if !reflect.DeepEqual(registered.ResponseTypes, []string{ResponseTypeCode}) {
		t.Fatalf("default response_types = %#v", registered.ResponseTypes)
	}
	if registered.TokenEndpointAuthMethod != TokenEndpointAuthNone {
		t.Fatalf("default token_endpoint_auth_method = %q", registered.TokenEndpointAuthMethod)
	}
	if registered.Scope != strings.Join(SupportedScopes, " ") {
		t.Fatalf("default scope = %q", registered.Scope)
	}
}

func TestRegistrationRejectsInvalidRequestsWithoutPersistence(t *testing.T) {
	secretMarker := "must-not-leak-private-secret"
	tests := []struct {
		name        string
		method      string
		contentType string
		body        string
		wantStatus  int
	}{
		{name: "method", method: http.MethodGet, contentType: "application/json", body: `{}`, wantStatus: http.StatusMethodNotAllowed},
		{name: "missing content type", method: http.MethodPost, body: `{}`, wantStatus: http.StatusUnsupportedMediaType},
		{name: "wrong content type", method: http.MethodPost, contentType: "application/x-www-form-urlencoded", body: `{}`, wantStatus: http.StatusUnsupportedMediaType},
		{name: "malformed json", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":`, wantStatus: http.StatusBadRequest},
		{name: "multiple values", method: http.MethodPost, contentType: "application/json", body: `{}` + `{}`, wantStatus: http.StatusBadRequest},
		{name: "client supplied id", method: http.MethodPost, contentType: "application/json", body: `{"client_id":"attacker-chosen","redirect_uris":["https://connector.example/callback"]}`, wantStatus: http.StatusBadRequest},
		{name: "client secret", method: http.MethodPost, contentType: "application/json", body: `{"client_secret":"` + secretMarker + `","redirect_uris":["https://connector.example/callback"]}`, wantStatus: http.StatusBadRequest},
		{name: "private auth method", method: http.MethodPost, contentType: "application/json", body: `{"token_endpoint_auth_method":"client_secret_post","redirect_uris":["https://connector.example/callback"]}`, wantStatus: http.StatusBadRequest},
		{name: "null auth method", method: http.MethodPost, contentType: "application/json", body: `{"token_endpoint_auth_method":null,"redirect_uris":["https://connector.example/callback"]}`, wantStatus: http.StatusBadRequest},
		{name: "missing redirects", method: http.MethodPost, contentType: "application/json", body: `{}`, wantStatus: http.StatusBadRequest},
		{name: "http redirect", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["http://connector.example/callback"]}`, wantStatus: http.StatusBadRequest},
		{name: "redirect credentials", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://user:` + secretMarker + `@connector.example/callback"]}`, wantStatus: http.StatusBadRequest},
		{name: "redirect fragment", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://connector.example/callback#token"]}`, wantStatus: http.StatusBadRequest},
		{name: "duplicate redirects", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://connector.example/callback","https://connector.example/callback"]}`, wantStatus: http.StatusBadRequest},
		{name: "unsupported grant", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://connector.example/callback"],"grant_types":["client_credentials"]}`, wantStatus: http.StatusBadRequest},
		{name: "refresh without authorization code", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://connector.example/callback"],"grant_types":["refresh_token"]}`, wantStatus: http.StatusBadRequest},
		{name: "duplicate grant", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://connector.example/callback"],"grant_types":["authorization_code","authorization_code"]}`, wantStatus: http.StatusBadRequest},
		{name: "unsupported response", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://connector.example/callback"],"response_types":["token"]}`, wantStatus: http.StatusBadRequest},
		{name: "duplicate response", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://connector.example/callback"],"response_types":["code","code"]}`, wantStatus: http.StatusBadRequest},
		{name: "unknown scope", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://connector.example/callback"],"scope":"mcp:connect secret:admin"}`, wantStatus: http.StatusBadRequest},
		{name: "duplicate scope", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://connector.example/callback"],"scope":"mcp:connect mcp:connect"}`, wantStatus: http.StatusBadRequest},
		{name: "noncanonical scope separator", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://connector.example/callback"],"scope":"mcp:connect\tstudio:read"}`, wantStatus: http.StatusBadRequest},
		{name: "oversized", method: http.MethodPost, contentType: "application/json", body: `{"redirect_uris":["https://connector.example/callback"],"client_name":"` + strings.Repeat("x", int(DefaultMaxRegistrationBytes)) + `"}`, wantStatus: http.StatusRequestEntityTooLarge},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newRegistrationStore()
			response := doRegistration(t, (&Provider{store: store}).Handler(), test.method, test.contentType, test.body)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", response.Code, test.wantStatus, response.Body.String())
			}
			if store.calls != 0 {
				t.Fatalf("RegisterClient called %d times for rejected request", store.calls)
			}
			if strings.Contains(response.Body.String(), secretMarker) {
				t.Fatalf("error response leaked request secret: %s", response.Body.String())
			}
			if test.wantStatus == http.StatusMethodNotAllowed && response.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("Allow = %q, want POST", response.Header().Get("Allow"))
			}
		})
	}
}

func TestRegistrationSanitizesPersistenceErrors(t *testing.T) {
	const secretMarker = "database-password-must-not-leak"
	store := newRegistrationStore()
	store.err = errors.New(secretMarker)
	response := doRegistration(t, (&Provider{store: store}).Handler(), http.MethodPost, "application/json", `{
		"client_name":"Connector",
		"redirect_uris":["https://connector.example/callback"]
	}`)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
	body, err := io.ReadAll(response.Result().Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if strings.Contains(string(body), secretMarker) {
		t.Fatalf("persistence error leaked through response: %s", body)
	}
	if store.calls != 1 {
		t.Fatalf("RegisterClient calls = %d, want 1", store.calls)
	}
}
