#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p out/verification
scratch="$(mktemp -d)"
dll="$PWD/out/jit/HttpMigration.dll"
dotnet build http/HttpMigration.csproj -c Release --configfile NuGet.Config -p:GatewayLibraryBuild=true -p:NuGetAudit=false -p:UseSharedCompilation=false -o out/jit
dotnet build bench-aot/BenchAot.csproj -c Release --configfile NuGet.Config -p:NuGetAudit=false -p:GatewayAssemblies="$PWD/out/jit" -o out/aot-reference
dotnet out/aot-reference/BenchAot.dll > out/verification/aot-reference.json
python3 - <<'PY'
import json,pathlib
out=pathlib.Path('out/verification');d=json.loads((out/'aot-reference.json').read_text());assert d['Checks']==2508
(out/'wire-sha.txt').write_text(d['Sha256']+'\n');print('Verified 2508 event variants and key/ledger reload; fingerprint',d['Sha256'])
PY

if [[ "${1:-}" == "--reference-only" ]]; then exit 0; fi
for test in identity upstream-sync conversation calendar scheduled-tasks body-lifetime console runtime-cache model-runtime background agent-tools count-tokens affinity account-management accounts standalone failover resource catalog live-catalog catalog-cache outbound self-test allocation operations-parity npm-catalog console-login; do
  args=("--$test-check")
  case "$test" in accounts) args=(--accounts-self-test);; self-test) args=(--self-test);; esac
  case "$test" in upstream-sync|conversation|calendar|body-lifetime|console|runtime-cache|model-runtime|background|agent-tools|count-tokens|affinity|account-management|accounts|standalone|failover|live-catalog|catalog-cache|operations-parity|console-login) args+=("$scratch/$test");; esac
  echo "Checking $test"
  timeout -s TERM -k 10 180 dotnet "$dll" "${args[@]}" > "out/verification/$test.json"
done
for entry in streaming/StreamingMigration openai-tests/OpenAiChecks scheduling-tests/SchedulingChecks security-tests/SecurityChecks ledger-tests/LedgerChecks maintenance-tests/MaintenanceChecks login-tests/LoginChecks; do
  folder="${entry%/*}"; name="${entry#*/}"
  dotnet build "$entry.csproj" -c Release --configfile NuGet.Config -p:NuGetAudit=false -p:UseSharedCompilation=false -o "out/foundations/$folder"
  args=(--self-test)
  case "$folder" in security-tests|ledger-tests|maintenance-tests|login-tests) args+=("$scratch/$folder");; esac
  timeout -s TERM -k 10 180 dotnet "out/foundations/$folder/$name.dll" "${args[@]}" > "out/verification/foundation-$folder.json"
done
node --test tests/web/app.test.cjs > out/verification/frontend.txt
