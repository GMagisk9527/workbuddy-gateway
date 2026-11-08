param(
  [string]$Commit = "",
  [string]$Output = "",
  [switch]$BuildIfMissing
)

$ErrorActionPreference = "Stop"
$arguments = @("-B", "csharp/ci/release.py")
if ($Commit) { $arguments += @("--commit", $Commit) }
if ($Output) { $arguments += @("--output", $Output) }
if ($BuildIfMissing) { $arguments += "--build-if-missing" }
& python @arguments
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
