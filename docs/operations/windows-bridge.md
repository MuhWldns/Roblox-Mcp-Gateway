# Windows Bridge — service install and lifecycle

This runbook covers the Windows service lifecycle of the RobloxKit Bridge:
install, enrollment, service start, reboot behavior, stop, and uninstall. The
service runs the same remote bridge mode as the interactive `BRIDGE_MODE=remote`
run and reports every lifecycle state both to a structured local log and to
the Windows service control manager (SCM).

## What the installer ships

`installer/RobloxBridge.iss` installs the Bridge binary and product files
only. It deliberately does **not** install, bundle, or modify the official
Roblox MCP, the Roblox Studio MCP launcher, Node.js, or Electron: the Bridge
launches the **already-installed** official Roblox MCP launcher through
`BRIDGE_MCP_LAUNCHER`.

Installed layout:

| Path | Purpose |
| --- | --- |
| `C:\Program Files\RobloxBridge\RobloxBridge.exe` | the Bridge binary |
| `C:\Program Files\RobloxBridge\docs\windows-bridge.md` | this runbook |
| `C:\ProgramData\RobloxBridge\` | credential blob + structured service log |

The installer registers the Windows service `RobloxBridge` with start type
**demand** (manual) and crash-restart enabled. The service process receives
its environment from ONE `REG_MULTI_SZ` value named `Environment` on
`HKLM\SYSTEM\CurrentControlSet\Services\RobloxBridge` — the only registry
shape the service control manager actually delivers to a service process
(separate `REG_SZ` values do not reach it). The installer writes the
complete block:

- `BRIDGE_MODE=service`
- `BRIDGE_GATEWAY_URL` — `wss://gateway.example.invalid/bridge` (replace the
  placeholder).
- `BRIDGE_CREDENTIAL_PATH` — `C:\ProgramData\RobloxBridge\device.credential`.
- `BRIDGE_MCP_LAUNCHER` — replace with the absolute path of the official
  Roblox MCP launcher already on the machine.
- `BRIDGE_CONNECT_TIMEOUT` — `10s` (the dial budget).
- `BRIDGE_HEARTBEAT_INTERVAL` — `30s`.
- `BRIDGE_RESPONSE_TIMEOUT` — `10s` (one hub exchange).
- `BRIDGE_QUEUE_LIMIT` — `64`.
- `BRIDGE_MAX_MESSAGE_BYTES` — `1048576`.
- `BRIDGE_SERVICE_LOG` — `C:\ProgramData\RobloxBridge\service.log`.

## DPAPI identity caveat (read before enrolling)

The device credential file is encrypted with Windows DPAPI under the account
that performs enrollment. A blob is meaningless to every other account, on
any machine. Therefore:

> The service account and the enrolling account must be the same identity.

Two supported layouts:

1. **Dedicated service account (recommended).** Create or pick the account,
   log on interactively as that account, complete enrollment, then run the
   service under that identity:
   `sc config RobloxBridge obj= DOMAIN\service-account password= ...`
2. **LocalSystem.** Run enrollment under LocalSystem (for example via
   `psexec -s`) so the blob is encrypted for the service's own identity.

## Install

1. Run `RobloxBridge-setup-<version>.exe` (admin). Files land in
   `C:\Program Files\RobloxBridge`, the service `RobloxBridge` is registered
   (start type: demand) with crash-restart enabled, and the complete
   `Environment` multi-string block is written.
2. Edit the `Environment` block, at minimum `BRIDGE_GATEWAY_URL` and
   `BRIDGE_MCP_LAUNCHER`. Read the block, change the entries, write the whole
   block back (never discard the other entries):
   ```powershell
   $key = 'HKLM:\SYSTEM\CurrentControlSet\Services\RobloxBridge'
   $envs = @((Get-ItemProperty -Path $key -Name Environment).Environment)
   $envs = @($envs | ForEach-Object {
       if ($_ -like 'BRIDGE_GATEWAY_URL=*') { 'BRIDGE_GATEWAY_URL=wss://gateway.example.com/bridge' }
       elseif ($_ -like 'BRIDGE_MCP_LAUNCHER=*') { 'BRIDGE_MCP_LAUNCHER=C:\Path\To\Official Roblox MCP launcher' }
       else { $_ }
   })
   Set-ItemProperty -Path $key -Name Environment -Value $envs
   ```

## Enroll

Enrollment binds the device and — on the first enrollment for the Roblox
identity — starts the one 14-day trial on the server. Run it under the
**chosen service identity** (see the DPAPI caveat), because the saved
credential is DPAPI-encrypted for the running account.

1. **Choose the service identity** (dedicated account recommended):
   ```bat
   sc config RobloxBridge obj= DOMAIN\service-account password= <password>
   ```
2. **Grant that account the data-directory ACL** (mandatory, least
   privilege):
   ```bat
   icacls C:\ProgramData\RobloxBridge /grant "DOMAIN\service-account:(OI)(CI)M"
   ```
   `(OI)(CI)M` grants Modify — read/write/append on the directory and
   everything inherited into it — and nothing more: the service account can
   store its DPAPI credential and append the service log, but cannot change
   ACLs, take ownership, or touch anything outside
   `C:\ProgramData\RobloxBridge`.
3. **Run the built-in enrollment mode as that account** (an interactive
   logon session of the service account):
   ```powershell
   $env:BRIDGE_MODE            = 'enroll'
   $env:BRIDGE_GATEWAY_URL     = 'wss://gateway.example.com/bridge'
   $env:BRIDGE_CREDENTIAL_PATH = 'C:\ProgramData\RobloxBridge\device.credential'
   & 'C:\Program Files\RobloxBridge\RobloxBridge.exe'
   ```
   The Bridge claims a fresh device id, prints the **exact verification URL
   and user code**, polls the exchange, saves the `rkd_…` credential under
   the running identity, and prints the enrolled device id. The plaintext
   credential is never printed.
   Enrollment requires ONLY those two variables — no `BRIDGE_MCP_LAUNCHER`
   and no runtime bounds (the clean-install enrollment mode ignores them).
4. Approve the printed user code in the browser session (the URL carries the
   code) with the Roblox identity that owns this device.
5. **Append `BRIDGE_DEVICE_ID` to the `Environment` block without discarding
   the other entries**:
   ```powershell
   $key = 'HKLM:\SYSTEM\CurrentControlSet\Services\RobloxBridge'
   $envs = @((Get-ItemProperty -Path $key -Name Environment).Environment)
   $envs = @($envs | Where-Object { $_ -notlike 'BRIDGE_DEVICE_ID=*' }) +
          @("BRIDGE_DEVICE_ID=<device-id-printed-by-enrollment>")
   Set-ItemProperty -Path $key -Name Environment -Value $envs
   ```

## Start and verify service mode

```bat
sc config RobloxBridge start= auto
sc start RobloxBridge
sc query RobloxBridge
```

`start= auto` after enrollment makes the service start at boot, so the Bridge
reconnects after every reboot without a logon. Verify:

- `sc query` shows `RUNNING`.
- `C:\ProgramData\RobloxBridge\service.log` carries one JSON line per state
  event (`event: bridge_state`, `state: authenticating … connected`), plus
  `service_start`, `stop_requested`, and `service_exit` lifecycle records.
- The terminal dashboard shows the device **Online**.

Service status semantics visible in `sc query` (the SCM only ever sees
numeric states and exit codes; every readable reason —
`CREDENTIAL_STORE_UNAVAILABLE` included — lives in the structured service
log):

| Bridge state | SCM state |
| --- | --- |
| starting (dial, MCP child, Studio probe) | `START_PENDING` with advancing checkpoint |
| connected | `RUNNING` (stop/shutdown accepted) |
| reconnecting / degraded | `RUNNING`, checkpoint advances |
| stop requested | `STOP_PENDING` (graceful MCP child stop, then WSS close) |
| fatal startup error | start fails with a numeric non-zero service-specific exit code; the code (e.g. `CREDENTIAL_STORE_UNAVAILABLE`) appears only in the service log |

The service stop path is strictly ordered: the MCP child is stopped with a
bounded graceful stop first, and only then is the gateway WebSocket closed —
an in-flight tool call is never replayed after a service stop.

## Reboot

With `start= auto` set (post-enrollment), rebooting reconnects the Bridge
automatically: the service starts, loads the DPAPI credential, dials the
gateway, and returns to `RUNNING`. History is preserved server-side; no
second trial can be created by restarts or reconnects.

## Stop

```bat
sc stop RobloxBridge
```

The SCM sends the stop control; the Bridge cancels its run context, stops the
MCP child gracefully, closes the WebSocket, reports `STOP_PENDING` → stopped,
and exits 0.

## Uninstall

Uninstalling stops and deletes the service registration. It does **not**
delete the credential file, the service log, or any enrollment artifacts
under `C:\ProgramData\RobloxBridge` (remove them manually if desired).

> **Uninstalling NEVER frees the server-side license slot.** The device
> binding — and the license slot it occupies — remains bound on the server
> until an administrator transfers the device to another machine or revokes
> the binding through the admin surface. Uninstalling the Windows service
> changes nothing server-side; reinstall/re-enroll on the same identity never
> creates a second trial.

## Troubleshooting

- **Service fails to start, exit code 1063** in the log — the process was
  started interactively with `BRIDGE_MODE=service` instead of through the
  SCM. Use `sc start RobloxBridge`.
- **`CREDENTIAL_STORE_UNAVAILABLE` in the service log** — DPAPI identity
  mismatch; see the caveat above. This code surfaces in
  `C:\ProgramData\RobloxBridge\service.log` — the service control manager
  only sees the numeric non-zero startup failure.
- **`MCP_PROCESS_UNAVAILABLE`** — `BRIDGE_MCP_LAUNCHER` still set to the
  installer placeholder or the official Roblox MCP launcher is missing. The
  installer does not ship it.
- **No `bridge_state` lines in the log** — the service account cannot write
  `BRIDGE_SERVICE_LOG`; re-run the mandatory pre-enrollment ACL step:
  `icacls C:\ProgramData\RobloxBridge /grant "DOMAIN\service-account:(OI)(CI)M"`.
