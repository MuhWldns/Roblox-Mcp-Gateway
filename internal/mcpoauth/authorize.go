package mcpoauth

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"

	"github.com/ory/fosite"

	"robloxkit/internal/session"
)

// authorizeEchoParams are the authorize request parameters the consent form
// echoes verbatim so the approval decision replays the original request.
var authorizeEchoParams = []string{
	"response_type",
	"client_id",
	"redirect_uri",
	"scope",
	"state",
	"code_challenge",
	"code_challenge_method",
	"resource",
}

// ConsentDevice is one selectable target device on the consent page.
type ConsentDevice struct {
	ID   string
	Name string
}

// ConsentStudio is one selectable Studio session on the consent page.
type ConsentStudio struct {
	ID       string
	DeviceID string
	StudioID string
}

type consentView struct {
	Action              string
	ClientName          string
	ClientID            string
	Redirect            string
	Resource            string
	Scopes              []string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Devices             []ConsentDevice
	Studios             []ConsentStudio
}

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Authorize {{.ClientName}}</title>
</head>
<body>
<h1>Authorize {{.ClientName}}</h1>
<p>{{.ClientID}} requests access to <code>{{.Resource}}</code>.</p>
<form method="POST" action="{{.Action}}">
<input type="hidden" name="response_type" value="code">
<input type="hidden" name="client_id" value="{{.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.Redirect}}">
<input type="hidden" name="scope" value="{{.Scope}}">
<input type="hidden" name="state" value="{{.State}}">
<input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
<input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
<input type="hidden" name="resource" value="{{.Resource}}">
<fieldset>
<legend>Scopes</legend>
{{range .Scopes}}<label><input type="checkbox" name="grant" value="{{.}}" checked> {{.}}</label><br>
{{end}}</fieldset>
<label>Device
<select name="device_id">
{{range .Devices}}<option value="{{.ID}}">{{.Name}}</option>
{{end}}</select>
</label>
<label>Studio session
<select name="studio_session_id">
<option value="">None</option>
{{range .Studios}}<option value="{{.ID}}">{{.StudioID}} ({{.DeviceID}})</option>
{{end}}</select>
</label>
<button type="submit" name="action" value="approve">Approve</button>
<button type="submit" name="action" value="deny">Deny</button>
</form>
</body>
</html>
`))

// AuthorizeHTTP serves GET /oauth/authorize by rendering the consent form and
// POST /oauth/authorize by recording the approve or deny decision.
func (p *Provider) AuthorizeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		writeProviderError(w, http.StatusMethodNotAllowed, fosite.ErrInvalidRequest.
			WithHint("The authorize endpoint accepts GET and POST requests."))
		return
	}

	userID, ok := p.sessionUser(r)
	if !ok {
		p.redirectLogin(w, r)
		return
	}

	ar, err := p.fosite.NewAuthorizeRequest(r.Context(), r)
	if err != nil {
		p.fosite.WriteAuthorizeError(r.Context(), w, ar, err)
		return
	}
	if err := p.checkAuthorizeRequest(ar); err != nil {
		writeProviderError(w, http.StatusBadRequest, err)
		return
	}

	if r.Method == http.MethodGet {
		p.renderConsent(w, r, ar, userID)
		return
	}
	p.handleConsentDecision(w, r, ar, userID)
}

// sessionUser validates the browser session cookie and returns its user id.
func (p *Provider) sessionUser(r *http.Request) (string, bool) {
	cookie, err := r.Cookie(session.CookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	webSession, err := p.config.Sessions.Validate(r.Context(), cookie.Value)
	if err != nil || webSession.UserID == "" {
		return "", false
	}
	return webSession.UserID, true
}

// redirectLogin sends unauthenticated authorize requests to the login page
// with the full original request URL as the "next" parameter, preserving
// state, client, redirect, and PKCE parameters for the resumed flow.
func (p *Provider) redirectLogin(w http.ResponseWriter, r *http.Request) {
	target := url.URL{Path: p.config.LoginPath}
	query := url.Values{}
	query.Set("next", r.URL.String())
	target.RawQuery = query.Encode()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Location", target.String())
	w.WriteHeader(http.StatusSeeOther)
}

// checkAuthorizeRequest applies the gateway policy layer on top of fosite's
// parse: the resource indicator must match the protected /mcp resource
// exactly and PKCE must use S256. These checks run before any consent
// interaction, so a violating request never reaches persistence.
func (p *Provider) checkAuthorizeRequest(ar fosite.AuthorizeRequester) error {
	form := ar.GetRequestForm()
	if err := ValidateResourceURL(form.Get("resource")); err != nil {
		return fosite.ErrInvalidRequest.WithHintf("The 'resource' parameter must be an absolute https URL: %v.", err)
	}
	if form.Get("resource") != p.resource {
		return fosite.ErrInvalidRequest.WithHintf("The 'resource' parameter must equal %q.", p.resource)
	}
	if form.Get("code_challenge") == "" {
		return fosite.ErrInvalidRequest.WithHint("Clients must include a code_challenge when performing the authorize code flow.")
	}
	if form.Get("code_challenge_method") != CodeChallengeMethodS256 {
		return fosite.ErrInvalidRequest.WithHint("Clients must use code_challenge_method=S256; plain is not allowed.")
	}
	if len(form.Get("code_challenge")) > maxCodeChallengeLength {
		return fosite.ErrInvalidRequest.WithHintf("The code_challenge exceeds %d characters.", maxCodeChallengeLength)
	}
	return nil
}

// renderConsent renders the approval form for a parsed authorize request.
func (p *Provider) renderConsent(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, userID string) {
	ctx := r.Context()
	form := ar.GetRequestForm()
	client, err := p.store.ClientByPublicID(ctx, form.Get("client_id"))
	if err != nil {
		writeProviderError(w, http.StatusBadRequest, fosite.ErrInvalidClient.WithHint("The requested OAuth 2.0 Client does not exist."))
		return
	}
	devices, err := mcpSelectDevices(ctx, p.db, userID)
	if err != nil {
		writeProviderError(w, http.StatusInternalServerError, fosite.ErrServerError.WithHint("The consent page could not load devices."))
		return
	}
	studios, err := mcpSelectStudioSessions(ctx, p.db, userID)
	if err != nil {
		writeProviderError(w, http.StatusInternalServerError, fosite.ErrServerError.WithHint("The consent page could not load Studio sessions."))
		return
	}
	view := consentView{
		Action:              AuthorizePath,
		ClientName:          client.ClientName,
		ClientID:            client.ClientID,
		Redirect:            form.Get("redirect_uri"),
		Resource:            form.Get("resource"),
		Scopes:              ar.GetRequestedScopes(),
		Scope:               form.Get("scope"),
		State:               form.Get("state"),
		CodeChallenge:       form.Get("code_challenge"),
		CodeChallengeMethod: form.Get("code_challenge_method"),
		Devices:             devices,
		Studios:             studios,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	if err := consentTemplate.Execute(w, view); err != nil {
		// The response has started; the header and partial body already sent.
		_ = err
	}
}

// writeProviderError emits a self-produced error page. It is used for
// failures that must never redirect: invalid resource indicators, PKCE
// violations, and internal persistence failures.
func writeProviderError(w http.ResponseWriter, status int, err error) {
	rfc := fosite.ErrorToRFC6749Error(err)
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	payload := struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}{
		Error:       rfc.ErrorField,
		Description: rfc.DescriptionField,
	}
	_ = json.NewEncoder(w).Encode(payload)
}
