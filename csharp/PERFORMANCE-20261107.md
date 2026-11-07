# Native C# request preparation, 2026-11-07

The candidate reduces copying, repeated JSON parsing and large UTF-16 strings while preserving protocol and scheduling behavior. It is compared with Go `cacf5a0` and deployed Native AOT `b28ac2a`. Production was not replaced during these tests.

## Implementation

- Known-length bodies use `HttpRequest.BodyReader` and one exact UTF-8 array. Admission reserves the length before reading; allocation waits for an initial chunk. Later reads batch at most 32 KiB. Truncation, cancellation, EOF, unknown-length bodies and decompression limits retain their checks.
- Scoped request documents lend scalar JSON values to synchronous conversion. Adapters finish encoding before disposing the document. Public independently owned readers and response metadata keep their ownership guarantees.
- Large canonical strings stay UTF-8 through Messages and Responses user-text/tool-result conversion. Validated sizes are reused for exact output allocation. Unusual escapes and other unsupported fast cases keep the original behavior. Go-style HTML escaping and key ordering in Messages are preserved.
- Chat reuses a complete canonical input array only when the gateway explicitly transfers ownership. Other callers get an independent result. Default model/stream normalization, duplicate-property semantics and validation remain intact.
- `OutboundPreparation.IsCompatible` runs the same normalization implementation as `Prepare` on a shadow tree. Another parse is skipped only when normalization would leave the request unchanged; prompt rules and tool associations still run.
- Session identity is extracted from the already parsed original request. Prefix Affinity hashes the same serialized tools/first-two-message fragments within a conservative bound; large fragments use the existing byte scanner. Caller/model isolation, digest domains and configured prefix size are unchanged.
- Messages SSE yields once after flushing its first visible frame, allowing the transport to send it when a cached upstream completes every await synchronously. Event ordering, per-frame flushes and termination are preserved.
- Native AOT favors execution speed with the portable CPU baseline; no additional processor instruction set is required.

The local 4 MiB fixed-length read check allocates 4,195,200 bytes, down from 12,583,688 bytes. This measures body reading, not total request allocation or RSS.

## Final complete-gateway comparison

[Actions 42347820501](https://github.com/GMagisk9527/workbuddy-gateway/actions/runs/42347820501) uses source `d12cd539364320b592285934f5d912ef1074e82a` on `codex/aot-go-bench-b28ac2a`. Its production source files match this candidate. The reproducible workflow and fixture are at that revision in `.github/workflows/aot-go-comparison.yml` and `csharp/ci/aot-bench/`.

Each gateway runs alone on the same single CPU in an Alpine 3.21 container with external networking disabled, four synthetic accounts, a synthetic key and a nominal 512 MiB GC budget. The common HTTPS fixture uses TCP_NODELAY and identical cached SSE. Each fresh process gets 24 small warmups and four case warmups. Mode order rotates. Each of seven repetitions has 16 timed requests. The 147 tool-history rounds and 84 user-text rounds contain **3,696 successful timed requests**. Output length, terminal events and exact upstream call counts must match. This comparison uses no real models.

Each cell below is **Go / deployed AOT / candidate AOT**. TTFT is median time to first text; throughput is median round throughput. RSS is the median of per-process sampled peaks and the highest observed peak. Compare modes within this run: runner hardware varies between runs and differs from production.

### Tool-history fixture

| Case | TTFT ms | Requests/s | Median peak RSS MiB | Highest peak RSS MiB |
|---|---:|---:|---:|---:|
| Messages 1 KiB, c1, 64 frames | 1.874 / 1.548 / 1.429 | 288.48 / 282.54 / 285.64 | 17.33 / 48.58 / 50.13 | 17.41 / 48.64 / 50.39 |
| Messages 1 MiB, c1, 64 frames | 11.458 / 15.438 / 9.005 | 75.13 / 56.23 / 92.36 | 25.17 / 84.37 / 53.18 | 25.96 / 97.82 / 53.87 |
| Messages 1 MiB, c4, 64 frames | 41.137 / 59.398 / 34.284 | 81.62 / 57.95 / 98.07 | 32.54 / 104.69 / 79.83 | 33.94 / 138.29 / 85.49 |
| Messages 4 MiB, c1, 64 frames | 35.945 / 50.228 / 27.591 | 26.64 / 19.02 / 33.54 | 51.18 / 187.85 / 94.97 | 51.22 / 229.10 / 107.99 |
| Chat 1 MiB, c4, 64 frames | 41.494 / 54.254 / 37.584 | 75.62 / 63.61 / 94.66 | 31.91 / 124.28 / 60.30 | 33.51 / 158.34 / 68.73 |
| Responses 1 MiB, c4, 64 frames | 38.694 / 59.758 / 36.796 | 84.45 / 59.97 / 90.25 | 32.89 / 146.54 / 76.99 | 33.49 / 172.37 / 85.06 |
| Messages 1 KiB, c4, 1024 frames | 21.630 / 23.341 / 17.345 | 50.76 / 63.98 / 62.86 | 18.35 / 50.97 / 52.47 | 18.71 / 51.15 / 52.84 |

### Large ordinary user text

These cases remove tool history and put the large payload in a normal user message, checking that benefits extend beyond tool-result conversion.

| Case | TTFT ms | Requests/s | Median peak RSS MiB | Highest peak RSS MiB |
|---|---:|---:|---:|---:|
| Messages 1 MiB, c4, 64 frames | 46.199 / 58.377 / 37.297 | 72.31 / 59.61 / 90.31 | 32.85 / 93.69 / 80.96 | 35.49 / 129.80 / 85.10 |
| Messages 4 MiB, c1, 64 frames | 39.739 / 48.683 / 30.916 | 23.88 / 19.32 / 30.60 | 54.42 / 206.58 / 95.18 | 54.75 / 232.41 / 142.23 |
| Chat 1 MiB, c4, 64 frames | 44.124 / 53.311 / 38.258 | 73.26 / 68.84 / 92.92 | 32.52 / 132.02 / 67.16 | 34.84 / 180.31 / 67.45 |
| Responses 1 MiB, c4, 64 frames | 39.861 / 58.697 / 42.147 | 81.61 / 60.69 / 79.83 | 33.42 / 126.73 / 74.30 | 35.95 / 183.35 / 106.78 |

The tool-history 4 MiB Messages TTFT improves 23.2% against Go and 45.1% against deployed AOT; throughput rises 25.9% and 76.3% respectively. Long-SSE TTFT is 19.8% below Go with 23.8% higher throughput, though throughput is 1.8% below deployed AOT. Small-request throughput is approximately level. Ordinary-user Responses TTFT remains 5.7% above Go with 2.2% lower throughput.

**Memory has not reached Go parity.** Tool-history 4 MiB Messages median peak RSS falls from 187.85 to 94.97 MiB, versus Go's 51.18 MiB. In the ordinary-user variant the candidate's highest peak is 142.23 MiB, versus Go's 54.75 MiB. Small-request RSS rises slightly against deployed AOT. These results support substantial CPU/latency improvement, not universal performance parity.

## Compatibility and real HY3 checks

The benchmark revision passes strict full-gateway Native AOT compilation, native gateway smoke tests and full Linux JIT/foundation/frontend regressions. Targeted checks include 610 Affinity checks, 574 operations checks, 46 body lifetime checks, 110 outbound fast-path checks, 40 UTF-8 tool-result cases, 15 UTF-8 user-text cases and 225 OpenAI encoding/ownership checks. All 552 deterministic three-protocol output/error fingerprints match the pre-change reference. Billing/WAL recovery and cancellation tests remain enabled.

The user separately authorized real HY3 calls. The live catalog declared `hy3` at multiplier 0 without an expired promotion; `hy3-x` is a separate, priced model and was not called. Three production baseline SSE requests completed across Chat, Responses and Messages; Chat's upstream usage included `credit=0`.

The exact benchmark candidate, SHA-256 `c5e936f5970550d0a9fb809b2a6242dfae6b0379d70a51438e4649b67fb42f28`, ran in a temporary loopback-only instance on the production host with separate keys/ledger and a temporary copy of one enabled, unexpired CN credential. Refresh, polling, check-in, keepalive and price probing were disabled. This did not switch production or write its credential/configuration files.

Candidate checks completed for three-protocol SSE and three-protocol two-request tool round trips, including Responses reasoning-state replay. A compressed-summary continuation initially failed the harness's exact `OK` content assertion. A separate check accepted normal nonempty terminated output and observed `OK.` with HTTP 200, a terminal event and recorded credit 0. This was an overstrict harness expectation. There were 11 candidate model calls and three baseline calls, without model fallback or paid models. Nine successful first-run scenario records were retained; that run's per-request billing history was not exported before cleanup, so zero credit is not independently attested for every request. Temporary credential directories and processes were removed after each run.

These synthetic requests against a real model do not constitute a live Codex/DSH client session or a captured DSH fixture. Client-specific long sessions, international upstream behavior and account restrictions remain separate validation work. Real model timing is not used to attribute gateway speed improvements.

## Evidence

- Final run: [42347820501](https://github.com/GMagisk9527/workbuddy-gateway/actions/runs/42347820501). Artifacts: `alpine-native-aot-vs-go-benchmark`, `alpine-native-aot-vs-go-user-benchmark`, `benchmark-native-candidate`. Each comparison artifact includes raw `results.jsonl`, `summary.json`, binary SHA-256 values in `metadata.json` and synthetic gateway logs.
- Prior seven-repeat tool-history run: [42346216519](https://github.com/GMagisk9527/workbuddy-gateway/actions/runs/42346216519), 2,352 successful timed requests. Large-request improvements have the same direction; it predates final user-text and first-frame-yield changes.
- The final PR revision is also checked by `csharp-native-aot.yml`, which produces a deployment artifact without deploying it.

Implementation contracts: Microsoft [System.IO.Pipelines](https://learn.microsoft.com/dotnet/standard/io/pipelines), [ASP.NET Core performance guidance](https://learn.microsoft.com/aspnet/core/fundamentals/best-practices), [Native AOT guidance](https://learn.microsoft.com/aspnet/core/fundamentals/native-aot).
