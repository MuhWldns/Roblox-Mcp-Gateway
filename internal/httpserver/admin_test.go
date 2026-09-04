package httpserver_test

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"robloxkit/internal/audit"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/httpserver"
	"robloxkit/internal/mysqlstore"
	"robloxkit/internal/session"
)

// eventLog records registry disconnects and privileged entitlement store
// mutations on one shared timeline so tests can assert their ordering.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) record(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

// position returns the index of the first occurrence of event, or -1.
func (l *eventLog) position(event string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, candidate := range l.events {
		if candidate == event {
			return i
		}
	}
	return -1
}

func (l *eventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// recordingRegistry wraps the shared fake registry and logs every disconnect.
type recordingRegistry struct {
	inner *fakeRegistry
	log   *eventLog
}

func (r *recordingRegistry) Online(deviceID string) bool {
	return r.inner.Online(deviceID)
}

func (r *recordingRegistry) Disconnect(deviceID string) {
	r.log.record("disconnect:" + deviceID)
	r.inner.Disconnect(deviceID)
}

// recordingEntitlementStore delegates every privileged mutation to the real
// MySQL entitlement store after logging it, preserving production semantics.
type recordingEntitlementStore struct {
	*mysqlstore.EntitlementStore
	log *eventLog
}

func (s *recordingEntitlementStore) TransferDevice(ctx context.Context, now time.Time, actor entitlement.AdminActor, licenseID, oldDeviceID, newDeviceID, reason string) error {
	s.log.record("store:TransferDevice")
	return s.EntitlementStore.TransferDevice(ctx, now, actor, licenseID, oldDeviceID, newDeviceID, reason)
}

func (s *recordingEntitlementStore) RecoverIdentity(ctx context.Context, now time.Time, actor entitlement.AdminActor, userID, newIdentityID, reason, evidenceRef string) error {
	s.log.record("store:RecoverIdentity")
	return s.EntitlementStore.RecoverIdentity(ctx, now, actor, userID, newIdentityID, reason, evidenceRef)
}

func (s *recordingEntitlementStore) ExtendTrial(ctx context.Context, actor entitlement.AdminActor, entitlementID string, newEndsAt time.Time, reason string) error {
	s.log.record("store:ExtendTrial")
	return s.EntitlementStore.ExtendTrial(ctx, actor, entitlementID, newEndsAt, reason)
}

// adminStack composes the production router with the administration surface
// enabled: one configured administrator, the recording registry, and the
// recording entitlement store over the same migrated scratch database.
type adminStack struct {
	*routerStack
	adminCookie *http.Cookie
	adminID     string
	log         *eventLog
	registry    *recordingRegistry
	fake        *fakeRegistry
	oauth       *mysqlstore.OAuthStore
}

func newAdminStack(t *testing.T) *adminStack {
	t.Helper()
	base := newRouterStack(t)
	clock := &mutableClock{now: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}
	log := &eventLog{}
	fake := base.registry
	registry := &recordingRegistry{inner: fake, log: log}
	auditSvc := audit.NewService(mysqlstore.NewAuditStore(base.db))
	entStore := &recordingEntitlementStore{
		EntitlementStore: mysqlstore.NewEntitlementStore(base.db, clock, auditSvc),
		log:              log,
	}
	entitlements := entitlement.NewService(entStore, clock)

	stack := &adminStack{
		routerStack: base,
		log:         log,
		registry:    registry,
		fake:        fake,
		oauth:       mysqlstore.NewOAuthStore(base.db),
	}
	adminUser, adminCookie := stack.login(t, "admin-operator")
	stack.adminID = adminUser.ID
	stack.adminCookie = adminCookie

	// The enrollment and dashboard reads keep sharing the same database; the
	// router is rebuilt so the admin surface, the recording registry, and the
	// recording entitlement service are all wired in.
	base.entitlements = entitlements
	base.buildRouter(t, func(cfg *httpserver.Config) {
		cfg.Registry = registry
		cfg.Admin = &httpserver.AdminConfig{
			Entitlements: entitlements,
			OAuth:        stack.oauth,
			AdminUsers:   []string{adminUser.ID},
		}
	})
	return stack
}

// postAdmin issues a session-bound admin mutation with the CSRF pair.
func (s *adminStack) postAdmin(t *testing.T, path, body string) *http.Response {
	t.Helper()
	cookies, header := mutationCookies(t, s.routerStack, s.adminCookie)
	return s.do(t, http.MethodPost, path, cookies, header, body)
}

func (s *adminStack) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (s *adminStack) nullString(t *testing.T, query string, args ...any) sql.NullString {
	t.Helper()
	var value sql.NullString
	if err := s.db.QueryRowContext(t.Context(), query, args...).Scan(&value); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return value
}

// trialFingerprint captures every trial column that must survive recovery and
// extension unchanged, byte for byte.
func (s *adminStack) trialFingerprint(t *testing.T, userID string) string {
	t.Helper()
	return s.nullString(t,
		`SELECT CONCAT_WS('|', id, CAST(started_at AS CHAR), CAST(ends_at AS CHAR), COALESCE(extension_reason,''), COALESCE(extended_by,''))
		 FROM trial_entitlements WHERE user_id = ?`, userID).String
}

// adminActions returns the persisted admin audit rows for one action.
func (s *adminStack) adminActions(t *testing.T, action string) []map[string]sql.NullString {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(),
		`SELECT COALESCE(actor_user_id,''), COALESCE(reason,''), COALESCE(target_type,''), COALESCE(target_id,''), COALESCE(CAST(before_state AS CHAR),''), COALESCE(CAST(after_state AS CHAR),'')
		 FROM admin_actions WHERE action = ? ORDER BY created_at`, action)
	if err != nil {
		t.Fatalf("query admin actions: %v", err)
	}
	defer rows.Close()
	var out []map[string]sql.NullString
	for rows.Next() {
		var (
			actor, reason, targetType, targetID, before, after sql.NullString
		)
		if err := rows.Scan(&actor, &reason, &targetType, &targetID, &before, &after); err != nil {
			t.Fatalf("scan admin action: %v", err)
		}
		out = append(out, map[string]sql.NullString{
			"actor":       actor,
			"reason":      reason,
			"target_type": targetType,
			"target_id":   targetID,
			"before":      before,
			"after":       after,
		})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate admin actions: %v", err)
	}
	return out
}

func (s *adminStack) errorBody(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	type errorPayload struct {
		Error string `json:"error"`
	}
	var payload errorPayload
	decodeJSON(t, res, &payload)
	return payload.Error
}

// ---- preview response shapes ----------------------------------------------

type adminIdentityView struct {
	Subject     string `json:"subject"`
	DisplayName string `json:"display_name"`
}

type adminDeviceView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Online  bool   `json:"online"`
	Created string `json:"created_at"`
	Updated string `json:"updated_at"`
}

type adminLicenseView struct {
	Status         string `json:"status"`
	DeviceSlots    int    `json:"device_slots"`
	ActiveBindings int    `json:"active_bindings"`
}

type adminTransferPreview struct {
	UserID   string             `json:"user_id"`
	Identity *adminIdentityView `json:"identity"`
	Devices  []adminDeviceView  `json:"devices"`
	License  *adminLicenseView  `json:"license"`
	Version  string             `json:"version"`
}

type adminRecoveryPreview struct {
	UserID     string             `json:"user_id"`
	Identity   *adminIdentityView `json:"identity"`
	Devices    []adminDeviceView  `json:"devices"`
	Connectors []struct {
		ID              string  `json:"id"`
		ClientID        string  `json:"client_id"`
		ClientName      string  `json:"client_name"`
		DeviceID        string  `json:"device_id"`
		StudioSessionID *string `json:"studio_session_id"`
		CreatedAt       string  `json:"created_at"`
		RevokedAt       *string `json:"revoked_at"`
	} `json:"connectors"`
	License *adminLicenseView `json:"license"`
	Version string            `json:"version"`
}

type adminTrialPreview struct {
	UserID   string             `json:"user_id"`
	Identity *adminIdentityView `json:"identity"`
	Trial    *struct {
		ID        string `json:"id"`
		StartedAt string `json:"started_at"`
		EndsAt    string `json:"ends_at"`
		Active    bool   `json:"active"`
	} `json:"trial"`
	Version string `json:"version"`
}

func (s *adminStack) getTransferPreview(t *testing.T, userID string) adminTransferPreview {
	t.Helper()
	res := s.do(t, http.MethodGet, "/api/v1/admin/users/"+userID+"/transfer-preview", []*http.Cookie{s.adminCookie}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("transfer preview status = %d body=%q", res.StatusCode, s.errorBody(t, res))
	}
	var preview adminTransferPreview
	decodeJSON(t, res, &preview)
	return preview
}

func (s *adminStack) getRecoveryPreview(t *testing.T, userID string) adminRecoveryPreview {
	t.Helper()
	res := s.do(t, http.MethodGet, "/api/v1/admin/users/"+userID+"/recovery-preview", []*http.Cookie{s.adminCookie}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("recovery preview status = %d body=%q", res.StatusCode, s.errorBody(t, res))
	}
	var preview adminRecoveryPreview
	decodeJSON(t, res, &preview)
	return preview
}

func (s *adminStack) getTrialPreview(t *testing.T, userID string) adminTrialPreview {
	t.Helper()
	res := s.do(t, http.MethodGet, "/api/v1/admin/users/"+userID+"/trial-preview", []*http.Cookie{s.adminCookie}, nil, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("trial preview status = %d body=%q", res.StatusCode, s.errorBody(t, res))
	}
	var preview adminTrialPreview
	decodeJSON(t, res, &preview)
	return preview
}

func (s *adminStack) transferBody(userID, licenseID, oldDevice, newDevice, version, caseID, reason, evidence string) string {
	return `{"user_id":"` + userID + `","license_id":"` + licenseID +
		`","old_device_id":"` + oldDevice + `","new_device_id":"` + newDevice +
		`","expected_version":"` + version + `","case_id":"` + caseID +
		`","reason":"` + reason + `","evidence_ref":"` + evidence + `"}`
}

func (s *adminStack) recoveryBody(userID, version, caseID, reason, evidence, newIdentityID string) string {
	return `{"user_id":"` + userID + `","expected_version":"` + version + `","case_id":"` + caseID +
		`","reason":"` + reason + `","evidence_ref":"` + evidence +
		`","new_identity_id":"` + newIdentityID + `"}`
}

func (s *adminStack) extensionBody(userID, entitlementID, newEndsAt, version, caseID, reason, evidence string) string {
	return `{"user_id":"` + userID + `","entitlement_id":"` + entitlementID +
		`","new_ends_at":"` + newEndsAt + `","expected_version":"` + version +
		`","case_id":"` + caseID + `","reason":"` + reason + `","evidence_ref":"` + evidence + `"}`
}

// seedPaidLicense provisions a user with a paid license, one bound device, and
// one free active device ready to receive the slot.
func (s *adminStack) seedPaidLicense(t *testing.T) (userID string) {
	t.Helper()
	user, _ := s.login(t, "transfer-owner")
	s.insertDevice(t, user.ID, "device-old", "Old Laptop")
	s.insertDevice(t, user.ID, "device-new", "New Laptop")
	s.insertLicense(t, "license-1", user.ID, 2)
	s.insertBinding(t, "binding-1", "license-1", user.ID, "device-old")
	return user.ID
}

// ---- authorization ---------------------------------------------------------

func TestAdminEndpointsRequireAdminAuthorization(t *testing.T) {
	stack := newAdminStack(t)
	victim := stack.seedPaidLicense(t)

	// Unauthenticated requests never reach the admin gate.
	res := stack.do(t, http.MethodGet, "/api/v1/admin/users/"+victim+"/transfer-preview", nil, nil, "")
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("preview without session status = %d, want 401", res.StatusCode)
	}

	// A signed-in non-admin is forbidden on every admin endpoint.
	_, userSession := stack.login(t, "regular-user")
	res = stack.do(t, http.MethodGet, "/api/v1/admin/users/"+victim+"/transfer-preview", []*http.Cookie{userSession}, nil, "")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin preview status = %d, want 403", res.StatusCode)
	}
	res = stack.do(t, http.MethodGet, "/api/v1/admin/users/"+victim+"/recovery-preview", []*http.Cookie{userSession}, nil, "")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin recovery preview status = %d, want 403", res.StatusCode)
	}
	res = stack.do(t, http.MethodGet, "/api/v1/admin/users/"+victim+"/trial-preview", []*http.Cookie{userSession}, nil, "")
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin trial preview status = %d, want 403", res.StatusCode)
	}
	cookies, header := mutationCookies(t, stack.routerStack, userSession)
	for path, body := range map[string]string{
		"/api/v1/admin/transfers":        stack.transferBody(victim, "license-1", "device-old", "device-new", "v", "case-1", "r", "e"),
		"/api/v1/admin/recoveries":       stack.recoveryBody(victim, "v", "case-1", "r", "e", ""),
		"/api/v1/admin/trial-extensions": stack.extensionBody(victim, "trial-1", "2026-09-25T00:00:00Z", "v", "case-1", "r", "e"),
	} {
		res = stack.do(t, http.MethodPost, path, cookies, header, body)
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("non-admin POST %s status = %d, want 403", path, res.StatusCode)
		}
	}

	// The configured administrator reads the previews; unknown users 404.
	preview := stack.getTransferPreview(t, victim)
	if preview.UserID != victim || preview.Version == "" || len(preview.Devices) != 2 {
		t.Fatalf("transfer preview = %+v", preview)
	}
	if preview.License == nil || preview.License.DeviceSlots != 2 || preview.License.ActiveBindings != 1 {
		t.Fatalf("transfer preview license = %+v", preview.License)
	}
	if preview.Identity == nil || preview.Identity.DisplayName == "" {
		t.Fatalf("transfer preview identity = %+v", preview.Identity)
	}
	for _, device := range preview.Devices {
		if device.Status != "active" || device.Created == "" || device.Updated == "" {
			t.Fatalf("transfer preview device = %+v", device)
		}
	}

	res = stack.do(t, http.MethodGet, "/api/v1/admin/users/user-does-not-exist/transfer-preview", []*http.Cookie{stack.adminCookie}, nil, "")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown user preview status = %d, want 404", res.StatusCode)
	}
}

// ---- transfer --------------------------------------------------------------

func TestAdminTransferRequiresCaseReasonEvidenceAndVersion(t *testing.T) {
	stack := newAdminStack(t)
	victim := stack.seedPaidLicense(t)
	preview := stack.getTransferPreview(t, victim)

	base := stack.transferBody(victim, "license-1", "device-old", "device-new", preview.Version, "case-100", "hardware swap", "ticket-77")
	mutations := map[string]func(string) string{
		"missing case id":  func(b string) string { return strings.Replace(b, `"case_id":"case-100"`, `"case_id":""`, 1) },
		"missing reason":   func(b string) string { return strings.Replace(b, `"reason":"hardware swap"`, `"reason":"  "`, 1) },
		"missing evidence": func(b string) string { return strings.Replace(b, `"evidence_ref":"ticket-77"`, `"evidence_ref":""`, 1) },
		"missing version": func(b string) string {
			return strings.Replace(b, `"expected_version":"`+preview.Version+`"`, `"expected_version":""`, 1)
		},
		"missing user id":    func(b string) string { return strings.Replace(b, `"user_id":"`+victim+`"`, `"user_id":""`, 1) },
		"missing license id": func(b string) string { return strings.Replace(b, `"license_id":"license-1"`, `"license_id":""`, 1) },
		"missing old device": func(b string) string {
			return strings.Replace(b, `"old_device_id":"device-old"`, `"old_device_id":""`, 1)
		},
		"missing new device": func(b string) string {
			return strings.Replace(b, `"new_device_id":"device-new"`, `"new_device_id":""`, 1)
		},
		"same device": func(b string) string {
			return strings.Replace(b, `"new_device_id":"device-new"`, `"new_device_id":"device-old"`, 1)
		},
	}
	for name, mutate := range mutations {
		res := stack.postAdmin(t, "/api/v1/admin/transfers", mutate(base))
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d body=%q, want 400", name, res.StatusCode, stack.errorBody(t, res))
		}
	}

	// Nothing executed: the binding still sits on the old device.
	if got := stack.nullString(t, `SELECT device_id FROM license_device_bindings WHERE id = 'binding-1'`).String; got != "device-old" {
		t.Fatalf("binding device after rejected requests = %q", got)
	}
	if got := stack.count(t, `SELECT COUNT(*) FROM license_transfer_requests`); got != 0 {
		t.Fatalf("transfer requests after rejected requests = %d, want 0", got)
	}
}

func TestAdminTransferMovesSlotAndClosesOldConnectionFirst(t *testing.T) {
	stack := newAdminStack(t)
	victim := stack.seedPaidLicense(t)
	stack.fake.setOnline("device-old", true)

	preview := stack.getTransferPreview(t, victim)
	var online bool
	for _, device := range preview.Devices {
		if device.ID == "device-old" && device.Online {
			online = true
		}
	}
	if !online {
		t.Fatal("transfer preview did not report the old device online")
	}

	res := stack.postAdmin(t, "/api/v1/admin/transfers",
		stack.transferBody(victim, "license-1", "device-old", "device-new", preview.Version, "case-200", "hardware swap", "ticket-200"))
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("transfer status = %d body=%q", res.StatusCode, stack.errorBody(t, res))
	}

	// The slot moved in place: same binding row, new device.
	if got := stack.nullString(t, `SELECT device_id FROM license_device_bindings WHERE id = 'binding-1'`).String; got != "device-new" {
		t.Fatalf("binding device after transfer = %q, want device-new", got)
	}
	if got := stack.count(t, `SELECT COUNT(*) FROM license_device_bindings WHERE user_id = ?`, victim); got != 1 {
		t.Fatalf("binding rows after transfer = %d, want 1 (in-place move)", got)
	}
	if got := stack.nullString(t, `SELECT status FROM license_transfer_requests WHERE user_id = ?`, victim).String; got != "completed" {
		t.Fatalf("transfer request status = %q", got)
	}
	if got := stack.nullString(t, `SELECT reason FROM license_transfer_requests WHERE user_id = ?`, victim).String; got != "hardware swap" {
		t.Fatalf("transfer request reason = %q", got)
	}

	// The old device's live connection closed BEFORE the binding moved.
	disconnect := stack.log.position("disconnect:device-old")
	transfer := stack.log.position("store:TransferDevice")
	if disconnect < 0 || transfer < 0 {
		t.Fatalf("event log = %v", stack.log.snapshot())
	}
	if disconnect >= transfer {
		t.Fatalf("disconnect (%d) must precede the binding move (%d): %v", disconnect, transfer, stack.log.snapshot())
	}
	if got := stack.fake.disconnects(); len(got) != 1 || got[0] != "device-old" {
		t.Fatalf("registry disconnects = %v, want [device-old]", got)
	}

	// Exactly one admin audit row with actor, reason, and before/after state.
	actions := stack.adminActions(t, "license.transfer_device")
	if len(actions) != 1 {
		t.Fatalf("transfer admin audit rows = %d, want 1", len(actions))
	}
	action := actions[0]
	if action["actor"].String != stack.adminID {
		t.Fatalf("transfer audit actor = %q, want %q", action["actor"].String, stack.adminID)
	}
	if action["reason"].String != "hardware swap" {
		t.Fatalf("transfer audit reason = %q", action["reason"].String)
	}
	if !strings.Contains(action["before"].String, `"device_id": "device-old"`) ||
		!strings.Contains(action["after"].String, `"device_id": "device-new"`) {
		t.Fatalf("transfer audit before/after = %q / %q", action["before"].String, action["after"].String)
	}

	// The license row itself is untouched: same slots, same binding count.
	license := stack.getTransferPreview(t, victim)
	if license.License == nil || license.License.DeviceSlots != 2 || license.License.ActiveBindings != 1 {
		t.Fatalf("license after transfer = %+v", license.License)
	}
}

func TestAdminTransferRejectsStaleVersionAndDuplicateCase(t *testing.T) {
	stack := newAdminStack(t)
	victim := stack.seedPaidLicense(t)

	// Preview, then mutate the account so the version token goes stale.
	stale := stack.getTransferPreview(t, victim).Version
	stack.exec(t, `UPDATE devices SET name = 'Renamed Laptop' WHERE id = 'device-old'`)
	res := stack.postAdmin(t, "/api/v1/admin/transfers",
		stack.transferBody(victim, "license-1", "device-old", "device-new", stale, "case-300", "hardware swap", "ticket-300"))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stale version status = %d body=%q, want 409", res.StatusCode, stack.errorBody(t, res))
	}
	// The stale attempt executed nothing.
	if got := stack.nullString(t, `SELECT device_id FROM license_device_bindings WHERE id = 'binding-1'`).String; got != "device-old" {
		t.Fatalf("binding device after stale attempt = %q, want device-old", got)
	}

	// A wrong token is rejected the same way even without a state change.
	fresh := stack.getTransferPreview(t, victim)
	if fresh.Version == stale {
		t.Fatal("version token did not change after a device rename")
	}
	res = stack.postAdmin(t, "/api/v1/admin/transfers",
		stack.transferBody(victim, "license-1", "device-old", "device-new", "forged-token", "case-301", "hardware swap", "ticket-301"))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("forged version status = %d, want 409", res.StatusCode)
	}

	// Executing with the current token succeeds.
	res = stack.postAdmin(t, "/api/v1/admin/transfers",
		stack.transferBody(victim, "license-1", "device-old", "device-new", fresh.Version, "case-300", "hardware swap", "ticket-300"))
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("transfer status = %d body=%q", res.StatusCode, stack.errorBody(t, res))
	}

	// Replaying the same case id — even with a valid, current token for the
	// reversed direction — is rejected, and nothing new executes.
	current := stack.getTransferPreview(t, victim).Version
	res = stack.postAdmin(t, "/api/v1/admin/transfers",
		stack.transferBody(victim, "license-1", "device-new", "device-old", current, "case-300", "hardware swap", "ticket-300"))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate case status = %d body=%q, want 409", res.StatusCode, stack.errorBody(t, res))
	}
	if got := stack.count(t, `SELECT COUNT(*) FROM license_transfer_requests`); got != 1 {
		t.Fatalf("transfer requests after duplicate case = %d, want 1", got)
	}
	if got := stack.count(t, `SELECT COUNT(*) FROM admin_actions WHERE action = 'license.transfer_device'`); got != 1 {
		t.Fatalf("transfer audit rows after duplicate case = %d, want 1", got)
	}

	// An unknown binding still answers with a conflict, not a mutation.
	res = stack.postAdmin(t, "/api/v1/admin/transfers",
		stack.transferBody(victim, "license-1", "device-old", "device-new", stack.getTransferPreview(t, victim).Version, "case-302", "hardware swap", "ticket-302"))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stale binding status = %d, want 409", res.StatusCode)
	}
}

func TestAdminTransferMutationRequiresCSRF(t *testing.T) {
	stack := newAdminStack(t)
	victim := stack.seedPaidLicense(t)
	preview := stack.getTransferPreview(t, victim)

	// Session cookie without the double-submit pair is forbidden before any
	// write, exactly like the other dashboard mutations.
	res := stack.do(t, http.MethodPost, "/api/v1/admin/transfers", []*http.Cookie{stack.adminCookie},
		http.Header{"Content-Type": []string{"application/json"}},
		stack.transferBody(victim, "license-1", "device-old", "device-new", preview.Version, "case-400", "r", "e"))
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("transfer without CSRF status = %d, want 403", res.StatusCode)
	}
	if got := stack.count(t, `SELECT COUNT(*) FROM license_transfer_requests`); got != 0 {
		t.Fatalf("transfer requests after CSRF rejection = %d, want 0", got)
	}
}

// ---- identity recovery -----------------------------------------------------

func TestAdminRecoveryRevokesEverythingButTrial(t *testing.T) {
	stack := newAdminStack(t)
	user, victimSession := stack.login(t, "recovery-victim")
	secondPlain, _, err := stack.sessions.Create(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	stack.insertTrial(t, user.ID)
	stack.insertDevice(t, user.ID, "device-1", "Laptop")
	stack.insertDevice(t, user.ID, "device-2", "Desktop")
	stack.insertDeviceCredential(t, user.ID, "device-1")
	stack.insertDeviceCredential(t, user.ID, "device-2")
	stack.insertConnectorClient(t)
	stack.insertConnectorGrant(t, "grant-1", user.ID, "device-1", "")
	stack.insertConnectorTokens(t, user.ID, "grant-1")
	stack.fake.setOnline("device-1", true)
	stack.fake.setOnline("device-2", true)
	trialBefore := stack.trialFingerprint(t, user.ID)

	preview := stack.getRecoveryPreview(t, user.ID)
	if preview.Version == "" || len(preview.Devices) != 2 || len(preview.Connectors) != 1 {
		t.Fatalf("recovery preview = %+v", preview)
	}

	res := stack.postAdmin(t, "/api/v1/admin/recoveries",
		stack.recoveryBody(user.ID, preview.Version, "case-500", "stolen account", "evidence-9", "identity-new-1"))
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("recovery status = %d body=%q", res.StatusCode, stack.errorBody(t, res))
	}

	// Every web session is revoked — both the login session and the extra one.
	if got := stack.count(t, `SELECT COUNT(*) FROM web_sessions WHERE user_id = ? AND revoked_at IS NULL`, user.ID); got != 0 {
		t.Fatalf("live sessions after recovery = %d, want 0", got)
	}
	for name, sessionCookie := range map[string]*http.Cookie{
		"login session":  victimSession,
		"second session": {Name: session.CookieName, Value: secondPlain},
	} {
		res := stack.do(t, http.MethodGet, "/api/v1/me", []*http.Cookie{sessionCookie}, nil, "")
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("recovered user %s still valid: /me status = %d, want 401", name, res.StatusCode)
		}
	}

	// Every device credential is revoked.
	if got := stack.count(t, `SELECT COUNT(*) FROM device_credentials WHERE user_id = ? AND revoked_at IS NULL`, user.ID); got != 0 {
		t.Fatalf("live credentials after recovery = %d, want 0", got)
	}

	// The connector grant and both of its token kinds are revoked.
	if !stack.grantRevokedAt(t, "grant-1").Valid {
		t.Fatal("connector grant was not revoked")
	}
	if got := stack.count(t, `SELECT COUNT(*) FROM oauth_access_tokens WHERE user_id = ? AND revoked_at IS NULL`, user.ID); got != 0 {
		t.Fatalf("live access tokens after recovery = %d, want 0", got)
	}
	if got := stack.count(t, `SELECT COUNT(*) FROM oauth_refresh_tokens WHERE user_id = ? AND revoked_at IS NULL`, user.ID); got != 0 {
		t.Fatalf("live refresh tokens after recovery = %d, want 0", got)
	}

	// Every live Bridge connection was dropped.
	if got := stack.fake.disconnects(); len(got) != 2 || got[0] != "device-1" || got[1] != "device-2" {
		t.Fatalf("registry disconnects = %v, want [device-1 device-2]", got)
	}

	// The recovery case is recorded with its evidence reference.
	if got := stack.count(t, `SELECT COUNT(*) FROM account_recovery_cases WHERE user_id = ? AND status = 'completed' AND reason = 'stolen account' AND evidence_ref = 'evidence-9'`, user.ID); got != 1 {
		t.Fatalf("recovery case rows = %d, want 1", got)
	}

	// The trial window is byte-identical.
	if after := stack.trialFingerprint(t, user.ID); after != trialBefore {
		t.Fatalf("trial fingerprint changed: %q -> %q", trialBefore, after)
	}

	// Exactly one admin audit row carries actor, reason, and the new identity.
	actions := stack.adminActions(t, "identity.recover")
	if len(actions) != 1 {
		t.Fatalf("recovery admin audit rows = %d, want 1", len(actions))
	}
	action := actions[0]
	if action["actor"].String != stack.adminID || action["reason"].String != "stolen account" {
		t.Fatalf("recovery audit actor/reason = %q/%q", action["actor"].String, action["reason"].String)
	}
	if !strings.Contains(action["after"].String, `"new_identity_id": "identity-new-1"`) {
		t.Fatalf("recovery audit after state = %q", action["after"].String)
	}
}

func TestAdminRecoveryRejectsStaleVersionAndDuplicateCase(t *testing.T) {
	stack := newAdminStack(t)
	user, _ := stack.login(t, "recovery-victim")
	stack.insertTrial(t, user.ID)
	stack.insertDevice(t, user.ID, "device-1", "Laptop")
	stack.insertDeviceCredential(t, user.ID, "device-1")
	stack.insertConnectorClient(t)
	stack.insertConnectorGrant(t, "grant-1", user.ID, "device-1", "")
	stack.insertConnectorTokens(t, user.ID, "grant-1")

	// Stale version: the account changed after the preview.
	stale := stack.getRecoveryPreview(t, user.ID).Version
	stack.exec(t, `UPDATE devices SET name = 'Renamed Laptop' WHERE id = 'device-1'`)
	res := stack.postAdmin(t, "/api/v1/admin/recoveries",
		stack.recoveryBody(user.ID, stale, "case-600", "stolen account", "evidence-9", ""))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stale recovery status = %d body=%q, want 409", res.StatusCode, stack.errorBody(t, res))
	}
	// Nothing was revoked by the rejected attempt.
	if got := stack.count(t, `SELECT COUNT(*) FROM device_credentials WHERE user_id = ? AND revoked_at IS NULL`, user.ID); got != 1 {
		t.Fatalf("live credentials after stale recovery = %d, want 1", got)
	}
	if stack.grantRevokedAt(t, "grant-1").Valid {
		t.Fatal("connector grant was revoked by a rejected recovery")
	}

	// Valid execution, then the same case is rejected on replay.
	fresh := stack.getRecoveryPreview(t, user.ID).Version
	res = stack.postAdmin(t, "/api/v1/admin/recoveries",
		stack.recoveryBody(user.ID, fresh, "case-600", "stolen account", "evidence-9", ""))
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("recovery status = %d body=%q", res.StatusCode, stack.errorBody(t, res))
	}
	res = stack.postAdmin(t, "/api/v1/admin/recoveries",
		stack.recoveryBody(user.ID, stack.getRecoveryPreview(t, user.ID).Version, "case-600", "stolen account", "evidence-9", ""))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate recovery case status = %d body=%q, want 409", res.StatusCode, stack.errorBody(t, res))
	}
	if got := stack.count(t, `SELECT COUNT(*) FROM account_recovery_cases WHERE user_id = ?`, user.ID); got != 1 {
		t.Fatalf("recovery cases after duplicate case = %d, want 1", got)
	}
	if got := stack.count(t, `SELECT COUNT(*) FROM admin_actions WHERE action = 'identity.recover'`); got != 1 {
		t.Fatalf("recovery audit rows after duplicate case = %d, want 1", got)
	}
}

// ---- trial extension -------------------------------------------------------

func TestAdminExtensionExtendsSameEntitlementOnly(t *testing.T) {
	stack := newAdminStack(t)
	user, _ := stack.login(t, "extension-owner")
	stack.insertTrial(t, user.ID)

	preview := stack.getTrialPreview(t, user.ID)
	if preview.Trial == nil || preview.Trial.ID != "trial-router-1" || !preview.Trial.Active {
		t.Fatalf("trial preview = %+v", preview.Trial)
	}
	if preview.Version != preview.Trial.EndsAt {
		t.Fatalf("trial version %q must mirror the current expiry %q", preview.Version, preview.Trial.EndsAt)
	}

	res := stack.postAdmin(t, "/api/v1/admin/trial-extensions",
		stack.extensionBody(user.ID, "trial-router-1", "2026-09-25T00:00:00Z", preview.Version, "case-700", "goodwill extension", "ticket-700"))
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("extension status = %d body=%q", res.StatusCode, stack.errorBody(t, res))
	}

	// The same entitlement row survived: same id and start, only ends_at moved.
	var (
		entitlementID string
		startedAt     string
		endsAt        string
		reason        sql.NullString
		extendedBy    sql.NullString
	)
	if err := stack.db.QueryRowContext(t.Context(),
		`SELECT id, CAST(started_at AS CHAR), CAST(ends_at AS CHAR), extension_reason, extended_by FROM trial_entitlements WHERE user_id = ?`, user.ID,
	).Scan(&entitlementID, &startedAt, &endsAt, &reason, &extendedBy); err != nil {
		t.Fatalf("read extended trial: %v", err)
	}
	if entitlementID != "trial-router-1" {
		t.Fatalf("entitlement id after extension = %q, want trial-router-1", entitlementID)
	}
	if startedAt != "2026-09-01 00:00:00.000000" {
		t.Fatalf("started_at after extension = %q", startedAt)
	}
	if !strings.HasPrefix(endsAt, "2026-09-25") {
		t.Fatalf("ends_at after extension = %q, want 2026-09-25", endsAt)
	}
	if reason.String != "goodwill extension" || extendedBy.String != stack.adminID {
		t.Fatalf("extension reason/by = %q/%q", reason.String, extendedBy.String)
	}

	// No second trial record was created.
	if got := stack.countTrials(t); got != 1 {
		t.Fatalf("trial rows after extension = %d, want 1", got)
	}

	// Exactly one admin audit row with the old and new expiry.
	actions := stack.adminActions(t, "trial.extend")
	if len(actions) != 1 {
		t.Fatalf("extension admin audit rows = %d, want 1", len(actions))
	}
	action := actions[0]
	if action["actor"].String != stack.adminID || action["reason"].String != "goodwill extension" {
		t.Fatalf("extension audit actor/reason = %q/%q", action["actor"].String, action["reason"].String)
	}
	if action["target_id"].String != "trial-router-1" {
		t.Fatalf("extension audit target = %q", action["target_id"].String)
	}
	if !strings.Contains(action["before"].String, "2026-09-15T00:00:00Z") ||
		!strings.Contains(action["after"].String, "2026-09-25T00:00:00Z") {
		t.Fatalf("extension audit before/after = %q / %q", action["before"].String, action["after"].String)
	}
}

func TestAdminExtensionRejectsStaleVersionMismatchAndNonLater(t *testing.T) {
	stack := newAdminStack(t)
	user, _ := stack.login(t, "extension-owner")
	stack.insertTrial(t, user.ID)

	preview := stack.getTrialPreview(t, user.ID)

	// A new expiry that is not strictly later than the current one conflicts.
	res := stack.postAdmin(t, "/api/v1/admin/trial-extensions",
		stack.extensionBody(user.ID, "trial-router-1", "2026-09-14T00:00:00Z", preview.Version, "case-800", "too early", "ticket-800"))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("non-later extension status = %d body=%q, want 409", res.StatusCode, stack.errorBody(t, res))
	}
	res = stack.postAdmin(t, "/api/v1/admin/trial-extensions",
		stack.extensionBody(user.ID, "trial-router-1", "2026-09-15T00:00:00Z", preview.Version, "case-801", "same expiry", "ticket-801"))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("same-expiry extension status = %d, want 409", res.StatusCode)
	}

	// An entitlement id that is not the user's current trial is not found.
	res = stack.postAdmin(t, "/api/v1/admin/trial-extensions",
		stack.extensionBody(user.ID, "trial-of-someone-else", "2026-09-25T00:00:00Z", preview.Version, "case-802", "wrong target", "ticket-802"))
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong entitlement status = %d body=%q, want 404", res.StatusCode, stack.errorBody(t, res))
	}

	// Nothing extended so far.
	if got := stack.nullString(t, `SELECT CAST(ends_at AS CHAR) FROM trial_entitlements WHERE user_id = ?`, user.ID).String; !strings.HasPrefix(got, "2026-09-15") {
		t.Fatalf("ends_at after rejected extensions = %q", got)
	}

	// First extension succeeds, then a replay carrying the outdated expiry is
	// rejected as stale: the admin confirmed a version that no longer exists.
	res = stack.postAdmin(t, "/api/v1/admin/trial-extensions",
		stack.extensionBody(user.ID, "trial-router-1", "2026-09-25T00:00:00Z", preview.Version, "case-803", "goodwill extension", "ticket-803"))
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("extension status = %d body=%q", res.StatusCode, stack.errorBody(t, res))
	}
	res = stack.postAdmin(t, "/api/v1/admin/trial-extensions",
		stack.extensionBody(user.ID, "trial-router-1", "2026-09-30T00:00:00Z", preview.Version, "case-804", "second extension", "ticket-804"))
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("stale version extension status = %d body=%q, want 409", res.StatusCode, stack.errorBody(t, res))
	}
	if got := stack.count(t, `SELECT COUNT(*) FROM admin_actions WHERE action = 'trial.extend'`); got != 1 {
		t.Fatalf("extension audit rows = %d, want 1", got)
	}
}

func TestAdminExtensionRequiresTrialAndValidInput(t *testing.T) {
	stack := newAdminStack(t)
	user, _ := stack.login(t, "extension-owner")

	// A user without a trial has nothing to extend.
	preview := stack.getTrialPreview(t, user.ID)
	if preview.Trial != nil {
		t.Fatalf("trial preview for trialless user = %+v", preview.Trial)
	}
	res := stack.postAdmin(t, "/api/v1/admin/trial-extensions",
		stack.extensionBody(user.ID, "trial-router-1", "2026-09-25T00:00:00Z", "2026-09-15T00:00:00Z", "case-900", "reason", "ticket-900"))
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("trialless extension status = %d, want 404", res.StatusCode)
	}

	// Malformed expiry and non-UTC offsets are rejected without any write.
	stack.insertTrial(t, user.ID)
	fresh := stack.getTrialPreview(t, user.ID)
	for name, newEndsAt := range map[string]string{
		"unparseable expiry": "not-a-timestamp",
		"non-UTC expiry":     "2026-09-25T00:00:00+02:00",
	} {
		res = stack.postAdmin(t, "/api/v1/admin/trial-extensions",
			stack.extensionBody(user.ID, "trial-router-1", newEndsAt, fresh.Version, "case-901", "reason", "ticket-901"))
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", name, res.StatusCode)
		}
	}
	res = stack.postAdmin(t, "/api/v1/admin/trial-extensions",
		stack.extensionBody(user.ID, "trial-router-1", "2026-09-25T00:00:00Z", "not-a-timestamp", "case-902", "reason", "ticket-902"))
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unparseable version status = %d, want 400", res.StatusCode)
	}
	if got := stack.nullString(t, `SELECT CAST(ends_at AS CHAR) FROM trial_entitlements WHERE user_id = ?`, user.ID).String; !strings.HasPrefix(got, "2026-09-15") {
		t.Fatalf("ends_at after rejected extensions = %q", got)
	}
}
