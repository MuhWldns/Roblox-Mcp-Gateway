# Local smoke test for the RobloxKit release artifacts.
#
# Verifies, end to end:
#   1. the release artifacts exist and match bin/SHA-256SUMS;
#   2. every release binary was built from the current commit (VCS stamp);
#   3. the migrate CLI brings a scratch MySQL database up (status/version);
#   4. the gateway server starts against it, /healthz and /readyz answer 200;
#   5. an interrupt (SIGINT/CTRL_C) exits gracefully with code 0 well inside
#      the supervisor kill timeout.
#
# The Linux release binaries cannot execute on a Windows host, so steps 3-5
# build host-platform binaries from the same committed source tree into a
# scratch directory. Requires: go, git, and a reachable MySQL instance
# (MYSQL_TEST_DSN, default root@tcp(127.0.0.1:3306)/).
#
# The script restores the environment and working directory on exit and
# drops its scratch database and temporary files.

param(
	[string]$Dsn = $env:MYSQL_TEST_DSN,
	[string]$ReleaseDirectory,
	[switch]$VerifyReleaseOnly,
	[switch]$RollbackProof
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = Split-Path -Parent $PSScriptRoot
if (-not $ReleaseDirectory) {
	$ReleaseDirectory = Join-Path $root 'bin'
}
$releaseDir = [System.IO.Path]::GetFullPath($ReleaseDirectory)
if (-not $Dsn) {
	$Dsn = 'root@tcp(127.0.0.1:3306)/'
}

# Environment values touched by the smoke run are restored on exit.
$smokeEnvKeys = @(
	'MYSQL_DSN', 'PUBLIC_APP_URL', 'MCP_RESOURCE_URL', 'LISTEN_ADDRESS',
	'ALLOWED_ORIGIN', 'TRUSTED_PROXIES', 'TOKEN_PEPPER', 'HTTP_READ_TIMEOUT',
	'HTTP_WRITE_TIMEOUT', 'MYSQL_MAX_OPEN_CONNS', 'MYSQL_MAX_IDLE_CONNS',
	'BRIDGE_HEARTBEAT_INTERVAL', 'BRIDGE_TIMEOUT', 'BRIDGE_QUEUE_LIMIT',
	'BRIDGE_MAX_MESSAGE_BYTES', 'ROBLOX_CLIENT_ID', 'ROBLOX_PROVIDER_BASE_URL',
	'BRIDGE_ARTIFACT_PATH', 'BRIDGE_ARTIFACT_FILENAME', 'WEB_STATIC_DIR',
	'ADMIN_USER_IDS'
)
$savedEnv = @{}
foreach ($name in $smokeEnvKeys) {
	$savedEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}

$scratch = Join-Path ([System.IO.Path]::GetTempPath()) ("robloxkit-smoke-" + [DateTime]::UtcNow.Ticks)
$helperDir = Join-Path $root 'testdata\smoke-createdb'
# Only digits and underscores: safe as a MySQL identifier built by the helper.
$scratchDb = "robloxkit_smoke_" + (Get-Date).ToUniversalTime().ToString('yyyyMMddHHmmss')
$serverProcess = $null

function Restore-SmokeEnvironment {
	foreach ($name in $smokeEnvKeys) {
		if ($null -eq $savedEnv[$name]) {
			Remove-Item -Path "Env:$name" -ErrorAction SilentlyContinue
		} else {
			Set-Item -Path "Env:$name" -Value $savedEnv[$name]
		}
	}
}

function Assert-True {
	param([bool]$Condition, [string]$Message)
	if (-not $Condition) {
		throw $Message
	}
}

function Assert-ReleaseManifest {
	param([string]$Directory)

	$releaseRoot = [System.IO.Path]::GetFullPath($Directory)
	$sumsPath = Join-Path $releaseRoot 'SHA-256SUMS'
	Assert-True (Test-Path -LiteralPath $sumsPath -PathType Leaf) "missing $sumsPath"
	$lines = [System.IO.File]::ReadAllLines($sumsPath)
	Assert-True ($lines.Count -gt 0) 'SHA-256SUMS must contain at least one entry'
	$seen = New-Object 'System.Collections.Generic.HashSet[string]' ([System.StringComparer]::Ordinal)
	$verified = New-Object 'System.Collections.Generic.List[string]'
	$rootPrefix = $releaseRoot.TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar

	foreach ($line in $lines) {
		$match = [System.Text.RegularExpressions.Regex]::Match($line, '^(?<hash>[0-9a-f]{64}) \*(?<path>[^\r\n]+)$', [System.Text.RegularExpressions.RegexOptions]::CultureInvariant)
		Assert-True $match.Success "malformed checksum line: $line"
		$relative = $match.Groups['path'].Value
		Assert-True (-not $relative.Contains('\')) "checksum path must use forward slashes: $relative"
		Assert-True (-not $relative.StartsWith('/')) "absolute checksum path is forbidden: $relative"
		Assert-True ($relative -notmatch '^[A-Za-z]:') "absolute checksum path is forbidden: $relative"
		$segments = $relative.Split('/')
		Assert-True (-not ($segments | Where-Object { $_ -eq '' -or $_ -eq '.' -or $_ -eq '..' })) "unsafe checksum path: $relative"
		Assert-True ($seen.Add($relative)) "duplicate checksum path: $relative"

		$platformRelative = $relative.Replace('/', [System.IO.Path]::DirectorySeparatorChar)
		$artifactPath = [System.IO.Path]::GetFullPath((Join-Path $releaseRoot $platformRelative))
		Assert-True ($artifactPath.StartsWith($rootPrefix, [System.StringComparison]::OrdinalIgnoreCase)) "checksum path escapes the release directory: $relative"
		Assert-True (Test-Path -LiteralPath $artifactPath -PathType Leaf) "checksummed artifact is missing: $relative"
		$actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $artifactPath).Hash.ToLowerInvariant()
		$expected = $match.Groups['hash'].Value
		Assert-True ($actual -ceq $expected) "checksum mismatch for ${relative}: expected $expected, got $actual"
		$verified.Add($relative)
	}

	return $verified.ToArray()
}

function Assert-ManifestContains {
	param([string[]]$VerifiedPaths, [string[]]$RequiredPaths)
	$verified = New-Object 'System.Collections.Generic.HashSet[string]' ([System.StringComparer]::Ordinal)
	foreach ($path in $VerifiedPaths) { [void]$verified.Add($path) }
	foreach ($required in $RequiredPaths) {
		Assert-True ($verified.Contains($required)) "SHA-256SUMS does not cover required artifact: $required"
	}
}

function Assert-ReleaseBuildIdentity {
	param([string]$Directory, [string[]]$BinaryPaths)

	$identityPath = Join-Path $Directory 'BUILD-ID'
	Assert-True (Test-Path -LiteralPath $identityPath -PathType Leaf) "release is missing BUILD-ID: $Directory"
	$rawIdentity = [System.IO.File]::ReadAllText($identityPath)
	Assert-True ($rawIdentity -match '\A(?<commit>[0-9a-f]{40})/(?<version>[^\r\n]+)\n\z') "malformed BUILD-ID in $Directory"
	$identity = $rawIdentity.Substring(0, $rawIdentity.Length - 1)
	foreach ($relative in $BinaryPaths) {
		$binary = Join-Path $Directory $relative
		Assert-True (Test-Path -LiteralPath $binary -PathType Leaf) "release is missing identity-bearing binary: $relative"
		$embedded = (& go tool buildid $binary).Trim()
		Assert-True ($LASTEXITCODE -eq 0) "go tool buildid failed for $binary"
		Assert-True ($embedded -ceq $identity) "$relative build ID is '$embedded', want release identity '$identity'"
	}
	return $identity
}

function Copy-DirectoryContents {
	param([string]$Source, [string]$Destination)
	New-Item -ItemType Directory -Force -Path $Destination | Out-Null
	Get-ChildItem -LiteralPath $Source | Copy-Item -Destination $Destination -Recurse -Force
}

function New-LocalReleaseManifest {
	param([string]$Directory)
	$rootPath = [System.IO.Path]::GetFullPath($Directory)
	$relativePaths = Get-ChildItem -LiteralPath $rootPath -Recurse -File |
		Where-Object { $_.Name -ne 'SHA-256SUMS' } |
		ForEach-Object { $_.FullName.Substring($rootPath.Length + 1).Replace('\', '/') } |
		Sort-Object
	$lines = foreach ($relative in $relativePaths) {
		$hash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $rootPath $relative)).Hash.ToLowerInvariant()
		"$hash *$relative"
	}
	[System.IO.File]::WriteAllText(
		(Join-Path $rootPath 'SHA-256SUMS'),
		(($lines -join "`n") + "`n"),
		(New-Object System.Text.UTF8Encoding($false))
	)
}

function Set-AtomicReleaseSelector {
	param([string]$DeploymentRoot, [string]$ReleaseName)
	Assert-True ($ReleaseName -match '\A[A-Za-z0-9._-]+\z') "unsafe release selector: $ReleaseName"
	$selector = Join-Path $DeploymentRoot 'ACTIVE-RELEASE'
	# 2. Every release binary must match the manifest-covered build identity,
	# and that identity must belong to the commit being smoked.
	$releaseIdentity = Assert-ReleaseBuildIdentity -Directory $releaseDir -BinaryPaths @(
		'robloxkit-server-linux-amd64',
		'robloxkit-migrate-linux-amd64',
		'RobloxBridge-windows-amd64.exe'
	)
	$commit = (& git rev-parse HEAD).Trim()
	Assert-True ($LASTEXITCODE -eq 0) 'git rev-parse HEAD failed'
	Assert-True ($releaseIdentity.StartsWith("$commit/", [System.StringComparison]::Ordinal)) "release identity '$releaseIdentity' does not belong to current commit $commit"
	Write-Output "smoke: release binaries match BUILD-ID $releaseIdentity"
	return Join-Path (Join-Path $DeploymentRoot 'releases') $name
}

function Wait-HttpStatus {
	param([string]$Uri, [int]$TimeoutSeconds)
	$deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
	while ([DateTime]::UtcNow -lt $deadline) {
		try {
			$response = Invoke-WebRequest -Uri $Uri -UseBasicParsing -TimeoutSec 2
			if ($response.StatusCode -eq 200) {
				return $response
			}
		} catch {
			# Not up yet (or 503 while the pool warms); retry.
		}
		Start-Sleep -Milliseconds 300
	}
	throw "timed out waiting for 200 from $Uri"
}

function Remove-ScratchDatabase {
	# Uses the same driver-grade helper; runs from the module root.
	Push-Location $root
	try {
		& go run ./testdata/smoke-createdb drop $Dsn $scratchDb 2>$null | Out-Null
	} catch {
		Write-Warning "could not drop scratch database ${scratchDb}: $_"
	} finally {
		Pop-Location
	}
}

try {
	Push-Location $root

	# 1. Release artifacts exist, every required file is covered by the
	# manifest, and each GNU binary-format entry is safe before it is opened.
	$verifiedPaths = @(Assert-ReleaseManifest -Directory $releaseDir)
	Write-Output 'smoke: release artifacts match SHA-256SUMS'
	if ($VerifyReleaseOnly) {
		return
	}
	Assert-ManifestContains -VerifiedPaths $verifiedPaths -RequiredPaths @(
		'robloxkit-server-linux-amd64',
		'robloxkit-migrate-linux-amd64',
		'RobloxBridge-windows-amd64.exe',
		'dist/index.html'
	)

	# 2. Every release binary carries the current commit through its build
	# ID (the release stamps "commit/version" explicitly; automatic -buildvcs
	# stamping reads the wrong repository state inside a git worktree).
	$commit = (& git rev-parse HEAD).Trim()
	foreach ($name in @('robloxkit-server-linux-amd64', 'robloxkit-migrate-linux-amd64', 'RobloxBridge-windows-amd64.exe')) {
		$buildId = (& go tool buildid (Join-Path $releaseDir $name)).Trim()
		Assert-True ($buildId -like "$commit/*") "$name build ID is '$buildId', want it to start with the current commit $commit"
	}
	Write-Output "smoke: release binaries stamped with commit $commit"

	# 3. Scratch database through a driver-grade DSN parser. The helper is
	# a package under testdata/, which the go tool excludes from ./... and
	# ./cmd/... builds; it is removed again in the finally block.
	New-Item -ItemType Directory -Force -Path $scratch | Out-Null
	New-Item -ItemType Directory -Force -Path $helperDir | Out-Null
	Set-Content -Path (Join-Path $helperDir 'main.go') -Encoding ASCII -Value @'
// Command smoke-createdb creates or drops a scratch database and prints the
// derived target DSN. Written by scripts/smoke-vps.ps1 at run time; it exists
// only for the smoke run and is deleted afterwards.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: smoke-createdb <create|drop> <admin-dsn> <name>")
		os.Exit(2)
	}
	mode, rawDSN, name := os.Args[1], os.Args[2], os.Args[3]
	for _, r := range name {
		if r != '_' && (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			fmt.Fprintln(os.Stderr, "scratch database name must be [A-Za-z0-9_]")
			os.Exit(2)
		}
	}
	base, err := mysql.ParseDSN(rawDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse DSN: %v\n", err)
		os.Exit(2)
	}
	admin := *base
	admin.DBName = ""
	db, err := sql.Open("mysql", admin.FormatDSN())
	if err != nil {
		fmt.Fprintf(os.Stderr, "open admin connection: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch mode {
	case "create":
		if _, err := db.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
			fmt.Fprintf(os.Stderr, "create scratch database: %v\n", err)
			os.Exit(1)
		}
		target := *base
		target.DBName = name
		fmt.Println(target.FormatDSN())
	case "drop":
		if _, err := db.ExecContext(ctx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
			fmt.Fprintf(os.Stderr, "drop scratch database: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown mode:", mode)
		os.Exit(2)
	}
}
'@

	# Materialize the helper output before selecting: piping a native
	# command into Select-Object -First stops the pipeline and kills it.
	$helperOutput = @(& go run ./testdata/smoke-createdb create $Dsn $scratchDb)
	if ($LASTEXITCODE -ne 0) {
		throw "creating the scratch database failed (exit $LASTEXITCODE); is MySQL reachable at the MYSQL_TEST_DSN?"
	}
	$scratchDsn = ($helperOutput | Select-Object -First 1).Trim()
	if (-not $scratchDsn) {
		throw 'the scratch database helper printed no target DSN'
	}
	Write-Output "smoke: scratch database $scratchDb created"

	# 4. Host binaries from the same committed source tree.
	& go build -o (Join-Path $scratch 'smoke-migrate.exe') './cmd/migrate'
	if ($LASTEXITCODE -ne 0) { throw 'building the host migrate binary failed' }
	& go build -o (Join-Path $scratch 'smoke-server.exe') './cmd/server'
	if ($LASTEXITCODE -ne 0) { throw 'building the host server binary failed' }

	$env:MYSQL_DSN = $scratchDsn
	$upOutput = (& (Join-Path $scratch 'smoke-migrate.exe') '-command' 'up' 2>&1 | Out-String).Trim()
	if ($LASTEXITCODE -ne 0) { throw "migrate up failed: $upOutput" }
	Assert-True ($upOutput -match 'migration up completed at version (\d+)') "unexpected migrate up output: $upOutput"
	$expectedVersion = $Matches[1]

	& (Join-Path $scratch 'smoke-migrate.exe') '-command' 'status'
	if ($LASTEXITCODE -ne 0) { throw 'migrate status failed' }
	$versionOutput = (& (Join-Path $scratch 'smoke-migrate.exe') '-command' 'version' 2>&1 | Out-String).Trim()
	if ($LASTEXITCODE -ne 0) { throw "migrate version failed: $versionOutput" }
	Assert-True ($versionOutput -match "at version $expectedVersion\b") "migrate version output = $versionOutput, want version $expectedVersion"
	Write-Output "smoke: migrations up/status/version at schema version $expectedVersion"

	# 5. Production-shaped server configuration on a free loopback port.
	$listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
	$listener.Start()
	$port = $listener.LocalEndpoint.Port
	$listener.Stop()

	$env:PUBLIC_APP_URL = 'https://app.robloxkit-smoke.invalid'
	$env:MCP_RESOURCE_URL = 'https://api.robloxkit-smoke.invalid'
	$env:LISTEN_ADDRESS = "127.0.0.1:$port"
	$env:ALLOWED_ORIGIN = 'https://app.robloxkit-smoke.invalid'
	$env:TRUSTED_PROXIES = '127.0.0.1/32'
	$env:TOKEN_PEPPER = ('s' * 32)
	$env:HTTP_READ_TIMEOUT = '5s'
	$env:HTTP_WRITE_TIMEOUT = '5s'
	$env:MYSQL_MAX_OPEN_CONNS = '4'
	$env:MYSQL_MAX_IDLE_CONNS = '2'
	$env:BRIDGE_HEARTBEAT_INTERVAL = '10s'
	$env:BRIDGE_TIMEOUT = '30s'
	$env:BRIDGE_QUEUE_LIMIT = '16'
	$env:BRIDGE_MAX_MESSAGE_BYTES = '65536'
	$env:ROBLOX_CLIENT_ID = 'smoke-client'
	$env:ROBLOX_PROVIDER_BASE_URL = 'https://apis.robloxkit-smoke.invalid'
	$env:BRIDGE_ARTIFACT_PATH = (Join-Path $releaseDir 'RobloxBridge-windows-amd64.exe')
	$env:BRIDGE_ARTIFACT_FILENAME = 'RobloxBridge-windows-amd64.exe'
	$env:WEB_STATIC_DIR = (Join-Path $releaseDir 'dist')

	$serverProcess = Start-Process -FilePath (Join-Path $scratch 'smoke-server.exe') `
		-PassThru -WindowStyle Hidden `
		-RedirectStandardError (Join-Path $scratch 'server.stderr.log') `
		-RedirectStandardOutput (Join-Path $scratch 'server.stdout.log')
	# Touching the handle now keeps full process access, so ExitCode is
	# populated after the exit (a PS 5.1 Start-Process quirk).
	$null = $serverProcess.Handle

	$healthz = Wait-HttpStatus -Uri "http://127.0.0.1:$port/healthz" -TimeoutSeconds 30
	Write-Output ("smoke: GET healthz answered {0}" -f $healthz.StatusCode)
	$readyz = Wait-HttpStatus -Uri "http://127.0.0.1:$port/readyz" -TimeoutSeconds 30
	Write-Output ("smoke: GET readyz answered {0}" -f $readyz.StatusCode)
	# 6. Interrupt must drain and exit cleanly. Windows has no cross-process
	# SIGINT: a helper child attaches to the server's own console and
	# generates CTRL_BREAK to every process there, which the Go runtime
	# delivers as SIGINT (runtime/os_windows.go maps both CTRL_C and
	# CTRL_BREAK to SIGINT on Go 1.26), so the server drains and exits 0.
	# CTRL_C is unusable here: children created via CreateProcess carry the
	# ignore-CTRL_C flag and silently drop it. The helper shares the
	# generated event and terminates with STATUS_CONTROL_C_EXIT by design;
	# the smoke asserts on the server's outcome, not the helper's survival.
	# The helper runs from a file (not -Command) because PS 5.1 mangles
	# embedded quotes when it forwards script text on a native command line.
	$ctrlScriptPath = Join-Path $scratch 'send-ctrl-break.ps1'
	Set-Content -Path $ctrlScriptPath -Encoding ASCII -Value @'
param([uint32]$TargetPid)
$signature = @"
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool FreeConsole();
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool AttachConsole(uint dwProcessId);
[DllImport("kernel32.dll", SetLastError = true)] public static extern bool GenerateConsoleCtrlEvent(uint ctrlEvent, uint processGroupId);
"@
$type = Add-Type -MemberDefinition $signature -Name Win32Ctrl -Namespace RobloxKitSmoke -PassThru
[void]$type::FreeConsole()
if (-not $type::AttachConsole($TargetPid)) { exit 3 }
if (-not $type::GenerateConsoleCtrlEvent(1, 0)) { exit 4 }
exit 0
'@
	& powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $ctrlScriptPath $serverProcess.Id | Out-Null
	$helperExit = $LASTEXITCODE
	$helperSacrificed = -1073741510 # STATUS_CONTROL_C_EXIT: the helper shares the event
	Assert-True ($helperExit -eq 0 -or $helperExit -eq $helperSacrificed) "sending the interrupt failed (helper exit code $helperExit)"

	$graceful = $serverProcess.WaitForExit(20000)
	if (-not $graceful) {
		throw 'server did not exit within the bounded drain window after the interrupt'
	}
	# .NET quirk: ExitCode is only refreshed after a parameterless WaitForExit.
	$serverProcess.WaitForExit()
	Assert-True ($serverProcess.ExitCode -eq 0) "server exit code $($serverProcess.ExitCode), want 0 for a graceful drain"
	$serverProcess = $null
	Write-Output 'smoke: interrupt produced a clean exit (code 0)'

	Write-Output 'smoke: PASS'
} finally {
	if ($null -ne $serverProcess -and -not $serverProcess.HasExited) {
		Stop-Process -Id $serverProcess.Id -Force -ErrorAction SilentlyContinue
	}
	if (Test-Path (Join-Path $helperDir 'main.go')) {
		Remove-ScratchDatabase
	}
	Remove-Item -Recurse -Force $scratch -ErrorAction SilentlyContinue
	Remove-Item -Recurse -Force $helperDir -ErrorAction SilentlyContinue
	Restore-SmokeEnvironment
	Pop-Location -ErrorAction SilentlyContinue
}
