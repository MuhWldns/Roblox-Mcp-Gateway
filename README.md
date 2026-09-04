# Roblox MCP Gateway

Remote Roblox Studio MCP Gateway & Bridge — menghubungkan ChatGPT dan Claude ke Roblox Studio melalui cloud gateway yang aman.

```
ChatGPT / Claude                    Browser
        │                             │
        │ MCP Streamable HTTP         │ HTTPS
        ▼                             ▼
┌──────────────────────────────────────────────┐
│              Go Backend — VPS               │
│ Dashboard API · Roblox OAuth · MCP OAuth     │
│ MCP Gateway · WSS Hub · Routing · Policy     │
│ License · Rate Limit · Audit · Health        │
└──────────────────────┬───────────────────────┘
                       │
                       ▼
                 External MySQL

                       ▲
                       │ authenticated outbound WSS
              ┌────────┴─────────┐
              │ RobloxBridge.exe │
              └────────┬─────────┘
                       │ stdio
                       ▼
              Official Roblox MCP
                       │
                       ▼
                 Roblox Studio
```

## Architecture

| Component | Stack | Description |
|---|---|---|
| **Backend** | Go 1.26, MySQL 8.0 | Modular monolith: dashboard API, Roblox OAuth, MCP OAuth server, MCP Streamable HTTP, WebSocket hub, routing, license enforcement, rate limiting, audit |
| **Frontend** | React 19, Vite, TypeScript | SPA dashboard: login, devices, Studio sessions, MCP connectors, diagnostics, license management |
| **Bridge** | Go, Windows binary | Outbound WSS agent, manages official Roblox MCP via stdio, terminal status UI |
| **Database** | MySQL 8.0+ | External instance, `utf8mb4`, forward-only migrations via goose |

## Quick Start

### Prerequisites

- Go 1.26+
- Node.js 20+ (for frontend)
- MySQL 8.0+
- PM2 (production)

### Build

```sh
# Backend
go build -o bin/robloxkit-server ./cmd/server
go build -o bin/robloxkit-bridge ./cmd/bridge
go build -o bin/robloxkit-migrate ./cmd/migrate

# Frontend
cd web && npm ci && npm run build
```

### Run

```sh
# Set environment
export PUBLIC_APP_URL=https://app.example.test
export MCP_RESOURCE_URL=https://api.example.test/mcp
export ALLOWED_ORIGIN=https://app.example.test
export LISTEN_ADDRESS=127.0.0.1:8080
export TRUSTED_PROXIES=127.0.0.1/32
export TOKEN_PEPPER=$(openssl rand -base64 48)
export MYSQL_DSN=gateway:password@tcp(127.0.0.1:3306)/robloxkit?parseTime=true

# Optional: Roblox OAuth
export ROBLOX_CLIENT_ID=<your-client-id>
export ROBLOX_CLIENT_SECRET=<your-client-secret>
export ADMIN_USER_IDS=<comma-separated>

# Migrate
./bin/robloxkit-migrate -command up

# Serve
./bin/robloxkit-server
```

### Test

```sh
go test ./...          # 22/22 packages
cd web && npm test     # frontend tests
```

## Deployment

Full VPS deployment guide: [`docs/operations/vps-runbook.md`](docs/operations/vps-runbook.md)

- **Single instance, fork mode** — bridge hub registry, rate limiter, and WSS sessions are process-local
- **PM2 supervisor** — `ecosystem.config.cjs`, `instances: 1`, `exec_mode: fork`
- **nginx reverse proxy** with TLS termination
- **Atomic binary swap** via `mv` within same filesystem
- **Forward-only migrations** with MySQL dump before every deploy

## Windows Bridge

Windows Bridge installer: [`installer/RobloxBridge.iss`](installer/RobloxBridge.iss) (Inno Setup)

User guide: [`docs/operations/windows-bridge.md`](docs/operations/windows-bridge.md)

## Project Structure

```
├── cmd/
│   ├── server/          # Gateway binary entry point
│   ├── bridge/          # Windows Bridge binary entry point
│   └── migrate/         # Migration CLI
├── internal/
│   ├── appconfig/       # Configuration loading and validation
│   ├── bridgeapp/       # Bridge application logic
│   ├── bridgehub/       # WebSocket hub and connection registry
│   ├── device/          # Device enrollment and management
│   ├── e2egate/         # End-to-end test harness
│   ├── entitlement/     # License and trial management
│   ├── health/          # /healthz and /readyz
│   ├── httpserver/      # HTTP router, middleware, CSRF
│   ├── mcpgateway/      # MCP Streamable HTTP server
│   ├── mcpoauth/        # MCP OAuth authorization server
│   ├── mcpprocess/      # Official MCP process management
│   ├── mysqlstore/      # MySQL repositories
│   ├── robloxauth/      # Roblox OAuth integration
│   ├── routing/         # Device and Studio routing
│   ├── session/         # Web session management
│   └── statusui/        # Bridge terminal status
├── pkg/bridgeproto/      # Bridge protocol definitions
├── web/                  # React SPA frontend
├── migrations/           # Versioned SQL migrations
├── scripts/              # Build, smoke, and E2E scripts
├── installer/            # Inno Setup installer
├── docs/                 # Operations and planning
└── testdata/             # Test fixtures
```

## Security

- Four credential boundaries: Roblox OAuth, web sessions, device credentials, MCP OAuth tokens
- CSRF protection on all cookie-authenticated mutations
- Content-Security-Policy on all responses
- No credentials in `localStorage`, `sessionStorage`, URLs, or client logs
- Bridge never opens inbound ports — outbound WSS only
- All secrets via environment variables, never hardcoded

## License

Proprietary. All rights reserved.