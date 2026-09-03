# Remote Roblox Studio MCP Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Membangun layanan end-to-end yang menghubungkan ChatGPT dan Claude ke official Roblox Studio MCP melalui backend Go pada VPS dan `RobloxBridge.exe` pada Windows, dengan dashboard Vite, MySQL, Roblox login, free trial 14 hari, OAuth 2.1 MCP, serta license yang terikat ke Roblox identity dan device.

**Architecture:** Satu Go module menghasilkan binary `server`, `bridge`, dan `migrate`. Backend adalah modular monolith single-instance; MySQL menyimpan state persisten, sedangkan live Bridge connection dan pending request berada di memory. Frontend React/Vite adalah artifact terpisah. Package `pkg/bridgeproto` menjadi satu-satunya definisi WSS wire contract untuk server dan Bridge.

**Tech Stack:** Go 1.26.5; MCP Go SDK v1.7.0; coder/websocket v1.8.15; go-sql-driver/mysql v1.10.1; goose v3.28.0; Fosite v0.49.0; React 19.2.8; Vite 8.2.2; TypeScript 7.0.2; React Router 7.9.4; Vitest 4.1.11.

**Spec:** `PRD.md` version 3.1

## Global Constraints

- Backend MVP runs as exactly one process/instance; PM2 cluster mode is forbidden.
- Production MySQL 8.0+ is external; the application never creates or owns its container.
- Public network traffic uses HTTPS/WSS; the user PC opens no inbound port.
- Roblox OAuth, web session, device credential, and MCP OAuth token remain separate.
- A license binds one internal user, one explicit Roblox identity, and active device slots.
- A new Roblox User ID receives one 14×24-hour trial, starting only when the first device-binding transaction commits.
- Login and Bridge download do not start the trial. Reinstall, revoke, transfer, and account recovery do not reset it.
- Device transfer and Roblox identity recovery are admin-only, atomic, and audited.
- Device revocation does not free a license slot.
- Bridge accepts no remote executable path or arbitrary process argument.
- Unknown official MCP tools are denied until mapped to a scope.
- Calls with unknown side-effect outcome are never replayed automatically.
- Terminal status reflects real state and never exposes secrets, raw JSON-RPC, tool payloads, or stack traces.
- Public `/mcp` remains unavailable until auth, ownership, license/trial policy, rate limiting, and audit are active.
- Each task follows red-green-refactor and ends with a focused commit after verification passes.

---

## Repository Structure

```text
.
├── PRD.md
├── go.mod
├── go.sum
├── Makefile
├── .gitignore
├── cmd/
│   ├── server/main.go
│   ├── bridge/main.go
│   └── migrate/main.go
├── internal/
│   ├── appconfig/       # typed environment config and validation
│   ├── audit/           # append-only events and redaction
│   ├── bridgeapp/       # Bridge orchestration, WSS client, DPAPI
│   ├── bridgehub/       # authenticated server-side connection registry
│   ├── credential/      # opaque token generation and keyed digests
│   ├── device/          # enrollment and device lifecycle
│   ├── entitlement/     # paid license, trial, slot, recovery policy
│   ├── health/          # liveness/readiness state
│   ├── httpserver/      # route composition and browser middleware
│   ├── mcpgateway/      # MCP SDK adapter, authorization, correlation
│   ├── mcpoauth/        # connector OAuth authorization server
│   ├── mcpprocess/      # official MCP child process and stdio
│   ├── mysqlstore/      # SQL implementations of domain stores
│   ├── robloxauth/      # Roblox OAuth/OIDC login
│   ├── routing/         # pure device/Studio target resolution
│   ├── session/         # opaque browser sessions
│   └── statusui/        # terminal state machine and renderer
├── pkg/bridgeproto/     # shared versioned WSS envelopes
├── migrations/         # explicit MySQL migrations
├── testdata/fake-mcp/  # deterministic stdio MCP fixture
├── web/
│   ├── package.json
│   ├── package-lock.json
│   ├── vite.config.ts
│   ├── tsconfig.json
│   ├── index.html
│   └── src/
├── scripts/
│   ├── build-release.ps1
│   └── smoke-vps.ps1
├── installer/RobloxBridge.iss
└── docs/operations/
    ├── vps-runbook.md
    └── windows-bridge.md
```

Package boundaries:

- `pkg/bridgeproto` has wire types only; no process, database, or HTTP dependencies.
- `internal/mcpprocess` owns trusted launcher discovery and stdio only.
- `internal/bridgeapp` consumes credential storage, WSS, MCP process, and status sinks.
- `internal/bridgehub` owns active connections and bounded writer queues.
- `internal/routing` is a pure policy package; it performs no I/O.
- `internal/mcpgateway` adapts MCP transport to routing, policy, correlation, and Bridge delivery.
- `internal/robloxauth` never issues MCP tokens.
- `internal/mcpoauth` never stores or receives Roblox provider tokens.
- Domain packages declare store interfaces; `internal/mysqlstore` implements them.

---

## Phase 1 — Local Bridge Proof

### Task 1: Bootstrap module and typed configuration

**Files:**
- Create: `go.mod`
- Create: `.gitignore`
- Create: `Makefile`
- Create: `internal/appconfig/config.go`
- Test: `internal/appconfig/config_test.go`
- Create: `cmd/server/main.go`
- Create: `cmd/bridge/main.go`


**Interfaces:**
- Produces `appconfig.LoadServer(getenv func(string) string) (Server, error)`.
- Produces `appconfig.LoadBridge(getenv func(string) string) (Bridge, error)`.
- `Server` contains public URLs, listen address, MySQL DSN, allowed origin, trusted proxies, secrets, timeouts, and limits.
- `Bridge` contains gateway URL, credential path, trusted local launcher path, timeouts, and limits.

- [ ] **Step 1: Initialize Git and Go metadata**

```powershell
git init
go mod init robloxkit
```

Create `.gitignore` entries for `.env*`, `bin/`, `web/dist/`, `web/node_modules/`, coverage, logs, and local credential fixtures.

- [ ] **Step 2: Write failing config tests**

```go
func TestLoadServerRejectsMissingProductionSettings(t *testing.T) {
    _, err := LoadServer(func(string) string { return "" })
    require.ErrorContains(t, err, "MYSQL_DSN")
    require.ErrorContains(t, err, "TOKEN_PEPPER")
}

func TestLoadBridgeRequiresWSS(t *testing.T) {
    env := map[string]string{"BRIDGE_GATEWAY_URL": "ws://api.test/bridge"}
    _, err := LoadBridge(func(k string) string { return env[k] })
    require.ErrorContains(t, err, "wss")
}
```

- [ ] **Step 3: Verify red state**

```powershell
go test ./internal/appconfig -run TestLoad -v
```

Expected: FAIL because loaders are undefined.

- [ ] **Step 4: Implement minimal typed loading**

Use `net/url`, `strconv`, and `time.ParseDuration`. Collect all validation failures with `errors.Join`. Environment reads occur only in this package.

- [ ] **Step 5: Add minimal entry points and verify**

```powershell
go test ./internal/appconfig -v
go test ./...
git add go.mod go.sum .gitignore Makefile cmd internal/appconfig
git commit -m "build: bootstrap Go services and configuration"
```

Expected: both test commands PASS; invalid startup emits one sanitized error and exits non-zero.

### Task 2: Define the shared Bridge wire protocol

**Files:**
- Create: `pkg/bridgeproto/message.go`
- Create: `pkg/bridgeproto/validate.go`
- Test: `pkg/bridgeproto/message_test.go`
- Test: `pkg/bridgeproto/fuzz_test.go`

**Interfaces:**

```go
type Envelope struct {
    Version          int             `json:"version"`
    Type             MessageType     `json:"type"`
    GatewayRequestID string          `json:"gateway_request_id,omitempty"`
    DeviceID         string          `json:"device_id"`
    StudioID         string          `json:"studio_id,omitempty"`
    Deadline         time.Time       `json:"deadline,omitempty"`
    Payload          json.RawMessage `json:"payload,omitempty"`
}

func Encode(Envelope, Limits) ([]byte, error)
func Decode([]byte, Limits) (Envelope, error)
```

Message types: `hello`, `heartbeat`, `status`, `request`, `response`, `notification`, `cancel`, and `error`.

- [ ] **Step 1: Write round-trip and validation tests**

```go
func TestRequestRoundTripPreservesOriginalRPCID(t *testing.T) {
    raw := json.RawMessage(`{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{}}`)
    b, err := Encode(Envelope{Version: 1, Type: TypeRequest, GatewayRequestID: "gw_1", DeviceID: "dev_1", Payload: raw}, Limits{MaxPayloadBytes: 1 << 20})
    require.NoError(t, err)
    got, err := Decode(b, Limits{MaxPayloadBytes: 1 << 20})
    require.NoError(t, err)
    require.JSONEq(t, string(raw), string(got.Payload))
}
```

Also cover unknown version/type, missing correlation, malformed JSON, unknown fields, and oversized payload.

- [ ] **Step 2: Verify red state**

```powershell
go test ./pkg/bridgeproto -v
```

Expected: FAIL because protocol types are absent.

- [ ] **Step 3: Implement strict encode/decode and fuzz it**

Use explicit version `1`, `json.Decoder.DisallowUnknownFields`, pre-unmarshal byte limits, and message-specific required fields.

```powershell
go test ./pkg/bridgeproto -v -fuzz=FuzzDecode -fuzztime=10s
git add pkg/bridgeproto
git commit -m "feat: define versioned Bridge protocol"
```

Expected: PASS without panic.

### Task 3: Implement the terminal status state machine

**Files:**
- Create: `internal/statusui/state.go`
- Create: `internal/statusui/renderer.go`
- Test: `internal/statusui/state_test.go`
- Test: `internal/statusui/renderer_test.go`

**Interfaces:**

```go
type State string
type Event struct {
    State       State
    Code        string
    SafeMessage string
    RetryAfter  time.Duration
    DeviceName  string
    StudioCount int
}

type Machine struct { /* private state */ }
func (m *Machine) Transition(Event) error
func (r Renderer) Render(io.Writer, Event) error
```

States: initializing, enrollment-required, authenticating, connecting, MCP-starting, Studio-detecting, connected, reconnecting, degraded, fatal.

- [ ] **Step 1: Write state and redaction tests**

```go
func TestConnectedRequiresRealReadiness(t *testing.T) {
    m := NewMachine()
    require.Error(t, m.Transition(Event{State: Connected}))
    require.NoError(t, m.Transition(Event{State: Connecting}))
    require.NoError(t, m.Transition(Event{State: MCPStarting}))
    require.NoError(t, m.Transition(Event{State: StudioDetecting}))
    require.NoError(t, m.MarkReady(Readiness{Gateway: true, MCP: true}))
}
```

Assert output includes `SYSTEM CONNECTED`, `CONNECTION LOST`, retry delay, safe error code, and never echoes an internal diagnostic field.

- [ ] **Step 2: Verify red state, implement, and verify green**

```powershell
go test ./internal/statusui -v
# implement plain-text state validation and rendering
go test ./internal/statusui -v
git add internal/statusui
git commit -m "feat: add truthful Bridge terminal status"
```

Expected first run FAIL, second run PASS. ANSI color is optional and output remains complete without it.

### Task 4: Implement the official MCP child-process transport

**Files:**
- Create: `internal/mcpprocess/launcher.go`
- Create: `internal/mcpprocess/process.go`
- Create: `internal/mcpprocess/jsonrpc.go`
- Test: `internal/mcpprocess/process_test.go`
- Test: `internal/mcpprocess/jsonrpc_test.go`
- Create: `testdata/fake-mcp/main.go`

**Interfaces:**

```go
type Command struct { Path string; Args []string }
type Process interface {
    Start(context.Context) error
    Send(context.Context, json.RawMessage) error
    Responses() <-chan json.RawMessage
    Diagnostics() <-chan SafeProcessError
    Stop(context.Context) error
    Wait() error
}
```

`Launcher.Resolve()` returns a canonical trusted local path and fixed local arguments. No remote value can modify them.

- [ ] **Step 1: Create deterministic fake MCP and failing integration tests**

The fixture reads newline-delimited JSON-RPC, responds to `initialize`, `tools/list`, and echo `tools/call`, and writes diagnostic noise to stderr.

```go
func TestProcessSeparatesStderrFromProtocol(t *testing.T) {
    p := newFakeProcess(t)
    require.NoError(t, p.Start(t.Context()))
    require.NoError(t, p.Send(t.Context(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))
    require.JSONEq(t, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"test"}}`, string(<-p.Responses()))
    require.Contains(t, (<-p.Diagnostics()).Message, "fake diagnostic")
}
```

- [ ] **Step 2: Verify red state**

```powershell
go test ./internal/mcpprocess -v
```

Expected: FAIL.

- [ ] **Step 3: Implement process lifecycle and bounded framing**

Use `exec.CommandContext`, one stdin writer goroutine, separate stdout/stderr readers, maximum frame length, graceful stop followed by deadline-bound kill. A Windows `.bat` uses trusted `%COMSPEC% /d /s /c <validated-path>` only.

- [ ] **Step 4: Verify**

```powershell
go test ./internal/mcpprocess -race -v
git add internal/mcpprocess testdata/fake-mcp
git commit -m "feat: manage official MCP stdio process"
```

Expected: PASS; stderr never reaches response channel and no goroutine survives cancellation.

### Task 5: Assemble and smoke-test the local Bridge

**Files:**
- Create: `internal/bridgeapp/local.go`
- Test: `internal/bridgeapp/local_test.go`
- Modify: `cmd/bridge/main.go`
- Create: `scripts/build-release.ps1`

**Interfaces:**

```go
func RunLocal(ctx context.Context, deps LocalDeps) error
```

Consumes `statusui.Machine` and `mcpprocess.Process`. `SYSTEM CONNECTED` appears only after local MCP initialization and Studio readiness.

- [ ] **Step 1: Write orchestration tests**

Test exact event order, initialization failure, child crash/backoff, `Ctrl+C`, exit codes, and no replay after child failure.

- [ ] **Step 2: Red-green cycle**

```powershell
go test ./internal/bridgeapp -run TestRunLocal -v
# implement RunLocal and signal.NotifyContext wiring
go test ./internal/bridgeapp -run TestRunLocal -race -v
```

Expected first run FAIL, second PASS.

- [ ] **Step 3: Run actual binary against fake MCP**

```powershell
go build -o bin/fake-mcp.exe ./testdata/fake-mcp
go build -o bin/RobloxBridge.exe ./cmd/bridge
$env:BRIDGE_MCP_LAUNCHER=(Resolve-Path bin/fake-mcp.exe)
.\bin\RobloxBridge.exe
```

Expected: terminal reaches `SYSTEM CONNECTED`; `Ctrl+C` exits `0`.

- [ ] **Step 4: Phase 1 real-Studio gate and commit**

Run the Bridge with the locally installed official Roblox MCP. Exercise `initialize`, `tools/list`, and one read-only tool call. Do not commit local paths or raw payloads.

```powershell
git add cmd/bridge internal/bridgeapp scripts/build-release.ps1
git commit -m "feat: complete local Bridge proof"
```

---

## Phase 2 — Identity, Trial, Device Binding, and WSS

### Task 6: Add external MySQL and explicit migrations

**Files:**
- Create: `internal/mysqlstore/open.go`
- Create: `internal/mysqlstore/migrations.go`
- Test: `internal/mysqlstore/open_test.go`
- Test: `internal/mysqlstore/migrations_test.go`
- Create: `migrations/00001_identity_sessions.sql`
- Create: `migrations/00002_entitlements_devices.sql`
- Create: `migrations/00003_connector_oauth.sql`
- Create: `migrations/00004_audit_usage.sql`
- Create: `cmd/migrate/main.go`

**Interfaces:**

```go
func Open(ctx context.Context, dsn string, cfg PoolConfig) (*sql.DB, error)
func Migrate(ctx context.Context, db *sql.DB, command string) error
```

Supported production migration commands: `up`, `status`, `version`. Server startup checks schema compatibility and never auto-migrates.

- [ ] **Step 1: Write migration tests against `MYSQL_TEST_DSN`**

The test creates a uniquely named temporary database, applies migrations, inspects required tables/indexes/foreign keys, re-runs `up`, and drops the database in cleanup.

```go
func TestMigrationsCreateTrialAndBindingConstraints(t *testing.T) {
    db := newTemporaryDatabase(t)
    require.NoError(t, Migrate(t.Context(), db, "up"))
    require.True(t, hasUniqueIndex(t, db, "trial_entitlements", "user_id"))
    require.True(t, hasUniqueIndex(t, db, "trial_entitlement_identities", "provider", "provider_subject"))
    require.True(t, hasForeignKey(t, db, "license_device_bindings", "device_id"))
}
```

- [ ] **Step 2: Verify red state and implement exact schema**

```powershell
go test ./internal/mysqlstore -run TestMigrations -v
```

Expected: FAIL. Implement `utf8mb4`, UTC timestamps, binary token digests, unique `(provider, provider_subject)` identity, unique trial `user_id`, globally unique historical trial `(provider, provider_subject)`, device slot constraints, token lineage, append-only audit, and no plaintext token columns.

- [ ] **Step 3: Implement goose runner and verify**

Pin MySQL driver v1.10.1 and goose v3.28.0. Use `parseTime=true`, UTC, connection/read/write timeouts, and controlled migration-only multi-statements.

```powershell
go test ./internal/mysqlstore -v
git add cmd/migrate internal/mysqlstore migrations go.mod go.sum
git commit -m "feat: add explicit MySQL schema migrations"
```

### Task 7: Implement opaque credentials and web sessions

**Files:**
- Create: `internal/credential/token.go`
- Test: `internal/credential/token_test.go`
- Create: `internal/session/service.go`
- Test: `internal/session/service_test.go`
- Create: `internal/mysqlstore/session_store.go`
- Test: `internal/mysqlstore/session_store_test.go`

**Interfaces:**

```go
func Generate(prefix string, bytes int, pepper []byte) (plain string, digest [32]byte, err error)
func Digest(plain string, pepper []byte) [32]byte
func (s *Service) Create(ctx context.Context, userID string) (plain string, Session, error)
func (s *Service) Validate(ctx context.Context, plain string) (Session, error)
func (s *Service) Rotate(ctx context.Context, plain string) (string, Session, error)
func (s *Service) RevokeAll(ctx context.Context, userID string) error
```

Cookie: `__Host-robloxkit_session`, `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`.

- [ ] **Step 1: Write lifecycle tests**

Cover entropy, wrong pepper, expiry, rotation invalidation, revoke-all, constant-time digest comparison, and database storing no plaintext.

- [ ] **Step 2: Red-green cycle and commit**

```powershell
go test ./internal/credential ./internal/session ./internal/mysqlstore -run 'Test(Token|Session)' -v
# implement with crypto/rand and HMAC-SHA-256
go test ./internal/credential ./internal/session ./internal/mysqlstore -race -v
git add internal/credential internal/session internal/mysqlstore
git commit -m "feat: add opaque revocable web sessions"
```

Expected first run FAIL, second PASS. No browser JWT is introduced.

### Task 8: Implement Roblox login and internal identity binding

**Files:**
- Create: `internal/robloxauth/client.go`
- Create: `internal/robloxauth/flow.go`
- Create: `internal/robloxauth/handler.go`
- Test: `internal/robloxauth/flow_test.go`
- Test: `internal/robloxauth/handler_test.go`
- Create: `internal/mysqlstore/identity_store.go`
- Test: `internal/mysqlstore/identity_store_test.go`

**Interfaces:**

```go
func (f *Flow) Begin(ctx context.Context) (AuthorizeURL, LoginTransaction, error)
func (f *Flow) Complete(ctx context.Context, callback Callback) (RobloxIdentity, error)
func (s *IdentityStore) UpsertRobloxIdentity(ctx context.Context, identity RobloxIdentity) (User, error)
```

Provider endpoints: `/oauth/v1/authorize`, `/oauth/v1/token`, `/oauth/v1/userinfo` on `https://apis.roblox.com`. `sub` is the only identity key.

- [ ] **Step 1: Write provider fixture and failure tests**

Use `httptest.Server`; assert PKCE S256, state, nonce, exact redirect URI, `openid profile`, single-use login transaction, missing `sub`, stale callback, identity collision, and username metadata change.

- [ ] **Step 2: Red-green cycle**

```powershell
go test ./internal/robloxauth ./internal/mysqlstore -run 'Test(Roblox|Identity)' -v
# implement flow, handlers, and transactional identity upsert
go test ./internal/robloxauth ./internal/mysqlstore -race -v
```

Expected first run FAIL, second PASS. Provider tokens never enter API responses or logs.

- [ ] **Step 3: Commit**

```powershell
git add internal/robloxauth internal/mysqlstore
git commit -m "feat: bind Roblox login to internal identities"
```

### Task 9: Implement entitlement, free-trial, and device-slot policy

**Files:**
- Create: `internal/entitlement/model.go`
- Create: `internal/entitlement/service.go`
- Test: `internal/entitlement/service_test.go`
- Create: `internal/mysqlstore/entitlement_store.go`
- Create: `internal/audit/event.go`
- Create: `internal/audit/service.go`
- Create: `internal/mysqlstore/audit_store.go`
- Test: `internal/mysqlstore/entitlement_store_test.go`

**Interfaces:**

```go
type Clock interface { Now() time.Time }
type FirstDeviceBinding struct {
    UserID, IdentityID, Provider, ProviderSubject, DeviceID string
    CredentialDigest [32]byte
    AuditCorrelation string
}
func (s *Service) BindFirstDevice(ctx context.Context, in FirstDeviceBinding) (Entitlement, Binding, error)
func (s *Service) Authorize(ctx context.Context, subject Subject) (Decision, error)
func (s *Service) TransferDevice(ctx context.Context, actor AdminActor, licenseID, oldDeviceID, newDeviceID, reason string) error
func (s *Service) RecoverIdentity(ctx context.Context, actor AdminActor, userID, newIdentityID, reason, evidenceRef string) error
func (s *Service) ExtendTrial(ctx context.Context, actor AdminActor, entitlementID string, newEndsAt time.Time, reason string) error
```

- [ ] **Step 1: Write the complete trial and license invariant suite**

```go
func TestFirstBindingStartsExactlyOneFourteenDayTrial(t *testing.T) {
    clock := fixedClock(time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC))
    ent, _, err := service(clock).BindFirstDevice(t.Context(), FirstDeviceBinding{UserID: "user_1", IdentityID: "identity_1", Provider: "roblox", ProviderSubject: "1516563360", DeviceID: "device_1", CredentialDigest: digest("credential")})
    require.NoError(t, err)
    require.Equal(t, clock.Now(), ent.StartedAt)
    require.Equal(t, clock.Now().Add(14*24*time.Hour), ent.EndsAt)
}
```

Also cover: login/download do not start trial; failed binding transaction does not consume eligibility; second binding does not create a second trial; new internal account with historical Roblox identity is ineligible; revoke/reinstall/transfer/recovery do not reset; expiry blocks enrollment/WSS/MCP but permits dashboard/download; device revoke retains slot; concurrent last-slot activation has one winner; recovery revokes all credentials.

- [ ] **Step 2: Verify red state**

```powershell
go test ./internal/entitlement ./internal/mysqlstore -run 'Test(Trial|License|Device|Recovery)' -v
```

Expected: FAIL.

- [ ] **Step 3: Implement atomic transactions and row locks**

Use `SELECT ... FOR UPDATE` on user identity, entitlement, and slot rows. Create trial, historical trial-identity binding, first device binding, credential record, and append-only audit event in one transaction. `audit.Event` contains actor, action, correlation, reason, and safe before/after identifiers but no secret. Extension updates only the existing `ends_at` and adds an admin audit event.

- [ ] **Step 4: Verify concurrency repeatedly and commit**

```powershell
go test ./internal/entitlement ./internal/mysqlstore -run TestConcurrentLastSlot -count=50 -v
go test ./internal/entitlement ./internal/mysqlstore -race -v
git add internal/entitlement internal/audit internal/mysqlstore
git commit -m "feat: enforce one-time trial and device-bound licenses"
```

Expected: exactly one concurrent activation succeeds.

### Task 10: Deliver authenticated download and minimal enrollment web flow

**Files:**
- Create: `internal/device/download.go`
- Create: `internal/device/enrollment.go`
- Create: `internal/device/handler.go`
- Test: `internal/device/download_test.go`
- Test: `internal/device/enrollment_test.go`
- Create: `internal/mysqlstore/device_store.go`
- Create: `internal/bridgeapp/credential_store.go`
- Create: `internal/bridgeapp/credential_store_windows.go`
- Create: `internal/bridgeapp/credential_store_nonwindows.go`
- Test: `internal/bridgeapp/credential_store_test.go`
- Create: `internal/httpserver/router.go`
- Create: `internal/httpserver/csrf.go`
- Test: `internal/httpserver/router_test.go`
- Modify: `cmd/server/main.go`
- Create: `web/package.json`
- Create: `web/package-lock.json`
- Create: `web/tsconfig.json`
- Create: `web/vite.config.ts`
- Create: `web/index.html`
- Create: `web/src/main.tsx`
- Create: `web/src/api/client.ts`
- Create: `web/src/routes/Login.tsx`
- Create: `web/src/routes/Download.tsx`
- Create: `web/src/routes/Enroll.tsx`
- Test: `web/src/routes/Enroll.test.tsx`

**Interfaces:**

```go
func (d *DownloadHandler) ServeHTTP(http.ResponseWriter, *http.Request)
func (e *Enrollment) Begin(ctx context.Context, claim DeviceClaim) (UserCode, VerificationURL, error)
func (e *Enrollment) Approve(ctx context.Context, userID, userCode string) error
func (e *Enrollment) Exchange(ctx context.Context, deviceCode string) (DeviceCredential, error)
type CredentialStore interface { Load() ([]byte, error); Save([]byte) error; Delete() error }
```

Minimal routes: Roblox login/callback/logout; authenticated Bridge artifact metadata/download; enrollment approval; `GET /api/v1/me`; and CSRF token issuance. Frontend API calls use relative URLs plus `credentials: "include"` and never receive Roblox/device/MCP tokens.

- [ ] **Step 1: Write download, enrollment, and minimal-route tests**

Test download `401` without web session, checksum/version headers with session, no trial activation on login/download, exact CORS origin, required CSRF on approval, short-lived single-use codes, approval ownership, hostname display, first successful exchange starting trial atomically, exhausted slot, and partial transaction rollback.

- [ ] **Step 2: Write DPAPI tests**

On Windows, assert stored bytes do not contain plaintext and round-trip under the same user. Non-Windows returns `ErrUnsupportedSecureStore` and never writes plaintext.

- [ ] **Step 3: Pin and scaffold the minimal Vite application**

Pin React 19.2.8, React DOM 19.2.8, Vite 8.2.2, TypeScript 7.0.2, React Router 7.9.4, Vitest 4.1.11, Testing Library React 16.3.3, user-event 14.6.7, and jsdom 30.0.1. Write failing UI tests for login redirect, authenticated download, checksum display, enrollment confirmation, explicit “download does not start trial”, and first-binding trial state.

- [ ] **Step 4: Verify red state**

```powershell
go test ./internal/device ./internal/bridgeapp ./internal/httpserver -v
npm --prefix web test -- --run
```

Expected: both commands FAIL because handlers, secure store, router, and UI are absent.

- [ ] **Step 5: Implement the Phase 2 vertical slice**

Implement device handlers, one transaction covering trial/binding/credential, DPAPI, cookie+CSRF route composition, and minimal login/download/enrollment pages. The executable remains copyable; authorization remains enforced during enrollment/WSS/MCP.

- [ ] **Step 6: Verify behavior and production frontend build**

```powershell
go test ./internal/device ./internal/bridgeapp ./internal/httpserver -race -v
npm --prefix web test -- --run
npm --prefix web run build
```

Expected: PASS; `web/dist/index.html` exists; login/download alone leave trial absent; first committed binding creates exactly one trial.

- [ ] **Step 7: Browser-drive onboarding and commit**

Run the real minimal frontend/backend with the Roblox provider fixture: login, download, enter enrollment code, approve device, and observe trial start. Verify browser local/session storage contains no credential/token.

```powershell
git add cmd/server internal/device internal/bridgeapp internal/httpserver internal/mysqlstore web
git commit -m "feat: deliver licensed Bridge onboarding flow"
```
### Task 11: Build authenticated, bounded Bridge WebSocket hub

**Files:**
- Create: `internal/bridgehub/auth.go`
- Create: `internal/bridgehub/connection.go`
- Create: `internal/bridgehub/registry.go`
- Create: `internal/bridgehub/handler.go`
- Test: `internal/bridgehub/handler_test.go`
- Test: `internal/bridgehub/registry_test.go`

**Interfaces:**

```go
func (r *Registry) Register(deviceID string, conn *Connection) (replaced *Connection)
func (r *Registry) Send(ctx context.Context, deviceID string, env bridgeproto.Envelope) error
func (r *Registry) Disconnect(deviceID, safeReason string)
```

`/bridge` accepts only a valid device credential whose user, Roblox identity, entitlement, and device binding remain active.

- [ ] **Step 1: Write TLS WSS tests**

Cover valid, malformed, expired, revoked, wrong-owner, expired-trial, and unbound-device credentials; hello timeout; duplicate replacement; read limit; heartbeat timeout; full writer queue; and active revoke.

- [ ] **Step 2: Red-green cycle with coder/websocket**

```powershell
go test ./internal/bridgehub -v
# implement connection registry, one writer goroutine per connection, and close codes
go test ./internal/bridgehub -race -count=10 -v
```

Set the read limit immediately. Do not keep `r.Context()` after WebSocket hijack. Expected first run FAIL, second PASS with stable goroutine count.

- [ ] **Step 3: Commit**

```powershell
git add internal/bridgehub go.mod go.sum
git commit -m "feat: add authenticated bounded Bridge hub"
```

### Task 12: Add Bridge WSS client, reconnect, and no-replay behavior

**Files:**
- Create: `internal/bridgeapp/wsclient.go`
- Create: `internal/bridgeapp/backoff.go`
- Create: `internal/bridgeapp/remote.go`
- Test: `internal/bridgeapp/wsclient_test.go`
- Test: `internal/bridgeapp/backoff_test.go`
- Modify: `cmd/bridge/main.go`

**Interfaces:**

```go
func (b Backoff) Next(attempt int, random io.Reader) time.Duration
func RunRemote(ctx context.Context, deps RemoteDeps) error
```

- [ ] **Step 1: Write disconnect and reconnect tests**

Simulate a disconnect after a tool call reaches Bridge but before response. Assert the call is not resent; terminal transitions connected → reconnecting → connected; full status snapshot is sent after reauthentication.

- [ ] **Step 2: Red-green cycle**

```powershell
go test ./internal/bridgeapp -run 'Test(Remote|Reconnect|NoReplay)' -v
# implement bearer-header dialing, capped jitter, read/write loops, and status sync
go test ./internal/bridgehub ./internal/bridgeapp -race -count=10 -v
```

Expected first run FAIL, second PASS.

- [ ] **Step 3: Phase 2 smoke gate and commit**

Run server, external test MySQL, real Bridge, and fake/official MCP. Revoke the online device. Expected: WSS closes, reconnect fails, and slot/trial history remains.

```powershell
git add cmd/bridge internal/bridgeapp
git commit -m "feat: connect Bridge through authenticated WSS"
```

---

## Phase 3 — Connector OAuth and Remote MCP

### Task 13: Implement MCP OAuth discovery and storage

**Files:**
- Create: `internal/mcpoauth/model.go`
- Create: `internal/mcpoauth/discovery.go`
- Create: `internal/mcpoauth/store.go`
- Test: `internal/mcpoauth/discovery_test.go`
- Create: `internal/mysqlstore/oauth_store.go`
- Test: `internal/mysqlstore/oauth_store_test.go`

**Interfaces:**
- Protected-resource metadata identifies `/mcp` and one authorization server.
- Authorization-server metadata publishes issuer, authorization, token, revocation, S256, and supported client registration.
- Store atomically consumes authorization codes and rotates refresh-token families.

- [ ] **Step 1: Write metadata and storage tests**

Assert HTTPS absolute URLs, issuer consistency, S256 only, single-use code, hashed tokens, resource binding, and refresh reuse revocation.

- [ ] **Step 2: Red-green cycle**

```powershell
go test ./internal/mcpoauth ./internal/mysqlstore -run 'Test(Discovery|OAuth)' -v
# implement metadata and transactional storage
go test ./internal/mcpoauth ./internal/mysqlstore -race -v
```

Client ID Metadata Documents use an SSRF-safe HTTPS fetcher with DNS/IP validation, no private networks, strict size/content type, and controlled redirects.

- [ ] **Step 3: Commit**

```powershell
git add internal/mcpoauth internal/mysqlstore
git commit -m "feat: add MCP OAuth discovery and token storage"
```

### Task 14: Implement connector authorization, consent, token, and revocation

**Files:**
- Create: `internal/mcpoauth/provider.go`
- Create: `internal/mcpoauth/authorize.go`
- Create: `internal/mcpoauth/consent.go`
- Create: `internal/mcpoauth/token.go`
- Create: `internal/mcpoauth/revoke.go`
- Test: `internal/mcpoauth/provider_test.go`
- Test: `internal/mcpoauth/consent_test.go`

**Interfaces:**
- Endpoints: `/oauth/authorize`, `/oauth/token`, `/oauth/revoke`.
- Connector grant binds user, client, exact scopes, `/mcp` resource, active entitlement, device, and optional Studio.

- [ ] **Step 1: Write full OAuth tests**

Cover login redirect, exact redirect URI, state, mandatory S256, consent approve/deny, expired trial, wrong device owner, scope narrowing, code reuse, wrong verifier/resource, refresh rotation/reuse, and revocation.

- [ ] **Step 2: Red-green cycle with Fosite v0.49.0**

```powershell
go test ./internal/mcpoauth -run 'Test(Authorize|Consent|Token|Revoke)' -v
# compose authorization-code, PKCE-S256, refresh, and revocation handlers only
go test ./internal/mcpoauth ./internal/entitlement ./internal/mysqlstore -race -v
```

Disable implicit/password grants and debug messages. Persist grant and audit event in one transaction. Expected first run FAIL, second PASS.

- [ ] **Step 3: Commit**

```powershell
git add internal/mcpoauth internal/mysqlstore go.mod go.sum
git commit -m "feat: authorize ChatGPT and Claude connectors"
```

### Task 15: Implement Studio routing and request correlation

**Files:**
- Create: `internal/routing/resolver.go`
- Test: `internal/routing/resolver_test.go`
- Create: `internal/mcpgateway/pending.go`
- Create: `internal/mcpgateway/errors.go`
- Test: `internal/mcpgateway/pending_test.go`

**Interfaces:**

```go
func Resolve(grant GrantTarget, request RequestTarget, online []Studio) (ResolvedTarget, error)
func (p *Pending) Register(sessionID string, originalID json.RawMessage, deadline time.Time) (gatewayID string, result <-chan Result, err error)
func (p *Pending) Resolve(gatewayID string, result Result) error
func (p *Pending) CancelSession(sessionID string)
func (p *Pending) FailDevice(deviceID string, cause error)
```

- [ ] **Step 1: Write routing/correlation tests**

Cover explicit allowed Studio, default Studio, sole online Studio, ambiguity, cross-device Studio, offline device, and two clients both using JSON-RPC ID `1`.

- [ ] **Step 2: Red-green and race cycle**

```powershell
go test ./internal/routing ./internal/mcpgateway -v
# implement pure precedence and bounded pending registry
go test ./internal/routing ./internal/mcpgateway -race -count=25 -v
```

Late/duplicate responses return `ErrUnknownCorrelation`; cleanup happens exactly once.

- [ ] **Step 3: Commit**

```powershell
git add internal/routing internal/mcpgateway
git commit -m "feat: resolve Studio targets and correlate requests"
```

### Task 16: Expose authenticated MCP Streamable HTTP

**Files:**
- Create: `internal/mcpgateway/server.go`
- Create: `internal/mcpgateway/auth.go`
- Create: `internal/mcpgateway/tools.go`
- Create: `internal/mcpgateway/relay.go`
- Create: `internal/mcpgateway/policy.go`
- Test: `internal/mcpgateway/server_test.go`
- Test: `internal/mcpgateway/relay_test.go`
- Test: `internal/mcpgateway/policy_test.go`
- Create: `internal/httpserver/ratelimit.go`
- Test: `internal/httpserver/ratelimit_test.go`

**Interfaces:**
- Uses MCP Go SDK v1.7.0 `NewStreamableHTTPHandler`.
- Bearer token info supplies user, grant, resource, and scopes.
- `Policy.RequiredScope(toolName string) (scope string, allowed bool)` maps normalized official tool names to scopes; absent entries return `allowed=false`.
- A bounded in-memory limiter protects `/mcp` by connector grant and user before Bridge delivery; denied calls create a synchronous audit event.

- [ ] **Step 1: Write protected MCP, policy, and limiter tests**

Cover `401` plus `WWW-Authenticate`, wrong resource, revoked/expired token, expired trial, initialize/initialized, filtered `tools/list`, unknown-tool denial, allowed/denied `tools/call`, timeout, cancellation, disconnect, unknown outcome, late response, per-grant burst exhaustion, concurrent in-flight exhaustion, and audited policy/rate denials.

- [ ] **Step 2: Verify red state**

```powershell
go test ./internal/mcpgateway ./internal/httpserver -run 'Test(MCP|Bearer|Tool|Relay|Policy|Rate)' -v
```

Expected: FAIL because handler, policy, relay, and limiter are absent.

- [ ] **Step 3: Implement protected transport and verify**

Implement the SDK handler, origin validation, policy checks, minimum MCP limiter/audit, WSS relay, cancellation, and sanitized errors. Re-check grant, identity, entitlement, binding, target, rate limit, and scope on every call.

```powershell
go test ./internal/mcpgateway ./internal/bridgehub ./internal/bridgeapp ./internal/httpserver ./internal/audit -race -v
```

Expected: PASS.

- [ ] **Step 4: Real ChatGPT and Claude gate**

Expose one TLS endpoint. Independently validate discovery, login, consent, token exchange, initialize, `tools/list`, and one read-only `tools/call` in ChatGPT and Claude. If either host needs compatibility registration, add only the observed mechanism and its regression test.

- [ ] **Step 5: Commit**

```powershell
git add internal/mcpgateway internal/mcpoauth internal/httpserver internal/audit
git commit -m "feat: expose licensed remote MCP gateway"
```

---

## Phase 4 — Vite Dashboard

### Task 17: Expand Phase 2 onboarding into an authenticated dashboard shell

**Files:**
- Modify: `web/package.json`
- Modify: `web/package-lock.json`
- Modify: `web/vite.config.ts`
- Modify: `web/src/main.tsx`
- Create: `web/src/router.tsx`
- Modify: `web/src/api/client.ts`
- Create: `web/src/layout/AppShell.tsx`
- Modify: `web/src/routes/Login.tsx`
- Create: `web/src/routes/ErrorPage.tsx`
- Test: `web/src/router.test.tsx`

**Interfaces:**
- Retains the Phase 2 relative API client with `credentials: "include"`.
- Session loader calls `GET /api/v1/me`; `401` redirects to `/login`.
- Adds routes for devices, studios, connectors, license, diagnostics, and admin while preserving download/enrollment routes.

- [ ] **Step 1: Write failing shell and route tests**

Test unauthenticated redirect, authenticated shell, nested navigation, route-level API error boundary, preserved enrollment route, and cookie credentials.

- [ ] **Step 2: Verify red state, implement, and build**

```powershell
npm --prefix web test -- --run
# implement createBrowserRouter, authenticated shell, navigation, and error boundary
npm --prefix web test -- --run
npm --prefix web run build
```

Expected first run FAIL, later commands PASS; existing Phase 2 onboarding remains functional.

- [ ] **Step 3: Commit**

```powershell
git add web
git commit -m "feat: expand onboarding into dashboard shell"
```

### Task 18: Expand browser API middleware and add health routes

**Files:**
- Modify: `internal/httpserver/router.go`
- Create: `internal/httpserver/middleware.go`
- Modify: `internal/httpserver/csrf.go`
- Create: `internal/httpserver/api.go`
- Modify: `internal/httpserver/router_test.go`
- Create: `internal/httpserver/csrf_test.go`
- Create: `internal/health/handler.go`
- Test: `internal/health/handler_test.go`
- Modify: `cmd/server/main.go`

**Interfaces:**
- Dashboard reads: `/api/v1/me`, devices, studios, connectors, license, diagnostics, and existing Bridge artifact metadata/download.
- Mutations: device rename/revoke, connector target/revoke, and session revoke-all; existing enrollment approval remains available.
- Existing session-bound CSRF protection remains required through `X-CSRF-Token`.

- [ ] **Step 1: Write middleware and endpoint tests**

Cover request ID, panic redaction, secure headers, exact CORS, cookie validation, CSRF, body limit, method rejection, cross-user IDs, preservation of authenticated download/enrollment routes, `/healthz`, and `/readyz` without detail leakage.

- [ ] **Step 2: Red-green cycle**

```powershell
go test ./internal/httpserver ./internal/health -v
# implement standard HTTP middleware and route composition
go test ./internal/httpserver ./internal/health ./internal/robloxauth ./internal/session -race -v
```

Keep `/mcp` bearer auth and `/bridge` device auth outside cookie middleware.

- [ ] **Step 3: Smoke server and commit**

Start with test MySQL; expect health/readiness `200`, `/api/v1/me` without session `401`, and OAuth metadata `200`.

```powershell
git add cmd/server internal/httpserver internal/health
git commit -m "feat: expose secured dashboard API"
```

### Task 19: Build full dashboard management screens

**Files:**
- Modify: `web/src/routes/Download.tsx`
- Create: `web/src/routes/Devices.tsx`
- Create: `web/src/routes/Studios.tsx`
- Create: `web/src/routes/Connectors.tsx`
- Create: `web/src/routes/License.tsx`
- Create: `web/src/routes/Diagnostics.tsx`
- Create: `web/src/components/StatusBadge.tsx`
- Create: `web/src/components/ConfirmDialog.tsx`
- Test: `web/src/routes/Download.test.tsx`
- Test: `web/src/routes/Devices.test.tsx`
- Test: `web/src/routes/Connectors.test.tsx`
- Test: `web/src/routes/License.test.tsx`

**Interfaces:**
- Extends the working flow: Roblox login → Bridge download → enrollment approval → trial/license state → connector authorization.
- License page has no self-unbind or self-rebind control.

- [ ] **Step 1: Write screen behavior tests**

Test checksum/version display, explicit “download does not start trial”, trial countdown from server timestamps, device online/heartbeat, revoke warning that slot remains used, Studio ambiguity, connector scope/target/revoke, expired trial CTA, and absence of token values.

- [ ] **Step 2: Red-green cycle**

```powershell
npm --prefix web test -- --run
# implement accessible screens and confirmations
npm --prefix web test -- --run
npm --prefix web run build
```

Expected first run FAIL, later commands PASS.

- [ ] **Step 3: Browser-drive the real surface**

Run server and Vite; log in with provider fixture, download artifact, approve enrollment, verify trial starts only after binding, choose Studio, and revoke connector. At desktop and narrow viewport, status must not rely on color alone. Verify local/session storage contains no credential/token.

- [ ] **Step 4: Commit**

```powershell
git add web/src
git commit -m "feat: add Bridge onboarding and management dashboard"
```

---

## Phase 5 — Administration and Hardening

### Task 20: Add audited transfer, identity recovery, and trial extension

**Files:**
- Create: `internal/httpserver/admin.go`
- Test: `internal/httpserver/admin_test.go`
- Create: `web/src/routes/Admin.tsx`
- Create: `web/src/routes/DeviceTransfer.tsx`
- Create: `web/src/routes/AccountRecovery.tsx`
- Create: `web/src/routes/TrialExtension.tsx`
- Test: `web/src/routes/Admin.test.tsx`

**Interfaces:**
- Execute request requires case ID, reason, evidence reference, expected row version, and CSRF token.
- Identity recovery revokes all sessions, connector grants/tokens, device credentials, and active connections; trial history remains unchanged.
- Trial extension requires a new UTC expiry later than the current expiry and calls `entitlement.Service.ExtendTrial(ctx, actor, entitlementID, newEndsAt, reason)`; it never creates a second trial record.

- [ ] **Step 1: Write authorization and atomicity tests**

Cover user `403`, missing reason/evidence, stale/double execution, old-device disconnect, atomic slot move, identity supersession, full revocation, unchanged trial start/end during recovery, and audited extension that changes only `ends_at` on the existing entitlement.

- [ ] **Step 2: Red-green cycle**

```powershell
go test ./internal/httpserver ./internal/entitlement -run 'Test(Admin|Transfer|Recovery|Extension)' -v
npm --prefix web test -- --run Admin
# implement admin APIs and typed-confirmation UI
go test ./internal/httpserver ./internal/entitlement -race -v
npm --prefix web test -- --run Admin
```

Expected initial runs FAIL, final runs PASS.

- [ ] **Step 3: Live privileged-action tests and commit**

Execute transfer against a connected test Bridge: the old connection closes before the new binding activates and audit contains actor/reason/before/after. Extend one test trial: the same entitlement ID remains, only `ends_at` changes, and audit records actor/reason/old expiry/new expiry.

```powershell
git add internal/httpserver internal/entitlement web/src/routes
git commit -m "feat: add audited transfer recovery and trial extension"
```

### Task 21: Expand rate limiting, audit, usage, and secret redaction

**Files:**
- Modify: `internal/audit/event.go`
- Create: `internal/audit/redact.go`
- Modify: `internal/audit/service.go`
- Modify: `internal/mysqlstore/audit_store.go`
- Modify: `internal/httpserver/ratelimit.go`
- Test: `internal/audit/redact_test.go`
- Modify: `internal/httpserver/ratelimit_test.go`
- Create: `internal/mysqlstore/audit_store_test.go`

**Interfaces:**

```go
func (s *Service) Record(ctx context.Context, event Event) error
func (l *Limiter) Allow(now time.Time, key Key, cost int) Decision
func (u *UsageStore) Increment(ctx context.Context, gatewayRequestID string, usage Usage) error
```

- [ ] **Step 1: Write redaction, expanded-limit, and usage tests**

Inject Authorization, cookies, Roblox tokens, device credentials, enrollment/auth codes, PKCE verifier, DSN, and raw tool payload; assert none appears. Test burst/refill for login, OAuth, enrollment, WSS, admin, and MCP keys; concurrent request limit; and idempotent usage.

- [ ] **Step 2: Verify red state**

```powershell
go test ./internal/audit ./internal/httpserver ./internal/mysqlstore -run 'Test(Redact|Rate|Audit|Usage)' -v
```

Expected: FAIL for the new endpoint limits, redaction cases, and persistent usage.

- [ ] **Step 3: Implement hardening and verify**

Security-critical mutations continue to fail if transactional audit fails. Extend the Task 16 MCP limiter to login, OAuth, enrollment, WSS, and admin endpoints. Tool-call success audit uses a bounded queue and exposes a drop metric; denials remain synchronous.

```powershell
go test ./internal/audit ./internal/httpserver ./internal/mcpgateway ./internal/mysqlstore -race -v
```

Expected: PASS with no injected secret in captured logs.

- [ ] **Step 4: Commit**

```powershell
git add internal/audit internal/httpserver internal/mcpgateway internal/mysqlstore
git commit -m "feat: expand rate limits audit and usage accounting"
```

### Task 22: Add graceful shutdown and operational observability

**Files:**
- Create: `internal/health/readiness.go`
- Create: `internal/httpserver/server.go`
- Test: `internal/httpserver/server_test.go`
- Modify: `cmd/server/main.go`

**Interfaces:**

```go
func (s *Server) Shutdown(ctx context.Context) error
```

Shutdown order: mark unready, reject new MCP/WSS work, drain/fail pending calls, close WSS, close HTTP, then close MySQL.

- [ ] **Step 1: Write shutdown-order and safe-health tests**

Use spies to assert the exact order and bounded timeout. Health output contains no DSN, user count, raw dependency error, or device ID.

- [ ] **Step 2: Red-green and process smoke cycle**

```powershell
go test ./internal/health ./internal/httpserver -run 'Test(Ready|Shutdown)' -v
# implement lifecycle controller and slog JSON output
go test ./internal/health ./internal/httpserver -race -v
```

Start server, confirm readiness `200`, interrupt it, observe readiness becomes non-200 before exit, and Bridge reconnects without replay.

- [ ] **Step 3: Commit**

```powershell
git add cmd/server internal/health internal/httpserver
git commit -m "feat: add health observability and graceful shutdown"
```

---

## Phase 6 — VPS Release and Windows Distribution

### Task 23: Add deterministic release and VPS runbook

**Files:**
- Modify: `scripts/build-release.ps1`
- Create: `scripts/smoke-vps.ps1`
- Create: `ecosystem.config.cjs`
- Create: `docs/operations/vps-runbook.md`
- Test: `internal/appconfig/production_test.go`

**Interfaces:**
- Outputs Linux AMD64 server/migrate binaries, Windows AMD64 Bridge, `web/dist/`, and SHA-256 checksums.
- PM2 uses fork mode and `instances: 1`.

- [ ] **Step 1: Write production config tests**

Reject HTTP public URLs, wildcard browser origin, missing proxy trust, short cryptographic material, invalid limits, and any multi-instance marker.

- [ ] **Step 2: Implement release script**

```powershell
go test ./... -race
npm --prefix web test -- --run
npm --prefix web run build
$env:GOOS='linux'; $env:GOARCH='amd64'; go build -trimpath -o bin/robloxkit-server-linux-amd64 ./cmd/server
go build -trimpath -o bin/robloxkit-migrate-linux-amd64 ./cmd/migrate
$env:GOOS='windows'; go build -trimpath -o bin/RobloxBridge-windows-amd64.exe ./cmd/bridge
```

Script restores environment, adds version/commit linker flags, calculates checksums, and never packages `.env` or logs.

- [ ] **Step 3: Add PM2 and runbook**

PM2 config: `exec_mode: "fork"`, `instances: 1`, automatic restart, restart delay, and kill timeout longer than application drain. Runbook specifies MySQL backup, migration status/up, binary and frontend atomic swap, proxy WSS/MCP streaming, health checks, logs, and rollback.

- [ ] **Step 4: Verify artifacts and commit**

```powershell
powershell -File scripts/build-release.ps1
git add scripts ecosystem.config.cjs docs/operations internal/appconfig
git commit -m "ops: add single-instance VPS release workflow"
```

Expected: all binaries, `dist/`, and checksums exist; tests/build pass.

### Task 24: Add Windows service/installer and final E2E gate

**Files:**
- Create: `internal/bridgeapp/service_windows.go`
- Create: `internal/bridgeapp/service_nonwindows.go`
- Test: `internal/bridgeapp/service_windows_test.go`
- Modify: `cmd/bridge/main.go`
- Create: `installer/RobloxBridge.iss`
- Create: `docs/operations/windows-bridge.md`

**Interfaces:**
- Interactive mode renders terminal states.
- Service mode writes identical state events to structured local log and Windows service supervisor.
- Uninstall never frees the server-side license slot.

- [ ] **Step 1: Write mode and shutdown tests**

Assert both modes consume the same events; service stop gracefully closes child MCP and WSS; fatal startup returns non-zero/service failure.

- [ ] **Step 2: Red-green cycle and installer implementation**

```powershell
go test ./internal/bridgeapp -run TestService -v
# implement with golang.org/x/sys/windows/svc and create Inno Setup definition
go test ./internal/bridgeapp -run TestService -race -v
```

Installer includes only Bridge and product files—not official Roblox MCP, Node, or Electron. DPAPI credential persists according to chosen install-user/service identity. Documentation states uninstall leaves license binding occupied.

- [ ] **Step 3: Verify clean Windows VM lifecycle**

Install, login/download, enroll, confirm trial starts at binding, reach `SYSTEM CONNECTED`, run service mode, reboot, reconnect, stop, uninstall, and confirm binding/trial history remains.

- [ ] **Step 4: Run full automated verification**

```powershell
go test ./... -race
npm --prefix web test -- --run
npm --prefix web run build
powershell -File scripts/build-release.ps1
```

Expected: all commands PASS.

- [ ] **Step 5: Execute final production-like E2E matrix**

1. New Roblox User ID logs in; trial is absent.
2. Authenticated Bridge download succeeds; trial remains absent.
3. First enrollment atomically creates device binding and a 14×24-hour trial.
4. ChatGPT completes OAuth, initialize, `tools/list`, and one read-only call.
5. Claude completes the same flow independently.
6. Wrong Roblox account and cross-user/device/Studio access are denied.
7. Expired trial blocks enrollment, WSS readiness, connector authorization, and MCP execution while dashboard/download remain available.
8. Reinstall/revoke/transfer/recovery does not create a second trial.
9. Admin transfer atomically closes the old connection and preserves history.
10. Admin identity recovery revokes sessions/tokens/credentials/connections and preserves trial timestamps.
11. Multi-Studio ambiguity is denied; explicit allowed target succeeds.
12. Bridge disconnect and child MCP crash produce no automatic replay.
13. Backend graceful restart allows Bridge reconnect.
14. Temporary MySQL outage makes readiness fail without secret leakage.

- [ ] **Step 6: Commit distribution work**

```powershell
git add cmd/bridge internal/bridgeapp installer docs/operations/windows-bridge.md
git commit -m "feat: ship Windows Bridge service and installer"
```

---

## Mandatory Review Checkpoints

- **After Task 5:** Local Bridge successfully drives real official Roblox MCP. Stop implementation if this path is unstable.
- **After Task 12:** Roblox identity, one-time trial, device binding, authenticated WSS, revoke, reconnect, and no-replay are proven.
- **After Task 16:** ChatGPT and Claude both complete real connector OAuth and MCP calls. Do not expand dashboard before this gate passes.
- **After Task 19:** Browser-driven login, download, enrollment, trial display, target selection, diagnostics, and revocation work without browser-stored secrets.
- **After Task 21:** Rate limits, audit, usage accounting, and secret redaction are proven.
- **After Task 22:** Readiness, operational observability, and graceful shutdown are proven.
- **After Task 24:** Clean Windows/VPS production-like E2E matrix passes.

## Final Verification Contract

The project is not complete until this observed path succeeds:

```text
Roblox login
  → authenticated Bridge download
  → first device binding starts one 14-day trial
  → Bridge terminal SYSTEM CONNECTED
  → ChatGPT and Claude OAuth
  → MCP initialize
  → tools/list
  → read-only tools/call
  → response from the selected Roblox Studio
```

The completion evidence must also include wrong-account denial, expired trial, no trial reset, revoked/unlicensed device, admin transfer, identity recovery, ambiguous Studio, disconnect, MCP crash, unknown outcome without replay, backend restart, and MySQL outage behavior.
