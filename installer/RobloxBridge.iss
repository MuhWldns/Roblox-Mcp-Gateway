; RobloxBridge — Inno Setup definition
;
; Scope guard: this installer ships the RobloxKit Bridge binary and product
; files ONLY. It deliberately does NOT install, bundle, copy, or modify the
; official Roblox MCP, the Roblox Studio MCP launcher, Node.js, or Electron —
; BRIDGE_MCP_LAUNCHER must point at an already-present official Roblox MCP
; launcher that the operator installs separately.
;
; Service environment contract: the service control manager populates a
; service process environment from ONE REG_MULTI_SZ value named
; "Environment" on HKLM\SYSTEM\CurrentControlSet\Services\RobloxBridge —
; separate REG_SZ values under an Environment SUBKEY do NOT reach the
; process. The installer writes the complete multi-string block below via
; RegWriteMultiStringValue. Post-enrollment, BRIDGE_DEVICE_ID is appended
; to that same block WITHOUT discarding the other entries (PowerShell
; procedure in docs/operations/windows-bridge.md).
;
; DPAPI caveat (also documented in docs/operations/windows-bridge.md): the
; device credential is encrypted with Windows DPAPI under the account that
; performed the enrollment. The service account and the enrolling account
; must therefore be the same identity; a credential blob is meaningless to
; any other user (and to LocalSystem if enrollment ran as a normal user).
; Enroll while running as the service account, or configure the service to
; run as the enrolling account (sc config RobloxBridge obj= DOMAIN\user
; password= ...), then grant that account modify access to the data
; directory (see the runbook's mandatory pre-enrollment ACL step). Verify
; with: sc qc RobloxBridge
;
; Uninstall contract: stop the service, wait BOUNDED until the SCM reports
; Stopped, abort the uninstall visibly on timeout, and only then delete the
; registration and files. Uninstalling NEVER frees the server-side license
; slot — the binding stays occupied until an admin transfers the device or
; revokes the binding on the server.
;
; NOTE: compile-verification requires the Inno Setup command line compiler
; (ISCC.exe). If it is unavailable on the build machine, validate on a
; machine that has Inno Setup 6+. The contract test at
; installer/contract_test.go pins the generated contract in source form.

#define MyAppName "RobloxBridge"
#define MyAppVersion "0.1.0"
#define MyAppServiceName "RobloxBridge"
#define MyAppPublisher "RobloxKit"
#define MyAppExeName "RobloxBridge.exe"
#define MyAppDataDir "{commonappdata}\RobloxBridge"

[Setup]
AppId={{7C1B0F52-9E63-4B8A-9B2D-3F5A1C6E8D24}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
DefaultDirName={autopf}\{#MyAppName}
DefaultGroupName={#MyAppName}
DisableProgramGroupPage=yes
; Service registration and the HKLM service environment need elevation.
PrivilegesRequired=admin
OutputBaseFilename=RobloxBridge-setup-{#MyAppVersion}
OutputDir=..\bin\installer
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
ArchitecturesInstallIn64BitMode=x64compatible
UninstallDisplayIcon={app}\{#MyAppExeName}
; The per-machine data directory is NOT removed by uninstall: the DPAPI
; credential blob and the service log stay until an operator clears them.

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Files]
; The Bridge binary and product files ONLY — never the official Roblox MCP,
; Node, or Electron runtimes.
Source: "..\bin\RobloxBridge-windows-amd64.exe"; DestDir: "{app}"; DestName: "{#MyAppExeName}"; Flags: ignoreversion
Source: "..\docs\operations\windows-bridge.md"; DestDir: "{app}\docs"; Flags: ignoreversion

[Dirs]
; Per-machine data directory: DPAPI credential blob, structured service log.
Name: "{#MyAppDataDir}"; Permissions: users-modify

[Icons]
Name: "{group}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"

[Code]
const
  ServiceName = 'RobloxBridge';
  ServiceStopTimeoutSeconds = 30;

// The complete service environment block. The SCM delivers exactly this
// REG_MULTI_SZ to the service process; post-enrollment the operator appends
// BRIDGE_DEVICE_ID=<device-id> to the same block without discarding entries.
// Inno 6 RegWriteMultiStringValue takes ONE String whose entries are joined
// by #0 characters (the documented example form; Inno adds the REG_MULTI_SZ
// termination itself, so no trailing #0 is appended here).
function ServiceEnvironment: string;
begin
  Result :=
    'BRIDGE_MODE=service' + #0 +
    'BRIDGE_GATEWAY_URL=wss://gateway.example.invalid/bridge' + #0 +
    'BRIDGE_CREDENTIAL_PATH=' + ExpandConstant('{#MyAppDataDir}') + '\device.credential' + #0 +
    'BRIDGE_MCP_LAUNCHER=REPLACE-WITH-OFFICIAL-ROBLOX-MCP-LAUNCHER-PATH' + #0 +
    'BRIDGE_CONNECT_TIMEOUT=10s' + #0 +
    'BRIDGE_HEARTBEAT_INTERVAL=30s' + #0 +
    'BRIDGE_RESPONSE_TIMEOUT=10s' + #0 +
    'BRIDGE_QUEUE_LIMIT=64' + #0 +
    'BRIDGE_MAX_MESSAGE_BYTES=1048576' + #0 +
    'BRIDGE_SERVICE_LOG=' + ExpandConstant('{#MyAppDataDir}') + '\service.log';
end;

procedure Fail(What: string);
begin
  RaiseException('Roblox Bridge install failed: ' + What);
end;

// Install step: files are in place, so register the service, then write the
// one Environment REG_MULTI_SZ the SCM actually delivers.
procedure CurStepChanged(CurStep: TSetupStep);
var
  ResultCode: Integer;
begin
  if CurStep <> ssPostInstall then
    exit;

  // Register the service on demand start: enrollment must complete before
  // the service can connect (the runbook flips it to auto start afterwards).
  if not Exec(ExpandConstant('{sys}\sc.exe'),
      'create ' + ServiceName + ' binPath= "' + ExpandConstant('{app}\{#MyAppExeName}') + '"' +
      ' start= demand DisplayName= "Roblox Bridge"',
      '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    Fail('could not register the RobloxBridge service (sc create returned ' + IntToStr(ResultCode) + ')');

  if not Exec(ExpandConstant('{sys}\sc.exe'),
      'description ' + ServiceName + ' "RobloxKit Bridge: connects an enrolled Roblox Studio device to the licensed gateway."',
      '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    Fail('could not describe the RobloxBridge service');

  // Crash resilience: restart the service 5 seconds after an unexpected exit.
  if not Exec(ExpandConstant('{sys}\sc.exe'),
      'failure ' + ServiceName + ' reset= 86400 actions= restart/5000',
      '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    Fail('could not configure the service recovery policy');

  // ONE REG_MULTI_SZ value named Environment on the service key — the only
  // shape the SCM appends to the service process environment.
  if not RegWriteMultiStringValue(HKEY_LOCAL_MACHINE,
      'SYSTEM\CurrentControlSet\Services\' + ServiceName,
      'Environment', ServiceEnvironment()) then
    Fail('could not write the service Environment block (REG_MULTI_SZ)');
end;

// ServiceStopAndWait stops the service and waits BOUNDED until the SCM
// reports Stopped; raises on timeout so the uninstall fails visibly instead
// of racing a live service. Returns True when the service existed (Win32
// 1060 = ERROR_SERVICE_DOES_NOT_EXIST reports False so the caller can skip
// the delete and the uninstall continues successfully).
function ServiceStopAndWait: Boolean;
var
  ResultCode: Integer;
begin
  Result :=
    Exec(ExpandConstant('{sys}\sc.exe'), 'query ' + ServiceName,
      '', SW_HIDE, ewWaitUntilTerminated, ResultCode) and (ResultCode <> 1060);
  if not Result then
    exit;

  if not Exec(ExpandConstant('{sys}\sc.exe'), 'stop ' + ServiceName,
      '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
    Fail('could not request the RobloxBridge service to stop');

  // Bounded wait: the ServiceController polls the real SCM state; a nonzero
  // exit code means the service never reached Stopped within the timeout.
  if not Exec(ExpandConstant('{sys}\WindowsPowerShell\v1.0\powershell.exe'),
      '-NoProfile -ExecutionPolicy Bypass -Command ' +
      '"Add-Type -AssemblyName System.ServiceProcess; ' +
      '$svc = New-Object System.ServiceProcess.ServiceController(''' + ServiceName + '''); ' +
      'try { $svc.WaitForStatus(''Stopped'', [TimeSpan]::FromSeconds(' + IntToStr(ServiceStopTimeoutSeconds) + ')); exit 0 } ' +
      'catch { exit 1 }"',
      '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or (ResultCode <> 0) then
    RaiseException('Roblox Bridge service did not stop within ' +
      IntToStr(ServiceStopTimeoutSeconds) + ' seconds; uninstall aborted. ' +
      'Stop the service manually (sc stop ' + ServiceName + ') and run the uninstall again.');
end;

// Uninstall step: stop, wait BOUNDED until Stopped, fail visibly on timeout,
// and only then delete the registration. Files removal follows after this
// step completes. The Environment block and the data directory under
// {#MyAppDataDir} are deliberately left in place.
//
// IMPORTANT: uninstalling NEVER frees the server-side license slot. The
// device binding (and its license slot) stays occupied until an admin
// transfers the device or revokes the binding on the server — see
// docs/operations/windows-bridge.md ("Uninstall").
procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  ResultCode: Integer;
  ServiceExists: Boolean;
begin
  if CurUninstallStep <> usUninstall then
    exit;

  ServiceExists := ServiceStopAndWait;

  // Delete the registration only after the SCM confirmed Stopped — and
  // only when the service actually exists: an absent service (query
  // returned 1060) must let the uninstall continue successfully, and a
  // delete that races the service away (also 1060) is success too.
  if ServiceExists then
    if not Exec(ExpandConstant('{sys}\sc.exe'), 'delete ' + ServiceName,
        '', SW_HIDE, ewWaitUntilTerminated, ResultCode) or
        ((ResultCode <> 0) and (ResultCode <> 1060)) then
      RaiseException('Could not delete the RobloxBridge service registration (sc delete returned ' +
        IntToStr(ResultCode) + '). Stop the service manually and run the uninstall again.');
end;
