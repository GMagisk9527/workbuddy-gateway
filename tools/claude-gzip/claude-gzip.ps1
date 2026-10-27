param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$ClaudeGzipArguments
)
if ($MyInvocation.ExpectingInput) {
    $input | & node (Join-Path $PSScriptRoot 'bin/claude-gzip.cjs') @ClaudeGzipArguments
} else {
    & node (Join-Path $PSScriptRoot 'bin/claude-gzip.cjs') @ClaudeGzipArguments
}
exit $LASTEXITCODE
