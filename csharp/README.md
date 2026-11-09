# C# gateway migration and Linux Native AOT validation

This directory contains the .NET 11 RC3 gateway, its offline checks, and the Linux Native AOT build. The SDK is pinned in `global.json`.

Direct `hy4free` requests use a per-request output limit of 32,768 tokens by
default. Set `output.hy4freeMaxTokens` in the admin configuration to change it.
A value of `0` disables the gateway limit. The gateway uses any lower limit
specified by the caller.

Successful direct model requests with an outbound body of at least 16 KiB
include cache diagnostics in the request log. The keyed tags cover up to
64 KiB each for tool definitions, system messages, and the first two non-system
messages. Tags are scoped to the caller and model and reset on restart.
Set `logs.cacheDiagnostics` to `false` to disable collection. Existing
`logs.hy4CacheDiagnostics` values apply when the new setting is absent.

## Release preparation

Prepare a production bundle from an exact commit without compiling on the
production server:

```powershell
powershell -NoProfile -File csharp/ci/prepare-release.ps1
```

The command locates the newest successful `CSharp Linux Native AOT` run for that
SHA, downloads `linux-musl-native-gateway`, verifies the ELF architecture and
both SHA-256 values and the synthetic smoke evidence, then writes a manifest
under `csharp/out/releases/<sha>/`. If that exact release is already prepared,
the command revalidates and reuses it. A missing CI artifact fails by default;
pass `-BuildIfMissing` to dispatch Actions from the checked-out branch, which
must already be pushed at the requested SHA, and wait. A different artifact is
never overwritten. The prepared bundle and manifest are the verified inputs
for the separately authorized deployment process. The server is never used as
a build machine.

The Actions jobs cache the pinned Alpine SDK and NuGet packages under the job
workspace cache. A cache miss still produces the same self-contained artifact;
it only affects build time.

The GitHub Actions workflow uses Alpine 3.21 and `linux-musl-x64`, matching the production libc. It installs the native compiler only in an ephemeral build container and has no deployment credentials or deployment step.

Three results are intentionally separate:

- `jit-regressions` runs the gateway's offline checks, seven foundation suites and frontend tests, with a separate timeout on each executable.
- `native-components` compiles the SSE event writer plus key/ledger persistence checks to a native executable with reflection JSON disabled and linker/AOT warnings treated as errors. It links the exact event source files, without the unrelated streaming CLI/probe entry point. All 2,508 SSE variants must match the JIT legacy writer's byte fingerprint; native key creation, quota settlement, duplicate protection and durable reload must also pass.
- `full-gateway-aot` compiles the entire gateway with AOT and linker warnings treated as errors. It runs the compiled binary against a synthetic HTTPS upstream on an isolated Docker network. The smoke test covers streaming and nonstreaming Messages, Chat Completions and Responses, request compression, search and fetch, admin APIs, the web UI, and persisted keys, config and billing after restart. It publishes the `linux-musl-native-gateway` artifact after the test passes.

The SSE event writer emits canonical UTF-8 bytes directly, preserving the previous escaping, key order, thinking/tool events and error framing. Key and ledger disk JSON now use generated metadata while preserving the disk schema. Existing optimized message preparation is included unchanged.

Local Linux prerequisites are documented by Microsoft: [Native AOT](https://learn.microsoft.com/dotnet/core/deploying/native-aot/) and [ASP.NET Core Native AOT](https://learn.microsoft.com/aspnet/core/fundamentals/native-aot). Windows-to-Linux Native AOT is not supported; use the workflow's Alpine build environment.

Run the same checks in Alpine with the pinned SDK on PATH:

```sh
bash ci/verify.sh
bash ci/native.sh
dotnet publish http/HttpMigration.csproj -c Release -r linux-musl-x64 --self-contained true \
  --source https://api.nuget.org/v3/index.json -p:GatewayNativeAot=true \
  -p:GatewayLibraryBuild=true -p:EnableAotAnalyzer=true -p:EnableTrimAnalyzer=true \
  -p:TrimmerSingleWarn=false -p:InvariantGlobalization=true
```

On Alpine x86-64, install the normal gateway dependencies, including `libstdc++`, CA certificates, OpenSSL libraries and `zstd-libs` for compressed requests. Extract the gateway archive and verify the executable against the supplied checksum:

```sh
tar -xzf workbuddy-gateway-linux-musl-x64.tar.gz
sha256sum -c SHA256SUMS
```

Run `./HttpMigration --state-check <accounts> <keys>` against a copy of the deployment state before switching the service. Start the binary with `./HttpMigration --standalone-pool <accounts> <keys> <port> [catalog]` using the existing service environment. Single-account installations can use `--standalone` with the same arguments. The native executable includes the production startup and state check. The JIT executable also includes the offline tests and probes.
