$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$bin = Join-Path $root 'bin'
New-Item -ItemType Directory -Force -Path $bin | Out-Null

$env:CGO_ENABLED = '0'
$env:GOOS = 'windows'
$env:GOARCH = 'amd64'

go build -trimpath -ldflags='-s -w' -o (Join-Path $bin 'RobloxBridge.exe') (Join-Path $root 'cmd/bridge')
Write-Output "Built $bin\RobloxBridge.exe"
