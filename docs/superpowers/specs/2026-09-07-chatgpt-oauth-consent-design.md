# ChatGPT OAuth Consent Design

## Status

Approved in chat on 2026-09-07 for the minimal DCR path. This specification covers the existing RobloxKit remote MCP connector and extends its current OAuth 2.1 authorization-code flow; it does not introduce a second authentication system. RFC 9207 issuer identification and CIMD are explicitly deferred so the first deliverable stays focused on making `/mcp` usable from ChatGPT.

## Problem

RobloxKit already exposes the core connector OAuth surface: RFC 9728 protected-resource metadata, RFC 8414 authorization-server metadata, dynamic client registration (DCR), authorization-code + PKCE S256, refresh-token rotation, revocation, consent grants, and bearer protection for `/mcp`.

Two user-facing gaps prevent the intended ChatGPT flow from working cleanly:

1. `/oauth/authorize` redirects an unauthenticated browser to `/login?next=...`, but the React login page drops `next`, `robloxauth.Handler.Begin` does not bind it to the Roblox transaction, and the callback always redirects to `/download`. The original ChatGPT authorization request is therefore lost after Roblox login.
2. The existing consent page is a generic unstyled form with editable scope checkboxes. The product requires an explicit English “Connect Roblox Studio to ChatGPT?” decision, a fixed requested permission package, and an exact device + active Studio target.

Repeated, identical authorization requests may skip consent only while the same RobloxKit browser session remains active and only while the previously approved target and grant remain valid.

## Goals

- Resume the exact original OAuth authorize request after successful Roblox login.
- Present a clear English Connect/Cancel consent page using the validated DCR client name.
- Bind every connector grant to one user-owned active device and one active Studio session on that device.
- Approve the complete requested supported scope set as one package; users do not edit individual scopes on the consent page.
- Remember the last explicit approval only within the same active browser session.
- Auto-approve a repeated request only when client, resource, requested scopes, user, grant, device, Studio session, and entitlement still match.
- Preserve the existing OAuth security properties and MCP default-deny tool policy.

## Non-goals

- Client ID Metadata Document (CIMD) integration. ChatGPT will use the existing DCR endpoint for this iteration.
- Login with ChatGPT or Roblox provider tokens outside the existing Roblox OIDC flow.
- Anonymous or `No Auth` access to `/mcp`.
- A React consent application or new consent JSON API.
- Changing access-token, refresh-token, or authorization-code lifetimes.
- Changing the Bridge enrollment, WebSocket authentication, or MCP relay protocol.

## User flow

```text
ChatGPT
  |
  | unauthenticated MCP request
  v
POST /mcp -> 401 + WWW-Authenticate resource_metadata + full fixed scope package
  |
  v
OAuth discovery + POST /oauth/register
  |
  v
GET /oauth/authorize?client_id=...&redirect_uri=...&state=...
                     &code_challenge=...&code_challenge_method=S256
                     &resource=https://mcp.rbxskuy.web.id/mcp&scope=...
  |
  +-- no RobloxKit session
  |      |
  |      v
  |   /login?next=<internal authorize URL>
  |      |
  |      v
  |   Roblox OIDC login
  |      |
  |      v
  |   callback creates RobloxKit web session
  |      |
  |      v
  |   303 to the bound internal authorize URL
  |
  +-- active RobloxKit session
         |
         +-- exact valid session consent exists -> issue new code
         |
         +-- otherwise -> render consent
                              |
                              +-- Cancel -> access_denied + original state
                              |
                              +-- Connect -> persist grant + session consent
                                             -> issue code + original state
```

There is no second ChatGPT login. ChatGPT is the OAuth client; RobloxKit authenticates the human through Roblox and asks the human to authorize that client.

## Login continuation

### Contract

`/login` accepts an optional `next` query parameter. Only an internal OAuth authorize URL is accepted:

- path must equal `/oauth/authorize`;
- scheme and host must be empty;
- userinfo and fragment must be absent;
- the query must parse normally;
- the target length must be bounded;
- invalid targets are discarded and the normal `/download` success redirect is used.

The React login page preserves the validated-looking `next` string only by forwarding it to `/api/v1/auth/roblox/login`; the backend remains authoritative and revalidates it.

`robloxauth.Handler.Begin` passes the validated continuation into the existing Roblox login transaction. `LoginTransaction` stores `ReturnTo` beside state, nonce, PKCE verifier, browser binding, and expiry. The continuation never appears in the Roblox provider request and is returned only after the transaction completes with the correct state and binding.

The continuation inherits the existing login transaction guarantees:

- single-use;
- bound to the `__Host-robloxkit_login` cookie;
- removed before provider token exchange completes;
- in-memory and capacity-bounded;
- default five-minute TTL.

After a successful callback and web-session creation, the handler redirects to `ReturnTo`; without a valid continuation it redirects to the configured `/download` fallback. Failed or replayed Roblox callbacks never redirect to the continuation.

## Consent page

The page remains server-rendered by `internal/mcpoauth` so OAuth parameters and CSRF handling stay in the authorization-server boundary.

### Required content

- Heading: `Connect Roblox Studio to <ClientName>?`
- Signed-in Roblox display name.
- Validated OAuth application name and client identifier.
- Requested capabilities as human-readable text.
- Device selector containing only active devices owned by the signed-in user.
- Studio selector containing only active Studio sessions owned by the user and bound to the selected device.
- Explanatory text that access is limited to the selected device and Studio session.
- `Cancel` and `Connect to <ClientName>` actions.

For a ChatGPT DCR registration whose validated name is `ChatGPT`, the primary action reads `Connect to ChatGPT`.

### Fixed permission package

The page displays all requested supported scopes as capabilities but does not render scope checkboxes. Approval submits the exact scope string from the parsed authorize request. The backend reconstructs the granted scope list from the validated authorize request rather than trusting a browser-supplied list.

Suggested capability labels:

| Scope | User-facing capability |
|---|---|
| `mcp:connect` | Connect ChatGPT to the selected Roblox Studio session |
| `studio:read` | Inspect the project, scripts, instances, and Studio state |
| `studio:edit` | Modify scripts and instances |
| `studio:execute` | Execute approved Luau operations |
| `studio:playtest` | Start, inspect, and stop playtests |
| `studio:asset` | Search, upload, and insert supported assets |
| `studio:input` | Send approved keyboard and mouse input during playtests |

Unknown or unsupported scopes are rejected before consent by the existing scope validation.

### Target behavior

The browser may use a small inline script to filter Studio options when the device selector changes. This is presentation only. The approval handler independently checks:

- device belongs to the session user and is active;
- Studio session belongs to the same user, is active, and is bound to that device;
- entitlement permits MCP use.

A Studio session is required for approval. The existing optional Studio binding becomes mandatory for new consent approvals in this flow.

If no valid device or Studio session exists, the page disables the Connect action and instructs the user to run RobloxBridge and open Roblox Studio. It must not choose another user’s target or silently bind a later session.

## Session-bound remembered consent

### Persistence

Add `oauth_session_consents` in migration `00007_oauth_session_consents.sql`:

- `web_session_id CHAR(36) NOT NULL`
- `client_id CHAR(36) NOT NULL` (internal OAuth client row ID)
- `grant_id CHAR(36) NOT NULL`
- `requested_scopes JSON NOT NULL`
- `resource VARCHAR(2048) NOT NULL`
- `created_at TIMESTAMP(6) NOT NULL`
- `updated_at TIMESTAMP(6) NOT NULL`
- primary key `(web_session_id, client_id)`
- foreign keys to `web_sessions(id)`, `oauth_clients(id)`, and `oauth_grants(id)`

One row stores the last explicit approval for a client in a browser session. Approving another target or scope package replaces that row. A new browser session has a new `web_session_id` and therefore cannot inherit the previous session’s remembered approval.

### Auto-approval eligibility

On authorize GET, after normal Fosite parsing and resource/PKCE validation, the provider may issue a code without rendering consent only when all conditions are true:

1. the browser session is active and its ID matches `web_session_id`;
2. the stored client matches the resolved DCR client;
3. stored resource exactly matches the requested and configured MCP resource;
4. stored requested scopes equal the authorize request as a canonical set;
5. the referenced grant belongs to the same user and client and is not revoked;
6. the grant scopes equal the request scopes;
7. the grant’s device belongs to the user and remains active;
8. the grant’s Studio session belongs to the user, remains active, and remains bound to that device;
9. the entitlement still permits MCP access.

If any condition fails, the provider renders consent. It never auto-selects a different target and never widens or narrows scopes silently.

Explicit approval persists the durable grant, session-consent row, secret-free approval audit, and authorization-code digest in one database transaction. Any audit or persistence failure rolls back every record and no authorization code is issued.

Auto-approval does not mutate the durable grant or record a new user-consent audit because no new user decision occurred; it only creates a fresh single-use authorization code bound to the already validated grant values.

Logout and revoke-all invalidate remembered consent immediately because the referenced web session no longer validates. Physical deletion may be deferred or cascaded; validity must not depend on cleanup timing.

## Authorization code issuance

Extract the common code-issuance path used by explicit approval and eligible session auto-approval. It must:

- grant only the exact validated requested scopes;
- bind the code to user, internal client, exact redirect URI, PKCE challenge, resource, device, and Studio session;
- store only the keyed digest of the code;
- persist before redirecting to the client;
- preserve the original OAuth `state` through Fosite’s authorize response.

Cancel continues to use Fosite’s `access_denied` response and must not write a grant, session consent, code, token, or approval audit.

## Client registration and discovery

The existing DCR path remains authoritative:

- authorization-server metadata advertises `/oauth/register`;
- ChatGPT registers a public authorization-code client;
- `token_endpoint_auth_method` is `none`;
- PKCE `S256` is mandatory;
- registered redirect URIs and client name are persisted and later resolved by public `client_id`;
- consent never trusts a client display name from authorize query parameters.

CIMD fetch support remains unused in this iteration. No second registration convention is added beside DCR.

The existing protected-resource and authorization-server discovery documents remain on the MCP resource origin. `/mcp` continues returning a Bearer challenge that points to the path-specific protected-resource metadata URL.

For the fixed-permission product model, every unauthenticated or invalid-token `401` challenge includes `scope` containing all entries in `SupportedScopes`, in their canonical order. This is the authoritative initial scope request for ChatGPT and prevents a successful connection from receiving only `mcp:connect` and therefore an empty relayed tool catalog.

The authorization-server metadata does not advertise `authorization_response_iss_parameter_supported` in this iteration. ChatGPT therefore uses its callback-ID-specific redirect URI. That URI is supplied and validated through DCR; there is no separate manually configured redirect allowlist. RFC 9207 and the stable ChatGPT redirect URI are deferred.

## Security invariants

- `/mcp` remains inaccessible without a valid bearer access token.
- Authorization codes are single-use and PKCE S256-bound.
- Access tokens remain opaque, one-hour credentials stored only as keyed digests.
- Refresh tokens remain opaque, 30-day rotating credentials with family reuse detection.
- Client, redirect URI, resource, scope, user, device, and Studio bindings are exact-match checks.
- Consent POST uses the existing hardened, constant-time CSRF double-submit check.
- No open redirect: login continuation accepts only the exact internal authorize path.
- No cross-tenant target: ownership checks run again on every approval and auto-approval.
- No stale target: active device and active Studio status are checked immediately before code issuance.
- No privilege escalation: tool names absent from the versioned tool-to-scope map stay denied.
- No secrets in HTML, redirects beyond protocol-required authorization codes, application logs, audit metadata, or error bodies.
- Database and audit failures fail closed.

## Error behavior

| Condition | Result |
|---|---|
| No RobloxKit browser session | Redirect to `/login` with bound internal continuation |
| Invalid continuation | Ignore it and use `/download` after login |
| Unknown/invalid OAuth client | OAuth `invalid_client`; no consent |
| Missing resource or non-S256 PKCE | OAuth `invalid_request`; no consent |
| Missing or invalid bearer token at `/mcp` | `401` with `resource_metadata` and the complete fixed `scope` package |
| Tool outside the token's granted scope | MCP JSON-RPC scope-denied error; no Bridge delivery |
| No active device or Studio | Consent page explains the requirement; Connect disabled |
| Cancel | Redirect to registered client with `error=access_denied` and original `state` |
| Expired entitlement | Access denied; no grant or code |
| Foreign/revoked/inactive target | Access denied; no grant or code |
| Remembered consent mismatch | Render consent; do not fail and do not switch targets |
| Database/audit/code persistence failure | Server error; no partial approval or code |
| Expired/replayed Roblox login transaction | Existing sanitized invalid-login response |

## Implementation boundaries

Expected source areas:

- `internal/robloxauth/flow.go` and `handler.go`: validated login continuation bound to the existing transaction.
- `web/src/routes/Login.tsx`: preserve `next` when beginning Roblox login and when already authenticated.
- `internal/mcpoauth/authorize.go`: consent view, Roblox identity display, fixed permission package, target UI, and auto-approval branch.
- `internal/mcpoauth/consent.go`: mandatory Studio binding, common code issuance, transactional session-consent upsert.
- `internal/mcpoauth/store.go` and `internal/mysqlstore/oauth_store.go`: session-consent persistence and lookup.
- `migrations/00007_oauth_session_consents.sql`: session-bound consent memory.
- `cmd/server/main.go`: additional identity/store wiring only where required by the final interfaces.

Existing OAuth token, refresh rotation, revocation, Bridge, and relay implementations remain unchanged. The gateway bearer challenge receives one narrow adjustment so every `401` advertises the complete fixed scope package; tool-level scope denials remain MCP JSON-RPC errors rather than transport-level `403` responses.

## Verification

### Backend behavioral tests

1. An authorize request without a browser session survives Roblox login and resumes with every original OAuth parameter unchanged.
2. An invalid external or malformed `next` value cannot redirect the callback away from the internal authorize endpoint.
3. Consent renders validated client name, Roblox display name, requested capabilities, owned active devices, and active Studio sessions.
4. Connect grants the exact requested supported scope package and persists the selected device and Studio.
5. Cancel returns `access_denied` with original state and writes no grant, code, token, session consent, or audit.
6. A repeated identical authorize request in the same web session issues a code for the remembered target without rendering consent.
7. A new web session always renders consent even when the user has an existing durable grant.
8. Client, resource, or scope changes render consent.
9. Revoked grants, inactive devices, inactive/moved Studio sessions, expired entitlement, and foreign targets prevent auto-approval.
10. Grant, session-consent, audit, and authorization-code persistence are atomic on explicit approval.
11. Authorization-code replay, redirect mismatch, resource mismatch, and PKCE mismatch remain rejected.
12. The exchanged bearer token opens MCP Streamable HTTP only for the approved target and scopes.
13. Missing and invalid bearer tokens receive `401` with the path-specific `resource_metadata` URL and all `SupportedScopes` in canonical order.
14. A ChatGPT-style DCR registration, authorization-code + PKCE exchange, MCP initialize, `tools/list`, and one read-only tool call succeed end to end.

### Frontend tests

- `/login?next=<authorize>` forwards the encoded continuation to the Roblox login endpoint.
- An authenticated visit to `/login?next=<authorize>` resumes that internal target instead of navigating to `/download`.
- A normal login with no continuation retains the current `/download` behavior.

### Runtime verification

- Run the real browser flow from an unauthenticated ChatGPT connector attempt through Roblox login, consent, callback, token exchange, and one read-only Studio tool call.
- Confirm the consent page visually at desktop and narrow mobile widths.
- Repeat authorization in the same browser session and confirm consent is skipped only for the identical active target and scope set.
- Log out, reconnect, and confirm consent is required again.
- Revoke or stop the selected Studio target and confirm remembered consent cannot silently route to another Studio.

If a real ChatGPT callback identifier is unavailable during local development, complete the same flow with the repository’s OAuth integration harness and explicitly defer only the external ChatGPT-hosted redirect verification until the callback value is supplied by the ChatGPT plugin form.
