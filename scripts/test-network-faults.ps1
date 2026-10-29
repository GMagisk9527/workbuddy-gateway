<#
.SYNOPSIS
  Run loopback-only TCP fault tests with Toxiproxy v2.13.0.
.DESCRIPTION
  Optionally include an independently built native compression proxy. Uses
  synthetic credentials and a temporary ledger, with no production requests.
#>
param(
    [string]$ToxiproxyPath = $env:WORKBUDDY_TOXIPROXY,
    [string]$CompressProxyPath = $env:WORKBUDDY_COMPRESS_BIN,
    [switch]$Race
)
$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
if (-not $ToxiproxyPath) { throw 'Provide -ToxiproxyPath for the pinned v2.13.0 server; see tests/README.md.' }
$tool = (Get-Item -LiteralPath $ToxiproxyPath -ErrorAction Stop).FullName
$compress = if ($CompressProxyPath) { (Get-Item -LiteralPath $CompressProxyPath -ErrorAction Stop).FullName } else { $null }
$names = @('WORKBUDDY_TOXIPROXY', 'WORKBUDDY_COMPRESS_BIN', 'TMP', 'TEMP',
    'GOOS', 'GOARCH', 'CGO_ENABLED', 'WORKBUDDY_BROWSER_FIXTURE', 'WORKBUDDY_HEALTH_BROWSER_FIXTURE')
$previous = @{}
foreach ($name in $names) { $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
$id = [Guid]::NewGuid().ToString('N')
$tempRoot = Join-Path $root ".codex-tmp/network-faults/$id"
[IO.Directory]::CreateDirectory($tempRoot) | Out-Null
[IO.Directory]::CreateDirectory((Join-Path $root 'logs')) | Out-Null
$log = Join-Path $root "logs/network-faults-$id.log"
Push-Location -LiteralPath $root
try {
    foreach ($name in $names) { [Environment]::SetEnvironmentVariable($name, $null, 'Process') }
    $env:WORKBUDDY_TOXIPROXY = $tool
    $env:WORKBUDDY_COMPRESS_BIN = $compress
    $env:TMP = $tempRoot
    $env:TEMP = $tempRoot
    $arguments = @('test', '-tags=networkfaults', '-run', '^TestNetworkFault', '-count=1', '-timeout=3m', '-v')
    if ($Race) { $arguments += '-race' }
    $arguments += '.'
    $PSNativeCommandUseErrorActionPreference = $false
    & go @arguments *> $log
    $code = $LASTEXITCODE
    Get-Content -LiteralPath $log -Tail 25
    if ($code -ne 0) { throw "Network fault tests failed ($code); log: $log" }
    Write-Host "Network fault tests passed; log: $log"
    $global:LASTEXITCODE = 0
} catch {
    $global:LASTEXITCODE = 1
    throw
} finally {
    foreach ($name in $names) { [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process') }
    Pop-Location
}
