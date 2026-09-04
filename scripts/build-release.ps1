# Deterministic release build for the RobloxKit gateway.
#
# Produces:
#   bin/robloxkit-server-linux-amd64     gateway server (VPS)
#   bin/robloxkit-migrate-linux-amd64    migration CLI (VPS)
#   bin/RobloxBridge-windows-amd64.exe   Windows bridge
#   bin/BUILD-ID                        commit/version identity
#   bin/SHA-256SUMS                     checksums for every artifact
#
# The release never packages .env files, logs, or node_modules: the artifact
# list below is explicit, and every collected path is checked against the
# forbidden set before it is hashed.
#
# Race detector note: this script deliberately runs `go test` WITHOUT -race.
# Releases are built with CGO_ENABLED=0 and the race detector requires cgo,
# so -race cannot run here; race coverage belongs to development runs.
#
# The script restores GOOS/GOARCH/CGO_ENABLED and the working directory
# before exiting, so dot-sourcing it never leaks build state into a session.

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$root = Split-Path -Parent $PSScriptRoot
$bin = Join-Path $root 'bin'
$webDist = Join-Path $root 'web\dist'
$distOut = Join-Path $bin 'dist'

# Environment values are captured per-process; the finally block restores
# them exactly, including "was unset".
$savedEnv = @{}
foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED')) {
	$savedEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
Push-Location $root

function Restore-Environment {
	foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED')) {
		if ($null -eq $savedEnv[$name]) {
			Remove-Item -Path "Env:$name" -ErrorAction SilentlyContinue
		} else {
			Set-Item -Path "Env:$name" -Value $savedEnv[$name]
		}
	}
	Pop-Location
}

function Invoke-Checked {
	param(
		[string]$FilePath,
		[string[]]$ArgumentList
	)
	& $FilePath @ArgumentList
	if ($LASTEXITCODE -ne 0) {
		throw "$FilePath $($ArgumentList -join ' ') failed with exit code $LASTEXITCODE"
	}
}

try {
	New-Item -ItemType Directory -Force -Path $bin | Out-Null

	# Deterministic rebuilds: start from a clean slate for exactly the
	# artifacts this script owns. Unrelated files in bin/ are left alone.
	$releaseBinaries = @(
		'robloxkit-server-linux-amd64',
		'robloxkit-migrate-linux-amd64',
		'RobloxBridge-windows-amd64.exe'
	)
	# Version traceability: every artifact carries the exact commit and
	# version through the build ID. Automatic -buildvcs stamping is
	# deliberately disabled: inside a git worktree the go command reads the
	# MAIN repository's HEAD and dirty flag, which would embed wrong
	# metadata into the release binaries.
	$commit = (& git rev-parse HEAD).Trim()
	if ($LASTEXITCODE -ne 0 -or -not $commit) {
		throw 'git rev-parse HEAD failed: the release must be built from a git checkout'
	}
	$version = (& git describe --tags --always --dirty).Trim()
	$ldflags = "-s -w -buildid=$commit/$version"
	Write-Output "Release version: $version"
	Write-Output "Release commit:  $commit"

	# 1. Backend test suite. No -race: releases build with CGO disabled.
	$env:CGO_ENABLED = '0'
	Invoke-Checked 'go' @('test', './...', '-count=1')

	# 2. Frontend test suite, then the production bundle.
	Invoke-Checked 'npm' @('--prefix', 'web', 'test', '--', '--run')
	Invoke-Checked 'npm' @('--prefix', 'web', 'run', 'build')
	if (-not (Test-Path (Join-Path $webDist 'index.html'))) {
		throw 'web build did not produce web/dist/index.html'
	}

	# 3. Cross-compiled release binaries. -trimpath removes machine paths;
	# -s -w strips symbol tables; the commit/version ride in the build ID.
	$env:GOOS = 'linux'
	$env:GOARCH = 'amd64'
	Invoke-Checked 'go' @('build', '-trimpath', '-buildvcs=false', '-ldflags', $ldflags,
		'-o', (Join-Path $bin 'robloxkit-server-linux-amd64'), './cmd/server')
	Invoke-Checked 'go' @('build', '-trimpath', '-buildvcs=false', '-ldflags', $ldflags,
		'-o', (Join-Path $bin 'robloxkit-migrate-linux-amd64'), './cmd/migrate')

	$env:GOOS = 'windows'
	$env:GOARCH = 'amd64'
	Invoke-Checked 'go' @('build', '-trimpath', '-buildvcs=false', '-ldflags', $ldflags,
		'-o', (Join-Path $bin 'RobloxBridge-windows-amd64.exe'), './cmd/bridge')


	# BUILD-ID is a manifest-covered identity that remains available on a
	# production host without the Go toolchain. Smoke verification also
	# compares it with the IDs embedded in all three binaries.
	$buildIdentityPath = Join-Path $bin 'BUILD-ID'
	[System.IO.File]::WriteAllText(
		$buildIdentityPath,
		("$commit/$version`n"),
		(New-Object System.Text.UTF8Encoding($false))
	)
	# 4. Frontend artifacts ship inside the release.
	New-Item -ItemType Directory -Force -Path $distOut | Out-Null
	Copy-Item -Path (Join-Path $webDist '*') -Destination $distOut -Recurse -Force

	# 5. Collect every artifact relative to bin/ and refuse forbidden paths.
	$artifactPaths = @($releaseBinaries) + @('BUILD-ID')
	$artifactPaths += Get-ChildItem -Path $distOut -Recurse -File |
		ForEach-Object { $_.FullName.Substring($bin.Length + 1).Replace('\', '/') }

	foreach ($relative in $artifactPaths) {
		if ($relative -match '(^|/)\.env|\.log$|(^|/)node_modules(/|$)') {
			throw "refusing to package forbidden path: $relative"
		}
	}

	# 6. SHA-256SUMS: lowercase hex, GNU sha256sum BINARY format (hash<space>*path),
	# LF line endings, paths relative to the release root so any platform —
	# Linux, macOS, and MSYS/Git Bash alike — verifies with `sha256sum -c`.
	# The binary-mode marker matters: text-mode entries make MSYS sha256sum
	# translate CRLF inside binaries and reject valid artifacts.
	$checksumLines = foreach ($relative in ($artifactPaths | Sort-Object)) {
		$hash = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $bin $relative)).Hash.ToLowerInvariant()
		"$hash *$relative"
	}
	$checksumsPath = Join-Path $bin 'SHA-256SUMS'
	[System.IO.File]::WriteAllText(
		$checksumsPath,
		(($checksumLines -join "`n") + "`n"),
		(New-Object System.Text.UTF8Encoding($false))
	)

	# 7. Release summary.
	Write-Output ''
	Write-Output 'Release artifacts:'
	foreach ($relative in ($artifactPaths | Sort-Object)) {
		$file = Get-Item (Join-Path $bin $relative)
		$size = '{0:N0} bytes' -f $file.Length
		$hash = (Get-FileHash -Algorithm SHA256 -Path $file.FullName).Hash.ToLowerInvariant()
		Write-Output ('  {0}  {1}  {2}' -f $relative, $size, $hash)
	}
	Write-Output "  SHA-256SUMS written to $checksumsPath"
	Write-Output "Release build completed for $version ($commit)."
} finally {
	Restore-Environment
}
