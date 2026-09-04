package e2egate

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"robloxkit/internal/device"
	"robloxkit/internal/mcpoauth"
	"robloxkit/internal/statusui"
)

// The 14-row production matrix. Rows run sequentially against one live stack
// on one migrated scratch database; each row owns its fresh users/devices.
// Rows 4 and 5 are the DEFERRED real ChatGPT/Claude host flows with the
// identical local substitute through the real fosite endpoints.
func TestE2EProductionMatrix(t *testing.T) {
	st := newLiveStack(t)
	claim := func(name string) device.DeviceClaim {
		return device.DeviceClaim{
			DeviceID:      gateUUID(t), // devices.id is CHAR(36)
			Name:          name,
			Hostname:      "E2EGATE-" + name,
			Platform:      "windows",
			BridgeVersion: "e2egate",
		}
	}

	// The admin identity is pre-seeded in the stack; its login is the admin
	// session every admin row uses.
	adminSession := st.login("subject-admin")

	t.Run("row01_new_user_login_trial_absent", func(t *testing.T) {
		st.useT(t)
		user := st.login("subject-row1")
		status, me := user.getJSON(st.base + "/api/v1/me")
		if status != http.StatusOK {
			t.Fatalf("me status = %d", status)
		}
		if trial, ok := me["trial"]; ok && trial != nil {
			t.Fatalf("new user trial must be absent, got %v", trial)
		}
	})

	t.Run("row02_authenticated_download_trial_still_absent", func(t *testing.T) {
		st.useT(t)
		user := st.login("subject-row1")
		resp := user.do(http.MethodGet, st.base+"/api/v1/bridge/download", nil, nil)
		body := st.readBody(resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("download status = %d", resp.StatusCode)
		}
		if string(body) != "e2egate bridge artifact bytes" {
			t.Fatalf("download body = %q", body)
		}
		status, me := user.getJSON(st.base + "/api/v1/me")
		if status != http.StatusOK {
			t.Fatalf("me status = %d", status)
		}
		if trial, ok := me["trial"]; ok && trial != nil {
			t.Fatalf("trial must remain absent after download, got %v", trial)
		}
	})

	t.Run("row03_first_enrollment_creates_trial_device_credential_atomically", func(t *testing.T) {
		st.useT(t)
		user := st.login("subject-row3")
		userID := st.latestUserID()
		credentialToken, deviceID := st.enroll(user, claim("workstation-row3"))
		if credentialToken == "" || deviceID == "" {
			t.Fatal("enrollment returned no credential")
		}
		// The committed contract: one 14x24h trial, the device, and its
		// credential — atomically. The paid slot binding belongs to the
		// license surface and is never created by enrollment.
		trials := st.queryRows("SELECT id, started_at, ends_at, TIMESTAMPDIFF(HOUR, started_at, ends_at) AS hours FROM trial_entitlements WHERE user_id = ?", userID)
		if len(trials) != 1 {
			t.Fatalf("want exactly 1 trial, got %d", len(trials))
		}
		hours, ok := trials[0]["hours"].(int64)
		if !ok || hours != 336 {
			t.Fatalf("trial window must be 14x24h = 336h, got %v", trials[0]["hours"])
		}
		if n := st.queryInt("SELECT COUNT(*) FROM devices WHERE user_id = ? AND status = 'active'", userID); n != 1 {
			t.Fatalf("want exactly 1 active device, got %d", n)
		}
		if n := st.queryInt("SELECT COUNT(*) FROM device_credentials WHERE user_id = ? AND revoked_at IS NULL", userID); n != 1 {
			t.Fatalf("want exactly 1 live device credential, got %d", n)
		}
		if n := st.queryInt("SELECT COUNT(*) FROM license_device_bindings WHERE user_id = ?", userID); n != 0 {
			t.Fatalf("enrollment must never create a paid slot binding, got %d", n)
		}
	})

	t.Run("row04_chatgpt_real_host_deferred_connector_oauth_mcp_path", func(t *testing.T) {
		st.useT(t)
		// DEFERRED by design: real ChatGPT requires a deployed TLS domain.
		// Substitute: the identical OAuth + initialize + tools/list +
		// read-only tools/call path through the real fosite /oauth endpoints
		// and the real /mcp gateway, relayed through a live trial bridge.
		user := st.login("subject-row4")
		credentialToken, deviceID := st.enroll(user, claim("workstation-row4"))
		st.seedStudio(deviceID, st.latestUserID(), "studio-row4")
		runner := st.startBridge(credentialToken, deviceID, "row4 device", 1)
		runner.awaitConnected(15 * time.Second)
		st.awaitRegistry(deviceID, true, 5*time.Second)

		clientID := st.registerConnector("https://chatgpt.com/aip/connector-e2egate", "https://chatgpt.com/aip/oauth/callback")
		flow := st.connectorFlow(user, clientID, "https://chatgpt.com/aip/oauth/callback", deviceID, st.studioSessionID(deviceID, "studio-row4"))
		flow.openMCPSession()
		envelope, _, err := flow.call("tools/list", `{}`)
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		var catalog struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(envelope["result"], &catalog); err != nil || len(catalog.Tools) == 0 {
			t.Fatalf("tools/list result %s: %v", envelope["result"], err)
		}
		if catalog.Tools[0].Name != "get_instance_tree" {
			t.Fatalf("tools[0] = %q, want get_instance_tree", catalog.Tools[0].Name)
		}
		text := flow.callText("tools/call", `{"name":"get_instance_tree","arguments":{"text":"tree"}}`)
		if text != "studio ok" {
			t.Fatalf("read-only call text = %q, want \"studio ok\" from the selected Studio", text)
		}
	})

	t.Run("row05_claude_real_host_deferred_connector_oauth_mcp_path", func(t *testing.T) {
		st.useT(t)
		// DEFERRED by design (same ruling as row 4): the identical
		// independent connector flow under a second client identity.
		user := st.login("subject-row5")
		credentialToken, deviceID := st.enroll(user, claim("workstation-row5"))
		st.seedStudio(deviceID, st.latestUserID(), "studio-row5")
		runner := st.startBridge(credentialToken, deviceID, "row5 device", 1)
		runner.awaitConnected(15 * time.Second)
		clientID := st.registerConnector("https://claude.ai/api/mcp/e2egate", "https://claude.ai/oauth/callback")
		flow := st.connectorFlow(user, clientID, "https://claude.ai/oauth/callback", deviceID, st.studioSessionID(deviceID, "studio-row5"))
		flow.openMCPSession()
		text := flow.callText("tools/call", `{"name":"get_instance_tree","arguments":{"text":"tree"}}`)
		if text != "studio ok" {
			t.Fatalf("read-only call text = %q", text)
		}
	})

	t.Run("row06_wrong_account_and_cross_user_device_studio_denials", func(t *testing.T) {
		st.useT(t)
		// (a) Wrong Roblox account: device A's credential presented on
		// device B's identity is refused before any validated envelope.
		userB := st.login("subject-row6b")
		credB, deviceB := st.enroll(userB, claim("workstation-row6b"))
		runnerB := st.startBridge(credB, deviceB, "row6 B", 1)
		runnerB.awaitConnected(15 * time.Second)

		userA := st.login("subject-row6a")
		credA, deviceA := st.enroll(userA, claim("workstation-row6a"))
		impostor := st.startBridge(credA, deviceB, "impostor", 1)
		time.Sleep(2 * time.Second)
		if impostor.events.count(statusui.Fatal) != 0 {
			t.Fatalf("impostor run must stay in retry, not fatal; states %v", impostor.events.states())
		}
		if impostor.events.count(statusui.Reconnecting) == 0 {
			t.Fatalf("impostor must be cycling through reconnect attempts; states %v", impostor.events.states())
		}
		if st.authenticatedEnvelopeCount(deviceA) != 0 {
			t.Fatal("impostor identity must never deliver a validated envelope")
		}
		impostor.cancel()
		st.awaitRegistry(deviceA, false, 5*time.Second)
		st.awaitRegistry(deviceB, true, 3*time.Second)

		// (b) Cross-user device target: user B's consent cannot target
		// user A's device.
		clientID := st.registerConnector("https://evil.example/connector", "https://evil.example/callback")
		authorize := st.authorizeValues(clientID, "https://evil.example/callback")
		resp := userB.do(http.MethodGet, st.base+"/oauth/authorize?"+authorize.Encode(), nil, nil)
		_ = st.readBody(resp)
		form := authorize
		form.Set("action", "approve")
		form.Set("device_id", deviceA)
		form["grant"] = []string{"mcp:connect"}
		resp = userB.do(http.MethodPost, st.base+"/oauth/authorize", strings.NewReader(form.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		_ = st.readBody(resp)
		loc, _ := resp.Location()
		if resp.StatusCode != http.StatusSeeOther || loc == nil || loc.Query().Get("error") != "access_denied" {
			t.Fatalf("cross-user device consent must redirect with access_denied, got status %d loc %v", resp.StatusCode, loc)
		}

		// (c) Cross-Studio target: another user's Studio session is denied
		// at consent the same way.
		form = authorize
		form.Set("action", "approve")
		form.Set("device_id", deviceB)
		form.Set("studio_session_id", "ss-not-user-bs")
		form["grant"] = []string{"mcp:connect"}
		resp = userB.do(http.MethodPost, st.base+"/oauth/authorize", strings.NewReader(form.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		_ = st.readBody(resp)
		loc, _ = resp.Location()
		if resp.StatusCode != http.StatusSeeOther || loc == nil || loc.Query().Get("error") != "access_denied" {
			t.Fatalf("cross-Studio consent must redirect with access_denied, got status %d loc %v", resp.StatusCode, loc)
		}

		// (d) License-only access loses MCP the moment its slot binding
		// goes away (the transfer shape): per-call reauthorization denies.
		licUser, licDevice, _ := st.seedLicensedOnlyUser("subject-row6c")
		staleToken := st.seedGrantToken(licUser, licDevice)
		deviceNew := gateUUID(t)
		st.exec("INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, 'row6 new', 'active')", deviceNew, licUser)
		preview := st.adminPreview(adminSession, "transfer", licUser)
		version, _ := preview["version"].(string)
		st.adminExecute(adminSession, "/api/v1/admin/transfers", map[string]string{
			"user_id": licUser, "license_id": st.licenseID(licUser),
			"old_device_id": licDevice, "new_device_id": deviceNew,
			"expected_version": version, "case_id": "case-row6",
			"reason": "row6 stale binding", "evidence_ref": "e2egate",
		})
		status, sessionID, _ := st.postMCP(staleToken, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`)
		listStatus, _, listMessages := st.postMCP(staleToken, sessionID, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
		denied := listStatus != http.StatusOK
		if !denied && len(listMessages) > 0 {
			var probe map[string]json.RawMessage
			_ = json.Unmarshal(listMessages[0], &probe)
			_, denied = probe["error"]
		}
		if !denied {
			t.Fatalf("license-only relay without its binding must be denied (initialize %d, list %d)", status, listStatus)
		}
	})

	t.Run("row07_expired_trial_blocks_gated_paths_dashboard_download_stay", func(t *testing.T) {
		st.useT(t)
		userID := gateUUID(t)
		st.exec("INSERT INTO users (id) VALUES (?)", userID)
		st.exec("INSERT INTO user_identities (id, user_id, provider, provider_subject, display_name, status) VALUES (?, ?, 'roblox', 'subject-row7', 'Expired Fixture', 'active')",
			gateUUID(t), userID)
		st.exec("INSERT INTO trial_entitlements (id, user_id, started_at, ends_at) VALUES (?, ?, ?, ?)",
			gateUUID(t), userID, time.Now().UTC().Add(-15*24*time.Hour), time.Now().UTC().Add(-24*time.Hour))
		deviceID := gateUUID(t)
		credentialToken := st.seedDeviceCredential(userID, deviceID)

		// Enrollment is blocked: the one-time trial cannot be re-created.
		sessionUser := st.login("subject-row7")
		st.enrollExpectStatus(sessionUser, claim("late-row7"), http.StatusForbidden)

		// WSS is blocked: the expired trial with no paid license is refused.
		runner := st.startBridge(credentialToken, deviceID, "expired device", 1)
		select {
		case err := <-runner.result:
			if err == nil {
				t.Fatal("expired-trial WSS dial must fail terminally")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the expired-trial dial rejection")
		}
		if runner.events.count(statusui.Connected) != 0 {
			t.Fatalf("expired trial must never connect; states %v", runner.events.states())
		}

		// Connector authorization is blocked at consent (expired window).
		clientID := st.registerConnector("https://expired.example/connector", "https://expired.example/callback")
		if !st.connectorFlowExpectDenial(sessionUser, clientID) {
			t.Fatal("consent must deny the expired trial")
		}

		// MCP is blocked for a seeded token.
		token := st.seedGrantToken(userID, deviceID)
		mcpStatus, _, _ := st.postMCP(token, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`)
		if mcpStatus == http.StatusOK {
			t.Fatal("expired-trial MCP initialize must be denied")
		}

		// Dashboard + download stay available.
		status, _ := sessionUser.getJSON(st.base + "/api/v1/me")
		if status != http.StatusOK {
			t.Fatalf("me must stay available, got %d", status)
		}
		resp := sessionUser.do(http.MethodGet, st.base+"/api/v1/bridge/download", nil, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("download must stay available, got %d", resp.StatusCode)
		}
		_ = st.readBody(resp)
	})

	t.Run("row08_reinstall_revoke_transfer_recovery_never_second_trial", func(t *testing.T) {
		st.useT(t)
		user := st.login("subject-row8")
		userID := st.latestUserID()
		credential1, device1 := st.enroll(user, claim("workstation-row8-first"))
		_ = credential1
		fingerprint := st.trialFingerprint(userID)
		if len(fingerprint) != 1 {
			t.Fatalf("want exactly one trial after first enrollment, got %d", len(fingerprint))
		}

		// (a) Reinstall/re-enroll: the one-time trial refuses a second
		// enrollment with 403 and no second trial row appears.
		st.exec("UPDATE device_credentials SET revoked_at = NOW(6) WHERE device_id = ?", device1)
		st.enrollExpectStatus(user, claim("workstation-row8-reinstall"), http.StatusForbidden)
		if got := st.trialFingerprint(userID); len(got) != 1 || fmt.Sprint(got[0]) != fmt.Sprint(fingerprint[0]) {
			t.Fatalf("re-enroll changed the trial: was %v now %v", fingerprint, got)
		}

		// (b) Admin transfer to a second (seeded) device: same trial.
		device2 := gateUUID(t)
		st.exec("INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, 'row8 second', 'active')", device2, userID)
		st.seedLicenseAndBinding(userID, device1)
		preview := st.adminPreview(adminSession, "transfer", userID)
		version, _ := preview["version"].(string)
		st.adminExecute(adminSession, "/api/v1/admin/transfers", map[string]string{
			"user_id": userID, "license_id": st.licenseID(userID),
			"old_device_id": device1, "new_device_id": device2,
			"expected_version": version, "case_id": "case-row8-transfer",
			"reason": "row8 matrix transfer", "evidence_ref": "e2egate",
		})
		if got := st.trialFingerprint(userID); len(got) != 1 || fmt.Sprint(got[0]) != fmt.Sprint(fingerprint[0]) {
			t.Fatalf("transfer changed the trial: was %v now %v", fingerprint, got)
		}

		// (c) Admin recovery: revoke everything, trial timestamps preserved.
		preview = st.adminPreview(adminSession, "recovery", userID)
		version, _ = preview["version"].(string)
		st.adminExecute(adminSession, "/api/v1/admin/recoveries", map[string]string{
			"user_id": userID, "expected_version": version,
			"case_id": "case-row8-recovery", "reason": "row8 matrix recovery",
			"evidence_ref": "e2egate",
		})
		if got := st.trialFingerprint(userID); len(got) != 1 || fmt.Sprint(got[0]) != fmt.Sprint(fingerprint[0]) {
			t.Fatalf("recovery changed the trial: was %v now %v", fingerprint, got)
		}
	})

	t.Run("row09_admin_transfer_closes_old_connection_history_preserved", func(t *testing.T) {
		st.useT(t)
		// License-only fixture: expired trial (append-only, seeded) + paid
		// license + slot binding, so the binding move is the WSS lifeline.
		userID, deviceOld, credentialOld := st.seedLicensedOnlyUser("subject-row9")
		deviceNew := gateUUID(t)
		st.exec("INSERT INTO devices (id, user_id, name, status) VALUES (?, ?, 'row9 new', 'active')", deviceNew, userID)
		runner := st.startBridge(credentialOld, deviceOld, "row9 old device", 1)
		runner.awaitConnected(15 * time.Second)
		st.awaitRegistry(deviceOld, true, 5*time.Second)
		trialBefore := st.trialFingerprint(userID)

		preview := st.adminPreview(adminSession, "transfer", userID)
		version, _ := preview["version"].(string)
		st.adminExecute(adminSession, "/api/v1/admin/transfers", map[string]string{
			"user_id": userID, "license_id": st.licenseID(userID),
			"old_device_id": deviceOld, "new_device_id": deviceNew,
			"expected_version": version, "case_id": "case-row9",
			"reason": "hardware swap", "evidence_ref": "e2egate",
		})
		st.awaitRegistry(deviceOld, false, 5*time.Second)
		bindingDevice := st.queryString("SELECT device_id FROM license_device_bindings WHERE user_id = ? AND status = 'active'", userID)
		if bindingDevice != deviceNew {
			t.Fatalf("binding must now target %s, got %s", deviceNew, bindingDevice)
		}
		if n := st.queryInt("SELECT COUNT(*) FROM license_transfer_requests WHERE status = 'completed' AND user_id = ?", userID); n != 1 {
			t.Fatalf("want 1 completed transfer request row, got %d", n)
		}
		if n := st.awaitAudit("license.transfer_device", st.adminUserID, 5*time.Second); n < 1 {
			t.Fatalf("want the transfer admin_actions row, got %d", n)
		}
		// History preserved: the trial rows are untouched, and the old device
		// reconnect is refused (license-only, binding moved away).
		if got := st.trialFingerprint(userID); fmt.Sprint(got) != fmt.Sprint(trialBefore) {
			t.Fatalf("trial history changed: was %v now %v", trialBefore, got)
		}
		oldReconnect := st.startBridge(credentialOld, deviceOld, "row9 old device reconnect", 1)
		select {
		case err := <-oldReconnect.result:
			if err == nil {
				t.Fatal("old device reconnect must be refused after the transfer")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for the post-transfer reconnect refusal")
		}
	})

	t.Run("row10_admin_recovery_revokes_surface_preserves_trial", func(t *testing.T) {
		st.useT(t)
		user := st.login("subject-row10")
		userID := st.latestUserID()
		credentialToken, deviceID := st.enroll(user, claim("workstation-row10"))
		st.seedStudio(deviceID, userID, "studio-row10")
		clientID := st.registerConnector("https://row10.example/connector", "https://row10.example/callback")
		_ = st.connectorFlow(user, clientID, "https://row10.example/callback", deviceID, "")
		runner := st.startBridge(credentialToken, deviceID, "row10 device", 1)
		runner.awaitConnected(15 * time.Second)
		fingerprint := st.trialFingerprint(userID)
		if len(fingerprint) != 1 {
			t.Fatalf("want one trial row, got %d", len(fingerprint))
		}

		preview := st.adminPreview(adminSession, "recovery", userID)
		version, _ := preview["version"].(string)
		st.adminExecute(adminSession, "/api/v1/admin/recoveries", map[string]string{
			"user_id": userID, "expected_version": version,
			"case_id": "case-row10", "reason": "identity recovery",
			"evidence_ref": "e2egate",
		})
		st.awaitRegistry(deviceID, false, 5*time.Second)
		status, _ := user.getJSON(st.base + "/api/v1/me")
		if status != http.StatusUnauthorized {
			t.Fatalf("recovered session must be 401, got %d", status)
		}
		if n := st.queryInt("SELECT COUNT(*) FROM oauth_access_tokens oa JOIN oauth_grants g ON oa.grant_id = g.id WHERE g.user_id = ? AND oa.revoked_at IS NULL", userID); n != 0 {
			t.Fatalf("want 0 live access tokens, got %d", n)
		}
		if n := st.queryInt("SELECT COUNT(*) FROM oauth_grants WHERE user_id = ? AND revoked_at IS NULL", userID); n != 0 {
			t.Fatalf("want 0 live grants, got %d", n)
		}
		if n := st.queryInt("SELECT COUNT(*) FROM device_credentials WHERE user_id = ? AND revoked_at IS NULL", userID); n != 0 {
			t.Fatalf("want 0 live device credentials, got %d", n)
		}
		if got := st.trialFingerprint(userID); fmt.Sprint(got) != fmt.Sprint(fingerprint) {
			t.Fatalf("trial fingerprint changed: was %v now %v", fingerprint, got)
		}
		if n := st.awaitAudit("identity.recover", st.adminUserID, 5*time.Second); n < 1 {
			t.Fatal("want the recovery admin_actions row")
		}
	})

	t.Run("row11_multi_studio_ambiguity_denied_explicit_succeeds", func(t *testing.T) {
		st.useT(t)
		user := st.login("subject-row11")
		userID := st.latestUserID()
		credentialToken, deviceID := st.enroll(user, claim("workstation-row11"))
		st.seedStudio(deviceID, userID, "studio-ambiguity-a")
		st.seedStudio(deviceID, userID, "studio-ambiguity-b")
		runner := st.startBridge(credentialToken, deviceID, "row11 device", 2)
		runner.awaitConnected(15 * time.Second)

		clientID := st.registerConnector("https://ambiguity.example/connector", "https://ambiguity.example/callback")
		flow := st.connectorFlow(user, clientID, "https://ambiguity.example/callback", deviceID, "")
		flow.openMCPSession()
		code, message := flow.callError("tools/call", `{"name":"get_instance_tree","arguments":{"text":"x"}}`)
		if code == 0 {
			t.Fatal("ambiguous relay must fail with a JSON-RPC error")
		}
		t.Logf("ambiguity denial: code=%d message=%q", code, message)

		grantID := st.resolveGrantID(user, clientID)
		target := st.studioSessionID(deviceID, "studio-ambiguity-a")
		status, out := user.postJSON(st.base+"/api/v1/connectors/"+grantID+"/target", map[string]string{
			"device_id": deviceID, "studio_session_id": target,
		})
		if status != http.StatusOK && status != http.StatusNoContent {
			t.Fatalf("set target status = %d: %v", status, out)
		}
		text := flow.callText("tools/call", `{"name":"get_instance_tree","arguments":{"text":"tree"}}`)
		if text != "studio ok" {
			t.Fatalf("explicit-target call text = %q", text)
		}
	})

	t.Run("row12_disconnect_and_child_crash_produce_no_replay", func(t *testing.T) {
		st.useT(t)
		// (a) A live Bridge connection disappears with one uniquely tagged
		// connector request already inside the old child. The same supervisor
		// must establish a replacement connection and child, process a fresh
		// connector request, and never deliver the old tag to that child.
		user := st.login("subject-row12")
		credentialToken, deviceID := st.enroll(user, claim("workstation-row12"))
		st.seedStudio(deviceID, st.latestUserID(), "studio-row12")
		clientID := st.registerConnector("https://replay.example/connector", "https://replay.example/callback")
		runner := st.startBridge(credentialToken, deviceID, "row12 device", 1)
		runner.awaitConnected(15 * time.Second)
		flow := st.connectorFlow(user, clientID, "https://replay.example/callback", deviceID, st.studioSessionID(deviceID, "studio-row12"))
		flow.openMCPSession()

		oldDisconnectSentinel := "row12-disconnect-old"
		newDisconnectSentinel := "row12-disconnect-new"
		oldChild := runner.currentProcess()
		oldChild.setHoldRequests(true)
		oldCallDone := make(chan error, 1)
		go func() {
			envelope, _, callErr := flow.call("tools/call", fmt.Sprintf(`{"name":"get_instance_tree","arguments":{"text":%q}}`, oldDisconnectSentinel))
			if callErr == nil && envelope != nil {
				if rawErr, isErr := envelope["error"]; isErr {
					callErr = fmt.Errorf("relay failure: %s", rawErr)
				} else {
					callErr = fmt.Errorf("unexpected success: %s", envelope["result"])
				}
			}
			oldCallDone <- callErr
		}()
		oldChild.awaitToolCallArgument(t, oldDisconnectSentinel, 5*time.Second)
		disconnectCursor := runner.events.cursor()

		st.registry.Disconnect(deviceID, "row12 deliberate disconnect")
		select {
		case callErr := <-oldCallDone:
			if callErr == nil {
				t.Fatal("the in-flight call must fail when the Bridge disconnects")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the in-flight call hung after the Bridge disconnected")
		}

		runner.awaitChildCount(2, 20*time.Second)
		runner.awaitStateAfter(statusui.Connected, disconnectCursor, 20*time.Second)
		replacementChild := runner.currentProcess()
		if text := flow.callText("tools/call", fmt.Sprintf(`{"name":"get_instance_tree","arguments":{"text":%q}}`, newDisconnectSentinel)); text != "studio ok" {
			t.Fatalf("post-disconnect barrier call text = %q", text)
		}
		if !replacementChild.hasToolCallArgument(newDisconnectSentinel) {
			t.Fatalf("replacement child did not record fresh sentinel %q", newDisconnectSentinel)
		}
		if replacementChild.hasToolCallArgument(oldDisconnectSentinel) {
			t.Fatalf("replacement child replayed old sentinel %q", oldDisconnectSentinel)
		}

		// (b) The child now dies independently while a different uniquely
		// tagged call is in flight. Neither the Bridge context nor Process.Stop
		// triggers the failure; the supervisor must notice Wait returning,
		// reconnect, and satisfy the same fresh-request barrier.
		user2 := st.login("subject-row12b")
		credential2, device2 := st.enroll(user2, claim("workstation-row12b"))
		st.seedStudio(device2, st.latestUserID(), "studio-row12b")
		clientID2 := st.registerConnector("https://crash.example/connector", "https://crash.example/callback")
		runnerB := st.startBridge(credential2, device2, "row12 crash device", 1)
		runnerB.awaitConnected(15 * time.Second)
		if got := runnerB.childCount(); got != 1 {
			t.Fatalf("the first connection must start exactly one child, got %d", got)
		}
		flowB := st.connectorFlow(user2, clientID2, "https://crash.example/callback", device2, st.studioSessionID(device2, "studio-row12b"))
		flowB.openMCPSession()

		oldCrashSentinel := "row12-crash-old"
		newCrashSentinel := "row12-crash-new"
		crashedChild := runnerB.currentProcess()
		crashedChild.setHoldRequests(true)
		crashCallDone := make(chan error, 1)
		go func() {
			envelope, _, callErr := flowB.call("tools/call", fmt.Sprintf(`{"name":"get_instance_tree","arguments":{"text":%q}}`, oldCrashSentinel))
			if callErr == nil && envelope != nil {
				if rawErr, isErr := envelope["error"]; isErr {
					callErr = fmt.Errorf("relay failure: %s", rawErr)
				} else {
					callErr = fmt.Errorf("unexpected success: %s", envelope["result"])
				}
			}
			crashCallDone <- callErr
		}()
		crashedChild.awaitToolCallArgument(t, oldCrashSentinel, 5*time.Second)
		crashCursor := runnerB.events.cursor()

		crashedChild.crash() // independent child death: never cancel and never Stop
		select {
		case callErr := <-crashCallDone:
			if callErr == nil {
				t.Fatal("the in-flight call must fail when the MCP child crashes mid-call")
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the in-flight call hung after the child crash")
		}

		runnerB.awaitChildCount(2, 20*time.Second)
		runnerB.awaitStateAfter(statusui.Connected, crashCursor, 20*time.Second)
		freshChild := runnerB.currentProcess()
		if text := flowB.callText("tools/call", fmt.Sprintf(`{"name":"get_instance_tree","arguments":{"text":%q}}`, newCrashSentinel)); text != "studio ok" {
			t.Fatalf("post-crash barrier call text = %q", text)
		}
		if !freshChild.hasToolCallArgument(newCrashSentinel) {
			t.Fatalf("fresh post-crash child did not record sentinel %q", newCrashSentinel)
		}
		if freshChild.hasToolCallArgument(oldCrashSentinel) {
			t.Fatalf("fresh post-crash child replayed old sentinel %q", oldCrashSentinel)
		}
	})

	t.Run("row13_backend_graceful_restart_allows_reconnect", func(t *testing.T) {
		st.useT(t)
		user := st.login("subject-row13")
		credentialToken, deviceID := st.enroll(user, claim("workstation-row13"))
		st.seedStudio(deviceID, st.latestUserID(), "studio-row13")
		runner := st.startBridge(credentialToken, deviceID, "row13 device", 1)
		runner.awaitConnected(15 * time.Second)

		// Graceful shutdown of the real lifecycle controller.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := st.server.Shutdown(ctx); err != nil {
			t.Fatalf("graceful shutdown failed: %v", err)
		}
		st.awaitRegistry(deviceID, false, 10*time.Second)

		// The bridge keeps retrying through the backoff.
		deadline := time.Now().Add(10 * time.Second)
		reconnecting := false
		for time.Now().Before(deadline) {
			if runner.events.count(statusui.Reconnecting) > 0 {
				reconnecting = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !reconnecting {
			t.Fatalf("bridge must attempt reconnects; states %v", runner.events.states())
		}

		// Re-bind the same port with fresh components over the same DB.
		st.compose()
		runner2 := st.startBridge(credentialToken, deviceID, "row13 device restart", 1)
		runner2.awaitConnected(20 * time.Second)
		st.awaitRegistry(deviceID, true, 10*time.Second)
	})

	t.Run("row14_mysql_outage_readiness_fails_without_secret_leakage", func(t *testing.T) {
		st.useT(t)
		// The outage from the application's point of view: the pool is
		// closed, so every readiness ping fails — the same surface as the
		// database being down, without needing server-instance privileges.
		if err := st.db.Close(); err != nil {
			t.Fatalf("close pool: %v", err)
		}

		client := st.newClient()
		var body []byte
		var status int
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			resp := client.do(http.MethodGet, st.base+"/readyz", nil, nil)
			status = resp.StatusCode
			body = st.readBody(resp)
			if status == http.StatusServiceUnavailable {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if status != http.StatusServiceUnavailable {
			t.Fatalf("readyz during outage must be 503, got %d (body %s)", status, body)
		}
		if strings.TrimSpace(string(body)) != `{"status":"unavailable"}` {
			t.Fatalf("readiness outage body must be the fixed sanitized document, got %q", body)
		}
		for _, secret := range []string{"root@tcp", "e2egate-device-pepper", "e2egate-oauth-pepper", "rkd_", "mysql"} {
			if strings.Contains(strings.ToLower(string(body)), strings.ToLower(secret)) {
				t.Fatalf("readiness body leaked %q: %s", secret, body)
			}
		}
		liveResp := client.do(http.MethodGet, st.base+"/healthz", nil, nil)
		if liveResp.StatusCode != http.StatusOK {
			t.Fatalf("healthz must stay 200 during the outage, got %d", liveResp.StatusCode)
		}
		_ = st.readBody(liveResp)

		// Restore: fresh pool + fresh stack components on the same port.
		st.shutdown()
		st.reopenDB()
		st.compose()
		readyDeadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(readyDeadline) {
			resp := client.do(http.MethodGet, st.base+"/readyz", nil, nil)
			status = resp.StatusCode
			body = st.readBody(resp)
			if status == http.StatusOK {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if status != http.StatusOK {
			t.Fatalf("readyz after restore must be 200, got %d (body %s)", status, body)
		}
	})
}

// ---- connector OAuth + MCP helpers ----

type connectorAuthorizer struct {
	t         *testing.T
	stack     *liveStack
	clientID  string
	redirect  string
	verifier  string
	token     string
	sessionID string
	nextID    int
}

func (st *liveStack) registerConnector(name, redirect string) string {
	st.t.Helper()
	client, err := st.oauthStore.RegisterClient(st.t.Context(), mcpoauth.Client{
		ClientID: name, ClientName: name, RedirectURIs: []string{redirect},
	})
	if err != nil {
		st.t.Fatalf("register connector client: %v", err)
	}
	return client.ClientID
}

func (st *liveStack) connectorFlow(session *liveClient, clientID, redirect, deviceID, studioSessionID string) *connectorAuthorizer {
	t := st.t
	t.Helper()
	flow := &connectorAuthorizer{t: t, stack: st, clientID: clientID, redirect: redirect}
	verifier := "pkce-verifier-" + gateUUID(t)
	flow.verifier = verifier
	challenge := base64Raw(sha256Sum([]byte(verifier)))

	authorize := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"scope":                 {"mcp:connect studio:read studio:edit"},
		"state":                 {"state-" + gateUUID(t)},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {st.base + "/mcp"},
	}
	resp := session.do(http.MethodGet, st.base+"/oauth/authorize?"+authorize.Encode(), nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize (consent form) status = %d", resp.StatusCode)
	}
	_ = st.readBody(resp)

	form := url.Values{}
	for key, values := range authorize {
		form[key] = append([]string(nil), values...)
	}
	form.Set("action", "approve")
	form.Set("device_id", deviceID)
	form.Set("studio_session_id", studioSessionID)
	form["grant"] = []string{"mcp:connect", "studio:read", "studio:edit"}
	resp = session.do(http.MethodPost, st.base+"/oauth/authorize", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("consent approve status = %d (body %s)", resp.StatusCode, st.readBody(resp))
	}
	location, err := resp.Location()
	if err != nil {
		t.Fatalf("consent redirect: %v", err)
	}
	q := location.Query()
	if q.Get("error") != "" {
		t.Fatalf("consent approve redirected with error %q: %q", q.Get("error"), q.Get("error_description"))
	}
	code := q.Get("code")
	if code == "" {
		t.Fatal("consent redirect carried no code")
	}

	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {clientID},
		"code_verifier": {verifier},
		"resource":      {st.base + "/mcp"},
	}
	resp = st.newClient().do(http.MethodPost, st.base+"/oauth/token", strings.NewReader(tokenForm.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	tokenBody := st.readBody(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token exchange status = %d (body %s)", resp.StatusCode, tokenBody)
	}
	var tokens struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(tokenBody, &tokens); err != nil || tokens.AccessToken == "" {
		t.Fatalf("decode token response %q: %v", tokenBody, err)
	}
	flow.token = tokens.AccessToken
	return flow
}

// connectorFlowExpectDenial reports whether the consent approval was denied
// (access_denied redirect or error page) for an expired window.
func (st *liveStack) connectorFlowExpectDenial(session *liveClient, clientID string) bool {
	st.t.Helper()
	authorize := st.authorizeValues(clientID, "https://expired.example/callback")
	resp := session.do(http.MethodGet, st.base+"/oauth/authorize?"+authorize.Encode(), nil, nil)
	_ = st.readBody(resp)
	form := authorize
	form.Set("action", "approve")
	form.Set("device_id", "")
	form["grant"] = []string{"mcp:connect"}
	resp = session.do(http.MethodPost, st.base+"/oauth/authorize", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	body := st.readBody(resp)
	if resp.StatusCode != http.StatusSeeOther {
		return true
	}
	loc, _ := resp.Location()
	if loc == nil {
		return true
	}
	if loc.Query().Get("error") == "access_denied" {
		return true
	}
	return strings.Contains(string(body), "not active")
}

// openMCPSession completes initialize + initialized against the real /mcp.
func (f *connectorAuthorizer) openMCPSession() {
	f.t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2egate-connector","version":"1.0"}}}`
	status, sessionID, messages := f.stack.postMCP(f.token, "", body)
	if status != http.StatusOK {
		f.t.Fatalf("initialize status = %d", status)
	}
	if sessionID == "" {
		f.t.Fatal("initialize response missing Mcp-Session-Id")
	}
	f.sessionID = sessionID
	if len(messages) == 0 {
		f.t.Fatal("initialize carried no JSON-RPC message")
	}
	var initResp struct {
		Result struct {
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(messages[0], &initResp); err != nil {
		f.t.Fatalf("decode initialize %s: %v", messages[0], err)
	}
	if initResp.Result.ServerInfo.Name != "RobloxKit Remote Gateway" {
		f.t.Fatalf("initialize serverInfo = %q", initResp.Result.ServerInfo.Name)
	}
	notif := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	status, _, _ = f.stack.postMCP(f.token, f.sessionID, notif)
	if status != http.StatusAccepted {
		f.t.Fatalf("initialized notification status = %d", status)
	}
}

func (f *connectorAuthorizer) call(method, params string) (map[string]json.RawMessage, int, error) {
	f.t.Helper()
	f.nextID++
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, f.nextID, method, params)
	status, _, messages := f.stack.postMCP(f.token, f.sessionID, body)
	if status != http.StatusOK || len(messages) == 0 {
		return nil, status, fmt.Errorf("http status %d, messages %d", status, len(messages))
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(messages[0], &envelope); err != nil {
		f.t.Fatalf("decode %s response %s: %v", method, messages[0], err)
	}
	return envelope, status, nil
}

func (f *connectorAuthorizer) callText(method, params string) string {
	f.t.Helper()
	envelope, _, err := f.call(method, params)
	if err != nil {
		f.t.Fatalf("%s failed: %v", method, err)
	}
	if rawErr, ok := envelope["error"]; ok {
		f.t.Fatalf("%s returned JSON-RPC error: %s", method, rawErr)
	}
	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(envelope["result"], &result); err != nil {
		f.t.Fatalf("decode %s result %s: %v", method, envelope["result"], err)
	}
	if len(result.Content) == 0 {
		f.t.Fatalf("%s result has no content: %s", method, envelope["result"])
	}
	return result.Content[0].Text
}

func (f *connectorAuthorizer) callError(method, params string) (int, string) {
	f.t.Helper()
	envelope, _, err := f.call(method, params)
	if err != nil {
		f.t.Fatalf("%s transport failed: %v", method, err)
	}
	rawErr, ok := envelope["error"]
	if !ok {
		f.t.Fatalf("%s unexpectedly succeeded: %s", method, envelope)
	}
	var wire struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rawErr, &wire); err != nil {
		f.t.Fatalf("decode error object %s: %v", rawErr, err)
	}
	return wire.Code, wire.Message
}

// postMCP posts one JSON-RPC body to the real /mcp endpoint and parses the
// SSE response frames.
func (st *liveStack) postMCP(token, sessionID, body string) (int, string, []json.RawMessage) {
	st.t.Helper()
	req, err := http.NewRequest(http.MethodPost, st.base+"/mcp", strings.NewReader(body))
	if err != nil {
		st.t.Fatalf("build mcp request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // local test TLS only
	}}
	resp, err := client.Do(req)
	if err != nil {
		st.t.Fatalf("POST /mcp: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		st.t.Fatalf("read /mcp response: %v", err)
	}
	var messages []json.RawMessage
	for _, line := range strings.Split(string(raw), "\n") {
		if data, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "data: "); ok {
			messages = append(messages, json.RawMessage(data))
		}
	}
	return resp.StatusCode, resp.Header.Get("Mcp-Session-Id"), messages
}

// ---- admin + enrollment expect helpers ----

func (st *liveStack) adminPreview(session *liveClient, kind, userID string) map[string]any {
	st.t.Helper()
	status, out := session.getJSON(st.base + "/api/v1/admin/users/" + userID + "/" + kind + "-preview")
	if status != http.StatusOK {
		st.t.Fatalf("%s preview status = %d: %v", kind, status, out)
	}
	return out
}

func (st *liveStack) adminExecute(session *liveClient, path string, payload map[string]string) {
	st.t.Helper()
	status, out := session.postJSON(st.base+path, payload)
	if status != http.StatusNoContent {
		st.t.Fatalf("%s execute status = %d: %v", path, status, out)
	}
}

// resolveGrantID reads the session user's connector grant for the client.
func (st *liveStack) resolveGrantID(session *liveClient, clientID string) string {
	st.t.Helper()
	status, out := session.getJSON(st.base + "/api/v1/connectors")
	if status != http.StatusOK {
		st.t.Fatalf("connectors status = %d", status)
	}
	connectors, _ := out["connectors"].([]any)
	for _, entry := range connectors {
		connector, _ := entry.(map[string]any)
		if connector["client_id"] == clientID {
			if id, ok := connector["id"].(string); ok {
				return id
			}
		}
	}
	st.t.Fatalf("no connector grant found for %s in %v", clientID, out)
	return ""
}

// enrollExpectStatus runs begin/approve/exchange and asserts the exchange
// status.
func (st *liveStack) enrollExpectStatus(session *liveClient, claim device.DeviceClaim, wantStatus int) {
	st.t.Helper()
	raw, err := json.Marshal(claim)
	if err != nil {
		st.t.Fatalf("marshal claim: %v", err)
	}
	resp := st.newClient().do(http.MethodPost, st.base+"/api/v1/device/enrollment/begin",
		strings.NewReader(string(raw)), map[string]string{"Content-Type": "application/json"})
	var begin struct {
		UserCode string `json:"user_code"`
	}
	if err := json.Unmarshal(st.readBody(resp), &begin); err != nil {
		st.t.Fatalf("decode begin: %v", err)
	}
	status, _ := session.postJSON(st.base+"/api/v1/enrollments/approve", map[string]string{"user_code": begin.UserCode})
	if status != http.StatusNoContent {
		st.t.Fatalf("approve status = %d", status)
	}
	raw, err = json.Marshal(map[string]string{"device_code": begin.UserCode})
	if err != nil {
		st.t.Fatalf("marshal exchange: %v", err)
	}
	resp = st.newClient().do(http.MethodPost, st.base+"/api/v1/device/enrollment/exchange",
		strings.NewReader(string(raw)), map[string]string{"Content-Type": "application/json"})
	body := st.readBody(resp)
	if resp.StatusCode != wantStatus {
		st.t.Fatalf("exchange status = %d, want %d (body %s)", resp.StatusCode, wantStatus, body)
	}
}

// authorizeValues builds a valid authorize query for the connector client.
func (st *liveStack) authorizeValues(clientID, redirect string) url.Values {
	verifier := "seed-verifier-" + gateUUID(st.t)
	challenge := base64Raw(sha256Sum([]byte(verifier)))
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"scope":                 {"mcp:connect"},
		"state":                 {"state-" + gateUUID(st.t)},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {st.base + "/mcp"},
	}
}

func base64Raw(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}
