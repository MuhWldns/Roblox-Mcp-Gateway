package mcpoauth_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"robloxkit/internal/audit"
	"robloxkit/internal/credential"
	"robloxkit/internal/mcpoauth"
)

// failingAuditStore simulates audit persistence failure so tests can prove
// the grant and its audit event commit as one transaction.
type failingAuditStore struct{}

func (failingAuditStore) Append(context.Context, audit.Event) error {
	return errors.New("audit store unavailable")
}

func (failingAuditStore) AppendInTx(context.Context, *sql.Tx, audit.Event) error {
	return errors.New("audit store unavailable")
}

// consentGrant reads the single grant row the fixture tests expect.
func (fx *mcpFixture) consentGrant(t *testing.T) (id, deviceID, studioID string, scopes []string) {
	t.Helper()
	var rawScopes []byte
	var studio sql.NullString
	if err := fx.db.QueryRowContext(t.Context(),
		`SELECT id, device_id, studio_session_id, scopes FROM oauth_grants`).
		Scan(&id, &deviceID, &studio, &rawScopes); err != nil {
		t.Fatalf("select consent grant: %v", err)
	}
	if err := json.Unmarshal(rawScopes, &scopes); err != nil {
		t.Fatalf("decode grant scopes: %v", err)
	}
	return id, deviceID, studio.String, scopes
}

func TestConsentFormShowsScopesDevicesAndEchoesParams(t *testing.T) {
	fx := newMcpFixture(t, nil)
	cookie := fx.sessionCookie(t, fx.userID)
	q := fx.authorizeQuery(func(v url.Values) { v.Set("state", "form-state-12345678") })

	resp := fx.authorizeGet(t, q, cookie)
	body := readAll(t, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("consent form status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("consent content type = %q, want text/html", ct)
	}

	for _, want := range []string{
		"Test Connector",
		"mcp:connect",
		"studio:read",
		"studio:edit",
		"Primary Workstation",
		`name="state"`,
		`value="form-state-12345678"`,
		`name="redirect_uri"`,
		`value="` + mcpTestRedirect + `"`,
		`name="resource"`,
		`value="` + mcpTestResource + `"`,
		`name="code_challenge"`,
		`value="` + mcpTestChallenge + `"`,
		`name="action"`,
		`value="approve"`,
		`value="deny"`,
		`name="device_id"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("consent form missing %q", want)
		}
	}

	// The plaintext verifier is never part of the request, and the form must
	// not leak anything beyond the authorize parameters themselves.
	if strings.Contains(body, mcpTestVerifier) {
		t.Fatal("consent form must not reveal any verifier")
	}
}

func TestConsentWithoutSessionRedirectsToLogin(t *testing.T) {
	fx := newMcpFixture(t, nil)
	form := consentForm(fx.authorizeQuery(nil))
	form.Set("action", "approve")
	form.Set("device_id", fx.deviceID)
	resp := fx.doRequest(t, http.MethodPost, mcpoauth.AuthorizePath, form, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 login redirect", resp.StatusCode)
	}
	if location := resp.Header.Get("Location"); !strings.HasPrefix(location, mcpTestLogin+"?") {
		t.Fatalf("unauthenticated consent POST redirect %q does not target login", location)
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_grants"); n != 0 {
		t.Fatal("unauthenticated consent must never persist a grant")
	}
}

func TestConsentDenyRedirectsAccessDenied(t *testing.T) {
	fx := newMcpFixture(t, nil)
	q := fx.authorizeQuery(func(v url.Values) { v.Set("state", "deny-state-123456789") })

	params := redirectParams(t, fx.denyConsent(t, q))
	if params.Get("error") != "access_denied" {
		t.Fatalf("error = %q, want access_denied", params.Get("error"))
	}
	if params.Get("state") != "deny-state-123456789" {
		t.Fatalf("state = %q, want echo", params.Get("state"))
	}
	if params.Get("code") != "" {
		t.Fatal("denied consent must not issue a code")
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_grants"); n != 0 {
		t.Fatal("denied consent must not persist a grant")
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_authorization_codes"); n != 0 {
		t.Fatal("denied consent must not persist a code")
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM audit_logs"); n != 0 {
		t.Fatal("denied consent must not write audit events")
	}
}

func TestConsentExpiredTrialDenied(t *testing.T) {
	fx := newMcpFixture(t, func(spec *mcpFixtureSpec) {
		spec.trialStart = time.Now().UTC().Add(-15 * 24 * time.Hour)
		spec.trialEnd = time.Now().UTC().Add(-24 * time.Hour)
	})
	q := fx.authorizeQuery(func(v url.Values) { v.Set("state", "trial-state-123456789") })

	params := redirectParams(t, fx.approveConsent(t, q, nil))
	if params.Get("error") != "access_denied" {
		t.Fatalf("error = %q, want access_denied for an expired trial", params.Get("error"))
	}
	if params.Get("code") != "" {
		t.Fatal("expired trial must not issue a code")
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_grants"); n != 0 {
		t.Fatal("expired trial must not persist a grant")
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_authorization_codes"); n != 0 {
		t.Fatal("expired trial must not persist a code")
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM audit_logs"); n != 0 {
		t.Fatal("expired trial must not write audit events")
	}
}

func TestConsentWrongDeviceOwnerDenied(t *testing.T) {
	fx := newMcpFixture(t, nil)

	// A device that belongs to a different user.
	params := redirectParams(t, fx.approveConsent(t, fx.authorizeQuery(nil), func(f *url.Values) {
		f.Set("device_id", fx.otherUserDevID)
	}))
	if params.Get("error") != "access_denied" {
		t.Fatalf("error = %q, want access_denied for a foreign device", params.Get("error"))
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_grants"); n != 0 {
		t.Fatal("foreign device must not produce a grant")
	}

	// A device that does not exist at all.
	params = redirectParams(t, fx.approveConsent(t, fx.authorizeQuery(nil), func(f *url.Values) {
		f.Set("device_id", "not-a-real-device-id-123456")
	}))
	if params.Get("error") != "access_denied" {
		t.Fatalf("error = %q, want access_denied for an unknown device", params.Get("error"))
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_grants"); n != 0 {
		t.Fatal("unknown device must not produce a grant")
	}
}

func TestConsentScopeNarrowing(t *testing.T) {
	fx := newMcpFixture(t, nil)
	q := fx.authorizeQuery(nil) // requests mcp:connect studio:read studio:edit

	// Approving a subset narrows the grant, code, and echoed scope.
	code, _ := fx.consentCode(t, q, func(f *url.Values) {
		(*f)["grant"] = []string{"mcp:connect", "studio:read"}
	})
	_, deviceID, _, scopes := fx.consentGrant(t)
	if deviceID != fx.deviceID {
		t.Fatalf("grant device = %q, want %q", deviceID, fx.deviceID)
	}
	assertScopeSet(t, strings.Join(scopes, " "), "mcp:connect studio:read")

	var codeScopes []string
	var raw []byte
	if err := fx.db.QueryRowContext(t.Context(),
		`SELECT scopes FROM oauth_authorization_codes`).Scan(&raw); err != nil {
		t.Fatalf("select code scopes: %v", err)
	}
	if err := json.Unmarshal(raw, &codeScopes); err != nil {
		t.Fatalf("decode code scopes: %v", err)
	}
	assertScopeSet(t, strings.Join(codeScopes, " "), "mcp:connect studio:read")

	status, tokens, errResp := fx.exchangeToken(t, code, nil)
	if status != http.StatusOK {
		t.Fatalf("exchange failed: status = %d error = %q (%s)", status, errResp.Error, errResp.Description)
	}
	assertScopeSet(t, tokens.Scope, "mcp:connect studio:read")

	// Approving a scope the request never asked for is rejected.
	params := redirectParams(t, fx.approveConsent(t, fx.authorizeQuery(nil), func(f *url.Values) {
		(*f)["grant"] = []string{"studio:input"}
	}))
	if params.Get("error") != "access_denied" {
		t.Fatalf("error = %q, want access_denied for scope escalation", params.Get("error"))
	}

	// Approving nothing is rejected.
	params = redirectParams(t, fx.approveConsent(t, fx.authorizeQuery(nil), func(f *url.Values) {
		f.Del("grant")
	}))
	if params.Get("error") != "access_denied" {
		t.Fatalf("error = %q, want access_denied for an empty scope set", params.Get("error"))
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_grants"); n != 1 {
		t.Fatalf("denied approvals must not change the grant count, got %d", n)
	}
}

func TestConsentStudioSessionBinding(t *testing.T) {
	fx := newMcpFixture(t, nil)

	// A Studio session owned by the user and bound to the chosen device.
	code, _ := fx.consentCode(t, fx.authorizeQuery(nil), func(f *url.Values) {
		(*f)["grant"] = []string{"mcp:connect"}
		f.Set("studio_session_id", fx.studioID)
	})
	_, deviceID, studioID, _ := fx.consentGrant(t)
	if studioID != fx.studioID {
		t.Fatalf("grant studio session = %q, want %q", studioID, fx.studioID)
	}
	if deviceID != fx.deviceID {
		t.Fatalf("grant device = %q, want %q", deviceID, fx.deviceID)
	}

	var codeStudio string
	codeDigest := credential.Digest(code, fx.pepper)
	if err := fx.db.QueryRowContext(t.Context(),
		`SELECT studio_session_id FROM oauth_authorization_codes WHERE code_digest = ?`,
		codeDigest[:]).Scan(&codeStudio); err != nil {
		t.Fatalf("select code studio session: %v", err)
	}
	if codeStudio != fx.studioID {
		t.Fatalf("code studio session = %q, want %q", codeStudio, fx.studioID)
	}

	// A Studio session bound to a different device of the same user.
	params := redirectParams(t, fx.approveConsent(t, fx.authorizeQuery(nil), func(f *url.Values) {
		f.Set("studio_session_id", fx.otherStudioID)
	}))
	if params.Get("error") != "access_denied" {
		t.Fatalf("cross-device studio: error = %q, want access_denied", params.Get("error"))
	}

	// A Studio session that belongs to another user.
	params = redirectParams(t, fx.approveConsent(t, fx.authorizeQuery(nil), func(f *url.Values) {
		f.Set("studio_session_id", fx.otherUserStuID)
	}))
	if params.Get("error") != "access_denied" {
		t.Fatalf("foreign studio: error = %q, want access_denied", params.Get("error"))
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_grants"); n != 1 {
		t.Fatalf("only the first consent may persist, grants = %d", n)
	}
}

func TestConsentGrantUpsertsPerUserClientDevice(t *testing.T) {
	fx := newMcpFixture(t, nil)
	q := fx.authorizeQuery(nil)

	_, _ = fx.consentCode(t, q, func(f *url.Values) {
		(*f)["grant"] = []string{"mcp:connect", "studio:read", "studio:edit"}
	})
	_, _ = fx.consentCode(t, q, func(f *url.Values) {
		(*f)["grant"] = []string{"mcp:connect"}
	})

	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_grants"); n != 1 {
		t.Fatalf("repeated consent must reuse the grant row, got %d", n)
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_authorization_codes"); n != 2 {
		t.Fatalf("each approval must issue its own code, got %d", n)
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM audit_logs WHERE action = ?", mcpGrantAudit); n != 2 {
		t.Fatalf("each approval must audit, got %d", n)
	}
	_, _, _, scopes := fx.consentGrant(t)
	assertScopeSet(t, strings.Join(scopes, " "), "mcp:connect")
}

func TestConsentAuditFailureRollsBackGrant(t *testing.T) {
	fx := newMcpFixture(t, func(spec *mcpFixtureSpec) {
		spec.audits = failingAuditStore{}
	})
	resp := fx.approveConsent(t, fx.authorizeQuery(nil), nil)
	body := readAll(t, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("audit failure must fail the consent, status = %d (body: %s)", resp.StatusCode, body)
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_grants"); n != 0 {
		t.Fatal("grant must roll back when its audit event cannot persist")
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM audit_logs"); n != 0 {
		t.Fatal("no audit row can exist when the audit store fails")
	}
	if n := fx.queryInt(t, "SELECT COUNT(*) FROM oauth_authorization_codes"); n != 0 {
		t.Fatal("no code may be issued when the grant transaction failed")
	}
}

func TestConsentGrantAuditsApproval(t *testing.T) {
	fx := newMcpFixture(t, nil)
	_, _ = fx.consentCode(t, fx.authorizeQuery(nil), nil)

	var action, targetID, userID string
	if err := fx.db.QueryRowContext(t.Context(),
		`SELECT action, target_id, user_id FROM audit_logs`).Scan(&action, &targetID, &userID); err != nil {
		t.Fatalf("select grant audit: %v", err)
	}
	if action != mcpGrantAudit {
		t.Fatalf("audit action = %q, want %q", action, mcpGrantAudit)
	}
	grantID, _, _, _ := fx.consentGrant(t)
	if targetID != grantID {
		t.Fatalf("audit target = %q, want the grant id %q", targetID, grantID)
	}
	if userID != fx.userID {
		t.Fatalf("audit user = %q, want %q", userID, fx.userID)
	}
}
