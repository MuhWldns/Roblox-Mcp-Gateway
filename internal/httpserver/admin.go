package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"robloxkit/internal/dashboard"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/mcpoauth"
)

// AdminConfig composes the privileged administration surface: the audited
// device transfer, the identity recovery, and the trial extension. Every
// execute request must carry a support case id, a reason, an evidence
// reference, and the expected version of the row it mutates, so a case built
// on stale state is rejected instead of applied blindly.
type AdminConfig struct {
	// Entitlements executes the three audited mutations and evaluates trial
	// state. It is the frozen entitlement service.
	Entitlements *entitlement.Service

	// OAuth revokes connector grants together with their access and refresh
	// tokens while an identity recovery runs.
	OAuth mcpoauth.Store

	// AdminUsers lists the internal user ids allowed to execute privileged
	// actions. Every other signed-in user receives 403.
	AdminUsers []string
}

// Admin-surface sentinel errors mapped to distinct HTTP statuses.
var (
	// errAdminCaseExecuted reports a support case id that already executed.
	errAdminCaseExecuted = errors.New("admin: case already executed")
	// errAdminStaleVersion reports an expected version that no longer
	// matches the committed state.
	errAdminStaleVersion = errors.New("admin: expected version is stale")
	// errAdminNotFound reports a named object the committed reads cannot see.
	errAdminNotFound = errors.New("admin: not found")
)

// adminAPI serves the session-bound administration endpoints. Reads preview
// the committed, user-scoped state and mint version tokens; mutations
// re-read, verify the token, and delegate to the audited services.
type adminAPI struct {
	store        dashboard.Store
	identities   IdentityReader
	entitlements *entitlement.Service
	oauth        mcpoauth.Store
	registry     BridgeRegistry
	admins       map[string]bool
	now          func() time.Time

	// cases guards against double execution of one support case id. The
	// durable replay guard is the expected-version check: every replay
	// either fails the version check or hits the store's row-level checks.
	mu    sync.Mutex
	cases map[string]struct{}
}

func newAdminAPI(cfg Config) *adminAPI {
	admins := make(map[string]bool, len(cfg.Admin.AdminUsers))
	for _, id := range cfg.Admin.AdminUsers {
		admins[id] = true
	}
	return &adminAPI{
		store:        cfg.Dashboard,
		identities:   cfg.IdentityReader,
		entitlements: cfg.Admin.Entitlements,
		oauth:        cfg.Admin.OAuth,
		registry:     cfg.Registry,
		admins:       admins,
		now:          func() time.Time { return time.Now().UTC() },
		cases:        make(map[string]struct{}),
	}
}

// authorized answers 403 unless the session user is a configured
// administrator. It runs inside the session middleware.
func (a *adminAPI) authorized(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, err := sessionUserID(r)
		if err != nil {
			writeAPIError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !a.admins[userID] {
			writeAPIError(w, http.StatusForbidden, "administrator access required")
			return
		}
		next(w, r)
	}
}

// beginCase reserves the support case id. A second execution of the same id —
// including a concurrent one — is rejected; the reservation is released when
// the mutation fails so a transient failure never burns the case.
func (a *adminAPI) beginCase(caseID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, done := a.cases[caseID]; done {
		return errAdminCaseExecuted
	}
	a.cases[caseID] = struct{}{}
	return nil
}

func (a *adminAPI) releaseCase(caseID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.cases, caseID)
}

// adminAccountState is the committed, user-scoped state the admin tools
// preview and verify.
type adminAccountState struct {
	devices    []dashboard.DeviceRow
	connectors []dashboard.ConnectorRow
	license    *dashboard.LicenseRow
}

// empty reports whether the committed device/connector/license reads can see
// anything at all for the user.
func (s adminAccountState) empty() bool {
	return len(s.devices) == 0 && len(s.connectors) == 0 && s.license == nil
}

// readAccountState loads the committed user-scoped rows. Dashboard reads are
// keyed by the passed user id, so an administration request names the
// account it acts on explicitly.
func (a *adminAPI) readAccountState(ctx context.Context, userID string) (adminAccountState, error) {
	devices, err := a.store.Devices(ctx, userID)
	if err != nil {
		return adminAccountState{}, err
	}
	connectors, err := a.store.Connectors(ctx, userID)
	if err != nil {
		return adminAccountState{}, err
	}
	license, err := a.store.License(ctx, userID)
	if err != nil {
		return adminAccountState{}, err
	}
	return adminAccountState{devices: devices, connectors: connectors, license: license}, nil
}

// invisible reports whether nothing at all is readable for the user: no
// devices, no license, no connector grants, and no Roblox identity. The
// truly-unknown user and the completely-empty account are deliberately
// indistinguishable. Identity reads only fail after the state reads have
// already succeeded, so a database outage surfaces as 500, never 404.
func (a *adminAPI) invisible(ctx context.Context, userID string, state adminAccountState) bool {
	if !state.empty() {
		return false
	}
	_, err := a.identities.RobloxIdentity(ctx, userID)
	return err != nil
}

func licenseKey(license *dashboard.LicenseRow) string {
	if license == nil {
		return "none"
	}
	return fmt.Sprintf("%s|%d|%d", license.Status, license.DeviceSlots, license.ActiveBindings)
}

func devicesKey(devices []dashboard.DeviceRow) string {
	parts := make([]string, 0, len(devices))
	for _, device := range devices {
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s",
			device.ID, device.Name, device.Status, device.UpdatedAt.UTC().Format(time.RFC3339Nano)))
	}
	return strings.Join(parts, "#")
}

func connectorsKey(connectors []dashboard.ConnectorRow) string {
	parts := make([]string, 0, len(connectors))
	for _, connector := range connectors {
		revoked := "none"
		if connector.RevokedAt != nil {
			revoked = connector.RevokedAt.UTC().Format(time.RFC3339Nano)
		}
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s",
			connector.ID, connector.ClientID, connector.DeviceID, connector.StudioSessionID, revoked))
	}
	return strings.Join(parts, "#")
}

// hashAdminState folds a canonical state key into the short typed-confirmation
// token the administrator echoes back. It is a state fingerprint, not a
// secret.
func hashAdminState(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])[:16]
}

// transferVersion fingerprints the license and device state a transfer
// reconfigures. Any rename, revoke, enrollment, or slot change invalidates
// it. The binding's own device identity is asserted by the committed store
// under its row lock, so a concurrent transfer is caught there.
func transferVersion(state adminAccountState) string {
	return hashAdminState("license:" + licenseKey(state.license) + "#" + devicesKey(state.devices))
}

// recoveryVersion fingerprints the whole revocable surface: devices,
// connector grants, and license.
func recoveryVersion(state adminAccountState) string {
	return hashAdminState("license:" + licenseKey(state.license) + "#" +
		devicesKey(state.devices) + "#" + connectorsKey(state.connectors))
}

// adminIdentityView is the safe identity block every preview returns.
type adminIdentityView struct {
	Subject     string `json:"subject"`
	DisplayName string `json:"display_name"`
}

// identityView resolves the account's Roblox identity; nil means the reads
// could not see one.
func (a *adminAPI) identityView(ctx context.Context, userID string) *adminIdentityView {
	identity, err := a.identities.RobloxIdentity(ctx, userID)
	if err != nil {
		return nil
	}
	return &adminIdentityView{Subject: identity.Subject, DisplayName: identity.DisplayName}
}

type adminDeviceView struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Hostname       *string `json:"hostname"`
	Platform       *string `json:"platform"`
	BridgeVersion  *string `json:"bridge_version"`
	Status         string  `json:"status"`
	Online         bool    `json:"online"`
	LastHeartbeat  *string `json:"last_heartbeat"`
	MCPState       *string `json:"mcp_state"`
	ReconnectCount int     `json:"reconnect_count"`
	LastError      *string `json:"last_error"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

func (a *adminAPI) deviceViews(devices []dashboard.DeviceRow) []adminDeviceView {
	views := make([]adminDeviceView, 0, len(devices))
	for _, device := range devices {
		v := adminDeviceView{
			ID:             device.ID,
			Name:           device.Name,
			Hostname:       device.Hostname,
			Platform:       device.Platform,
			BridgeVersion:  device.BridgeVersion,
			Status:         device.Status,
			Online:         a.registry != nil && a.registry.Online(device.ID),
			MCPState:       device.MCPState,
			ReconnectCount: device.ReconnectCount,
			LastError:      device.LastError,
			CreatedAt:      device.CreatedAt.UTC().Format(timeFormat),
			UpdatedAt:      device.UpdatedAt.UTC().Format(timeFormat),
		}
		if device.LastHeartbeat != nil {
			ts := device.LastHeartbeat.UTC().Format(timeFormat)
			v.LastHeartbeat = &ts
		}
		views = append(views, v)
	}
	return views
}

func licenseView(license *dashboard.LicenseRow) any {
	if license == nil {
		return nil
	}
	return map[string]any{
		"status":          license.Status,
		"device_slots":    license.DeviceSlots,
		"active_bindings": license.ActiveBindings,
		"roblox_username": license.RobloxUsername,
		"license_id":      license.LicenseID,
		"subscription_id": license.SubscriptionID,
		"subscription":    license.SubscriptionState,
		"usage_last_30d":  license.UsageLast30Days,
		"usage_last_7d":   license.UsageLast7Days,
	}
}

// trialDecision resolves the user's committed trial window for validation.
func (a *adminAPI) trialDecision(ctx context.Context, userID string) (entitlement.Decision, error) {
	subject := entitlement.Subject{UserID: userID, Provider: "roblox"}
	if identity, err := a.identities.RobloxIdentity(ctx, userID); err == nil {
		subject.ProviderSubject = identity.Subject
	}
	return a.entitlements.Authorize(ctx, subject)
}

// trialView renders the user's trial entitlement. The second return is the
// expected-version token: the current expiry itself, so the administrator
// confirms the exact window being extended.
func (a *adminAPI) trialView(ctx context.Context, userID string) (any, string) {
	decision, err := a.trialDecision(ctx, userID)
	if err != nil || decision.Entitlement.ID == "" {
		return nil, ""
	}
	endsAt := decision.Entitlement.EndsAt.UTC()
	return map[string]any{
		"id":         decision.Entitlement.ID,
		"started_at": decision.Entitlement.StartedAt.UTC().Format(timeFormat),
		"ends_at":    endsAt.Format(time.RFC3339Nano),
		"active":     decision.Active,
	}, endsAt.Format(time.RFC3339Nano)
}

// transferPreview shows the account state a transfer acts on: its devices
// with live presence, its active license, and the version token the execute
// request must echo.
func (a *adminAPI) transferPreview(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	state, err := a.readAccountState(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "transfer preview unavailable")
		return
	}
	if a.invisible(r.Context(), userID, state) {
		writeAPIError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":  userID,
		"identity": a.identityView(r.Context(), userID),
		"devices":  a.deviceViews(state.devices),
		"license":  licenseView(state.license),
		"version":  transferVersion(state),
	})
}

// recoveryPreview shows the whole revocable surface of the account plus the
// version token the recovery request must echo.
func (a *adminAPI) recoveryPreview(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	state, err := a.readAccountState(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "recovery preview unavailable")
		return
	}
	if a.invisible(r.Context(), userID, state) {
		writeAPIError(w, http.StatusNotFound, "user not found")
		return
	}
	connectors := make([]map[string]any, 0, len(state.connectors))
	for _, connector := range state.connectors {
		view := map[string]any{
			"id":          connector.ID,
			"client_id":   connector.ClientID,
			"client_name": connector.ClientName,
			"device_id":   connector.DeviceID,
			"scopes":      connector.Scopes,
			"created_at":  connector.CreatedAt.UTC().Format(timeFormat),
		}
		if connector.StudioSessionID != "" {
			view["studio_session_id"] = connector.StudioSessionID
		}
		if connector.RevokedAt != nil {
			view["revoked_at"] = connector.RevokedAt.UTC().Format(timeFormat)
		}
		connectors = append(connectors, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":    userID,
		"identity":   a.identityView(r.Context(), userID),
		"devices":    a.deviceViews(state.devices),
		"connectors": connectors,
		"license":    licenseView(state.license),
		"version":    recoveryVersion(state),
	})
}

// trialPreview shows the account's one trial entitlement and its current
// expiry — the version token the extension request must echo.
func (a *adminAPI) trialPreview(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	state, err := a.readAccountState(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "trial preview unavailable")
		return
	}
	if a.invisible(r.Context(), userID, state) {
		writeAPIError(w, http.StatusNotFound, "user not found")
		return
	}
	trial, version := a.trialView(r.Context(), userID)
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":  userID,
		"identity": a.identityView(r.Context(), userID),
		"trial":    trial,
		"version":  version,
	})
}

// trimRequire trims every required body field and names the first empty one.
func trimRequire(fields map[string]string) (map[string]string, string) {
	trimmed := make(map[string]string, len(fields))
	for name, value := range fields {
		trimmed[name] = strings.TrimSpace(value)
		if trimmed[name] == "" {
			return nil, name
		}
	}
	return trimmed, ""
}

// transfer moves an active license slot from one device to another. The old
// device's live connection closes before the new binding activates, and the
// row-level move runs in the committed store's locked transaction.
func (a *adminAPI) transfer(w http.ResponseWriter, r *http.Request) {
	adminID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		UserID          string `json:"user_id"`
		LicenseID       string `json:"license_id"`
		OldDeviceID     string `json:"old_device_id"`
		NewDeviceID     string `json:"new_device_id"`
		ExpectedVersion string `json:"expected_version"`
		CaseID          string `json:"case_id"`
		Reason          string `json:"reason"`
		EvidenceRef     string `json:"evidence_ref"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	fields, missing := trimRequire(map[string]string{
		"case id":          body.CaseID,
		"reason":           body.Reason,
		"evidence ref":     body.EvidenceRef,
		"expected version": body.ExpectedVersion,
		"user id":          body.UserID,
		"license id":       body.LicenseID,
		"old device id":    body.OldDeviceID,
		"new device id":    body.NewDeviceID,
	})
	if missing != "" {
		writeAPIError(w, http.StatusBadRequest, missing+" is required")
		return
	}
	if fields["old device id"] == fields["new device id"] {
		writeAPIError(w, http.StatusBadRequest, "old and new device must differ")
		return
	}
	if err := a.beginCase(fields["case id"]); err != nil {
		writeAdminError(w, err)
		return
	}

	state, err := a.readAccountState(r.Context(), fields["user id"])
	if err != nil {
		a.releaseCase(fields["case id"])
		writeAPIError(w, http.StatusInternalServerError, "transfer unavailable")
		return
	}
	if state.license == nil {
		a.releaseCase(fields["case id"])
		writeAPIError(w, http.StatusNotFound, "no active license for this user")
		return
	}
	deviceActive := func(id string) bool {
		for _, device := range state.devices {
			if device.ID == id {
				return device.Status == "active"
			}
		}
		return false
	}
	if !deviceActive(fields["old device id"]) || !deviceActive(fields["new device id"]) {
		a.releaseCase(fields["case id"])
		writeAPIError(w, http.StatusNotFound, "device not found for this user")
		return
	}
	if transferVersion(state) != fields["expected version"] {
		a.releaseCase(fields["case id"])
		writeAdminError(w, errAdminStaleVersion)
		return
	}

	// Close the old device's live connection before the binding moves so the
	// old Bridge can never speak for a slot it no longer holds.
	if a.registry != nil {
		a.registry.Disconnect(fields["old device id"])
	}
	err = a.entitlements.TransferDevice(r.Context(), entitlement.AdminActor{UserID: adminID},
		fields["license id"], fields["old device id"], fields["new device id"], fields["reason"])
	if err != nil {
		a.releaseCase(fields["case id"])
		writeAdminError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// recover revokes every web session, connector grant and token, and device
// credential of the account, disconnects its live Bridge connections, and
// records the recovery case — all without touching the trial window.
func (a *adminAPI) recover(w http.ResponseWriter, r *http.Request) {
	adminID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		UserID          string `json:"user_id"`
		ExpectedVersion string `json:"expected_version"`
		CaseID          string `json:"case_id"`
		Reason          string `json:"reason"`
		EvidenceRef     string `json:"evidence_ref"`
		NewIdentityID   string `json:"new_identity_id"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	fields, missing := trimRequire(map[string]string{
		"case id":          body.CaseID,
		"reason":           body.Reason,
		"evidence ref":     body.EvidenceRef,
		"expected version": body.ExpectedVersion,
		"user id":          body.UserID,
	})
	if missing != "" {
		writeAPIError(w, http.StatusBadRequest, missing+" is required")
		return
	}
	newIdentityID := strings.TrimSpace(body.NewIdentityID)
	if err := a.beginCase(fields["case id"]); err != nil {
		writeAdminError(w, err)
		return
	}

	state, err := a.readAccountState(r.Context(), fields["user id"])
	if err != nil {
		a.releaseCase(fields["case id"])
		writeAPIError(w, http.StatusInternalServerError, "recovery unavailable")
		return
	}
	if a.invisible(r.Context(), fields["user id"], state) {
		a.releaseCase(fields["case id"])
		writeAdminError(w, errAdminNotFound)
		return
	}
	if recoveryVersion(state) != fields["expected version"] {
		a.releaseCase(fields["case id"])
		writeAdminError(w, errAdminStaleVersion)
		return
	}

	// Drop every live Bridge connection first so no device keeps speaking
	// with credentials that are about to die.
	if a.registry != nil {
		for _, device := range state.devices {
			a.registry.Disconnect(device.ID)
		}
	}
	// Revoke every live connector grant together with all of its tokens.
	for _, connector := range state.connectors {
		if connector.RevokedAt != nil {
			continue
		}
		if err := a.oauth.RevokeGrant(r.Context(), connector.ID, a.now()); err != nil {
			a.releaseCase(fields["case id"])
			writeAPIError(w, http.StatusInternalServerError, "recovery unavailable")
			return
		}
	}
	// The committed recovery revokes the device credentials and every web
	// session, records the recovery case with its evidence reference, and
	// emits the single admin audit event. The trial window is never touched.
	err = a.entitlements.RecoverIdentity(r.Context(), entitlement.AdminActor{UserID: adminID},
		fields["user id"], newIdentityID, fields["reason"], fields["evidence ref"])
	if err != nil {
		a.releaseCase(fields["case id"])
		writeAdminError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// extend lengthens the existing trial entitlement's expiry only. The same
// row is updated; no second trial record can ever appear.
func (a *adminAPI) extend(w http.ResponseWriter, r *http.Request) {
	adminID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		UserID          string `json:"user_id"`
		EntitlementID   string `json:"entitlement_id"`
		NewEndsAt       string `json:"new_ends_at"`
		ExpectedVersion string `json:"expected_version"`
		CaseID          string `json:"case_id"`
		Reason          string `json:"reason"`
		EvidenceRef     string `json:"evidence_ref"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	fields, missing := trimRequire(map[string]string{
		"case id":          body.CaseID,
		"reason":           body.Reason,
		"evidence ref":     body.EvidenceRef,
		"expected version": body.ExpectedVersion,
		"user id":          body.UserID,
		"entitlement id":   body.EntitlementID,
		"new expiry":       body.NewEndsAt,
	})
	if missing != "" {
		writeAPIError(w, http.StatusBadRequest, missing+" is required")
		return
	}
	newEndsAt, err := parseUTCTimestamp(fields["new expiry"])
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "new expiry must be a UTC RFC 3339 timestamp")
		return
	}
	expectedVersion, err := parseUTCTimestamp(fields["expected version"])
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "expected version must be the current trial expiry (RFC 3339)")
		return
	}
	if err := a.beginCase(fields["case id"]); err != nil {
		writeAdminError(w, err)
		return
	}

	state, err := a.readAccountState(r.Context(), fields["user id"])
	if err != nil {
		a.releaseCase(fields["case id"])
		writeAPIError(w, http.StatusInternalServerError, "extension unavailable")
		return
	}
	if a.invisible(r.Context(), fields["user id"], state) {
		a.releaseCase(fields["case id"])
		writeAdminError(w, errAdminNotFound)
		return
	}
	decision, err := a.trialDecision(r.Context(), fields["user id"])
	if err != nil {
		a.releaseCase(fields["case id"])
		writeAPIError(w, http.StatusInternalServerError, "extension unavailable")
		return
	}
	if decision.Entitlement.ID == "" {
		a.releaseCase(fields["case id"])
		writeAPIError(w, http.StatusNotFound, "no trial entitlement for this user")
		return
	}
	if decision.Entitlement.ID != fields["entitlement id"] {
		a.releaseCase(fields["case id"])
		writeAPIError(w, http.StatusNotFound, "trial entitlement not found for this user")
		return
	}
	if !decision.Entitlement.EndsAt.Equal(expectedVersion) {
		a.releaseCase(fields["case id"])
		writeAdminError(w, errAdminStaleVersion)
		return
	}
	if !newEndsAt.After(decision.Entitlement.EndsAt) {
		a.releaseCase(fields["case id"])
		writeAPIError(w, http.StatusConflict, "extension must be later than the current expiry")
		return
	}
	err = a.entitlements.ExtendTrial(r.Context(), entitlement.AdminActor{UserID: adminID},
		fields["entitlement id"], newEndsAt, fields["reason"])
	if err != nil {
		a.releaseCase(fields["case id"])
		writeAdminError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseUTCTimestamp accepts only RFC 3339 timestamps on the UTC offset.
func parseUTCTimestamp(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	if _, offset := parsed.Zone(); offset != 0 {
		return time.Time{}, errors.New("timestamp must be UTC")
	}
	return parsed.UTC(), nil
}

// writeAdminError maps the admin sentinels and the frozen entitlement errors
// to sanitized responses.
func writeAdminError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errAdminCaseExecuted):
		writeAPIError(w, http.StatusConflict, "this case was already executed")
	case errors.Is(err, errAdminStaleVersion):
		writeAPIError(w, http.StatusConflict, "the account changed since the preview; reload and try again")
	case errors.Is(err, errAdminNotFound):
		writeAPIError(w, http.StatusNotFound, "not found")
	case errors.Is(err, entitlement.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "not found")
	case errors.Is(err, entitlement.ErrBindingNotFound):
		writeAPIError(w, http.StatusConflict, "the license slot no longer sits on the old device")
	case errors.Is(err, entitlement.ErrInvalidExtension):
		writeAPIError(w, http.StatusConflict, "extension must be later than the current expiry")
	default:
		writeAPIError(w, http.StatusInternalServerError, "admin action unavailable")
	}
}
