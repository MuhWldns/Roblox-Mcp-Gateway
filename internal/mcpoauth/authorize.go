package mcpoauth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	ID       string
	Name     string
	Selected bool
}

// ConsentStudio is one selectable Studio session on the consent page.
type ConsentStudio struct {
	ID       string
	DeviceID string
	StudioID string
	Selected bool
}

// ConsentCapability translates one protocol scope into a user-facing action.
type ConsentCapability struct {
	Scope       string
	Description string
}

type consentView struct {
	Action              string
	ClientName          string
	ClientID            string
	DisplayName         string
	Redirect            string
	Resource            string
	Capabilities        []ConsentCapability
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Devices             []ConsentDevice
	Studios             []ConsentStudio
	HasTarget           bool
	CSRFToken           string
	ScriptNonce         string
}

var consentTemplate = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light">
<title>Connect Roblox Studio to {{.ClientName}}?</title>
<style>
:root { color: #1a2332; background: #1a2332; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif; }
* { box-sizing: border-box; }
::selection { color: #fff; background: #c53030; }
body { min-height: 100vh; margin: 0; background: #1a2332; }
main { min-height: 100vh; display: grid; place-items: center; padding: 32px 20px; }
.consent { width: min(100%, 680px); padding: 36px; border-radius: 14px; background: #fff; box-shadow: 0 18px 50px rgba(8, 15, 27, .32); }
.brand { display: flex; align-items: baseline; justify-content: space-between; gap: 16px; margin-bottom: 30px; }
.brand strong { font-size: 18px; letter-spacing: -.02em; }
.brand span, .identity { color: #5b6873; font-size: 13px; }
h1 { max-width: 19ch; margin: 0 0 12px; font-size: 30px; line-height: 1.2; letter-spacing: -.025em; text-wrap: balance; }
.intro { max-width: 65ch; margin: 0 0 28px; color: #5b6873; line-height: 1.6; }
.client-id { display: block; margin-top: 8px; color: #5b6873; font: 12px/1.5 "SF Mono", "Fira Code", Consolas, monospace; overflow-wrap: anywhere; }
section { margin-top: 28px; }
h2 { margin: 0 0 12px; font-size: 16px; line-height: 1.35; }
.capabilities { display: grid; gap: 10px; margin: 0; padding: 0; list-style: none; }
.capabilities li { position: relative; padding-left: 24px; color: #2d3748; font-size: 14px; line-height: 1.5; }
.capabilities li::before { content: ""; position: absolute; top: 7px; left: 2px; width: 8px; height: 8px; border-radius: 50%; background: #00b06a; }
.targets { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
label { display: grid; gap: 7px; color: #2d3748; font-size: 13px; font-weight: 650; }
select { width: 100%; min-height: 46px; padding: 10px 38px 10px 12px; border: 1px solid #e2e5e9; border-radius: 7px; background: #fff; color: #1a2332; font: inherit; }
select:focus-visible, button:focus-visible { outline: 3px solid rgba(224, 59, 59, .32); outline-offset: 2px; border-color: #e03b3b; }
.limit { margin: 12px 0 0; color: #5b6873; font-size: 13px; line-height: 1.55; }
.unavailable { margin: 14px 0 0; padding: 12px 14px; border: 1px solid #f59e0b; border-radius: 7px; background: #fef3c7; color: #1a2332; font-size: 13px; line-height: 1.5; }
.actions { display: flex; justify-content: flex-end; gap: 10px; margin-top: 32px; }
button { min-height: 46px; padding: 11px 18px; border-radius: 7px; font: inherit; font-size: 14px; font-weight: 700; cursor: pointer; transition: background-color 150ms ease, border-color 150ms ease; }
.cancel { border: 1px solid #e2e5e9; background: #fff; color: #1a2332; }
.cancel:hover { background: #f1f3f5; }
.connect { border: 1px solid #e03b3b; background: #e03b3b; color: #fff; }
.connect:hover:not(:disabled) { border-color: #c53030; background: #c53030; }
.connect:disabled { border-color: #e2e5e9; background: #e2e5e9; color: #8b95a1; cursor: not-allowed; }
@media (max-width: 600px) {
  main { align-items: start; padding: 0; }
  .consent { min-height: 100vh; padding: 24px 20px; border-radius: 0; box-shadow: none; }
  .brand { margin-bottom: 24px; }
  h1 { font-size: 26px; }
  .targets { grid-template-columns: 1fr; }
  .actions { flex-direction: column-reverse; }
  button { width: 100%; }
}
</style>
</head>
<body>
<!-- THESIS: One exact Roblox Studio target, not a generic permission checklist. OWN-WORLD: RobloxKit navy field, white task surface, red action, system sans. STORY: Confirm identity, understand the fixed package, select device and Studio, connect or cancel. FIRST VIEWPORT: One centered consent surface with decision actions after the target controls. FORM: Focused authorization task inside the established dashboard system. FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance. -->
<main>
<form class="consent" method="POST" action="{{.Action}}">
<div class="brand"><strong>RobloxKit</strong><span class="identity">Signed in as {{.DisplayName}}</span></div>
<h1>Connect Roblox Studio to {{.ClientName}}?</h1>
<p class="intro"><strong>{{.ClientName}}</strong> is requesting access through RobloxKit. Review the fixed capabilities and choose the exact Studio session it may use.<code class="client-id">{{.ClientID}}</code></p>
<input type="hidden" name="response_type" value="code">
<input type="hidden" name="client_id" value="{{.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.Redirect}}">
<input type="hidden" name="scope" value="{{.Scope}}">
<input type="hidden" name="state" value="{{.State}}">
<input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
<input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
<input type="hidden" name="resource" value="{{.Resource}}">
<input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
<section aria-labelledby="capabilities-heading">
<h2 id="capabilities-heading">What {{.ClientName}} will be able to do</h2>
<ul class="capabilities">{{range .Capabilities}}<li data-scope="{{.Scope}}">{{.Description}}</li>{{end}}</ul>
</section>
<section aria-labelledby="target-heading">
<h2 id="target-heading">Choose the connection target</h2>
<div class="targets">
<label>Device<select id="device" name="device_id" required {{if not .HasTarget}}disabled{{end}}>{{range .Devices}}<option value="{{.ID}}" {{if .Selected}}selected{{end}}>{{.Name}}</option>{{end}}</select></label>
<label>Studio session<select id="studio" name="studio_session_id" required {{if not .HasTarget}}disabled{{end}}>{{range .Studios}}<option value="{{.ID}}" data-device-id="{{.DeviceID}}" {{if .Selected}}selected{{end}}>{{.StudioID}}</option>{{end}}</select></label>
</div>
<p class="limit">Access stays limited to this device and active Studio session. RobloxKit rechecks both before issuing access.</p>
<p id="unavailable" class="unavailable" {{if .HasTarget}}hidden{{end}}>Run RobloxBridge and open Roblox Studio, then reload this page to connect.</p>
</section>
<div class="actions">
<button class="cancel" type="submit" name="action" value="deny" formnovalidate>Cancel</button>
<button id="connect" class="connect" type="submit" name="action" value="approve" {{if not .HasTarget}}disabled{{end}}>Connect to {{.ClientName}}</button>
</div>
</form>
</main>
<script nonce="{{.ScriptNonce}}">
(() => {
  const device = document.getElementById("device");
  const studio = document.getElementById("studio");
  const connect = document.getElementById("connect");
  const unavailable = document.getElementById("unavailable");
  if (!device || !studio || !connect || !unavailable) return;
  const sync = () => {
    let selected = null;
    for (const option of studio.options) {
      const matches = option.dataset.deviceId === device.value;
      option.hidden = !matches;
      option.disabled = !matches;
      if (matches && selected === null) selected = option;
    }
    const currentMatches = studio.selectedOptions[0]?.dataset.deviceId === device.value;
    if (!currentMatches && selected) selected.selected = true;
    const available = selected !== null;
    studio.disabled = !available;
    connect.disabled = !available;
    unavailable.hidden = available;
  };
  device.addEventListener("change", sync);
  sync();
})();
</script>
</body>
</html>
`))

// AuthorizeHTTP serves GET /oauth/authorize by attempting the same-session
// remembered approval and otherwise rendering the consent form; POST records
// the approve or deny decision.
func (p *Provider) AuthorizeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost:
	default:
		writeProviderError(w, http.StatusMethodNotAllowed, fosite.ErrInvalidRequest.
			WithHint("The authorize endpoint accepts GET and POST requests."))
		return
	}

	webSession, ok := p.sessionUser(r)
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
		preselect, handled := p.authorizeRemembered(w, r, ar, webSession)
		if handled {
			return
		}
		p.renderConsent(w, r, ar, webSession.UserID, preselect)
		return
	}
	p.handleConsentDecision(w, r, ar, webSession)
}

// sessionUser validates the browser cookie and retains both the session and
// user identifiers needed by consent and same-session auto-approval.
func (p *Provider) sessionUser(r *http.Request) (session.Session, bool) {
	cookie, err := r.Cookie(session.CookieName)
	if err != nil || cookie.Value == "" {
		return session.Session{}, false
	}
	webSession, err := p.config.Sessions.Validate(r.Context(), cookie.Value)
	if err != nil || webSession.ID == "" || webSession.UserID == "" {
		return session.Session{}, false
	}
	return webSession, true
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

// renderConsent resolves only trusted client, identity, device, and active
// Studio data before rendering a fixed, non-editable permission package. A
// fresh pairing may preselect a target; a stale remembered consent may not.
func (p *Provider) renderConsent(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, userID string, preselect bool) {
	ctx := r.Context()
	form := ar.GetRequestForm()
	client, err := p.store.ClientByPublicID(ctx, form.Get("client_id"))
	if err != nil {
		writeProviderError(w, http.StatusBadRequest, fosite.ErrInvalidClient.WithHint("The requested OAuth 2.0 Client does not exist."))
		return
	}
	identity, err := p.config.Identities.RobloxIdentity(ctx, userID)
	if err != nil {
		writeProviderError(w, http.StatusInternalServerError, fosite.ErrServerError.WithHint("The signed-in Roblox identity could not be loaded."))
		return
	}
	capabilities, err := consentCapabilities(ar.GetRequestedScopes(), client.ClientName)
	if err != nil {
		writeProviderError(w, http.StatusBadRequest, fosite.ErrInvalidScope.WithHint("The authorize request contains an unsupported scope."))
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
	hasTarget := preselect && selectInitialConsentTarget(devices, studios)
	csrfToken, err := newCSRFToken()
	if err != nil {
		writeProviderError(w, http.StatusInternalServerError, fosite.ErrServerError.WithHint("The consent page could not be generated."))
		return
	}
	scriptNonce, err := newCSRFToken()
	if err != nil {
		writeProviderError(w, http.StatusInternalServerError, fosite.ErrServerError.WithHint("The consent page could not be generated."))
		return
	}
	http.SetCookie(w, consentCSRFCookie(csrfToken))
	displayName := identity.DisplayName
	if displayName == "" {
		displayName = identity.Subject
	}
	view := consentView{
		Action: AuthorizePath, ClientName: client.ClientName, ClientID: client.ClientID,
		DisplayName: displayName, Redirect: ar.GetRedirectURI().String(), Resource: form.Get("resource"),
		Capabilities: capabilities, Scope: strings.Join(ar.GetRequestedScopes(), " "), State: ar.GetRequestForm().Get("state"),
		CodeChallenge: ar.GetRequestForm().Get("code_challenge"), CodeChallengeMethod: ar.GetRequestForm().Get("code_challenge_method"),
		Devices: devices, Studios: studios, HasTarget: hasTarget, CSRFToken: csrfToken,
		ScriptNonce: scriptNonce,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'nonce-"+scriptNonce+"'; style-src 'self' 'unsafe-inline'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	w.WriteHeader(http.StatusOK)
	_ = consentTemplate.Execute(w, view)
}

func consentCapabilities(scopes []string, clientName string) ([]ConsentCapability, error) {
	out := make([]ConsentCapability, 0, len(scopes))
	for _, scope := range scopes {
		capability := ConsentCapability{Scope: scope}
		switch scope {
		case ScopeConnect:
			capability.Description = "Connect " + clientName + " to the selected Roblox Studio session"
		case ScopeStudioRead:
			capability.Description = "Inspect the project, scripts, instances, and Studio state"
		case ScopeStudioEdit:
			capability.Description = "Modify scripts and instances"
		case ScopeStudioExec:
			capability.Description = "Execute approved Luau operations"
		case ScopeStudioPlay:
			capability.Description = "Start, inspect, and stop playtests"
		case ScopeStudioAsset:
			capability.Description = "Search, upload, and insert supported assets"
		case ScopeStudioInput:
			capability.Description = "Send approved keyboard and mouse input during playtests"
		default:
			return nil, fmt.Errorf("mcpoauth: unsupported consent scope %q", scope)
		}
		out = append(out, capability)
	}
	return out, nil
}

func selectInitialConsentTarget(devices []ConsentDevice, studios []ConsentStudio) bool {
	for studioIndex := range studios {
		for deviceIndex := range devices {
			if devices[deviceIndex].ID == studios[studioIndex].DeviceID {
				devices[deviceIndex].Selected = true
				studios[studioIndex].Selected = true
				return true
			}
		}
	}
	return false
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
	desc := rfc.HintField
	if desc == "" {
		desc = rfc.DescriptionField
	}
	payload := struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}{
		Error:       rfc.ErrorField,
		Description: desc,
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// ConsentCSRFCookieName is exported so integration tests can locate the
// consent CSRF cookie in rendered responses.
const ConsentCSRFCookieName = consentCSRFCookieName

const (
	consentCSRFCookieName = "__Host-robloxkit_consent_csrf"
	consentCSRFTokenBytes = 32
	consentCSRFMaxAge     = 10 * time.Minute
)

// newCSRFToken generates a random URL-safe token for the consent form.
func newCSRFToken() (string, error) {
	raw := make([]byte, consentCSRFTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func consentCSRFCookie(token string) *http.Cookie {
	return &http.Cookie{
		Name:     consentCSRFCookieName,
		Value:    token,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(consentCSRFMaxAge / time.Second),
	}
}

// validateConsentCSRF compares the form token with the cookie token in constant time.
func validateConsentCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(consentCSRFCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	formToken := r.PostFormValue("csrf_token")
	if formToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(formToken)) == 1
}
