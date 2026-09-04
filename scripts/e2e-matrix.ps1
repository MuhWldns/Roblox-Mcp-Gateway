# scripts/e2e-matrix.ps1 - the final E2E gate.
#
# Aggregates the committed named tests that pin each production-matrix row
# and runs the live internal/e2egate package (the real production stack on a
# real migrated MySQL database) that proves the locally feasible matrix live.
# Rows 4 and 5 run their DEFERRED real-host substitutes (the identical OAuth
# + initialize + tools/list + read-only-call path through the real fosite
# endpoints with scripted connector clients).
#
# Usage:
#   $env:MYSQL_TEST_DSN = 'root@tcp(127.0.0.1:3306)/'
#
# The gate is STRICT: the live production rows are the release-blocking
# proof, so a missing or blank MYSQL_TEST_DSN is a hard refusal - the gate
# must never degrade into a partial run that could print PASS on an
# unconfigured machine. (The Go packages themselves stay skippable for
# plain `go test ./...`; strictness lives HERE, at the gate.)

$ErrorActionPreference = 'Stop'

if (-not $env:MYSQL_TEST_DSN -or $env:MYSQL_TEST_DSN.Trim() -eq '') {
    Write-Host "gate: FAIL - MYSQL_TEST_DSN is not set; the live production gate refuses to run (set it, e.g. 'root@tcp(127.0.0.1:3306)/')" -ForegroundColor Red
    exit 2
}

function Invoke-GateTest {
    param([string]$Package, [string]$Run, [string]$Label)
    Write-Host ""
    Write-Host "gate: $Label" -ForegroundColor Cyan
    if ($Run -ne '') {
        go test $Package -run $Run -count=1
    } else {
        go test $Package -count=1
    }
    if ($LASTEXITCODE -ne 0) {
        Write-Host "gate: FAIL $Label" -ForegroundColor Red
        exit 1
    }
}

Write-Host "gate: final production-like E2E matrix" -ForegroundColor Green

# --- Named package tests pinning the row contracts (order = matrix rows) ---

# Row 1-2: login, trial absent, authenticated download.
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row01_new_user_login_trial_absent|TestE2EProductionMatrix/row02_authenticated_download_trial_still_absent' -Label 'rows 01-02 login/download (live)'

# Row 3: first enrollment atomically creates trial + device + credential.
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row03_first_enrollment_creates_trial_device_credential_atomically' -Label 'row 03 enrollment atomic trial (live)'
Invoke-GateTest -Package './internal/device' -Run 'TestEnrollment' -Label 'row 03 enrollment service contract'

# Rows 4-5: connector OAuth + initialize + tools/list + read-only call
# (real-host rows DEFERRED; the e2egate substitutes run the identical path
# through the real fosite endpoints).
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row04_chatgpt_real_host_deferred_connector_oauth_mcp_path|TestE2EProductionMatrix/row05_claude_real_host_deferred_connector_oauth_mcp_path' -Label 'rows 04-05 connector OAuth+MCP substitutes (live)'
Invoke-GateTest -Package './internal/mcpoauth' -Run 'TestAuthorizeConsentIssuesCodeAndState|TestTokenExchangesCodeForTokens|TestTokenRejectsWrongVerifier|TestTokenRejectsCodeReuse|TestRevokeAccessTokenRevokesOnlyThatToken' -Label 'rows 04-05 fosite contract'

# Row 6: wrong account + cross-user/device/Studio denials.
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row06_wrong_account_and_cross_user_device_studio_denials' -Label 'row 06 cross denials (live)'
Invoke-GateTest -Package './internal/bridgehub' -Run 'TestBridgeHubWrongOwnerClaimRejected' -Label 'row 06 hub owner-claim denial'
Invoke-GateTest -Package './internal/mcpgateway' -Run 'TestLicenseOnlyBindingRemovalDeniedPerCall' -Label 'row 06 license-only binding-loss denial'

# Row 7: expired trial blocks gated paths; dashboard/download stay.
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row07_expired_trial_blocks_gated_paths_dashboard_download_stay' -Label 'row 07 expired trial (live)'
Invoke-GateTest -Package './internal/bridgehub' -Run 'TestBridgeHubRejectsExpiredTrial|TestBridgeHubLicenseOnlyWithoutBindingRejected' -Label 'row 07 hub expired/license-only'
Invoke-GateTest -Package './internal/mcpgateway' -Run 'TestExpiredTrialIsRejected' -Label 'row 07 gateway expired-trial denial'

# Row 8: reinstall/revoke/transfer/recovery never a second trial.
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row08_reinstall_revoke_transfer_recovery_never_second_trial' -Label 'row 08 no second trial (live)'
Invoke-GateTest -Package './internal/entitlement' -Run 'TestTrialRevokeReinstallTransferRecoveryDoNotReset|TestRecoveryRevokesAllCredentialsAndPreservesTrial' -Label 'row 08 entitlement invariants'

# Row 9: admin transfer closes old connection atomically, history preserved.
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row09_admin_transfer_closes_old_connection_history_preserved' -Label 'row 09 admin transfer (live)'
Invoke-GateTest -Package './internal/httpserver' -Run 'TestAdminTransferMovesSlotAndClosesOldConnectionFirst' -Label 'row 09 transfer ordering'

# Row 10: admin recovery revokes the whole surface, trial preserved.
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row10_admin_recovery_revokes_surface_preserves_trial' -Label 'row 10 admin recovery (live)'
Invoke-GateTest -Package './internal/httpserver' -Run 'TestAdminRecoveryRevokesEverythingButTrial' -Label 'row 10 recovery surface'

# Row 11: multi-Studio ambiguity denied, explicit target succeeds.
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row11_multi_studio_ambiguity_denied_explicit_succeeds' -Label 'row 11 Studio ambiguity (live)'
Invoke-GateTest -Package './internal/routing' -Label 'row 11 routing resolution'

# Row 12: disconnect/child crash produce no replay.
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row12_disconnect_and_child_crash_produce_no_replay' -Label 'row 12 no replay (live)'
Invoke-GateTest -Package './internal/bridgeapp' -Run 'TestNoReplayAfterDisconnectMidToolCall' -Label 'row 12 bridgeapp no-replay'

# Row 13: backend graceful restart allows reconnect.
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row13_backend_graceful_restart_allows_reconnect' -Label 'row 13 graceful restart (live)'
Invoke-GateTest -Package './internal/bridgeapp' -Run 'TestRemoteReconnectAcrossServerRestart' -Label 'row 13 bridgeapp reconnect'

# Row 14: temporary MySQL outage fails readiness without secret leakage.
Invoke-GateTest -Package './internal/e2egate' -Run 'TestE2EProductionMatrix/row14_mysql_outage_readiness_fails_without_secret_leakage' -Label 'row 14 outage readiness (live)'
Invoke-GateTest -Package './internal/health' -Label 'row 14 health probes'

Write-Host ""
Write-Host "gate: PASS - all 14 matrix rows proven (4/5 real hosts DEFERRED, local substitutes PASS)" -ForegroundColor Green
