# Repository Guidelines

## Project Overview
**RobloxKit / Roblox MCP Gateway** is a secure, remote bridge connecting AI clients (ChatGPT, Claude) to local Roblox Studio instances over the Model Context Protocol (MCP).
- **Core Problem Solved**: Allows remote AI assistants to safely interact with a user's locally running Roblox Studio via an outbound-only WebSocket bridge, without exposing local network ports.
- **Key Capabilities**: MCP Streamable HTTP gateway, OAuth 2.0 server (RFC 6749 / RFC 7591 / RFC 8414), Roblox OAuth login, device enrollment, multi-device management, licensing & trial enforcement, token pepper hashing, CSRF protection, and audit logging.

---

## Architecture & Data Flow

### High-Level Architecture
```
ChatGPT / Claude (MCP Client)          Browser (SPA Dashboard)
        │ Streamable HTTP                      │ HTTPS / REST
        ▼                                      ▼
┌───────────────────────────────────────────────────────────────┐
│                    Go Backend (VPS Gateway)                   │
│  - Dashboard API (`/api/v1/*`)        - Roblox OAuth Login    │
│  - MCP OAuth Server (Fosite)          - MCP Gateway (`/mcp`)  │
│  - Bridge WSS Hub (`/bridge/v1/ws`)   - Policy & Routing      │
│  - License & Entitlements             - Audit & Rate Limiting │
└───────────────────────────────┬───────────────────────────────┘
                                │ SQL (goose migrations)
                                ▼
                          External MySQL
                                ▲
                                │ Outbound Authenticated WSS
                        ┌───────┴────────┐
                        │ RobloxBridge   │ (Windows Agent / Service)
                        └───────┬────────┘
                                │ stdio (JSON-RPC)
                                ▼
                       Official Roblox MCP ──► Roblox Studio
```

### Data Flow & Request Lifecycle
1. **Device Pairing & Trial Abuse Prevention**: `cmd/bridge` derives a stable HMAC-SHA256 from multiple Windows hardware identifiers. Every new pairing requires this 64-character hex fingerprint digest. The backend stores only the 32-byte binary digest (`fingerprint_hash`) backed by a unique index (`uq_devices_fingerprint`) to prevent cross-account trial reuse across distinct accounts on the same computer (random installation `device_id` alone does not enforce trial uniqueness). Historical rows may remain `NULL` only until same-owner re-pairing backfills them.
2. **AI Client Tool Invocation**: ChatGPT/Claude connects via `/mcp` with an OAuth 2.1 bearer token issued by `mcpoauth` (mandatory PKCE S256, refresh token rotation, Fosite-backed transactional store).
3. **Policy & Entitlement Check**: `mcpgateway` validates bearer/resource/origin, active license/trial, and tool execution policy; admission is managed by `routing.Resolver` (resolution precedence: explicit studio > bound studio > sole online instance; rejects offline/cross-device/ambiguous targets).
4. **WSS Message Relay**: Request is framed as a strict `pkg/bridgeproto.Message` envelope and forwarded over an active authenticated WebSocket (`bridgehub`, which enforces SHA-256 credential digests, hello handshake, and connection uniqueness per device).
5. **Local Execution**: `bridgeapp` receives envelope and relays to child MCP process via `mcpprocess` through stdio using JSON-RPC 2.0 frames to communicate with the official Roblox MCP server.
6. **Response & Audit**: Result returns over WSS, correlated via `Pending` tracker, audited with sensitive argument/token redaction (`audit`), and streamed back to the client over HTTP SSE/JSON-RPC.
---

## Key Directories

```
.
├── cmd/
│   ├── server/             # Gateway server entry point (monolith HTTP/WSS/OAuth/Dashboard API)
│   ├── bridge/             # Local Windows client entry point (smart wizard, service, remote runner)
│   └── migrate/            # CLI database migration runner
├── internal/
│   ├── appconfig/          # Environment variable parser & strict config validators
│   ├── audit/              # Structured audit logging & sensitive payload redaction
│   ├── bridgeapp/          # Local bridge runner, credential storage, Windows service support
│   ├── bridgeconfig/       # Local bridge configuration persistence
│   ├── bridgehub/          # In-memory WebSocket hub & connection registry for live bridges
│   ├── device/             # Device enrollment & bridge binary download handlers
│   ├── e2egate/            # End-to-end integration test matrix against live stack
│   ├── entitlement/        # License slots, trials, and active subscription enforcement
│   ├── httpserver/         # HTTP router, middleware (CSRF, auth, rate limit, trusted proxies)
│   ├── mcpgateway/         # MCP Streamable HTTP endpoint, tool registry, and relay loop
│   ├── mcpoauth/           # OAuth 2.0 provider (Fosite integration, dynamic client registration)
│   ├── mcpprocess/         # Child process spawner for stdio MCP servers (JSON-RPC 2.0)
│   ├── mysqlstore/         # Database persistence layer (sessions, OAuth, devices, audit, licenses)
│   ├── robloxauth/         # Roblox OAuth2 authorization code flow & OpenID Connect parsing
│   ├── routing/            # Studio & device route resolver for incoming tool calls
│   ├── session/            # User session service backed by MySQL store
│   └── statusui/           # Terminal UI & state renderer for the local bridge
├── pkg/
│   └── bridgeproto/        # Binary/JSON envelope wire protocol & validators between Gateway and Bridge
├── migrations/             # SQL schema migrations (00001 to 00008) embedded via goose
├── scripts/                # Release builds, VPS smoke tests, E2E runner scripts
└── web/                    # React 19 + Vite frontend dashboard
    ├── src/
    │   ├── api/client.ts   # Typed API client, error classes, and request wrappers
    │   ├── layout/         # AppShell navigation & layout frames
    │   ├── routes/         # Dashboard pages (Devices, Studios, Connectors, License, Admin, etc.)
    │   └── router.tsx      # React Router 7 route definitions & auth loader guard
```

---

## Development Commands

### Backend (Go 1.26+)
```sh
# Build binaries
go build -o bin/robloxkit-server ./cmd/server
go build -o bin/robloxkit-bridge ./cmd/bridge

# Run unit & package tests
go test ./...

# Run tests for a specific package with verbose output
go test -v ./internal/mcpgateway

# Run fuzz tests
go test -fuzz=FuzzMessageEnvelope ./pkg/bridgeproto

# Run full E2E matrix tests (requires MySQL)
go test -v ./internal/e2egate -run TestE2EProductionMatrix
```

### Frontend (Node.js 20+, npm)
```sh
cd web

# Install dependencies
npm ci

# Start development server
npm run dev

# Run Vitest test suite
npm test

# Type check TypeScript
npm run typecheck

# Production build
npm run build
```

### Database Migrations
Migrations are managed with `goose` and embedded in `migrations/`:
```sh
# Apply forward-only migrations
./bin/robloxkit-migrate -command up
```

---

## Code Conventions & Common Patterns

### Go Backend
- **Composition Root**: `cmd/server/main.go` explicitly wires configuration, MySQL stores, OIDC client, bridge hub, router, and gateways.
- **Standard Library First**: Built on standard `net/http` and `slog` for structured JSON logging. No heavy web frameworks.
- **Graceful Teardown**: Strict shutdown order: mark health unready -> refuse MCP/WSS connections -> drain/fail pending requests -> close bridge hub -> shutdown HTTP server -> close MySQL pool.
- **Strict Error Handling**: Wrap errors with context (`fmt.Errorf("...: %w", err)`). Combine multi-field validation errors using `errors.Join()`.
- **Clock & Time Injection**: Never call `time.Now()` directly in domain logic; inject a `Clock` interface (e.g., `systemClock`) to enable deterministic testing.
- **Explicit Dependency Injection**: Construct dependencies in package constructors (e.g., `NewServer(...)`, `NewService(...)`). Avoid global mutable state.
- **Concurrency & Context**: Always propagate `context.Context`. Graceful teardown uses `signal.NotifyContext` with explicit timeout budgets (`shutdownBudget = 30 * time.Second`).
- **Data Protection**: Store all sensitive tokens/credentials hashed using SHA-256 with a secret pepper (`TOKEN_PEPPER`). Never store plaintext tokens or credentials; cookies use `__Host-` prefixes. For device fingerprints, store only the 32-byte binary HMAC-SHA256 digest derived from Windows hardware identifiers—never raw identifiers. Trial uniqueness is enforced via unique device fingerprint index, rejecting cross-account device reuse while preserving `NULL` for historical rows until same-owner re-pairing backfills them.

### Bridge Client (`cmd/bridge`)
- **Modes**: Automatically determines runtime mode (Windows Service mode under SCM, remote daemon, local test runner, or smart first-run wizard).
- **Device Fingerprint**: Derives a stable HMAC-SHA256 from multiple Windows hardware identifiers (e.g. MachineGuid, SMBIOS UUID, volume serial) to supply the mandatory 64-character hex digest during pairing without transmitting or storing raw hardware identifiers.
- **Resilience**: Outbound-only WSS client reconnects indefinitely with exponential backoff and jitter for transient errors, halting only on terminal authentication failure.
- **Process Management**: `mcpprocess` isolates child MCP processes with dedicated read/write queues and JSON-RPC frame validation.
### Frontend (React & TypeScript)
- **Framework & Routing**: React 19 + React Router 7 (`createBrowserRouter`).
- **Route Protection**: Use router loaders (`sessionLoader`) in `router.tsx` to verify auth via `getMe()` and redirect unauthenticated requests to `/login`.
- **Security**: Anti-CSRF token managed in module memory; credentials sent via standard `credentials: "include"`; zero secrets stored in `localStorage`.
- **Tailwind CSS v4**: Utility-first styling via `@tailwindcss/vite`.
- **API Client**: Centralized in `web/src/api/client.ts` with typed error classes (`UnauthorizedError`, `ApiError`).
---

## Important Files & Entry Points

| File | Purpose |
|---|---|
| `cmd/server/main.go` | Gateway entry point: wires database stores, OAuth, HTTP handlers, WSS bridge hub, and graceful shutdown |
| `cmd/bridge/main.go` | Bridge client entry point: supports smart first-run wizard, remote daemon, and Windows service mode |
| `internal/appconfig/config.go` | Validates environment variables (`PUBLIC_APP_URL`, `MYSQL_DSN`, `TOKEN_PEPPER`, etc.) |
| `internal/mcpgateway/server.go` | Implements MCP Streamable HTTP gateway endpoint and tool routing |
| `internal/bridgehub/registry.go`| Manages active WebSocket connections from remote bridge instances |
| `pkg/bridgeproto/message.go` | Defines envelope structure, request/response IDs, and payload validation for bridge communication |
| `web/src/main.tsx` & `router.tsx` | Frontend entry point, routing hierarchy, and auth protection loader |
| `migrations/` | Forward-only SQL migrations (`00001` through `00008`) embedded via `embed.go` |

---

## Runtime & Tooling Preferences

- **Go**: Version `1.26.0` (module `robloxkit`).
- **Node.js**: Node 20+ with `npm` (lockfile `package-lock.json`).
- **Database**: MySQL 8.0+ configured with `utf8mb4` and `parseTime=true`.
- **Operating Environment**:
  - Server: Linux / VPS under PM2 (`fork` mode, single instance) behind Nginx reverse proxy.
  - Bridge: Windows (standalone `.exe` or registered Windows Service).

---

## Testing & QA Strategy

- **Unit Tests**: Table-driven tests across all packages (`*_test.go`).
- **Fuzz Testing**: Implemented in `pkg/bridgeproto/fuzz_test.go` to test parser robustness against malformed messages.
- **Frontend Tests**: Component and route testing using `vitest` + `@testing-library/react` + `jsdom`.
- **E2E Production Matrix**: `internal/e2egate/matrix_test.go` runs a 14-row sequential scenario suite covering user registration, enrollment, OAuth flows, and tool execution.
- **Trial Abuse & Entitlement Tests**: Validates trial policy and cross-account device fingerprint collision prevention (rejecting multi-account trial reuse on the same computer while allowing same-owner re-pairing and legacy `NULL` backfills).
- **Verification Rule**: Always verify code changes by running targeted package tests (`go test ./internal/...`) and frontend checks (`npm test && npm run typecheck`).
