package mcpoauth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ory/fosite"

	"robloxkit/internal/audit"
	"robloxkit/internal/credential"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/session"
)

// consentDenial is a policy rejection. The decision handler maps it to an
// access_denied redirect; infrastructure failures propagate as errors.
type consentDenial struct {
	hint string
}

func (d *consentDenial) Error() string {
	return "mcpoauth: consent denied: " + d.hint
}

// consentApproval is the validated outcome of an approved consent decision.
type consentApproval struct {
	clientID        string // internal client row id
	grantID         string // pre-allocated durable grant id
	deviceID        string
	studioSessionID string
	scopes          []string
}

// handleConsentDecision records the POSTed approve or deny decision. Denials
// redirect to the client with access_denied; approvals validate the decision
// and generate the code first, then persist the durable grant, the remembered
// session consent, the secret-free audit event, and the code digest in one
// transaction before the redirect. Any transaction failure rolls every record
// back, so no partial approval and no orphan code can survive.
func (p *Provider) handleConsentDecision(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, webSession session.Session) {
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

	approval, err := p.approveConsent(ctx, webSession, ar, r.PostForm)
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

	// Persist the hashed single-use code bound to the recorded consent inside
	// the approval transaction, before any redirect can hand it to the client.
	now := p.now()
	code := resp.GetCode()
	row := AuthorizationCode{
		ID:              mcpNewIDOrEmpty(),
		UserID:          webSession.UserID,
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
	if err := p.recordApprovalTx(ctx, webSession, approval, row, credential.Digest(code, p.pepper)); err != nil {
		switch {
		case errors.As(err, &denial):
			p.fosite.WriteAuthorizeError(ctx, w, ar, fosite.ErrAccessDenied.WithHint(denial.hint))
		default:
			writeProviderError(w, http.StatusInternalServerError, fosite.ErrServerError.
				WithHint("The consent could not be recorded."))
		}
		return
	}
	p.fosite.WriteAuthorizeResponse(ctx, w, ar, resp)
}

// approveConsent classifies the consent decision before any persistence:
// active entitlement, an owned active device, an owned active Studio session
// on that device, and the exact validated scope package from the authorize
// request. The browser does not choose or narrow individual scopes. Every
// predicate is revalidated authoritatively under row locks inside
// recordApprovalTx; this pass only produces early, user-facing denials and
// never writes.
func (p *Provider) approveConsent(ctx context.Context, webSession session.Session, ar fosite.AuthorizeRequester, form url.Values) (*consentApproval, error) {
	userID := webSession.UserID
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
	if studioSessionID == "" {
		return nil, &consentDenial{hint: "An active Roblox Studio session must be selected."}
	}
	sessionDevice, sessionActive, err := mcpStudioOwnership(ctx, p.db, studioSessionID, userID)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: check studio session ownership: %w", err)
	}
	if sessionDevice == "" || !sessionActive || sessionDevice != deviceID {
		return nil, &consentDenial{hint: "The selected Studio session is not active on the selected device."}
	}

	approved := append([]string(nil), ar.GetRequestedScopes()...)
	if err := ValidateScopes(approved); err != nil {
		return nil, &consentDenial{hint: "The authorize request must contain at least one valid, distinct scope."}
	}

	client, err := p.store.ClientByPublicID(ctx, ar.GetClient().GetID())
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: resolve connector client: %w", err)
	}

	grantID, err := mcpNewID()
	if err != nil {
		return nil, err
	}

	return &consentApproval{
		clientID:        client.ID,
		grantID:         grantID,
		deviceID:        deviceID,
		studioSessionID: studioSessionID,
		scopes:          approved,
	}, nil
}

// recordApprovalTx is the single explicit-approval transaction. It
// revalidates every eligibility predicate under row locks — active web
// session, owned active device, owned active Studio session bound to that
// device, and entitlement — then upserts the durable grant, upserts the
// remembered session consent, appends the secret-free approval audit, and
// inserts the authorization-code digest. Any failure rolls every record back.
//
// Lock ordering is identical to the remembered-issuance transaction
// (web_sessions, oauth_grants, devices, studio_sessions, entitlement reads,
// then inserts), so the two paths cannot deadlock against each other.
func (p *Provider) recordApprovalTx(ctx context.Context, webSession session.Session, approval *consentApproval, code AuthorizationCode, digest [32]byte) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mcpoauth: begin approval transaction: %w", err)
	}
	defer tx.Rollback()
	now := p.now()

	if err := mcpLockWebSession(ctx, tx, webSession.ID, webSession.UserID, now); err != nil {
		return err
	}
	if err := mcpLockDevice(ctx, tx, approval.deviceID, webSession.UserID); err != nil {
		return err
	}
	if err := mcpLockStudioSession(ctx, tx, approval.studioSessionID, webSession.UserID, approval.deviceID); err != nil {
		return err
	}
	permitted, err := mcpEntitlementPermitsInTx(ctx, tx, webSession.UserID, now)
	if err != nil {
		return err
	}
	if !permitted {
		return &consentDenial{hint: "The trial window or license is not active for this account."}
	}

	stored, err := mcpSaveGrantInTx(ctx, tx, Grant{
		ID:              approval.grantID,
		UserID:          webSession.UserID,
		ClientID:        approval.clientID,
		DeviceID:        approval.deviceID,
		StudioSessionID: approval.studioSessionID,
		Scopes:          approval.scopes,
		Resource:        p.resource,
		CreatedAt:       now,
	})
	if err != nil {
		return err
	}
	if err := mcpSaveSessionConsentInTx(ctx, tx, SessionConsent{
		WebSessionID:    webSession.ID,
		ClientID:        approval.clientID,
		GrantID:         stored.ID,
		RequestedScopes: approval.scopes,
		Resource:        p.resource,
		CreatedAt:       now,
		UpdatedAt:       now,
	}); err != nil {
		return err
	}
	event := audit.Event{
		Actor:         audit.Actor{UserID: webSession.UserID, Kind: audit.ActorUser},
		Action:        GrantAuditAction,
		CorrelationID: mcpNewIDOrEmpty(),
		UserID:        webSession.UserID,
		TargetType:    "oauth_grant",
		TargetID:      stored.ID,
		After: map[string]string{
			"client":   approval.clientID,
			"device":   approval.deviceID,
			"studio":   approval.studioSessionID,
			"scopes":   strings.Join(approval.scopes, " "),
			"resource": p.resource,
		},
		CreatedAt: now,
	}
	if event.CorrelationID == "" {
		return errors.New("mcpoauth: audit correlation id missing")
	}
	if err := p.config.Audits.RecordInTx(ctx, tx, event); err != nil {
		return fmt.Errorf("mcpoauth: audit connector grant: %w", err)
	}
	if err := mcpInsertCodeInTx(ctx, tx, code, digest); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mcpoauth: commit approval transaction: %w", err)
	}
	return nil
}

// authorizeRemembered attempts the same-session auto-approval. It returns
// (preselect, handled): handled means the request was fully served; otherwise
// the caller must render consent. preselect reports whether the consent form
// may preselect a target: a fresh pairing may, while a stale remembered
// consent may never silently reselect its old target.
//
// The exact remembered-approval contract: the stored client matches the
// resolved DCR client, the stored resource matches the request, the stored
// requested scopes match the request as a canonical set, the referenced grant
// is live for the same user and client with the same scope set, and the
// grant's device and Studio session are still owned, active, and bound. Any
// mismatch re-renders consent without preselection.
func (p *Provider) authorizeRemembered(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, webSession session.Session) (preselect, handled bool) {
	ctx := r.Context()
	client, err := p.store.ClientByPublicID(ctx, ar.GetClient().GetID())
	if err != nil {
		// The same lookup fails inside renderConsent, which reports it.
		return true, false
	}
	consent, err := p.store.SessionConsent(ctx, webSession.ID, client.ID)
	if err != nil {
		// A missing memory and a lookup failure both fail closed into consent.
		return true, false
	}

	scopes := append([]string(nil), ar.GetRequestedScopes()...)
	if consent.Resource != p.resource || !mcpSameScopeSet(consent.RequestedScopes, scopes) {
		return false, false
	}
	grant, err := mcpSelectGrantByID(ctx, p.db, consent.GrantID, webSession.UserID)
	if err != nil || grant.RevokedAt != nil || grant.ClientID != client.ID ||
		grant.DeviceID == "" || grant.StudioSessionID == "" ||
		!mcpSameScopeSet(grant.Scopes, scopes) {
		return false, false
	}

	for _, scope := range scopes {
		ar.GrantScope(scope)
	}
	resp, err := p.fosite.NewAuthorizeResponse(ctx, ar, &fosite.DefaultSession{})
	if err != nil {
		p.fosite.WriteAuthorizeError(ctx, w, ar, err)
		return false, true
	}
	now := p.now()
	code := resp.GetCode()
	row := AuthorizationCode{
		ID:              mcpNewIDOrEmpty(),
		UserID:          webSession.UserID,
		ClientID:        client.ID,
		RedirectURI:     ar.GetRedirectURI().String(),
		CodeChallenge:   ar.GetRequestForm().Get("code_challenge"),
		Scopes:          scopes,
		DeviceID:        grant.DeviceID,
		StudioSessionID: grant.StudioSessionID,
		Resource:        p.resource,
		ExpiresAt:       now.Add(p.codeLife),
		CreatedAt:       now,
	}
	if row.ID == "" {
		writeProviderError(w, http.StatusInternalServerError, fosite.ErrServerError.
			WithHint("The authorization code could not be generated."))
		return false, true
	}
	if err := p.issueRememberedCodeTx(ctx, webSession, client, consent, row, credential.Digest(code, p.pepper)); err != nil {
		var denial *consentDenial
		switch {
		case errors.As(err, &denial):
			// Eligibility changed between the read and the transaction: the
			// user must re-approve explicitly, with no remembered preselection.
			return false, false
		default:
			writeProviderError(w, http.StatusInternalServerError, fosite.ErrServerError.
				WithHint("The remembered consent could not be used."))
			return false, true
		}
	}
	p.fosite.WriteAuthorizeResponse(ctx, w, ar, resp)
	return false, true
}

// issueRememberedCodeTx issues a fresh single-use authorization code for an
// exactly matching remembered consent. It revalidates every eligibility
// predicate under row locks — active web session, unchanged consent row,
// live grant, owned active device, owned active bound Studio session, and
// entitlement — and mutates nothing except inserting the new code digest: no
// grant update and no approval audit, because no new user decision occurred.
//
// A concurrent logout, target invalidation, or consent replacement either
// blocks until this transaction commits (sharing locks) or invalidates the
// remembered state this transaction re-reads, so a code can never be issued
// against a session or target invalidated before the response is written.
func (p *Provider) issueRememberedCodeTx(ctx context.Context, webSession session.Session, client Client, consent SessionConsent, code AuthorizationCode, digest [32]byte) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mcpoauth: begin remembered-code transaction: %w", err)
	}
	defer tx.Rollback()
	now := p.now()

	if err := mcpLockWebSession(ctx, tx, webSession.ID, webSession.UserID, now); err != nil {
		return err
	}
	consentRow, err := mcpLockSessionConsent(ctx, tx, webSession.ID, client.ID)
	if errors.Is(err, ErrSessionConsentNotFound) {
		return &consentDenial{hint: "The remembered consent is no longer available."}
	}
	if err != nil {
		return err
	}
	if consentRow.GrantID != consent.GrantID || consentRow.Resource != consent.Resource ||
		!mcpSameScopeSet(consentRow.RequestedScopes, consent.RequestedScopes) {
		return &consentDenial{hint: "The remembered consent no longer matches this request."}
	}

	grant, err := mcpLockGrantByID(ctx, tx, consentRow.GrantID, webSession.UserID)
	if errors.Is(err, ErrGrantNotFound) {
		return &consentDenial{hint: "The remembered grant is no longer available."}
	}
	if err != nil {
		return err
	}
	if grant.RevokedAt != nil || grant.ClientID != client.ID ||
		grant.DeviceID != code.DeviceID || grant.StudioSessionID != code.StudioSessionID ||
		!mcpSameScopeSet(grant.Scopes, code.Scopes) {
		return &consentDenial{hint: "The remembered grant no longer matches this request."}
	}
	if err := mcpLockDevice(ctx, tx, grant.DeviceID, grant.UserID); err != nil {
		return err
	}
	if err := mcpLockStudioSession(ctx, tx, grant.StudioSessionID, grant.UserID, grant.DeviceID); err != nil {
		return err
	}
	permitted, err := mcpEntitlementPermitsInTx(ctx, tx, webSession.UserID, now)
	if err != nil {
		return err
	}
	if !permitted {
		return &consentDenial{hint: "The trial window or license is not active for this account."}
	}
	if err := mcpInsertCodeInTx(ctx, tx, code, digest); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mcpoauth: commit remembered-code transaction: %w", err)
	}
	return nil
}

// mcpSameScopeSet compares two validated scope lists as sets. Both sides
// passed ValidateScopes, so their tokens are distinct and equal length plus
// subset containment implies set equality.
func mcpSameScopeSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, scope := range a {
		seen[scope] = struct{}{}
	}
	for _, scope := range b {
		if _, ok := seen[scope]; !ok {
			return false
		}
	}
	return true
}

// mcpLockWebSession verifies inside the caller's transaction that the browser
// session row still exists, belongs to the user, and is neither revoked nor
// expired, holding its row lock until commit so a concurrent logout cannot
// invalidate the session between the check and the code issuance.
func mcpLockWebSession(ctx context.Context, tx *sql.Tx, webSessionID, userID string, now time.Time) error {
	if webSessionID == "" || userID == "" {
		return &consentDenial{hint: "The browser session is no longer active."}
	}
	var (
		rowUser   string
		expiresAt time.Time
		revoked   sql.NullTime
	)
	err := tx.QueryRowContext(ctx,
		`SELECT user_id, expires_at, revoked_at FROM web_sessions WHERE id = ? FOR UPDATE`,
		webSessionID).Scan(&rowUser, &expiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return &consentDenial{hint: "The browser session is no longer active."}
	}
	if err != nil {
		return fmt.Errorf("mcpoauth: lock web session: %w", err)
	}
	if rowUser != userID || revoked.Valid || !now.Before(expiresAt) {
		return &consentDenial{hint: "The browser session is no longer active."}
	}
	return nil
}

// mcpLockDevice verifies inside the caller's transaction that the device is
// owned by the user and active, holding a shared lock until commit so a
// concurrent deactivation cannot slip between the check and the issuance.
func mcpLockDevice(ctx context.Context, tx *sql.Tx, deviceID, userID string) error {
	if deviceID == "" || userID == "" {
		return &consentDenial{hint: "The selected device is not an active device of this account."}
	}
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT status FROM devices WHERE id = ? AND user_id = ? FOR SHARE`,
		deviceID, userID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return &consentDenial{hint: "The selected device is not an active device of this account."}
	}
	if err != nil {
		return fmt.Errorf("mcpoauth: lock device: %w", err)
	}
	if status != "active" {
		return &consentDenial{hint: "The selected device is not an active device of this account."}
	}
	return nil
}

// mcpLockStudioSession verifies inside the caller's transaction that the
// Studio session is owned by the user, active, and bound to the device,
// holding a shared lock until commit.
func mcpLockStudioSession(ctx context.Context, tx *sql.Tx, studioSessionID, userID, deviceID string) error {
	if studioSessionID == "" || deviceID == "" || userID == "" {
		return &consentDenial{hint: "The selected Studio session is not active on the selected device."}
	}
	var (
		boundDevice string
		status      string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT device_id, status FROM studio_sessions WHERE id = ? AND user_id = ? FOR SHARE`,
		studioSessionID, userID).Scan(&boundDevice, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return &consentDenial{hint: "The selected Studio session is not active on the selected device."}
	}
	if err != nil {
		return fmt.Errorf("mcpoauth: lock studio session: %w", err)
	}
	if boundDevice != deviceID || status != "active" {
		return &consentDenial{hint: "The selected Studio session is not active on the selected device."}
	}
	return nil
}

// mcpEntitlementPermitsInTx re-reads the authoritative entitlement inside the
// caller's transaction under shared locks. Trial rows are trigger-immutable,
// and license status transitions block until this transaction commits, so a
// concurrently expiring or revoked entitlement cannot slip between the check
// and the code issuance. This repeats the entitlement store's own two
// authoritative reads without nesting a second transaction.
func mcpEntitlementPermitsInTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (bool, error) {
	var endsAt sql.NullTime
	err := tx.QueryRowContext(ctx,
		`SELECT ends_at FROM trial_entitlements WHERE user_id = ? FOR SHARE`, userID).Scan(&endsAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, fmt.Errorf("mcpoauth: read trial entitlement: %w", err)
	case now.Before(endsAt.Time):
		return true, nil
	}
	var licenses int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM licenses WHERE user_id = ? AND status = 'active' FOR SHARE`,
		userID).Scan(&licenses); err != nil {
		return false, fmt.Errorf("mcpoauth: count active licenses: %w", err)
	}
	return licenses > 0, nil
}

// mcpLockGrantByID loads the grant for update inside the caller's
// transaction, enforcing the user binding in the query itself.
func mcpLockGrantByID(ctx context.Context, tx *sql.Tx, grantID, userID string) (Grant, error) {
	if grantID == "" || userID == "" {
		return Grant{}, ErrGrantNotFound
	}
	return mcpScanGrant(tx.QueryRowContext(ctx,
		`SELECT id, user_id, client_id, device_id, studio_session_id, scopes, resource, created_at, revoked_at
		 FROM oauth_grants WHERE id = ? AND user_id = ? FOR UPDATE`,
		grantID, userID))
}

// mcpLockSessionConsent loads the remembered consent row for update inside
// the caller's transaction, so a concurrent explicit approval replacing the
// row is visible before any code is issued.
func mcpLockSessionConsent(ctx context.Context, tx *sql.Tx, webSessionID, clientID string) (SessionConsent, error) {
	var (
		consent SessionConsent
		scopes  []byte
	)
	err := tx.QueryRowContext(ctx,
		`SELECT web_session_id, client_id, grant_id, requested_scopes, resource, created_at, updated_at
		 FROM oauth_session_consents WHERE web_session_id = ? AND client_id = ? FOR UPDATE`,
		webSessionID, clientID).
		Scan(&consent.WebSessionID, &consent.ClientID, &consent.GrantID, &scopes, &consent.Resource, &consent.CreatedAt, &consent.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionConsent{}, ErrSessionConsentNotFound
	}
	if err != nil {
		return SessionConsent{}, fmt.Errorf("mcpoauth: lock session consent: %w", err)
	}
	consent.RequestedScopes, err = mcpScanStrings(scopes)
	if err != nil {
		return SessionConsent{}, err
	}
	return consent, nil
}

// mcpSaveSessionConsentInTx upserts the remembered explicit approval inside
// the caller's transaction: one row per (web session, client) holds the last
// approved grant, canonical requested scope set, and resource.
func mcpSaveSessionConsentInTx(ctx context.Context, tx *sql.Tx, consent SessionConsent) error {
	if consent.WebSessionID == "" || consent.ClientID == "" || consent.GrantID == "" {
		return errors.New("mcpoauth: session consent session, client, and grant are required")
	}
	if consent.UpdatedAt.IsZero() {
		consent.UpdatedAt = consent.CreatedAt
	}
	scopes, err := mcpJSONStrings(consent.RequestedScopes)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO oauth_session_consents
		       (web_session_id, client_id, grant_id, requested_scopes, resource, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE grant_id = VALUES(grant_id), requested_scopes = VALUES(requested_scopes),
		                        resource = VALUES(resource), updated_at = VALUES(updated_at)`,
		consent.WebSessionID, consent.ClientID, consent.GrantID, scopes, consent.Resource,
		consent.CreatedAt.UTC(), consent.UpdatedAt.UTC()); err != nil {
		return fmt.Errorf("mcpoauth: upsert session consent: %w", err)
	}
	return nil
}

// mcpInsertCodeInTx persists a new single-use authorization-code digest
// inside the caller's transaction, carrying the same exact resource, client,
// and redirect bindings the store path enforces.
func mcpInsertCodeInTx(ctx context.Context, tx *sql.Tx, code AuthorizationCode, digest [32]byte) error {
	if code.ID == "" || code.UserID == "" || code.ClientID == "" {
		return errors.New("mcpoauth: authorization code id, user, and client are required")
	}
	if code.CodeChallenge == "" {
		return errors.New("mcpoauth: authorization code PKCE challenge is required")
	}
	if code.ExpiresAt.IsZero() {
		return errors.New("mcpoauth: authorization code expiry is required")
	}
	if err := ValidateRedirectURI(code.RedirectURI); err != nil {
		return fmt.Errorf("mcpoauth: authorization code redirect URI: %w", err)
	}
	if err := ValidateResourceURL(code.Resource); err != nil {
		return fmt.Errorf("mcpoauth: authorization code resource: %w", err)
	}
	if err := ValidateScopes(code.Scopes); err != nil {
		return err
	}
	scopes, err := mcpJSONStrings(code.Scopes)
	if err != nil {
		return err
	}
	createdAt := code.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO oauth_authorization_codes
		       (id, user_id, client_id, redirect_uri, code_challenge, scopes, device_id, studio_session_id, resource, expires_at, created_at, code_digest)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		code.ID, code.UserID, code.ClientID, code.RedirectURI, code.CodeChallenge, scopes,
		mcpNullableString(code.DeviceID), mcpNullableString(code.StudioSessionID), code.Resource,
		code.ExpiresAt.UTC(), createdAt.UTC(), digest[:]); err != nil {
		return fmt.Errorf("mcpoauth: insert authorization code: %w", err)
	}
	return nil
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

// mcpSelectStudioSessions lists active Studio sessions on active devices
// owned by the user, so every rendered option is a valid target pair.
func mcpSelectStudioSessions(ctx context.Context, db *sql.DB, userID string) ([]ConsentStudio, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT s.id, s.device_id, s.studio_id
		 FROM studio_sessions s
		 JOIN devices d ON d.id = s.device_id AND d.user_id = s.user_id
		 WHERE s.user_id = ? AND s.status = 'active' AND d.status = 'active'
		 ORDER BY s.started_at DESC`, userID)
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
