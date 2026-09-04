package mcpoauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/ory/fosite"

	"robloxkit/internal/audit"
	"robloxkit/internal/credential"
	"robloxkit/internal/entitlement"
)

// consentDenial is a policy rejection. The decision handler maps it to an
// access_denied redirect; infrastructure failures propagate as errors.
type consentDenial struct {
	hint string
}

func (d *consentDenial) Error() string {
	return "mcpoauth: consent denied: " + d.hint
}

// consentApproval is the recorded outcome of an approved consent decision.
type consentApproval struct {
	clientID        string // internal client row id
	deviceID        string
	studioSessionID string
	scopes          []string
}

// handleConsentDecision records the POSTed approve or deny decision. Denials
// redirect to the client with access_denied; approvals persist the grant plus
// its audit event in one transaction, then issue the code.
func (p *Provider) handleConsentDecision(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, userID string) {
	ctx := r.Context()
	if !validateConsentCSRF(r) {
		writeProviderError(w, http.StatusForbidden, fosite.ErrInvalidRequest.
			WithHint("The consent form CSRF token is missing or invalid."))
		return
	}

 	switch action := r.PostFormValue("action"); action {
	case "deny":
		p.fosite.WriteAuthorizeError(ctx, w, ar, fosite.ErrAccessDenied)
		return
	case "approve":
	default:
		writeProviderError(w, http.StatusBadRequest, fosite.ErrInvalidRequest.
			WithHint("The consent action must be 'approve' or 'deny'."))
		return
	}

	approval, err := p.approveConsent(ctx, userID, ar, r.PostForm)
	var denial *consentDenial
	switch {
	case errors.As(err, &denial):
		p.fosite.WriteAuthorizeError(ctx, w, ar, fosite.ErrAccessDenied.WithHint(denial.hint))
		return
	case err != nil:
		writeProviderError(w, http.StatusInternalServerError, fosite.ErrServerError.
			WithHint("The consent could not be recorded."))
		return
	}

	for _, scope := range approval.scopes {
		ar.GrantScope(scope)
	}
	resp, err := p.fosite.NewAuthorizeResponse(ctx, ar, &fosite.DefaultSession{})
	if err != nil {
		p.fosite.WriteAuthorizeError(ctx, w, ar, err)
		return
	}

	// Persist the hashed single-use code bound to the recorded consent before
	// any redirect can hand it to the client.
	now := p.now()
	code := resp.GetCode()
	row := AuthorizationCode{
		ID:              mcpNewIDOrEmpty(),
		UserID:          userID,
		ClientID:        approval.clientID,
		RedirectURI:     ar.GetRedirectURI().String(),
		CodeChallenge:   ar.GetRequestForm().Get("code_challenge"),
		Scopes:          approval.scopes,
		DeviceID:        approval.deviceID,
		StudioSessionID: approval.studioSessionID,
		Resource:        p.resource,
		ExpiresAt:       now.Add(p.codeLife),
		CreatedAt:       now,
	}
	if row.ID == "" {
		writeProviderError(w, http.StatusInternalServerError, fosite.ErrServerError.
			WithHint("The authorization code could not be generated."))
		return
	}
	if err := p.store.SaveAuthorizationCode(ctx, row, credential.Digest(code, p.pepper)); err != nil {
		writeProviderError(w, http.StatusInternalServerError, fosite.ErrServerError.
			WithHint("The authorization code could not be persisted."))
		return
	}
	p.fosite.WriteAuthorizeResponse(ctx, w, ar, resp)
}

// approveConsent applies the consent policy layer: an active entitlement, a
// device the user owns, an optional Studio session bound to that device, and
// scope narrowing from the requested set. The grant and its secret-free audit
// event commit in one database transaction — an audit failure rolls the grant
// back.
func (p *Provider) approveConsent(ctx context.Context, userID string, ar fosite.AuthorizeRequester, form url.Values) (*consentApproval, error) {
	decision, err := p.config.Entitlements.Authorize(ctx, entitlement.Subject{UserID: userID})
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: authorize entitlement: %w", err)
	}
	if !decision.Permits(entitlement.ActionMCP) {
		return nil, &consentDenial{hint: "The trial window or license is not active for this account."}
	}

	deviceID := form.Get("device_id")
	owned, active, err := mcpDeviceOwnership(ctx, p.db, deviceID, userID)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: check device ownership: %w", err)
	}
	if !owned || !active {
		return nil, &consentDenial{hint: "The selected device is not an active device of this account."}
	}

	studioSessionID := form.Get("studio_session_id")
	if studioSessionID != "" {
		sessionDevice, sessionActive, err := mcpStudioOwnership(ctx, p.db, studioSessionID, userID)
		if err != nil {
			return nil, fmt.Errorf("mcpoauth: check studio session ownership: %w", err)
		}
		if sessionDevice == "" || !sessionActive || sessionDevice != deviceID {
			return nil, &consentDenial{hint: "The selected Studio session is not active on the selected device."}
		}
	}

	approved := form["grant"]
	if err := ValidateScopes(approved); err != nil {
		return nil, &consentDenial{hint: "At least one valid, distinct scope must be approved."}
	}
	requested := ar.GetRequestedScopes()
	for _, scope := range approved {
		if !requested.Has(scope) {
			return nil, &consentDenial{hint: "Approved scopes must be a subset of the requested scopes."}
		}
	}

	client, err := p.store.ClientByPublicID(ctx, ar.GetClient().GetID())
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: resolve connector client: %w", err)
	}

	grantID, err := mcpNewID()
	if err != nil {
		return nil, err
	}
	now := p.now()
	grant := Grant{
		ID:              grantID,
		UserID:          userID,
		ClientID:        client.ID,
		DeviceID:        deviceID,
		StudioSessionID: studioSessionID,
		Scopes:          approved,
		Resource:        p.resource,
		CreatedAt:       now,
	}

	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: begin grant transaction: %w", err)
	}
	defer tx.Rollback()
	stored, err := mcpSaveGrantInTx(ctx, tx, grant)
	if err != nil {
		return nil, err
	}
	event := audit.Event{
		Actor:         audit.Actor{UserID: userID, Kind: audit.ActorUser},
		Action:        GrantAuditAction,
		CorrelationID: mcpNewIDOrEmpty(),
		UserID:        userID,
		TargetType:    "oauth_grant",
		TargetID:      stored.ID,
		After: map[string]string{
			"client":   client.ClientID,
			"device":   deviceID,
			"studio":   studioSessionID,
			"scopes":   strings.Join(approved, " "),
			"resource": p.resource,
		},
		CreatedAt: now,
	}
	if event.CorrelationID == "" {
		return nil, errors.New("mcpoauth: audit correlation id missing")
	}
	if err := p.config.Audits.RecordInTx(ctx, tx, event); err != nil {
		return nil, fmt.Errorf("mcpoauth: audit connector grant: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("mcpoauth: commit grant transaction: %w", err)
	}

	return &consentApproval{
		clientID:        client.ID,
		deviceID:        deviceID,
		studioSessionID: studioSessionID,
		scopes:          approved,
	}, nil
}

// mcpDeviceOwnership reports whether deviceID is an owned device of userID
// and whether it is active.
func mcpDeviceOwnership(ctx context.Context, db *sql.DB, deviceID, userID string) (owned, active bool, err error) {
	if deviceID == "" || userID == "" {
		return false, false, nil
	}
	var status string
	err = db.QueryRowContext(ctx,
		`SELECT status FROM devices WHERE id = ? AND user_id = ?`, deviceID, userID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("mcpoauth: find device: %w", err)
	}
	return true, status == "active", nil
}

// mcpStudioOwnership resolves the device and activity of a Studio session the
// user claims. An empty device means the session was not found.
func mcpStudioOwnership(ctx context.Context, db *sql.DB, studioSessionID, userID string) (deviceID string, active bool, err error) {
	if studioSessionID == "" || userID == "" {
		return "", false, nil
	}
	var (
		device sql.NullString
		status string
	)
	err = db.QueryRowContext(ctx,
		`SELECT device_id, status FROM studio_sessions WHERE id = ? AND user_id = ?`,
		studioSessionID, userID).Scan(&device, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("mcpoauth: find studio session: %w", err)
	}
	return device.String, status == "active", nil
}

// mcpSelectDevices lists the user's active devices for the consent form.
func mcpSelectDevices(ctx context.Context, db *sql.DB, userID string) ([]ConsentDevice, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, name FROM devices WHERE user_id = ? AND status = 'active' ORDER BY created_at`, userID)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: list devices: %w", err)
	}
	defer rows.Close()
	var out []ConsentDevice
	for rows.Next() {
		var device ConsentDevice
		if err := rows.Scan(&device.ID, &device.Name); err != nil {
			return nil, fmt.Errorf("mcpoauth: scan device: %w", err)
		}
		out = append(out, device)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mcpoauth: list devices: %w", err)
	}
	return out, nil
}

// mcpSelectStudioSessions lists the user's active Studio sessions for the
// consent form.
func mcpSelectStudioSessions(ctx context.Context, db *sql.DB, userID string) ([]ConsentStudio, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, device_id, studio_id FROM studio_sessions WHERE user_id = ? AND status = 'active' ORDER BY started_at DESC`,
		userID)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: list studio sessions: %w", err)
	}
	defer rows.Close()
	var out []ConsentStudio
	for rows.Next() {
		var (
			studio   ConsentStudio
			device   sql.NullString
			studioID sql.NullString
		)
		if err := rows.Scan(&studio.ID, &device, &studioID); err != nil {
			return nil, fmt.Errorf("mcpoauth: scan studio session: %w", err)
		}
		studio.DeviceID = device.String
		studio.StudioID = studioID.String
		out = append(out, studio)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mcpoauth: list studio sessions: %w", err)
	}
	return out, nil
}
