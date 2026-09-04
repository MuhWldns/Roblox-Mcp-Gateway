package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"robloxkit/internal/bridgehub"
	"robloxkit/internal/dashboard"
	"robloxkit/internal/entitlement"
	"robloxkit/internal/mcpoauth"
	"robloxkit/internal/session"
)

// maxDeviceNameLength matches the devices.name column.
const maxDeviceNameLength = 255

// BridgeRegistry is the dashboard's view of the live Bridge hub: presence
// for device status and an immediate disconnect when a device is revoked.
type BridgeRegistry interface {
	// Online reports whether the device currently holds a live connection.
	Online(deviceID string) bool
	// Disconnect closes the device's live connection with a safe reason.
	Disconnect(deviceID string)
}

// NewBridgeRegistry adapts the Bridge hub's connection registry to the
// dashboard's narrower view.
func NewBridgeRegistry(registry *bridgehub.Registry) BridgeRegistry {
	return hubRegistry{registry: registry}
}

type hubRegistry struct{ registry *bridgehub.Registry }

func (r hubRegistry) Online(deviceID string) bool {
	if r.registry == nil {
		return false
	}
	_, ok := r.registry.Get(deviceID)
	return ok
}

func (r hubRegistry) Disconnect(deviceID string) {
	if r.registry == nil {
		return
	}
	r.registry.Disconnect(deviceID, "device revoked")
}

// dashboardAPI serves the session-bound dashboard reads and the
// self-service mutations. Reads never expose tokens or credentials;
// mutations re-check ownership in the store on every call.
type dashboardAPI struct {
	store        dashboard.Store
	sessions     *session.Service
	identities   IdentityReader
	entitlements Entitlements
	registry     BridgeRegistry
}

func (a *dashboardAPI) online(deviceID string) bool {
	return a.registry != nil && a.registry.Online(deviceID)
}

// devices serves the owner's device list including live presence and Bridge
// operational metadata (hostname, platform, bridge version, heartbeat, MCP
// state, reconnect count, and last error). All operational fields are
// nullable: they are absent for devices that have never connected.
func (a *dashboardAPI) devices(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := a.store.Devices(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "devices unavailable")
		return
	}
	type deviceView struct {
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
	list := make([]deviceView, 0, len(rows))
	for _, row := range rows {
		v := deviceView{
			ID:             row.ID,
			Name:           row.Name,
			Hostname:       row.Hostname,
			Platform:       row.Platform,
			BridgeVersion:  row.BridgeVersion,
			Status:         row.Status,
			Online:         a.online(row.ID),
			MCPState:       row.MCPState,
			ReconnectCount: row.ReconnectCount,
			LastError:      row.LastError,
			CreatedAt:      row.CreatedAt.UTC().Format(timeFormat),
			UpdatedAt:      row.UpdatedAt.UTC().Format(timeFormat),
		}
		if row.LastHeartbeat != nil {
			ts := row.LastHeartbeat.UTC().Format(timeFormat)
			v.LastHeartbeat = &ts
		}
		list = append(list, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": list})
}

// studios serves the owner's Studio session list.
func (a *dashboardAPI) studios(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := a.store.Studios(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "studios unavailable")
		return
	}
	type studioView struct {
		ID        string  `json:"id"`
		DeviceID  string  `json:"device_id"`
		StudioID  string  `json:"studio_id"`
		Status    string  `json:"status"`
		StartedAt string  `json:"started_at"`
		EndedAt   *string `json:"ended_at"`
	}
	list := make([]studioView, 0, len(rows))
	for _, row := range rows {
		view := studioView{
			ID:        row.ID,
			DeviceID:  row.DeviceID,
			StudioID:  row.StudioID,
			Status:    row.Status,
			StartedAt: row.StartedAt.UTC().Format(timeFormat),
		}
		if row.EndedAt != nil {
			ended := row.EndedAt.UTC().Format(timeFormat)
			view.EndedAt = &ended
		}
		list = append(list, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"studios": list})
}

// connectors serves the owner's connector grants with client names, target
// device and Studio, and revocation state. No token value ever appears.
func (a *dashboardAPI) connectors(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := a.store.Connectors(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "connectors unavailable")
		return
	}
	type connectorView struct {
		ID              string   `json:"id"`
		ClientID        string   `json:"client_id"`
		ClientName      string   `json:"client_name"`
		Scopes          []string `json:"scopes"`
		Resource        string   `json:"resource"`
		DeviceID        string   `json:"device_id"`
		StudioSessionID *string  `json:"studio_session_id"`
		CreatedAt       string   `json:"created_at"`
		RevokedAt       *string  `json:"revoked_at"`
	}
	list := make([]connectorView, 0, len(rows))
	for _, row := range rows {
		view := connectorView{
			ID:         row.ID,
			ClientID:   row.ClientID,
			ClientName: row.ClientName,
			Scopes:     row.Scopes,
			Resource:   row.Resource,
			DeviceID:   row.DeviceID,
			CreatedAt:  row.CreatedAt.UTC().Format(timeFormat),
		}
		if view.Scopes == nil {
			view.Scopes = []string{}
		}
		if row.StudioSessionID != "" {
			studio := row.StudioSessionID
			view.StudioSessionID = &studio
		}
		if row.RevokedAt != nil {
			revoked := row.RevokedAt.UTC().Format(timeFormat)
			view.RevokedAt = &revoked
		}
		list = append(list, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"connectors": list})
}
// license mirrors the /api/v1/me trial conventions, adds the paid-license
// slot state, and includes owner identity, subscription, transfer, recovery,
// and usage counters.
func (a *dashboardAPI) license(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	identity, err := a.identities.RobloxIdentity(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "license unavailable")
		return
	}
	decision, err := a.entitlements.Authorize(r.Context(), entitlement.Subject{
		UserID: userID, Provider: "roblox", ProviderSubject: identity.Subject,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "license unavailable")
		return
	}
	var trial any
	if decision.Entitlement.ID != "" {
		trial = map[string]any{
			"active":     decision.Active,
			"started_at": decision.Entitlement.StartedAt.UTC().Format(timeFormat),
			"ends_at":    decision.Entitlement.EndsAt.UTC().Format(timeFormat),
		}
	}
	row, err := a.store.License(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "license unavailable")
		return
	}
	var license any
	if row != nil {
		license = map[string]any{
			"status":          row.Status,
			"device_slots":    row.DeviceSlots,
			"active_bindings": row.ActiveBindings,
			"roblox_username": row.RobloxUsername,
			"license_id":      row.LicenseID,
			"subscription_id": row.SubscriptionID,
			"subscription":    row.SubscriptionState,
			"usage_last_30d":  row.UsageLast30Days,
			"usage_last_7d":   row.UsageLast7Days,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"trial": trial, "license": license})
}

// diagnostics serves the owner's service-side diagnostics summary: database
// reachability, registered device count, live connections, active Studio
// sessions, and per-device operational state. The body stays secret-free.
func (a *dashboardAPI) diagnostics(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	rows, err := a.store.Devices(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "diagnostics unavailable")
		return
	}
	online := 0
	devices := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		isOnline := a.online(row.ID)
		if isOnline {
			online++
		}
		dev := map[string]any{
			"id":              row.ID,
			"name":            row.Name,
			"status":          row.Status,
			"online":          isOnline,
			"mcp_state":       row.MCPState,
			"reconnect_count": row.ReconnectCount,
		}
		if row.Hostname != nil {
			dev["hostname"] = *row.Hostname
		}
		if row.BridgeVersion != nil {
			dev["bridge_version"] = *row.BridgeVersion
		}
		if row.LastHeartbeat != nil {
			dev["last_heartbeat"] = row.LastHeartbeat.UTC().Format(timeFormat)
		}
		if row.LastError != nil {
			dev["last_error"] = *row.LastError
		}
		devices = append(devices, dev)
	}
	activeStudios, err := a.store.StudioSessionsActive(r.Context(), userID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "diagnostics unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"database":               "ok",
		"devices_registered":     len(rows),
		"devices_online":         online,
		"studio_sessions_active": activeStudios,
		"devices":                devices,
	})
}

// renameDevice renames an owned device.
func (a *dashboardAPI) renameDevice(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > maxDeviceNameLength {
		writeAPIError(w, http.StatusBadRequest, "invalid device name")
		return
	}
	if err := a.store.RenameDevice(r.Context(), requestIDFromContext(r.Context()), userID, r.PathValue("device_id"), name); err != nil {
		writeDashboardMutationError(w, err, "rename unavailable", "device not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revokeDevice revokes an owned device, its credentials, and the live
// connection. The license slot deliberately stays occupied.
func (a *dashboardAPI) revokeDevice(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	deviceID := r.PathValue("device_id")
	if err := a.store.RevokeDevice(r.Context(), requestIDFromContext(r.Context()), time.Now().UTC(), userID, deviceID); err != nil {
		writeDashboardMutationError(w, err, "revoke unavailable", "device not found")
		return
	}
	if a.registry != nil {
		a.registry.Disconnect(deviceID)
	}
	w.WriteHeader(http.StatusNoContent)
}

// rotateDeviceCredential replaces the active credential for an owned,
// active device with a new opaque token, audits the change, and returns
// the new plaintext credential. The caller must deliver it to the device
// out-of-band.
func (a *dashboardAPI) rotateDeviceCredential(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	deviceID := r.PathValue("device_id")
	plain, err := a.store.RotateDeviceCredential(
		r.Context(),
		requestIDFromContext(r.Context()),
		userID,
		deviceID,
	)
	if err != nil {
		writeDashboardMutationError(w, err, "credential rotation unavailable", "device not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"credential": plain})
}

// setConnectorTarget repoints an owned connector grant.
func (a *dashboardAPI) setConnectorTarget(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		DeviceID        string `json:"device_id"`
		StudioSessionID string `json:"studio_session_id"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.DeviceID) == "" {
		writeAPIError(w, http.StatusBadRequest, "device id is required")
		return
	}
	if err := a.store.SetConnectorTarget(r.Context(), requestIDFromContext(r.Context()), userID,
		r.PathValue("grant_id"), strings.TrimSpace(body.DeviceID), strings.TrimSpace(body.StudioSessionID)); err != nil {
		writeDashboardMutationError(w, err, "target unavailable", "connector or target not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revokeConnector revokes an owned connector grant and all of its tokens.
func (a *dashboardAPI) revokeConnector(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := a.store.RevokeConnector(r.Context(), requestIDFromContext(r.Context()), time.Now().UTC(),
		userID, r.PathValue("grant_id")); err != nil {
		writeDashboardMutationError(w, err, "revoke unavailable", "connector not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revokeAllSessions revokes every web session of the signed-in user and
// clears the session cookie.
func (a *dashboardAPI) revokeAllSessions(w http.ResponseWriter, r *http.Request) {
	userID, err := sessionUserID(r)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := a.sessions.RevokeAll(r.Context(), userID); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "logout unavailable")
		return
	}
	http.SetCookie(w, session.Cookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

// writeDashboardMutationError maps store results to sanitized responses:
// ownership misses answer 404 with the same message whether the object is
// missing or foreign.
func writeDashboardMutationError(w http.ResponseWriter, err error, internal, notFound string) {
	if errors.Is(err, dashboard.ErrNotFound) {
		writeAPIError(w, http.StatusNotFound, notFound)
		return
	}
	writeAPIError(w, http.StatusInternalServerError, internal)
}

// decodeMutationBody decodes exactly one JSON document from a mutation
// request. Oversized bodies answer 413.
func decodeMutationBody(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// serveProtectedResourceMetadata publishes the RFC 9728 document for the
// /mcp resource. The document is public: no session, no cookies.
func serveProtectedResourceMetadata(meta mcpoauth.Metadata) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, meta.ProtectedResource())
	}
}

// serveAuthorizationServerMetadata publishes the RFC 8414 document for the
// connector authorization server. The document is public as well.
func serveAuthorizationServerMetadata(meta mcpoauth.Metadata) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, meta.AuthorizationServer())
	}
}
