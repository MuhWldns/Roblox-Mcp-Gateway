# ChatGPT OAuth Consent Implementation Plan

**Goal:** Connect ChatGPT to public `/mcp` through the existing DCR + OAuth authorization-code/PKCE flow, preserving authorization across Roblox login and binding approval to one active Studio.

**Spec:** `docs/superpowers/specs/2026-09-07-chatgpt-oauth-consent-design.md`

## Global constraints

- DCR only; no CIMD or RFC 9207 in this change.
- Exact client, redirect, resource, user, device, active Studio, and scope bindings.
- Approval scopes come only from the parsed authorize request.
- Explicit approval atomically commits grant, session consent, audit, and authorization code.
- Auto-approval writes only a new single-use authorization code.
- Login `next` accepts only relative exact `/oauth/authorize`, no authority/userinfo/fragment, valid query, maximum 4096 bytes.
- Every missing/invalid bearer 401 advertises every `mcpoauth.SupportedScopes` entry in canonical order plus path-specific `resource_metadata`.
- Tool-level insufficient scope remains a JSON-RPC error and never reaches Bridge.
- Existing token, refresh, Bridge, relay, and lifetime behavior remains unchanged.
- Add focused behavioral tests before each implementation change.

## Task 1: Preserve authorize continuation through Roblox login

Files: `internal/robloxauth/flow.go`, `handler.go`, their tests, `web/src/routes/Login.tsx`, `web/src/router.test.tsx`.

Contract:
- `FlowService.Begin(context.Context, string)` accepts an untrusted continuation candidate.
- `LoginTransaction` stores validated `ReturnTo`.
- `Complete` returns `(RobloxIdentity, string, error)`.
- Backend validation is authoritative; the frontend only conservatively forwards exact internal authorize targets.
- Valid continuation is single-use, browser-bound, absent from the Roblox provider URL, and returned only after successful completion.
- Invalid/empty continuation falls back to `/download`.

Verification: focused Go tests; router Vitest; frontend typecheck. Commit: `feat(auth): preserve OAuth authorize continuation`.

## Task 2: Persist session-bound remembered consent

Files: create `migrations/00007_oauth_session_consents.sql`; update migration embedding/tests, `internal/mcpoauth/model.go`, `store.go`, `internal/mysqlstore/oauth_store.go` and tests.

Contract:
- Add `SessionConsent` with web session, internal client, grant, canonical scopes, resource, created/updated timestamps.
- Add `ErrSessionConsentNotFound` and `Store.SessionConsent(ctx, webSessionID, clientID)`.
- Composite primary key `(web_session_id, client_id)` and foreign keys to web session, OAuth client, and grant.
- No standalone save API; explicit writes stay inside the provider-owned transaction.

Verification: focused MySQL-store and MCP OAuth tests. Commit: `feat(oauth): persist session-bound consent`.

## Task 3: Render fixed consent package and require active Studio

Files: `internal/mcpoauth/provider.go`, `authorize.go`, `consent.go`, consent tests, `cmd/server/main.go`, integration fixture wiring only.

Contract:
- Use existing Roblox identity reader.
- Retain web session ID and user ID.
- Render validated client/identity, human-readable requested capabilities, only owned active device/Studio targets, no scope checkboxes, and no optional Studio choice.
- Disable Connect with guidance when no valid target exists.
- Revalidate ownership, activity, device binding, and entitlement on POST.
- Ignore browser-supplied scope values; use exact parsed request scopes.

Verification: focused consent/package tests. Commit: `feat(oauth): add fixed Studio consent page`.

## Task 4: Atomically remember and auto-approve exact repeats

Files: `internal/mcpoauth/authorize.go`, `consent.go`, `provider.go`, tests.

Contract:
- Common code builder returns Fosite response plus authorization-code row without persistence.
- Explicit approval transaction upserts grant and session consent, records secret-free audit, inserts code digest, then commits before redirect.
- Remembered lookup revalidates web session, client, resource, canonical scopes, grant, entitlement, device, Studio, ownership, and binding immediately before issuance.
- Eligibility mismatch renders consent without target switching; infrastructure failure returns server error.
- Auto-approval persists only a fresh code and writes no approval audit.

Verification: atomic rollback and full remembered-consent matrix. Commit: `feat(oauth): remember exact session consent`.

## Task 5: Advertise complete scopes in bearer challenges

Files: `internal/mcpgateway/server.go`, `server_test.go`.

Contract:
- Missing and invalid bearer 401 responses use one Bearer challenge with exact path metadata URL and `strings.Join(mcpoauth.SupportedScopes, " ")`.
- Do not change entitlement 403 or tool-level JSON-RPC scope denial.

Verification: full gateway package tests. Commit: `fix(mcp): advertise fixed OAuth scopes`.

## Task 6: Prove ChatGPT DCR through MCP execution

Files: `internal/e2egate/e2egate_test.go`; matrix only if it enumerates required gates.

Contract:
- Use public metadata, DCR, unauthenticated authorize/login continuation, consent, exact token exchange, MCP initialize, non-empty `tools/list`, and one read-only relayed tool.
- Use validated `ChatGPT` client name, callback-ID-style HTTPS redirect, all supported scopes, original state, PKCE, resource, and registered redirect.
- Do not seed OAuth grants or tokens directly.

Verification: focused E2E, full Go suite, web tests/typecheck/build; then parent-run browser verification at desktop and narrow mobile widths. Commit: `test(e2e): cover ChatGPT OAuth MCP flow`.

## Review gates

- Task-scoped spec and quality review after every task; resolve all Critical/Important findings.
- Broad final review against merge base and approved spec.
- Runtime smoke: unauthenticated continuation, explicit consent, same-session repeat, new-session consent, and stale-target refusal.
- If no real ChatGPT callback ID is available, defer only the external ChatGPT-hosted redirect; the repository integration harness must prove everything else.
