# Admin access and ChatGPT MCP setup

This runbook covers two separate operator tasks:

1. granting a RobloxKit account access to the administration UI; and
2. connecting ChatGPT to the public RobloxKit MCP endpoint.

The procedures use the production origin `https://mcp.rbxskuy.web.id`. Never
put credentials, OAuth tokens, session cookies, or the contents of
`/etc/robloxkit/server.env` in tickets or chat messages.

## 1. Production endpoints

| Purpose | URL |
| --- | --- |
| RobloxKit dashboard | `https://mcp.rbxskuy.web.id/` |
| Administration UI | `https://mcp.rbxskuy.web.id/admin` |
| ChatGPT MCP server URL | `https://mcp.rbxskuy.web.id/mcp` |
| Protected-resource metadata | `https://mcp.rbxskuy.web.id/.well-known/oauth-protected-resource/mcp` |
| Authorization-server metadata | `https://mcp.rbxskuy.web.id/.well-known/oauth-authorization-server` |

The ChatGPT value is the complete HTTPS URL ending in `/mcp`, not the dashboard
URL and not an `/oauth/...` URL.

## 2. Enable an administrator

### 2.1 Security model

Administration is an application role, not an nginx password and not a Roblox
group rank. The server reads a comma-separated allowlist from
`ADMIN_USER_IDS` in `/etc/robloxkit/server.env` when the process starts.

Each entry must be the RobloxKit internal `user_id` returned by
`GET /api/v1/me`. Do not use a Roblox username, display name, or Roblox numeric
account ID. A signed-in account not present in the allowlist receives `403
administrator access required` from every admin preview and mutation endpoint.
The `/admin` page itself remains visible so it can render this explicit access
denial.

Grant this role only to named support operators. Admin actions can transfer a
paid-license slot, revoke an account's sessions, connector grants, tokens and
device credentials during recovery, or extend an existing trial.

### 2.2 Obtain the internal user ID

1. Sign in to `https://mcp.rbxskuy.web.id/` with the Roblox account that will
   become an administrator.
2. In that same browser session, open the browser developer console and run:

   ```js
   await fetch("/api/v1/me", { credentials: "same-origin" }).then((response) => response.json())
   ```

3. Copy only the returned `user_id`. Keep the value associated with the named
   operator in the access-control record; do not infer it from a display name.

A `401 authentication required` response means the browser session is not
signed in or has expired.

### 2.3 Update the VPS configuration

SSH to the VPS as an authorized operator and edit the root-owned environment
file without printing it:

```sh
sudoedit /etc/robloxkit/server.env
```

Add or replace the line below. Preserve existing administrators by separating
IDs with commas and no empty entries:

```dotenv
ADMIN_USER_IDS=<internal-user-id-1>,<internal-user-id-2>
```

Load the protected environment into the shell and restart the single PM2
process with the new values:

```sh
cd /opt/robloxkit
set -a
. /etc/robloxkit/server.env
set +a
pm2 restart ecosystem.config.cjs --update-env

curl -fsS "http://$LISTEN_ADDRESS/healthz"
curl -fsS "http://$LISTEN_ADDRESS/readyz"
pm2 status
```

Both probes must return `{"status":"ok"}` and PM2 must show exactly one
`robloxkit-server` instance online. Inspect the first minute of
`pm2 logs robloxkit-server` if either probe fails; do not paste configuration
values into the log or a support case.

### 2.4 Verify and use the admin UI

1. Open `https://mcp.rbxskuy.web.id/admin` with the allowlisted account.
2. Open the required tool and run its preview before changing state:
   - **Transfer a license slot** moves one active paid-license binding to a
     different device.
   - **Run an identity recovery** revokes the account's active sessions,
     connector grants and tokens, and device credentials. Treat it as a
     destructive incident action.
   - **Extend a trial** moves the expiry of an existing trial later; it does
     not create a second trial.
3. Record a unique support case ID, reason, and evidence reference.
4. Copy the version returned by the fresh preview into the expected-version
   confirmation field, review the complete effect, then execute once.
5. If the server reports stale state, stop, preview again, and review the new
   state. Do not bypass the version check or repeatedly submit an old preview.

A `403` on a preview means the session's internal user ID is not in the active
server allowlist. Check the ID type, comma formatting, and whether PM2 was
restarted with `--update-env` after sourcing the environment file.

### 2.5 Remove an administrator

Remove the internal ID from `ADMIN_USER_IDS`, source the environment again,
and repeat the PM2 restart and health checks from section 2.3. Verify that the
removed account receives `403` on an admin preview. Removing the role does not
revoke the account's normal dashboard session; use the recovery action first
if session revocation is also required.

## 3. ChatGPT connection prerequisites

Do not create the ChatGPT app until every check in section 4 passes.

- Use ChatGPT on the web. OpenAI currently provides full MCP actions to
  Business and Enterprise/Edu workspaces. Pro developer mode is limited to
  read/fetch MCP permissions and cannot exercise the complete Roblox Studio
  tool set.
- The workspace admin must enable developer mode. Enterprise/Edu workspaces
  may additionally require RBAC permission for the operator.
- The RobloxKit account must have an active trial or paid license.
- Its Bridge must be enrolled and online. Start the intended Roblox Studio
  session before authorization when the connector should be pinned to a
  specific Studio.
- The public endpoint must serve Streamable HTTP over valid HTTPS.
- OAuth protected-resource and authorization-server discovery must return JSON
  publicly. The OAuth flow uses authorization code with PKCE S256, dynamic
  client registration, refresh tokens, and bearer access tokens.

## 4. Remote MCP readiness gate

Run these checks from outside the VPS so they cover DNS, TLS, nginx, and the
application.

### 4.1 Discovery documents

```sh
curl -iS https://mcp.rbxskuy.web.id/.well-known/oauth-protected-resource/mcp
curl -iS https://mcp.rbxskuy.web.id/.well-known/oauth-authorization-server
```

Both responses must be `200` with `Content-Type: application/json` and valid
JSON. The protected-resource document must name
`https://mcp.rbxskuy.web.id/mcp`. The authorization-server document must
advertise HTTPS endpoints under `https://mcp.rbxskuy.web.id/oauth/`.

An HTML response beginning with `<!doctype html>` means nginx sent the SPA
fallback instead of proxying the well-known path. Do not continue to ChatGPT.

### 4.2 Streamable HTTP authentication challenge

```sh
curl -sS -D - -o /dev/null \
  -X POST https://mcp.rbxskuy.web.id/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2025-06-18' \
  --data '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"preflight","version":"1"}}}'
```

Before OAuth, the expected result is `401 Unauthorized` with a
`WWW-Authenticate: Bearer` challenge whose `resource_metadata` points to the
protected-resource metadata URL. `405 Method Not Allowed`, an HTML response,
or the absence of the Bearer challenge means the MCP transport is not ready.

### 4.3 Required public proxy paths

The active nginx virtual host must proxy all of these application paths to the
single RobloxKit backend:

```text
/mcp
/oauth/
/.well-known/oauth-protected-resource/mcp
/.well-known/oauth-authorization-server
```

`/mcp` must retain `proxy_buffering off` and a relay-compatible read timeout.
Do not load-balance the service: Bridge connections, MCP sessions, and rate
limiter state are process-local.

## 5. Add RobloxKit to ChatGPT

OpenAI changes labels and navigation as developer mode evolves. The current
workspace flow is:

1. Confirm that developer mode is enabled for your account:
   - **Business:** an admin/owner enables it from **Workspace settings → Apps →
     Create**, or from the account's Apps advanced settings when available.
   - **Enterprise/Edu:** an admin grants the required RBAC permission; the
     authorized user then enables **Settings → Apps → Advanced Settings →
     Developer mode**.
2. As an admin/owner, use **Workspace settings → Apps → Create**. An authorized
   developer can use **Settings → Apps → Create** when workspace policy allows
   it.
3. Enter a clear name such as `RobloxKit Studio` and a description stating that
   tool calls operate on the user's selected Roblox Studio session.
4. Enter this MCP server URL exactly:

   ```text
   https://mcp.rbxskuy.web.id/mcp
   ```

5. Select OAuth authentication if ChatGPT asks for an authentication method.
6. Choose **Scan Tools**. ChatGPT should discover OAuth, open the RobloxKit
   authorization flow, and ask the user to sign in if necessary.
7. On the RobloxKit consent page, verify the client name, scopes, target device,
   and optional Studio session. Approve only the scopes and target required for
   this connector.
8. Wait for the tool scan to finish, review every discovered tool and its
   permissions, then choose **Create**.
9. Keep the app as a draft while testing. Open a new chat, select the draft app
   from the tools menu, and exercise representative read and write requests.
   Confirm that requests reach the selected device and Studio and that
   consequential actions request confirmation where expected.
10. Publish only after the workspace owner has reviewed the tool list, OAuth
    behavior, write actions, and test evidence. Refresh or recreate the app
    after incompatible tool-schema or authentication changes; approved app
    metadata is not updated automatically.

If the OAuth authorization expires without refreshing, reconnect the app and
verify refresh-token issuance. RobloxKit advertises the `refresh_token` grant,
but its current scope list does not advertise `offline_access`; OpenAI notes
that providers without an offline-access scope may require users to
authenticate again.

## 6. Current production status (verified 2026-09-06)

The URLs in section 1 are the intended production URLs, but the ChatGPT MCP
connection is **not ready** in the currently deployed build:

- `ADMIN_USER_IDS` is unset, so admin actions return `403` until section 2 is
  completed.
- `POST /mcp` returns `405 Method Not Allowed` both through nginx and directly
  against the backend. The expected unauthenticated gateway response is `401`
  with a Bearer challenge.
- The backend serves both OAuth well-known documents as JSON, but the public
  nginx virtual host currently sends frontend HTML for those paths.
- The server entry point constructs OAuth metadata and the Bridge hub but does
  not yet construct and mount the OAuth provider or MCP gateway in
  `httpserver.Config`.

Before this status can be changed to ready, the server assembly must construct
`mcpoauth.Provider` and `mcpgateway.Gateway`, mount `OAuth`, `MCP`, and the
Bridge registry in the router, and nginx must proxy the OAuth and well-known
paths. Re-run all of section 4 after deployment. Do not attempt ChatGPT setup
while either readiness check fails.

## 7. Troubleshooting

| Symptom | Meaning and action |
| --- | --- |
| Admin tool returns `401` | The RobloxKit browser session is absent or expired. Sign in again. |
| Admin tool returns `403` | `ADMIN_USER_IDS` does not contain this session's internal `user_id`, or PM2 still has the old environment. |
| MCP probe returns `405` | The server process has not mounted the MCP gateway handler. |
| Well-known URL returns HTML | nginx routed discovery to the SPA fallback; proxy the exact well-known paths to the backend. |
| ChatGPT tool scan cannot authenticate | Verify both JSON discovery documents, `/oauth/` proxying, dynamic registration, redirect URI handling, and refresh-token issuance. |
| Consent shows no usable target | Enroll and start the Bridge, confirm license/trial state, and start the intended Studio session. |
| Tool call cannot reach Studio | Confirm the connector grant's device/Studio target, Bridge online state, granted scope, and active Studio session in the dashboard. |
| Tools changed but ChatGPT shows old schemas | Refresh the draft/connector metadata or recreate and republish it according to workspace policy. |

## 8. References

- [RobloxKit VPS runbook](./vps-runbook.md)
- [OpenAI: Developer mode and MCP apps in ChatGPT](https://help.openai.com/en/articles/12584461-developer-mode-apps-and-full-mcp-connectors-in-chatgpt-beta)
- [OpenAI: Connect and test your plugin](https://developers.openai.com/plugins/deploy/connect-chatgpt)
