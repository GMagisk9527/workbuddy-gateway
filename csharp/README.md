# C# gateway migration and Linux Native AOT validation

This directory contains the independently migrated .NET 11 RC3 gateway, its offline checks, and an opt-in Native AOT experiment. The SDK is fixed by `global.json`. No credentials, runtime stores, deployment reports or local SDK binaries are included.

The GitHub Actions workflow uses Alpine 3.21 and `linux-musl-x64`, matching the production libc. It installs the native compiler only in an ephemeral build container and has no deployment credentials or deployment step.

Two results are intentionally separate:

- `native-components` builds and runs the current gateway's JIT checks, seven foundation suites and frontend tests. It then compiles the SSE event writer plus key/ledger persistence checks to a native executable with reflection JSON disabled and linker/AOT warnings treated as errors. All 2,508 SSE variants must match the JIT legacy writer's byte fingerprint; native key creation, quota settlement, duplicate protection and durable reload must also pass.
- `full-gateway-aot` attempts strict AOT publication of the entire HTTP application. This is currently expected to expose remaining reflection serialization and request-delegate diagnostics. A successful component executable is **not** a production gateway release. The full job must become green before claiming whole-application AOT compatibility.

The SSE event writer emits canonical UTF-8 bytes directly, preserving the previous escaping, key order, thinking/tool events and error framing. Key and ledger disk JSON now use generated metadata while preserving the disk schema. Existing optimized message preparation is included unchanged.

Local Linux prerequisites are documented by Microsoft: [Native AOT](https://learn.microsoft.com/dotnet/core/deploying/native-aot/) and [ASP.NET Core Native AOT](https://learn.microsoft.com/aspnet/core/fundamentals/native-aot). Windows-to-Linux Native AOT is not supported; use the workflow's Alpine build environment.

Run the same checks in Alpine with the pinned SDK on PATH:

```sh
bash ci/verify.sh
bash ci/native.sh
dotnet publish http/HttpMigration.csproj -c Release -r linux-musl-x64 --source https://api.nuget.org/v3/index.json -p:GatewayNativeAot=true
```

The native artifact is a validation executable, without a model endpoint. No production configuration, API Key, account or billing data is needed. Do not replace the deployed gateway with it.
