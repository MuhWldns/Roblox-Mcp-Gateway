# PRD — Remote Roblox Studio MCP Gateway & Bridge

**Version:** 3.1  
**Status:** Approved  
**Target:** Web, Linux VPS, Windows, dan Roblox Studio  
**Primary goal:** Menghubungkan ChatGPT dan Claude ke Roblox Studio pada komputer user melalui gateway cloud yang aman.

---

## 1. Ringkasan Produk

Produk terdiri dari tiga komponen:

1. **Web application** berbasis React, Vite, dan TypeScript untuk login, account, device, Studio session, connector MCP, diagnostics, dan license.
2. **Backend modular monolith** berbasis Go untuk dashboard API, Roblox OAuth, OAuth layanan bagi MCP client, MCP Streamable HTTP, WebSocket gateway, routing, authorization, license enforcement, rate limiting, dan audit.
3. **RobloxBridge.exe** berbasis Go untuk Windows. Bridge menampilkan status pada terminal, membuat koneksi WSS outbound, menjalankan official Roblox Studio MCP secara lokal, dan meneruskan JSON-RPC melalui stdin/stdout.

User PC tidak membuka port publik. Bridge dapat bekerja di balik NAT/CGNAT tanpa port forwarding.

```text
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

---

## 2. Goals

- ChatGPT dan Claude dapat menghubungkan connector ke endpoint MCP publik menggunakan discovery dan OAuth standar.
- Request MCP dirutekan ke user, device, dan Studio session yang tepat.
- License terikat ke internal user, Roblox account, dan device yang telah diaktivasi.
- Bridge menjalankan dan mengawasi official Roblox MCP tanpa memodifikasi atau mendistribusikannya.
- Bridge memiliki terminal status yang sederhana dan jelas, tanpa GUI atau local dashboard pada MVP.
- Seluruh management user-facing dilakukan melalui web.
- Backend dan frontend dapat dideploy terpisah pada VPS; MySQL memakai instance yang sudah tersedia.
- Roblox OAuth token, web session, device credential, dan MCP OAuth token memiliki security boundary terpisah.
- Roblox User ID baru memperoleh satu free trial 14 hari yang dimulai saat device pertama berhasil di-bind.

## 3. Non-goals MVP

- Horizontal scaling backend gateway.
- Redis atau distributed connection registry.
- Kubernetes.
- GUI, Windows tray, atau local dashboard pada Bridge.
- Inbound connection ke user PC.
- Arbitrary shell execution melalui Bridge.
- Memodifikasi atau mendistribusikan official Roblox MCP.
- Self-service Roblox account rebind atau device license transfer.
- Billing provider integration penuh.
- Menyimpan Roblox OAuth token untuk fitur Open Cloud.
- Self-update dan code signing pada proof-of-path awal.

---

## 4. Prinsip Arsitektur

### 4.1 Server adalah aplikasi utama

Seluruh fungsi user-facing berada di web application dan backend:

- Authentication dan account.
- Device enrollment, status, dan revocation.
- MCP connector authorization dan revocation.
- Device dan Studio target selection.
- Bridge dan official MCP diagnostics.
- License, usage, dan audit.

Bridge adalah infrastructure agent. Terminal Bridge hanya menunjukkan status operasional lokal.

### 4.2 Tidak ada inbound connection ke user PC

```text
RobloxBridge.exe ── outbound WSS ──► Go Backend
```

Bridge tidak boleh membuka MCP endpoint publik, membuka listener pada network interface publik, memerlukan port forwarding, atau mengekspos official Roblox MCP ke internet.

### 4.3 Credential boundaries

Empat credential wajib dipisahkan:

1. Roblox OAuth untuk membuktikan identity ketika login web.
2. Opaque web session milik layanan untuk dashboard.
3. Device credential khusus setiap instalasi Bridge.
4. OAuth access/refresh token milik layanan untuk ChatGPT dan Claude.

Satu credential tidak boleh dipakai sebagai pengganti credential lain.

### 4.4 Single backend instance untuk MVP

Backend menyimpan live Bridge registry dan pending request correlation di memori. Karena itu:

- Backend MVP berjalan sebagai satu process dan satu instance.
- Jika memakai PM2, wajib fork mode dengan `instances: 1`.
- PM2 cluster mode dan multiple backend replica dilarang pada MVP.
- Frontend static dapat direplikasi.
- Multi-instance baru diperbolehkan setelah distributed connection ownership, inter-instance routing, pub/sub, revocation propagation, dan failover telah diuji.

---

## 5. Technology Stack

### 5.1 Backend

- Go versi stabil yang didukung saat implementasi.
- Official Model Context Protocol Go SDK dengan versi dipin.
- Standard-compatible HTTP router dan middleware.
- WebSocket library dengan context cancellation, ping/pong, deadline, bounded queue, dan bounded message size.
- Structured logging.
- MySQL driver dengan connection pooling dan context support.
- Versioned SQL migration tool.

Backend tidak menggunakan Node.js, TypeScript, atau Hono.

### 5.2 Bridge

- Go.
- Single Windows executable milik produk.
- Tidak membutuhkan Node.js atau Electron untuk menjalankan Bridge.
- Terminal status sederhana; tidak menggunakan GUI framework.
- Official Roblox MCP boleh memiliki dependency sendiri; Bridge tidak membundelnya tanpa izin.

### 5.3 Frontend

- React.
- Vite.
- TypeScript.
- Client-side router.
- Production artifact berupa hasil `vite build` dalam direktori `dist/`.
- Tidak memerlukan server-side rendering untuk MVP.

### 5.4 Database

- MySQL 8.0+ atau versi kompatibel yang masih mendapat security support.
- MySQL berjalan sebagai service/container eksternal yang sudah tersedia.
- Deployment aplikasi tidak membuat container database baru.
- Character set `utf8mb4`.
- Timestamp disimpan dalam UTC.
- Foreign key, unique constraint, dan index menjaga ownership serta lookup kritis.

### 5.5 VPS deployment

Deployment supervisor-agnostic. PM2, systemd, atau process manager lain dapat digunakan selama memenuhi operational contract.

Deployment unit:

1. Satu binary backend Go.
2. Frontend static artifact `dist/`.
3. External MySQL.
4. Reverse proxy dan TLS termination yang dikelola user.

PRD tidak mengunci Caddy, Nginx, Docker, atau layout host tertentu.

---

## 6. Backend Go Modular Monolith

```text
Go Backend
├── HTTP Server
├── Dashboard API
├── Roblox Identity
├── Web Sessions
├── MCP Authorization Server
├── MCP Streamable HTTP Server
├── Device Enrollment
├── Bridge WebSocket Hub
├── Device & Studio Router
├── License & Policy Enforcement
├── Request Correlation
├── Rate Limiting
├── Audit & Usage
├── MySQL Repositories
└── Health & Graceful Shutdown
```

### 6.1 Endpoint konseptual

```text
/api/v1/*                 Dashboard API
/auth/roblox/login        Mulai Roblox login
/auth/roblox/callback     Roblox authorization callback
/auth/logout              Revoke web session
/api/v1/downloads/bridge  Authenticated Bridge download
/oauth/*                  OAuth authorization server layanan
/.well-known/*            OAuth dan protected-resource metadata
/mcp                      MCP Streamable HTTP protected resource
/bridge                   Authenticated Bridge WebSocket
/healthz                  Liveness
/readyz                   Readiness
```

### 6.2 Middleware minimum

- Request ID dan correlation ID.
- Panic recovery tanpa membocorkan stack trace.
- Structured access log dengan redaction.
- Secure response headers.
- Origin/CORS policy khusus endpoint browser.
- Web session validation dan CSRF protection.
- Bearer token verification pada `/mcp`.
- Scope, ownership, license, dan rate-limit enforcement.
- Bounded request body dan timeout.
- Trusted proxy validation.

---

## 7. Frontend Web Application

### 7.1 Halaman MVP

#### Login dan Account

- Login/logout dengan Roblox.
- Internal user ID.
- Roblox user ID.
- Roblox username, display name, dan avatar sebagai metadata tampilan.
- License plan, status, dan expiry.
- Logout seluruh web session.

#### Download Bridge

- Hanya tersedia setelah Roblox login menghasilkan web session yang valid.
- Menyediakan signed/checksummed Windows Bridge artifact dan version information.
- Download tidak mengaktivasi trial dan tidak membuat device binding.
- File executable dapat disalin, sehingga download gate bukan security boundary; enrollment, WSS, dan MCP tetap melakukan license enforcement.

#### Devices

- Nama device, hostname, platform, dan Bridge version.
- Online/offline state dan last heartbeat.
- Official Roblox MCP state.
- Reconnection count dan sanitized last error.
- Device enrollment, rename, credential rotation, dan revoke.
- License slot state.
- Permintaan transfer device untuk diproses admin; tidak ada self-service unbind.

#### Studio Sessions

- Owner device.
- `studio_id`.
- Connected/disconnected state.
- Last seen.
- Pemilihan default Studio target connector.

#### MCP Connectors

- Client ChatGPT atau Claude.
- Granted scopes.
- Target device dan optional default Studio.
- Created, last used, expiry, dan revoked state.
- Revoke connector dan seluruh token terkait.

#### Diagnostics

- Backend reachability.
- Bridge WSS status.
- Official MCP process status.
- Version compatibility.
- Last heartbeat, reconnect count, dan sanitized error.

#### License dan Usage

- Owner internal user dan masked Roblox identity.
- Current plan dan expiry.
- Device limit, active bindings, dan available slot.
- Request/usage limit.
- Scope yang diizinkan plan.
- Transfer/recovery request status.

### 7.2 Frontend security

- Dashboard memakai cookie `HttpOnly`, `Secure`, dan `SameSite=Lax`.
- Roblox token, device credential, MCP access/refresh token, authorization code, dan PKCE verifier tidak tersedia bagi frontend JavaScript.
- Secret/token tidak disimpan di `localStorage`, `sessionStorage`, URL permanen, atau client log.
- Cookie-authenticated mutation membutuhkan CSRF protection.
- UI bukan security boundary; backend memeriksa ulang seluruh ownership dan policy.

---

## 8. Internal User dan Roblox OAuth

### 8.1 Identity model

Setiap account memiliki:

- Internal `user_id` sebagai primary identity layanan.
- Active Roblox identity dengan `roblox_user_id` dari OpenID Connect claim `sub`.
- Roblox username, display name, avatar URL, dan profile metadata hanya untuk tampilan.

Roblox username/display name tidak boleh menjadi identity key karena dapat berubah. Active `roblox_user_id` harus unique dan user tidak dapat menggantinya sendiri.

### 8.2 Login flow

Roblox OAuth hanya digunakan untuk login dashboard:

```text
Browser
  ↓ GET /auth/roblox/login
Backend membuat state, nonce, dan PKCE verifier
  ↓ redirect
Roblox authorization endpoint
  ↓ authorization code
Backend callback
  ↓ validate state + exchange code + validate identity
Roblox userinfo
  ↓ claim sub
Create/link internal user
  ↓
Create opaque web session
```

Requirements:

- Authorization Code flow.
- PKCE S256.
- Cryptographically random `state`.
- `nonce` untuk mengikat identity response.
- Scope minimum `openid profile`.
- Exact registered redirect URI.
- Callback state single-use dan short-lived.
- Claim `sub` menjadi `roblox_user_id`.
- Roblox token tidak diberikan ke frontend, Bridge, atau MCP client.
- Provider token dibuang setelah identity diperoleh kecuali fitur Open Cloud dirancang terpisah.

### 8.3 Web session

- Opaque cryptographically random token.
- Database hanya menyimpan hash token.
- Session memiliki created, last used, expiry, dan revoked timestamp.
- Session dirotasi setelah login berhasil.
- Logout merevoke session aktif.
- User dapat merevoke seluruh web session.

---

## 9. License Binding

### 9.1 Invariant utama

License terikat pada:

1. Internal `user_id` pemilik license.
2. Specific `roblox_identity_id` yang menunjuk ke `roblox_user_id` aktif saat license di-bind.
3. Daftar `device_id` yang telah diaktivasi ke slot license.

Setiap request berlisensi wajib memenuhi:

```text
connector_grant.user_id
  = device.user_id
  = license.user_id

dan
license.roblox_identity_id
  = active user identity untuk provider Roblox

dan
device_id memiliki active license-device binding
```

### 9.2 Free trial 14 hari

- Roblox User ID baru eligible tepat satu kali sepanjang histori layanan.
- Login pertama membuat internal user dan identity record, tetapi tidak memulai trial.
- Download Bridge tidak memulai trial.
- Jika belum ada paid entitlement, binding device pertama membuat trial entitlement dan device binding secara atomic.
- `trial_started_at` memakai waktu server UTC saat transaction binding berhasil commit.
- `trial_ends_at = trial_started_at + 14 × 24 jam`; trial tidak dijeda.
- Eligibility diperiksa terhadap internal `user_id` dan seluruh histori Roblox identity agar pembuatan ulang account internal atau recovery tidak menghasilkan trial kedua.
- Reinstall, device revoke, credential rotation, device transfer, atau Roblox account recovery/rebind tidak mereset atau memperpanjang trial.
- Setelah trial berakhir, user tetap dapat login, membuka dashboard, mengunduh Bridge untuk update/recovery, dan membeli license; enrollment baru, WSS readiness, connector authorization, serta MCP execution ditolak tanpa active paid entitlement.
- Admin tidak boleh menghapus histori trial. Extension trial hanya melalui explicit admin grant yang mencatat actor, reason, before/after, dan expiry baru pada audit.

### 9.3 Device slot

- User dapat mengaktivasi device baru selama masih ada slot kosong.
- Aktivasi membuat `license_device_binding` yang permanen sampai admin melakukan transfer/replacement.
- Rename device atau perubahan hostname tidak mengubah binding.
- Rotasi credential pada `device_id` yang sama tidak mengonsumsi slot baru.
- Revoke device/credential menghentikan akses tetapi tidak otomatis membebaskan license slot.
- Reinstall yang menghasilkan `device_id` baru dianggap device baru dan memerlukan slot baru atau transfer admin.
- Menyalin credential ke PC lain tidak menghasilkan valid device karena secret dilindungi DPAPI dan tetap terikat server-side ke device/user.

### 9.4 Transfer device

Device transfer/replacement hanya dapat dilakukan admin:

- User tidak memiliki endpoint self-service untuk unbind atau transfer.
- Admin wajib memilih binding lama, binding baru, dan reason.
- Perubahan mencatat actor admin, timestamp, before/after state, dan reason pada immutable audit trail.
- Binding lama tidak dihapus; status menjadi `replaced`, `revoked`, atau `superseded`.
- Credential dan koneksi device lama direvoke sebelum slot dipindahkan.
- Transfer harus atomic agar satu slot tidak aktif pada dua device.

### 9.5 Roblox account recovery/rebind

Roblox account tidak dapat diganti oleh user. Rebind hanya melalui admin recovery ketat:

1. Admin memverifikasi ownership license dan recovery evidence.
2. Admin mencatat reason dan reference pada audit.
3. Seluruh web session direvoke.
4. Seluruh MCP connector grant, access token, dan refresh token direvoke.
5. Seluruh device credential dan active Bridge connection direvoke.
6. Identity Roblox lama ditandai `superseded`; record tidak dihapus.
7. Roblox identity baru diverifikasi dan di-bind ke internal user.
8. `license.roblox_identity_id` dipindahkan secara atomic ke identity baru oleh workflow recovery yang sama.
9. User login, enroll/activate device, dan authorize connector ulang.

Rebind tidak boleh memindahkan license ke internal user lain secara diam-diam. Perubahan owner license merupakan admin operation terpisah dengan authorization lebih tinggi dan audit tersendiri.

### 9.6 License enforcement

```text
Authenticated connector
  ↓
Internal user + active Roblox identity valid?
  ↓
License active dan dimiliki user tersebut?
  ↓
Device memiliki active license binding?
  ↓
Connector grant menargetkan device tersebut?
  ↓
Scopes dan usage limit valid?
  ↓
Execute
```

Bridge tidak pernah menjadi source of truth license.

---

## 10. OAuth 2.1 untuk ChatGPT dan Claude

Roblox OAuth tidak mengotorisasi MCP client. Backend menjadi OAuth authorization server milik layanan; `/mcp` menjadi protected resource.

### 10.1 Protocol requirements

- OAuth 2.1 Authorization Code flow.
- PKCE S256 wajib.
- MCP Protected Resource Metadata.
- OAuth Authorization Server Metadata.
- Resource/audience binding ke endpoint MCP.
- Client ID Metadata Documents ketika didukung.
- Compatibility registration hanya jika target host nyata membutuhkannya.
- Tidak mendukung implicit grant atau resource owner password grant.
- Missing/invalid bearer token menghasilkan `401` dan `WWW-Authenticate` yang menunjuk protected-resource metadata.
- Insufficient scope ditolak sebelum tool dijalankan.

Protocol MCP dinegosiasikan berdasarkan versi yang didukung SDK, ChatGPT, dan Claude pada saat implementasi. Dukungan wajib dibuktikan dengan kedua host nyata.

### 10.2 Connector flow

```text
ChatGPT / Claude
  ↓ discover protected resource + authorization server
Authorization request + PKCE
  ↓
User login melalui web session Roblox
  ↓
Consent: client, scopes, device, optional Studio
  ↓ authorization code
Client exchanges code
  ↓
Short-lived access token + rotating refresh token
  ↓
Authenticated request ke /mcp
```

### 10.3 Token requirements

- Access token opaque, short-lived, scope-limited, resource-bound, dan revocable.
- Refresh token opaque, hashed, memiliki rotation lineage, dan dirotasi setiap penggunaan.
- Reuse refresh token lama merevoke token family.
- Authorization code single-use, hashed, dan bound ke client, redirect URI, PKCE challenge, user, scopes, resource, serta expiry.
- Token endpoint tidak mencatat code, verifier, secret, atau token.
- Revoking connector merevoke seluruh token terkait dan menghentikan active MCP session bila memungkinkan.

### 10.4 Scopes

- `mcp:connect`
- `studio:read`
- `studio:edit`
- `studio:execute`
- `studio:playtest`
- `studio:asset`
- `studio:input`

License menentukan maximum scopes. Consent user dapat memberi subset. Setiap `tools/call` diperiksa ulang terhadap scope, ownership, license, device binding, Studio target, dan current policy. Unknown tool bersifat default-deny.

---

## 11. Device Enrollment dan Credential

### 11.1 Device identity

Setiap instalasi Bridge membuat random `device_id`. Hostname bukan device identity.

Server menyimpan device ID, owner internal user ID, display name, hostname, platform/OS version, Bridge version, timestamps, revocation state, dan license binding state.

### 11.2 Enrollment flow

```text
Login Roblox pada web
  ↓
Download Bridge
  ↓
Bridge first run meminta one-time enrollment
  ↓
Backend menghasilkan user code + verification URL
  ↓
User mengonfirmasi device pada dashboard
  ↓
Backend memeriksa paid entitlement atau trial eligibility + available slot
  ↓
Jika device pertama dan eligible: mulai trial 14 hari
  ↓ atomic transaction
Create device credential + license-device binding
  ↓
Credential disimpan dengan Windows DPAPI
```

Requirements:

- Enrollment code short-lived, single-use, random, dan disimpan hashed.
- User melihat nama/hostname device sebelum menyetujui.
- Device credential random; server hanya menyimpan hash.
- Credential terikat ke `device_id` dan `user_id`.
- Credential dapat dirotasi, expired, dan direvoke.
- Revocation memutus active Bridge connection dan mencegah reconnect.
- Plaintext credential file dilarang.
- Device credential bukan Roblox token, web session, atau MCP token.
- Trial dianggap mulai hanya setelah transaction trial entitlement, device binding, dan device credential berhasil commit; kegagalan parsial tidak menghabiskan trial.

---

## 12. RobloxBridge.exe

### 12.1 Responsibilities

```text
RobloxBridge.exe
├── Terminal Status Renderer
├── Config Manager
├── Device Identity
├── Secure Credential Store
├── Enrollment Client
├── WSS Client
├── Official MCP Launcher Discovery
├── MCP Process Manager
├── JSON-RPC Relay
├── Request Correlation
├── Reconnection Manager
├── Heartbeat
├── Local Logging
└── Version Reporter
```

### 12.2 Terminal status UX

Bridge berjalan sebagai proses foreground interaktif pada MVP dan menggunakan terminal status sederhana agar ringan serta mudah didiagnosis. Tidak ada GUI, webview, tray, atau animasi palsu.

Contoh startup normal:

```text
Roblox Studio Bridge v1.0.0
----------------------------------------
[1/5] Loading device configuration ... OK
[2/5] Authenticating licensed device ... OK
[3/5] Connecting to gateway ... OK
[4/5] Starting official Roblox MCP ... OK
[5/5] Detecting Studio sessions ... OK

SYSTEM CONNECTED
Device : DESKTOP-ABC
Gateway: Connected
MCP    : Running
Studio : 1 session connected

Press Ctrl+C to stop.
```

Contoh reconnect:

```text
CONNECTION LOST
Retrying gateway connection in 4s (attempt 3) ...
SYSTEM CONNECTED
```

Contoh failure:

```text
SYSTEM ERROR
Code   : MCP_PROCESS_UNAVAILABLE
Message: Official Roblox MCP could not be started.
Action : Install/repair the official Roblox MCP, then restart Bridge.
```

Rules:

- Setiap status harus berasal dari state nyata, bukan timer kosmetik.
- Status minimum: initializing, enrollment required, authenticating, connecting, MCP starting, Studio detecting, connected, reconnecting, degraded, dan error.
- Output tetap terbaca tanpa ANSI color; color bersifat optional enhancement.
- Terminal tidak menampilkan credential, token, authorization code, raw JSON-RPC, full tool params/result, MySQL detail, atau stack trace.
- Normal mode ringkas; diagnostic detail ditulis ke bounded local log.
- Error menampilkan stable error code, safe message, dan actionable next step.
- `Ctrl+C` memicu graceful shutdown.
- Exit code `0` untuk normal stop; non-zero untuk startup/fatal failure.
- Jika fase production menjalankan Bridge sebagai Windows service tanpa console, state event yang sama ditulis ke structured local log dan dapat dilihat melalui supervisor; mode interaktif tetap memakai terminal.

### 12.3 Official MCP process

Bridge menjalankan official launcher yang tersedia pada mesin user, termasuk launcher Windows seperti:

```text
%LOCALAPPDATA%\Roblox\mcp.bat
```

Requirements:

- Path discovery tervalidasi.
- Gateway tidak dapat mengirim arbitrary executable atau command arguments.
- Bridge memperoleh stdin, stdout, dan stderr child process.
- stdout protocol diproses reliable dan tidak tercampur stderr.
- stderr masuk local log dengan redaction dan bounded retention.
- Bridge tidak mem-patch, mem-fork, atau memodifikasi official Roblox MCP.
- Child process ditutup graceful, lalu hard-stop hanya setelah timeout.

### 12.4 Failure handling

- Process exit dilaporkan sebagai sanitized MCP error status.
- Restart memakai exponential backoff dengan jitter.
- Repeated failure memasuki cooldown/circuit state.
- Child process restart tidak menyebabkan automatic replay `tools/call`.

---

## 13. Bridge WebSocket Protocol

Bridge terhubung ke:

```text
wss://<public-api-host>/bridge
```

### 13.1 Authentication lifecycle

```text
Connect WSS
  ↓
Authenticate device credential
  ↓
Validate active device, owner, license, dan device binding
  ↓
Register connection
  ↓
Bridge hello: version, platform, capabilities
  ↓
Start heartbeat dan status exchange
```

Default duplicate-device policy: authenticated connection baru menggantikan koneksi lama dan event diaudit.

### 13.2 Internal envelope

Versioned internal envelope membawa protocol version, message type, `gateway_request_id`, `device_id`, optional `studio_id`, deadline, dan raw MCP JSON-RPC payload.

Raw payload mempertahankan method, original request ID, params, result, error, dan notification semantics. Gateway request ID hanya untuk internal correlation.

### 13.3 Reliability dan backpressure

- Ping/pong dan application heartbeat memiliki configurable interval/timeout.
- Read/write deadline wajib.
- Message size, queue size, dan in-flight count dibatasi.
- Slow consumer diputus setelah bounded queue penuh.
- Pending request memiliki deadline dan cleanup.
- Late/duplicate response dibuang dan dicatat sebagai sanitized diagnostic.
- Notification tidak menunggu response.
- Reconnect memakai exponential backoff dengan jitter dan maximum delay.
- Setelah reconnect, Bridge re-authenticate dan mengirim full status snapshot.
- Request yang gagal saat disconnect tidak di-replay otomatis.

---

## 14. MCP Streamable HTTP Gateway

Endpoint publik:

```text
https://<public-api-host>/mcp
```

### 14.1 Transport requirements

- Official MCP Go SDK.
- MCP Streamable HTTP.
- Lifecycle `initialize`/`initialized` dan negotiated protocol version.
- Origin validation sesuai transport requirements.
- JSON atau streaming response sesuai client/SDK capability.
- Cancellation ketika protocol/client mendukung.
- Legacy transport tidak ditambahkan tanpa kebutuhan client nyata dan acceptance test.

### 14.2 Routing context

```text
internal user_id
  ↓
OAuth client + connector grant
  ↓
granted scopes + active license
  ↓
target device_id + active license binding
  ↓
target studio_id atau deterministic resolver
```

Studio resolution precedence:

1. Explicit `studio_id` jika tool contract menyediakan dan grant mengizinkan.
2. Default Studio pada connector grant.
3. Satu-satunya Studio online pada target device.
4. Jika ambigu, request ditolak dan user diminta memilih target di dashboard.

Gateway tidak boleh memilih secara diam-diam dari beberapa Studio aktif.

### 14.3 Request path

```text
MCP request
  ↓
Validate token, resource, expiry, dan scope
  ↓
Validate user, active Roblox identity, license, connector, dan device binding
  ↓
Resolve device dan Studio
  ↓
Map original request ke gateway_request_id
  ↓
Send melalui active Bridge WSS
  ↓
Bridge forwards raw JSON-RPC ke official MCP stdin
  ↓
Response dikembalikan ke originating MCP session
```

### 14.4 Authorization policy

- `initialize` mengikuti valid protocol state.
- `tools/list` dapat difilter berdasarkan scope.
- `tools/call` selalu diotorisasi ulang berdasarkan tool dan target.
- Tool-to-scope mapping berversi.
- Unknown/new tool default-deny.
- Authorization denial tidak diteruskan ke Bridge.
- Generic shell, filesystem, atau process execution di luar approved official MCP capability dilarang.

### 14.5 Error behavior

Deterministic sanitized error untuk device offline/revoked/unlicensed, Studio offline/ambiguous, Bridge disconnect, official MCP unavailable, timeout, cancellation, rate limit, invalid license, insufficient scope, revoked connector, dan invalid protocol state.

Request yang sudah terkirim lalu kehilangan koneksi memiliki hasil tidak diketahui. Gateway mengembalikan explicit error tanpa retry otomatis.

---

## 15. Device dan Studio Routing

```text
user_id
  ↓ ownership
active Roblox identity + license
  ↓
connector grant
  ↓ authorization
device_id + license binding
  ↓ active in-memory connection
bridge connection
  ↓ reported Studio membership
studio_id
```

Rules:

- User hanya dapat memilih device miliknya sendiri.
- Device revoked atau tanpa active license binding tidak dapat menjadi target.
- `studio_id` harus terikat ke device yang sama.
- MySQL menyimpan history/dashboard state; in-memory registry menjadi source of truth live connection.
- `last_seen` tidak membuktikan koneksi masih aktif.
- Connector target change diaudit dan memiliki deterministic session behavior.

---

## 16. MySQL Data Model

### 16.1 Identity dan session

- `users`
- `user_identities`
- `web_sessions`

`users` memakai internal primary key. `user_identities` menyimpan provider, provider subject, status, dan timestamps. `(provider, provider_subject)` unique.

### 16.2 Device dan Studio

- `devices`
- `device_enrollment_codes`
- `device_credentials`
- `bridge_connections`
- `studio_sessions`

### 16.3 MCP OAuth

- `oauth_clients`
- `oauth_authorization_codes`
- `oauth_grants`
- `oauth_access_tokens`
- `oauth_refresh_tokens`

### 16.4 License dan administration

- `subscriptions`
- `licenses`
- `trial_entitlements`
- `trial_entitlement_identities`
- `license_device_bindings`
- `license_transfer_requests`
- `account_recovery_cases`
- `admin_actions`

### 16.5 Observability

- `audit_logs`
- `usage_records`

### 16.6 Data invariants

- Semua tenant-owned row dapat ditelusuri ke internal `user_id`.
- User hanya memiliki satu active Roblox identity untuk login/license binding.
- License memiliki satu internal user owner dan satu explicit Roblox identity binding.
- Active license-device binding unique per license, device, dan slot ordinal.
- Binding lama tidak dihapus saat transfer; status serta replacement link dipertahankan.
- Device credential rotation tidak mengubah device ID atau license binding.
- Authorization/enrollment code single-use.
- Trial entitlement unique per internal user dan memiliki satu atau lebih `trial_entitlement_identities`. Setiap `(provider, provider_subject)`—termasuk Roblox User ID lama—unique secara global dan tetap terhubung ke histori entitlement yang sama setelah recovery. Entitlement menyimpan `started_at`, `ends_at`, serta optional audited extension; record tidak dihapus atau dibuat ulang.
- OAuth grant mengikat user, client, scopes, device, dan optional Studio.
- Refresh token menyimpan family/parent lineage.
- Bridge connection history bukan source of truth live routing.

### 16.7 Secret storage

Database menyimpan keyed hash/hash, bukan plaintext, untuk web session token, device credential, enrollment code, OAuth authorization code, MCP access token, dan MCP refresh token.

Roblox OAuth client secret, cryptographic material, token pepper, dan MySQL DSN berasal dari environment atau VPS secret manager, bukan repository.

### 16.8 Migration

- Versioned explicit migration.
- Migration dijalankan sebelum binary baru menerima traffic.
- Startup normal tidak melakukan destructive auto-migration.
- Production migration backward-safe sesuai rollout.
- Backup/restore tersedia sebelum future destructive migration.

---

## 17. Rate Limit dan Usage

Minimum enforcement:

- Active device count dan license slot.
- Request per user/connector/device dengan burst protection.
- Concurrent in-flight limit.
- Operation berdasarkan plan scopes.
- Expired license menolak request baru.
- Rate limit tidak hanya berada pada frontend atau reverse proxy.

MVP single instance dapat memakai live in-memory limiter dengan usage total persisten di MySQL. Distributed limiter wajib sebelum horizontal scaling.

---

## 18. Logging, Audit, dan Diagnostics

### 18.1 Structured log fields

- Timestamp, severity, service, dan version.
- Request/correlation ID.
- Internal user ID bila tersedia.
- Device ID dan Studio ID bila relevan.
- Connector/grant ID bila relevan.
- Event, duration, result class, dan sanitized error code.

### 18.2 Data yang dilarang masuk log/terminal

- Roblox access/refresh/ID token.
- Web session token.
- Device credential dan enrollment code.
- OAuth authorization code dan PKCE verifier.
- MCP access/refresh token.
- Cookie dan Authorization header.
- Raw tool params/result secara default.
- Environment secret dan full MySQL DSN.

### 18.3 Audit events

- Login success/failure dan logout.
- Device enrollment, credential rotation, rename, dan revoke.
- License-device binding creation, transfer, replacement, dan denial.
- Account recovery dan Roblox identity rebind.
- Connector consent, target/scope change, dan revoke.
- Refresh token reuse detection.
- Bridge connect/disconnect/replacement.
- Tool call decision, duration, dan result class.
- License/rate-limit denial.
- Admin action dengan actor, reason, before/after state, dan timestamp.

Audit append-oriented dan tidak dapat diedit melalui dashboard biasa.

---

## 19. Security Requirements

### 19.1 Network

- Public traffic hanya HTTPS/WSS.
- Reverse proxy meneruskan WSS dan MCP streaming secara benar.
- `X-Forwarded-*` hanya dipercaya dari configured trusted proxy.
- MySQL tidak diekspos ke public internet.
- Official Roblox MCP tidak exposed langsung.

### 19.2 Application

- Ownership check untuk seluruh user, identity, license, device, Studio, connector, dan audit access.
- Default-deny tool policy.
- CSRF protection untuk cookie-authenticated mutation.
- Strict redirect URI matching.
- Rate limit pada login, OAuth, enrollment, token, WSS, admin recovery, dan MCP calls.
- Bounded body/message/queue/in-flight count.
- Secure error tanpa stack trace, SQL detail, atau tenant data.
- Dependency version dipin dan direview.
- Unknown official MCP capability tidak otomatis accessible remotely.
- Admin transfer/recovery membutuhkan dedicated authorization dan tidak tersedia pada normal user role.

### 19.3 Bridge command boundary

Gateway hanya boleh mengirim JSON-RPC untuk official Roblox MCP, allowlisted lifecycle action, dan schema-validated configuration yang tidak dapat menjadi arbitrary executable/argument.

---

## 20. Connection Lifecycle

### 20.1 Bridge startup

```text
Start Bridge
  ↓
Load/create device identity
  ↓
Load credential atau enroll
  ↓
Connect dan authenticate WSS
  ↓
Validate owner + active license binding
  ↓
Start official Roblox MCP
  ↓
Initialize local MCP connection
  ↓
Report capabilities + Studio sessions
  ↓
Print SYSTEM CONNECTED
```

### 20.2 Disconnect/reconnect

```text
Connection lost
  ↓
Print CONNECTION LOST
  ↓
Fail affected pending calls tanpa replay
  ↓
Backoff + jitter
  ↓
Reconnect dan re-authenticate
  ↓
Revalidate license binding
  ↓
Send full status snapshot
  ↓
Print SYSTEM CONNECTED
```

### 20.3 Backend graceful shutdown

- Readiness menjadi false.
- New session/connection ditolak.
- Pending request memperoleh bounded drain window.
- Unfinished request mendapat deterministic unavailable error.
- WSS ditutup dengan safe reason.
- HTTP server dan MySQL pool ditutup.
- Process exit agar supervisor dapat restart.

---

## 21. VPS Operational Contract

### 21.1 Runtime

- Satu backend Go process.
- PM2 jika digunakan: fork mode, `instances: 1`.
- Automatic crash restart.
- Graceful shutdown signal diteruskan ke binary.
- Service berjalan sebagai non-root user jika memungkinkan.
- Secret tidak ditulis ke frontend artifact atau process log.

### 21.2 Reverse proxy

Wajib menyediakan valid TLS certificate, proxy backend routes, WSS upgrade pada `/bridge`, MCP streaming tanpa buffering keliru, bounded timeout, SPA fallback, dan sensitive-log redaction.

### 21.3 Configuration

- Public app URL dan MCP resource URL.
- Listen address/port dan trusted proxies.
- MySQL DSN dan pool limits.
- Roblox OAuth client ID/secret/redirect URI.
- Cryptographic keys/pepper.
- Allowed frontend origin.
- OAuth issuer dan token lifetime.
- WSS heartbeat, timeout, queue, dan size limit.
- License defaults dan rate limits.
- Log level dan retention target.

Startup gagal cepat jika critical configuration kosong atau invalid.

### 21.4 Health

- `/healthz`: process hidup.
- `/readyz`: siap menerima traffic dan critical dependency tersedia.
- Health response tidak membocorkan DSN, secret, raw error, user count, atau device IDs.

### 21.5 Frontend release

- Dibangun dengan `vite build`.
- `dist/` dideploy independen dari backend.
- API base URL didefinisikan jelas.
- Hashed assets memakai immutable cache; `index.html` tidak.

---

## 22. MVP Build Order

### Phase 1 — Local Bridge POC

Implement:

- Go Bridge dan terminal status.
- Official MCP launcher discovery.
- Child process lifecycle.
- Reliable stdin/stdout relay.
- Local JSON-RPC correlation.

Acceptance:

- Terminal menunjukkan state initialization yang nyata sampai `SYSTEM CONNECTED`.
- `initialize`, `tools/list`, dan satu safe `tools/call` berhasil pada Roblox Studio nyata.
- stderr tidak merusak stdout protocol stream.

### Phase 2 — Identity, Trial, Device Binding, dan WSS

Implement:

- External MySQL dan explicit migrations.
- Backend Roblox OAuth dan opaque web session.
- Minimal Vite onboarding untuk login dan konfirmasi enrollment; bukan dashboard penuh.
- Authenticated Bridge download endpoint dan artifact metadata.
- Go WebSocket hub.
- Device identity dan enrollment.
- License enforcement dan one-time trial 14 hari.
- Heartbeat, reconnect, bounded queue, dan status terminal.
- Remote correlation envelope.

Acceptance:

- Roblox claim `sub` terikat ke internal user dan license owner yang benar.
- User dapat login dan menyetujui device enrollment dari minimal onboarding flow.
- Download Bridge tidak memulai trial.
- Binding device pertama secara atomic memulai satu-satunya trial 14 hari untuk user/Roblox identity baru.
- Backend → WSS → Bridge → official MCP menjalankan initialize/list/call.
- License owner mismatch ditolak.
- Device revoke memutus koneksi dan tidak membebaskan slot.
- Disconnect tidak menyebabkan replay.

### Phase 3 — Remote MCP + OAuth

Implement:

- MCP Streamable HTTP `/mcp`.
- OAuth 2.1 authorization server.
- Discovery metadata, PKCE, consent, grants, token rotation/revocation.
- Device/Studio routing.
- Scope, ownership, license, dan device-binding enforcement.
- Minimum rate limit dan audit sebelum public exposure.

Acceptance:

- ChatGPT dan Claude menyelesaikan discovery, consent, token exchange, initialize, `tools/list`, dan `tools/call`.
- Cross-user, cross-account, cross-device, cross-Studio, unlicensed, revoked, expired, dan insufficient-scope access ditolak.

### Phase 4 — Full Vite Dashboard

Implement:

- Perluasan React/Vite/TypeScript onboarding menjadi dashboard lengkap.
- Account dan web-session management.
- Device enrollment/status/revoke.
- Studio target selection.
- Connector management.
- License slot/transfer status.
- Diagnostics dan usage.

Acceptance:

- Onboarding dari Roblox login sampai remote connector aktif tidak memerlukan pemindahan secret manual.
- Secret tidak berada di browser storage/log.
- User tidak dapat self-rebind Roblox account atau self-unbind license device.

### Phase 5 — Admin Recovery dan Production Hardening

Implement:

- Admin-only device transfer.
- Strict Roblox account recovery/rebind.
- License enforcement lengkap.
- Tool policy versioning.
- Rate-limit tuning dan audit retention.
- Backup/restore procedure.
- Security and abuse-case tests.

Acceptance:

- Transfer atomic dan binding lama tetap auditable.
- Roblox rebind merevoke seluruh session, connector, token, credential, dan connection.
- Expired/revoked/over-limit state ditolak konsisten.

### Phase 6 — Windows Production UX

Implement:

- Installer dan optional background startup/service.
- Terminal status refinement untuk interactive mode; structured status log untuk service mode.
- Crash recovery.
- Update/rollback.
- Code signing.
- Version compatibility enforcement.

---

## 23. Acceptance Criteria

### 23.1 Bridge

- [ ] Single Windows executable milik produk.
- [ ] Tidak membutuhkan Node.js atau Electron untuk Bridge.
- [ ] Terminal menampilkan initialization, connection, MCP, Studio, reconnect, dan error state yang nyata.
- [ ] Terminal mencapai `SYSTEM CONNECTED` hanya setelah gateway, official MCP, dan readiness minimum berhasil.
- [ ] Tidak ada secret, raw JSON-RPC, atau sensitive payload pada terminal.
- [ ] Menjalankan official Roblox MCP lokal.
- [ ] Reliable stdin/stdout dan separated stderr.
- [ ] Outbound WSS tanpa inbound port.
- [ ] Backoff + jitter untuk reconnect/restart.
- [ ] Tidak replay unknown-result tool calls.
- [ ] Random device ID dan DPAPI-protected credential.
- [ ] Tidak menjalankan arbitrary command dari gateway.

### 23.2 Backend Go

- [ ] Satu Go binary untuk API, auth, MCP, WSS, routing, license, dan health.
- [ ] MySQL eksternal dengan versioned migrations.
- [ ] Roblox login mengikat internal user ke claim `sub`.
- [ ] Opaque hashed revocable web session.
- [ ] Device enrollment, credential rotation/revoke, dan license binding bekerja.
- [ ] `/mcp` memakai Streamable HTTP.
- [ ] OAuth discovery dan PKCE bekerja pada ChatGPT dan Claude nyata.
- [ ] Device/Studio routing menjaga ownership dan license invariants.
- [ ] JSON-RPC semantics tetap utuh.
- [ ] Pending request memiliki deadline dan cleanup.
- [ ] Unknown tool default-deny.
- [ ] Rate limit, license enforcement, dan audit aktif sebelum public exposure.
- [ ] Graceful shutdown bounded.

### 23.3 License

- [ ] License memiliki satu internal user owner dan active Roblox identity.
- [ ] Roblox User ID baru eligible satu free trial sepanjang histori.
- [ ] Trial dimulai saat transaction binding device pertama commit, bukan saat login atau download.
- [ ] Trial berakhir setelah tepat 14 × 24 jam UTC dan tidak dijeda.
- [ ] Reinstall, revoke, transfer, atau Roblox account recovery tidak mereset trial.
- [ ] Trial expiry menolak enrollment baru, WSS readiness, connector authorization, dan MCP execution tanpa paid entitlement.
- [ ] Device wajib memiliki active `license_device_binding`.
- [ ] Revoke device tidak membebaskan slot.
- [ ] Credential rotation pada device sama tidak memakan slot baru.
- [ ] User tidak dapat self-transfer device atau self-rebind Roblox account.
- [ ] Admin transfer mencatat actor, reason, before/after, dan mempertahankan history.
- [ ] Admin Roblox recovery merevoke seluruh session/token/credential/connection.
- [ ] Login Roblox lain tidak dapat memakai license, device, atau connector milik owner.

### 23.4 Frontend

- [ ] Roblox login/logout.
- [ ] Internal ID dan Roblox user ID terlihat.
- [ ] Device list/status/enrollment/revoke.
- [ ] License slot dan transfer status.
- [ ] Studio status dan target selection.
- [ ] Connector scope/target/status/revoke.
- [ ] Bridge/MCP diagnostics dan version.
- [ ] License/subscription/usage.
- [ ] Tidak menyimpan token/credential pada browser storage.
- [ ] Deployable Vite `dist/` dengan SPA fallback.
- [ ] Bridge download membutuhkan authenticated web session tetapi tidak dianggap security boundary.

### 23.5 Networking dan Security

- [ ] User PC tidak membutuhkan inbound port.
- [ ] Bridge bekerja di balik NAT/CGNAT.
- [ ] Public traffic HTTPS/WSS.
- [ ] Official Roblox MCP tidak public.
- [ ] Reverse proxy mendukung WSS dan MCP streaming.
- [ ] Backend tepat satu instance pada MVP.
- [ ] `/healthz` dan `/readyz` aman.
- [ ] Roblox OAuth, web session, device credential, dan MCP token tidak dipakai silang.
- [ ] CSRF, strict redirect URI, resource/audience, scope, ownership, license, dan rate-limit checks aktif.
- [ ] Credential/code/token/cookie tidak masuk log.
- [ ] Cross-tenant tests menolak user/device/Studio lain.

---

## 24. Verification Strategy

### 24.1 Automated tests

- Token hashing, expiry, PKCE, state/nonce, scope policy, license binding, dan routing precedence.
- MySQL integration: unique Roblox identity, single-use code, token lineage, ownership, atomic device transfer, rebind revocation, dan migrations.
- JSON-RPC: response/error/notification, duplicate original ID antar-client, timeout, cancellation, dan late response.
- WSS: auth, heartbeat timeout, reconnect, duplicate device connection, backpressure, revoke, dan license invalidation.
- Terminal state machine: status hanya maju setelah real condition terpenuhi, redaction, reconnect transition, fatal exit code, dan graceful `Ctrl+C`.
- Security: CSRF, open redirect, token audience, insufficient scope, cross-tenant access, log redaction, oversized input, dan admin authorization.

### 24.2 Real end-to-end tests

Wajib terhadap Windows machine, Roblox Studio, official Roblox MCP, production-like VPS TLS/WSS boundary, ChatGPT connector, dan Claude connector nyata.

Golden path:

```text
Roblox login
  → approve dan enroll Bridge
  → transaction atomik mengaktifkan device slot serta trial bila eligible
  → terminal initialization
  → authenticated WSS
  → official MCP initialize
  → terminal SYSTEM CONNECTED
  → authorize connector
  → remote initialize
  → tools/list
  → safe tools/call
  → correct response
```

Failure path minimum:

- Roblox consent ditolak/state invalid.
- Login memakai Roblox account lain.
- Device tanpa binding atau credential revoked saat online.
- Device transfer concurrency.
- Connector token expired/revoked.
- Bridge disconnect di tengah call.
- Official MCP crash di tengah call.
- Multiple Studio tanpa target.
- Cross-user/device/Studio access.
- Backend graceful restart.
- MySQL sementara unavailable.
- Roblox identity admin rebind dan full revocation.

---

## 25. Operational Metrics

- Active Bridge connections dan Studio sessions.
- WSS auth failure/reconnect counts.
- Official MCP restart/error counts.
- Active MCP sessions.
- Request count dan duration berdasarkan normalized method/tool.
- Pending/in-flight gauge.
- Timeout, rate-limit, license denial, authorization denial, dan unknown-result counts.
- MySQL pool saturation/error counts.
- Device slot activation/transfer/recovery counts.

---

## 26. Risks dan Mitigations

### Single-instance availability

**Risk:** Backend restart memutus WSS dan MCP sessions.  
**Mitigation:** Graceful shutdown, supervisor restart, Bridge reconnect, bounded error, dan no automatic replay.

### Unknown remote side effects

**Risk:** Connection loss membuat hasil tool call tidak diketahui.  
**Mitigation:** Jangan replay otomatis; client/user memverifikasi state sebelum mencoba ulang.

### Upstream protocol/tool change

**Risk:** Official Roblox MCP, ChatGPT, Claude, atau MCP spec berubah.  
**Mitigation:** Pin dependency, negotiate protocol, default-deny unknown tool, compatibility matrix, dan real-host tests.

### License abuse

**Risk:** Credential dipindahkan atau user mengganti Roblox account/device berulang.  
**Mitigation:** Internal user + Roblox identity + device binding invariant, DPAPI, admin-only transfer/rebind, atomic replacement, dan full audit.

### Recovery abuse

**Risk:** Support/admin memindahkan license tanpa verifikasi.  
**Mitigation:** Dedicated admin role, reason/evidence requirement, immutable audit, full credential revocation, dan no silent owner transfer.

### Credential compromise

**Risk:** Device atau connector token dicuri.  
**Mitigation:** Hash at rest, short lifetime, refresh rotation, DPAPI, revocation, rate limit, dan redaction.

### Legal/platform policy

**Risk:** Commercial remote control Roblox Studio tunduk pada Terms/license restrictions.  
**Mitigation:** Tidak mendistribusikan/memodifikasi official MCP; legal dan platform-policy review sebelum launch.

---

## 27. Future Scaling

Sebelum mengaktifkan multi-instance backend:

- Distributed `device_id → gateway instance` mapping.
- Inter-instance request/response routing.
- Connection ownership fencing.
- Distributed rate limiting.
- Session/token/license revocation propagation.
- WSS dan MCP streaming load-balancer behavior.
- Chaos/failover tests untuk mencegah cross-user routing dan duplicate side effect.

Redis atau messaging system dipilih setelah kebutuhan throughput dan failure model terukur.

---

## 28. Legal dan Technical Boundary

- Produk tidak mendistribusikan atau memodifikasi official Roblox MCP tanpa izin.
- Bridge hanya menjalankan dan berkomunikasi dengan official MCP pada mesin user.
- Produk tidak memberikan arbitrary OS shell access.
- Roblox OAuth hanya digunakan sesuai scope dan provider policy.
- Sebelum commercial launch, review Roblox Terms of Use, OAuth/Open Cloud terms, official MCP license, privacy, dan remote-control requirements.

---

## 29. Definition of Done

MVP selesai hanya jika jalur berikut terbukti pada environment nyata:

```text
ChatGPT dan Claude
  ↓ discovery + OAuth 2.1 + PKCE
Go MCP Streamable HTTP Gateway pada VPS
  ↓ user + Roblox identity + license + device policy
Outbound WSS
  ↓
RobloxBridge.exe pada Windows
  ↓ terminal SYSTEM CONNECTED
Official Roblox MCP melalui stdio
  ↓
Roblox Studio
  ↓
initialize → tools/list → safe tools/call → response
```

Bukti mencakup happy path, wrong Roblox account, unlicensed device, revoke, admin transfer, admin account recovery, disconnect, crash, timeout, ambiguous Studio, scope denial, cross-tenant denial, dan backend restart. Dashboard selesai hanya jika user dapat login, enroll device, authorize connector, memilih target, melihat license/diagnostics, dan melakukan revoke tanpa memindahkan secret manual.
