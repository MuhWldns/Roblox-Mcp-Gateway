# RobloxKit Gateway — VPS Runbook

Operating procedure for the single-instance gateway deployment
(`/opt/robloxkit` on an Ubuntu LTS VPS with a local MySQL 8 server, PM2 as
the process supervisor, and nginx as the TLS reverse proxy).

Release artifacts come from `scripts/build-release.ps1` and are verified by
`scripts/smoke-vps.ps1` before upload.

---

## 1. Deployment invariants

These rules are contractual. `internal/appconfig/production_test.go`
(`LoadProductionServer`, `ValidateProduction`, `ValidateEcosystem`) enforces
them at build time; violating them in operations breaks the same guarantees.

- **One instance, fork mode.** The Bridge hub registry, the rate limiter
  buckets, and live WSS sessions are process-local state. Never run
  `pm2 scale`, never raise `instances`, never switch to `exec_mode: cluster`.
- **Kill timeout exceeds the drain budget.** The server drains for up to 30 s
  on SIGINT (`shutdownBudget` in `cmd/server`); PM2's `kill_timeout` is 40 s
  in `ecosystem.config.cjs`. If you ever change the drain budget, raise
  `kill_timeout` with it.
- **HTTPS everywhere public.** `PUBLIC_APP_URL`, `MCP_RESOURCE_URL`, and
  `ALLOWED_ORIGIN` must be `https://`; the browser origin must be exact (no
  wildcard hosts).
- **Proxy trust is explicit.** `TRUSTED_PROXIES` lists the reverse proxy's
  CIDR(s). Client IPs are derived from `X-Forwarded-For` only when the direct
  peer is in this list.
- **Secret material is long.** `TOKEN_PEPPER` must be at least 32 bytes.
  Generate with `openssl rand -base64 48`.
- **Migrations are forward-only and manual.** Schema changes run through the
  migrate CLI only; server startup never migrates.

## 2. Layout

```
/opt/robloxkit/
  ecosystem.config.cjs     PM2 definition (from the repository)
  bin/
    robloxkit-server       gateway binary (copied from robloxkit-server-linux-amd64)
    robloxkit-migrate      migration CLI (copied from robloxkit-migrate-linux-amd64)
    RobloxBridge.exe       bridge artifact served to devices
  web -> releases/<version>/dist   frontend symlink (atomic swap)
  releases/<version>/      retained previous releases (binary + dist)
/etc/robloxkit/server.env  environment file, root:robloxkit 0600
```

Prerequisites: MySQL 8 server, Node.js LTS with `npm i -g pm2`, nginx with
TLS certificates, a non-root service user (examples use `robloxkit`).

## 3. Environment file (`/etc/robloxkit/server.env`)

Every key below is required unless marked optional. The server fails fast at
startup if anything required is missing or invalid.

```sh
# Public URLs (https required, wildcard origins rejected)
PUBLIC_APP_URL=https://app.example.test
MCP_RESOURCE_URL=https://api.example.test/mcp
ALLOWED_ORIGIN=https://app.example.test

# Server
LISTEN_ADDRESS=127.0.0.1:8080
TRUSTED_PROXIES=127.0.0.1/32
TOKEN_PEPPER=<openssl rand -base64 48>
HTTP_READ_TIMEOUT=15s
HTTP_WRITE_TIMEOUT=60s
MYSQL_MAX_OPEN_CONNS=24
MYSQL_MAX_IDLE_CONNS=6

# Bridge relay budgets
BRIDGE_HEARTBEAT_INTERVAL=20s
BRIDGE_TIMEOUT=45s
BRIDGE_QUEUE_LIMIT=128
BRIDGE_MAX_MESSAGE_BYTES=1048576

# Database
MYSQL_DSN=gateway:<password>@tcp(127.0.0.1:3306)/robloxkit?parseTime=true

# Optional
ROBLOX_CLIENT_ID=<client id>            # required for Roblox login flows
ROBLOX_CLIENT_SECRET=<client secret>
ROBLOX_PROVIDER_BASE_URL=https://apis.roblox.com
ADMIN_USER_IDS=<comma-separated user ids>
WEB_STATIC_DIR=/opt/robloxkit/web
BRIDGE_ARTIFACT_PATH=/opt/robloxkit/bin/RobloxBridge.exe
BRIDGE_ARTIFACT_FILENAME=RobloxBridge.exe
```

The deployment never puts this file inside the release tree; the release
artifacts must not contain `.env` files, logs, or `node_modules`
(`build-release.ps1` refuses to package them).

## 4. First-time provisioning

```sh
# Database and least-privilege account
mysql -e "CREATE DATABASE robloxkit CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"
mysql -e "CREATE USER 'gateway'@'127.0.0.1' IDENTIFIED BY '<password>'"
mysql -e "GRANT SELECT, INSERT, UPDATE, DELETE ON robloxkit.* TO 'gateway'@'127.0.0.1'"

# Schema
sudo -u robloxkit env MYSQL_DSN='gateway:<password>@tcp(127.0.0.1:3306)/robloxkit?parseTime=true' \
  /opt/robloxkit/bin/robloxkit-migrate -command up

# Supervisor
cd /opt/robloxkit && pm2 start ecosystem.config.cjs && pm2 save
pm2 startup systemd -u robloxkit --hp /home/robloxkit
```

## 5. Deploying a release

Run `scripts/build-release.ps1` locally, then `scripts/smoke-vps.ps1`. Only
upload after the smoke passes.

### 5.1 Upload and verify

```sh
# From the workstation: bin/{robloxkit-server-linux-amd64,robloxkit-migrate-linux-amd64,
# RobloxBridge-windows-amd64.exe,dist/,SHA-256SUMS} -> /opt/robloxkit/incoming/
ssh vps 'mkdir -p /opt/robloxkit/incoming'
scp bin/* vps:/opt/robloxkit/incoming/

ssh vps 'cd /opt/robloxkit/incoming && sha256sum -c SHA-256SUMS'
```

`sha256sum -c` must report every line OK. A mismatch aborts the deploy.

### 5.2 Back up MySQL (every deploy, before migrations)

```sh
umask 077
mysqldump --single-transaction --triggers --routines --events \
  -h 127.0.0.1 -u gateway -p robloxkit \
  | gzip > /var/backups/robloxkit-$(date +%Y%m%dT%H%M%SZ).sql.gz
```

Keep several generations. The nightly cron should run the same command.

### 5.3 Migrate (status → up → version)

```sh
export MYSQL_DSN='gateway:<password>@tcp(127.0.0.1:3306)/robloxkit?parseTime=true'
cd /opt/robloxkit/incoming
./robloxkit-migrate-linux-amd64 -command status
./robloxkit-migrate-linux-amd64 -command up
./robloxkit-migrate-linux-amd64 -command version   # record the version
```

`status` shows pending migrations before anything runs; the final `version`
must equal the number printed by `up`. Migrations are forward-only: the only
way back is the dump from 5.2 (see §8).

### 5.4 Atomic binary and frontend swap

```sh
version=$(date +%Y%m%dT%H%M%SZ)
mkdir -p /opt/robloxkit/releases/$version

# Stage, verify again, then move into place. mv within one filesystem is an
# atomic rename; a running process keeps the old inode until restart.
install -m 0755 /opt/robloxkit/incoming/robloxkit-server-linux-amd64 \
  /opt/robloxkit/bin/robloxkit-server.new
install -m 0755 /opt/robloxkit/incoming/robloxkit-migrate-linux-amd64 \
  /opt/robloxkit/bin/robloxkit-migrate.new
install -m 0755 /opt/robloxkit/incoming/RobloxBridge-windows-amd64.exe \
  /opt/robloxkit/bin/RobloxBridge.exe.new
mv -f /opt/robloxkit/bin/robloxkit-server.new /opt/robloxkit/bin/robloxkit-server
mv -f /opt/robloxkit/bin/robloxkit-migrate.new /opt/robloxkit/bin/robloxkit-migrate
mv -f /opt/robloxkit/bin/RobloxBridge.exe.new /opt/robloxkit/bin/RobloxBridge.exe

cp -r /opt/robloxkit/incoming/dist /opt/robloxkit/releases/$version/dist
# Atomic frontend swap through a symlink; the server reads per request.
ln -sfn /opt/robloxkit/releases/$version/dist /opt/robloxkit/web.new
mv -T /opt/robloxkit/web.new /opt/robloxkit/web
```

### 5.5 Restart and verify

```sh
cd /opt/robloxkit
set -a; . /etc/robloxkit/server.env; set +a
pm2 restart ecosystem.config.cjs --update-env

curl -fsS http://127.0.0.1:8080/healthz   # {"status":"ok"}
curl -fsS http://127.0.0.1:8080/readyz    # {"status":"ok"}
pm2 status
```

Watch the first minute of `pm2 logs robloxkit-server` for the startup JSON
records (`server exited cleanly` appears only on shutdown). Bridge devices
reconnect automatically after the restart.

## 6. Reverse proxy (nginx)

The proxy terminates TLS and forwards to `127.0.0.1:8080`. `TRUSTED_PROXIES`
must contain the proxy's source CIDR (`127.0.0.1/32` for a local nginx).

```nginx
server {
    listen 443 ssl http2;
    server_name api.example.test;

    ssl_certificate     /etc/letsencrypt/live/api.example.test/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/api.example.test/privkey.pem;

    # Bound bodies: keep this at or above the application limits
    # (BRIDGE_MAX_MESSAGE_BYTES / MCP request bound), and no larger.
    client_max_body_size 2m;

    location /bridge {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;      # WSS upgrade
        proxy_set_header Connection "upgrade";
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_read_timeout 300s;                     # heartbeat keeps it alive
        proxy_send_timeout 300s;
    }

    location /mcp {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_buffering off;        # Streamable HTTP responses must stream
        proxy_cache off;
        chunked_transfer_encoding on;
        proxy_read_timeout 300s;    # relayed tool calls are bounded server-side
    }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header X-Forwarded-For $remote_addr;
    }
}
```

Do not enable load balancing across backends: there is exactly one backend.
If a second VPS ever becomes necessary, the distributed-limiter work in the
PRD's "Explicit non-goals" must land first.

## 7. Health checks and logs

- `GET /healthz` — liveness; 200 while the process serves. Wire uptime
  monitoring here (it must never touch the database).
- `GET /readyz` — readiness; 200 only while the MySQL pool answers. Use it
  for deploy verification and reverse-proxy health gates.
- Both answer `{"status":"ok"}` or `{"status":"unavailable"}`; bodies never
  carry dependency errors or configuration.

Logs:

| What | Where |
| --- | --- |
| Gateway lifecycle (JSON on stderr) | `~robloxkit/.pm2/logs/robloxkit-server-error.log` |
| PM2 stdout | `~robloxkit/.pm2/logs/robloxkit-server-out.log` |
| nginx access/error | `/var/log/nginx/access.log`, `/var/log/nginx/error.log` |
| MySQL | `/var/log/mysql/error.log` |

The gateway logs one JSON record per operational event and never writes a
DSN, credential, token, or raw payload. Install `pm2-logrotate` (or logrotate
for nginx/MySQL) so the JSON stream cannot fill the disk.

## 8. Rollback

**Binary and frontend (safe, routine).** Previous releases stay under
`/opt/robloxkit/releases/`:

```sh
prev=<previous-version-directory>
install -m 0755 /opt/robloxkit/releases/$prev/robloxkit-server /opt/robloxkit/bin/robloxkit-server.new
mv -f /opt/robloxkit/bin/robloxkit-server.new /opt/robloxkit/bin/robloxkit-server
ln -sfn /opt/robloxkit/releases/$prev/dist /opt/robloxkit/web.new
mv -T /opt/robloxkit/web.new /opt/robloxkit/web
pm2 restart robloxkit-server --update-env
curl -fsS http://127.0.0.1:8080/readyz
```

**Schema (disruptive).** Migrations are forward-only and were applied before
the binary swap, so a database rollback is a restore of the §5.2 dump:

```sh
pm2 stop robloxkit-server
mysql -e 'DROP DATABASE robloxkit; CREATE DATABASE robloxkit CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci'
zcat /var/backups/robloxkit-<timestamp>.sql.gz | mysql -h 127.0.0.1 -u gateway -p robloxkit
pm2 start robloxkit-server --update-env
```

Treat this as incident recovery, not a routine operation: it restores the
database to the pre-deploy instant, so requests processed after the backup
are lost. Roll the binary back in the same incident to match the schema.

## 9. Troubleshooting quick reference

| Symptom | Check |
| --- | --- |
| `startup failed` at boot, exit 1 | `server.env` key missing/invalid — the message names the setting, never the value |
| `readyz` 503 | MySQL down, wrong DSN, or schema not migrated (`migrate -command status`) |
| Bridges flap between offline/online | heartbeat budget vs. proxy timeouts (`BRIDGE_HEARTBEAT_INTERVAL` < `proxy_read_timeout`) |
| MCP streaming truncates | `proxy_buffering off` missing on `/mcp` |
| Client IPs wrong in audit records | `TRUSTED_PROXIES` missing the nginx CIDR, or proxy not setting `X-Forwarded-For` |
| Process killed mid-drain | `kill_timeout` below the 30 s drain budget — fix `ecosystem.config.cjs` |
