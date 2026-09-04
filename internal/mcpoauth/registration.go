package mcpoauth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
)

const (
	// DefaultMaxRegistrationBytes bounds dynamic client registration bodies.
	DefaultMaxRegistrationBytes int64 = 16 << 10
	registrationClientIDPrefix        = "mcp_client_"
	registrationClientIDEntropy       = 32
	maxRegistrationRedirectURIs       = 16
)

type registrationField[T any] struct {
	value   T
	present bool
	null    bool
}

func (f *registrationField[T]) UnmarshalJSON(data []byte) error {
	f.present = true
	if string(data) == "null" {
		f.null = true
		return nil
	}
	return json.Unmarshal(data, &f.value)
}

type registrationRequest struct {
	ClientName              registrationField[string]   `json:"client_name"`
	RedirectURIs            registrationField[[]string] `json:"redirect_uris"`
	GrantTypes              registrationField[[]string] `json:"grant_types"`
	ResponseTypes           registrationField[[]string] `json:"response_types"`
	TokenEndpointAuthMethod registrationField[string]   `json:"token_endpoint_auth_method"`
	Scope                   registrationField[string]   `json:"scope"`
}

type registrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope"`
}

type registrationErrorResponse struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// RegistrationHTTP serves RFC 7591-style dynamic registration for public
// authorization-code clients. It accepts only the metadata the gateway can
// enforce and generates the opaque public client_id itself.
func (p *Provider) RegistrationHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeRegistrationError(w, http.StatusMethodNotAllowed, "invalid_request", "The registration endpoint accepts POST requests only.")
		return
	}
	if !registrationJSONContentType(r.Header.Get("Content-Type")) {
		writeRegistrationError(w, http.StatusUnsupportedMediaType, "invalid_request", "The registration request must use application/json.")
		return
	}
	if r.ContentLength > DefaultMaxRegistrationBytes {
		writeRegistrationError(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "The registration request body is too large.")
		return
	}

	var request registrationRequest
	r.Body = http.MaxBytesReader(w, r.Body, DefaultMaxRegistrationBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeRegistrationDecodeError(w, err)
		return
	}
	if err := ensureRegistrationEOF(decoder); err != nil {
		writeRegistrationDecodeError(w, err)
		return
	}

	metadata, ok := canonicalRegistration(request)
	if !ok {
		writeRegistrationError(w, http.StatusBadRequest, "invalid_client_metadata", "The client registration metadata is invalid or unsupported.")
		return
	}
	clientID, err := generateRegistrationClientID()
	if err != nil {
		writeRegistrationError(w, http.StatusInternalServerError, "server_error", "The client registration could not be completed.")
		return
	}
	registered, err := p.store.RegisterClient(r.Context(), Client{
		ClientID:     clientID,
		ClientName:   metadata.ClientName,
		RedirectURIs: append([]string(nil), metadata.RedirectURIs...),
	})
	if err != nil {
		writeRegistrationError(w, http.StatusInternalServerError, "server_error", "The client registration could not be completed.")
		return
	}

	writeRegistrationJSON(w, http.StatusCreated, registrationResponse{
		ClientID:                registered.ClientID,
		ClientName:              registered.ClientName,
		RedirectURIs:            append([]string(nil), registered.RedirectURIs...),
		GrantTypes:              metadata.GrantTypes,
		ResponseTypes:           metadata.ResponseTypes,
		TokenEndpointAuthMethod: TokenEndpointAuthNone,
		Scope:                   metadata.Scope,
	})
}

type canonicalRegistrationMetadata struct {
	ClientName    string
	RedirectURIs  []string
	GrantTypes    []string
	ResponseTypes []string
	Scope         string
}

func canonicalRegistration(request registrationRequest) (canonicalRegistrationMetadata, bool) {
	if request.ClientName.null || request.RedirectURIs.null || request.GrantTypes.null ||
		request.ResponseTypes.null || request.TokenEndpointAuthMethod.null || request.Scope.null {
		return canonicalRegistrationMetadata{}, false
	}
	if !request.RedirectURIs.present || len(request.RedirectURIs.value) == 0 || len(request.RedirectURIs.value) > maxRegistrationRedirectURIs {
		return canonicalRegistrationMetadata{}, false
	}
	if hasDuplicateStrings(request.RedirectURIs.value) {
		return canonicalRegistrationMetadata{}, false
	}
	client := Client{ClientID: "validation-placeholder", ClientName: request.ClientName.value, RedirectURIs: request.RedirectURIs.value}
	if err := ValidateClient(client); err != nil {
		return canonicalRegistrationMetadata{}, false
	}

	grantTypes, ok := canonicalGrantTypes(request.GrantTypes)
	if !ok {
		return canonicalRegistrationMetadata{}, false
	}
	responseTypes, ok := canonicalResponseTypes(request.ResponseTypes)
	if !ok {
		return canonicalRegistrationMetadata{}, false
	}
	scope, ok := canonicalRegistrationScope(request.Scope)
	if !ok {
		return canonicalRegistrationMetadata{}, false
	}
	if request.TokenEndpointAuthMethod.present && request.TokenEndpointAuthMethod.value != TokenEndpointAuthNone {
		return canonicalRegistrationMetadata{}, false
	}

	return canonicalRegistrationMetadata{
		ClientName:    request.ClientName.value,
		RedirectURIs:  append([]string(nil), request.RedirectURIs.value...),
		GrantTypes:    grantTypes,
		ResponseTypes: responseTypes,
		Scope:         scope,
	}, true
}

func canonicalGrantTypes(field registrationField[[]string]) ([]string, bool) {
	if !field.present {
		return []string{GrantTypeAuthorizationCode}, true
	}
	if len(field.value) == 0 || hasDuplicateStrings(field.value) {
		return nil, false
	}
	seen := make(map[string]bool, len(field.value))
	for _, grantType := range field.value {
		if grantType != GrantTypeAuthorizationCode && grantType != GrantTypeRefreshToken {
			return nil, false
		}
		seen[grantType] = true
	}
	if !seen[GrantTypeAuthorizationCode] {
		return nil, false
	}
	canonical := []string{GrantTypeAuthorizationCode}
	if seen[GrantTypeRefreshToken] {
		canonical = append(canonical, GrantTypeRefreshToken)
	}
	return canonical, true
}

func canonicalResponseTypes(field registrationField[[]string]) ([]string, bool) {
	if !field.present {
		return []string{ResponseTypeCode}, true
	}
	if len(field.value) != 1 || field.value[0] != ResponseTypeCode {
		return nil, false
	}
	return []string{ResponseTypeCode}, true
}

func canonicalRegistrationScope(field registrationField[string]) (string, bool) {
	if !field.present {
		return strings.Join(SupportedScopes, " "), true
	}
	if field.value == "" {
		return "", false
	}
	requested := strings.Split(field.value, " ")
	if err := ValidateScopes(requested); err != nil {
		return "", false
	}
	allowed := make(map[string]bool, len(requested))
	for _, scope := range requested {
		allowed[scope] = false
	}
	canonical := make([]string, 0, len(requested))
	for _, scope := range SupportedScopes {
		if _, requestedScope := allowed[scope]; requestedScope {
			allowed[scope] = true
			canonical = append(canonical, scope)
		}
	}
	for _, supported := range allowed {
		if !supported {
			return "", false
		}
	}
	return strings.Join(canonical, " "), true
}

func registrationJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

func ensureRegistrationEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("mcpoauth: registration body contains multiple JSON values")
		}
		return err
	}
	return nil
}

func writeRegistrationDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeRegistrationError(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "The registration request body is too large.")
		return
	}
	writeRegistrationError(w, http.StatusBadRequest, "invalid_client_metadata", "The client registration metadata is invalid or unsupported.")
}

func generateRegistrationClientID() (string, error) {
	entropy := make([]byte, registrationClientIDEntropy)
	if _, err := rand.Read(entropy); err != nil {
		return "", err
	}
	return registrationClientIDPrefix + base64.RawURLEncoding.EncodeToString(entropy), nil
}

func hasDuplicateStrings(values []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			return true
		}
		seen[value] = struct{}{}
	}
	return false
}

func writeRegistrationError(w http.ResponseWriter, status int, code, description string) {
	writeRegistrationJSON(w, status, registrationErrorResponse{Error: code, Description: description})
}

func writeRegistrationJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
