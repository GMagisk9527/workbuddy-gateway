#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p out/native-evidence
dotnet publish bench-aot/BenchAot.csproj -c Release -r linux-musl-x64 --self-contained true --source https://api.nuget.org/v3/index.json \
  -p:PublishAot=true -p:JsonSerializerIsReflectionEnabledByDefault=false -p:IlcTreatWarningsAsErrors=true -p:ILLinkTreatWarningsAsErrors=true -p:TrimmerSingleWarn=false \
  -p:GatewayAssemblies="$PWD/out/jit" -p:NuGetAudit=false -o out/native
expected="$(cat out/verification/wire-sha.txt)"
out/native/BenchAot --expect "$expected" > out/native-evidence/native.json
file out/native/BenchAot | tee out/native-evidence/file.txt
ldd out/native/BenchAot | tee out/native-evidence/ldd.txt
sha256sum out/native/BenchAot | tee out/native-evidence/sha256.txt
if ldd out/native/BenchAot | grep -E 'libcoreclr|libhostfxr'; then echo 'Unexpected CLR runtime dependency'; exit 1; fi
tar -czf out/workbuddy-native-components-linux-musl-x64.tar.gz -C out/native BenchAot
