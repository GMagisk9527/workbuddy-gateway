# Native C# request performance, 2026-11-07

The later [conversation-metadata repair](CACHE-SESSION-20261107.md) is a separate compatibility change. Measurements below refer to the identified `6cb3be1` source/artifacts and have not been repeated for that repair.

The optimized Native AOT gateway is faster than Go in the measured large-request and long-SSE cases. All 15 cases have higher throughput in both CI and the production-machine isolation test. The production-machine small-request TTFT is 1.432 ms higher than Go. **Memory remains above Go; these measurements do not establish universal performance parity.** Production remains on `b28ac2a`; these changes were not merged or deployed.

## Implementation

- Known-length bodies use `HttpRequest.BodyReader` and an exact UTF-8 array, with admission before allocation, bounded read batches and existing truncation/decompression/cancellation checks.
- Request adapters borrow scalar JSON values only while their source document is alive. Large canonical strings stay UTF-8 instead of becoming large UTF-16 intermediates. Independent public readers and response metadata retain their ownership contracts.
- The original outbound normalization runs on a shadow of the conversion tree before encoding. Changed Chat, Responses and Messages requests avoid an intermediate complete JSON body, parse and second encoding. Messages keeps Go property ordering and escaping, including the final Prefix Affinity representation. User text and tool argument strings are not rewritten by this optimization.
- `Utf8JsonWriter` writes through `IBufferWriter<byte>` into a reserved exact output array. The previous Stream writer accumulated another whole-document buffer internally. A counting writer handles unknown sizes without retaining the document. Pessimistic encoder hints use cleared scratch arrays; the custom pool retains arrays only up to 64 KiB, with four arrays per bucket.
- Prefix Affinity encodes bounded canonical prefixes, with conservative fallback for unsupported escaping and correct handling of a split UTF-8 rune. Identity/model/caller isolation and the existing hash domains remain unchanged.
- Messages flushes and yields once at the first visible SSE frame. Ordering, later per-frame flushes and terminal events remain unchanged.
- Native AOT uses the portable CPU baseline, speed optimization and compiled `System.GC.ConserveMemory=5`. The benchmark-only CLI confirmed the runtime value of 5. Production environment variables and admission settings were not changed.

The local 4 MiB fixed-length body-read check allocates 4,195,200 bytes rather than 12,583,688 bytes. A 1 MiB Responses conversion microbenchmark falls from 6.386 ms / 4,587,462 B to 2.894 ms / 1,193,589 B with identical output. These are component measurements, not total gateway RSS or real-model latency.

## Paired CI comparison

[Actions 42357069574](https://github.com/GMagisk9527/workbuddy-gateway/actions/runs/42357069574) uses benchmark revision `c3cfe475baa10c6acf47af4dd5b330b784f82374`. Comparing all 201 C# source/project files with candidate `6cb3be1eac1d56c82e4f86af38186b32a8dd9ed0` leaves one functional difference: the benchmark adds a GC-configuration print-only CLI branch. The production gateway path is the same. Reproducible workflow and fixture: `.github/workflows/aot-go-comparison.yml` and `csharp/ci/aot-bench/` at that revision.

Go `cacf5a0`, previous optimization reference AOT `3e5e62f`, and candidate run alone on the same single CPU in Alpine 3.21 with external networking disabled. The common HTTPS fixture serves fixed SSE, using four synthetic accounts and a synthetic API key/ledger. Both runtimes receive a nominal 512 MiB GC budget; this is not a process RSS cap. Each fresh process gets 24 small warmups and four case warmups, followed by 64 timed requests per case. Mode order rotates across seven repetitions. Output length, valid termination and exact upstream call count are checked. No real model is called.

**Go and candidate completed all 13,440 timed requests.** The three-mode plan was 20,160 requests. Reference `3e5e62f` rejected 131 requests with HTTP 503 `memory_pressure` in four of seven bare 4 MiB Messages rounds; its highest sampled RSS there was 388.72 MiB. Those failures are preserved and excluded from latency/throughput claims. The run has 20,029 successful timed requests across all three modes, not 20,160. The previous reference is not the deployed `b28ac2a` binary.

Every table cell is **Go / candidate**. TTFT is time to first text, throughput is median round throughput, and RSS is sampled process memory. The median peak is the median of fresh-process peaks. Different runners and production hardware must not be compared by absolute timing.

### Tool history

| Case | TTFT ms | Requests/s | Median peak RSS MiB | Highest peak RSS MiB |
|---|---:|---:|---:|---:|
| Messages 1 KiB, c1, 64 frames | 2.582 / 1.625 | 208.48 / 232.19 | 17.52 / 51.92 | 17.67 / 52.13 |
| Messages 1 MiB, c1, 64 frames | 16.939 / 12.628 | 50.71 / 66.59 | 25.40 / 52.08 | 26.21 / 52.36 |
| Messages 1 MiB, c4, 64 frames | 64.981 / 50.717 | 51.99 / 65.27 | 34.35 / 62.27 | 37.08 / 63.84 |
| Messages 4 MiB, c1, 64 frames | 59.392 / 42.366 | 16.02 / 21.95 | 52.43 / 89.49 | 55.57 / 124.83 |
| Chat 1 MiB, c4, 64 frames | 63.063 / 52.552 | 50.98 / 66.78 | 33.20 / 57.28 | 33.61 / 61.62 |
| Responses 1 MiB, c4, 64 frames | 59.903 / 55.936 | 56.51 / 62.02 | 34.21 / 64.05 | 38.45 / 67.18 |
| Messages 1 KiB, c4, 1024 frames | 20.281 / 16.781 | 30.26 / 38.93 | 18.96 / 52.59 | 19.24 / 52.75 |

### Ordinary user text

| Case | TTFT ms | Requests/s | Median peak RSS MiB | Highest peak RSS MiB |
|---|---:|---:|---:|---:|
| Messages 1 MiB, c4, 64 frames | 75.770 / 59.951 | 45.88 / 56.59 | 40.23 / 62.34 | 41.41 / 63.52 |
| Messages 4 MiB, c1, 64 frames | 70.271 / 50.033 | 13.55 / 18.98 | 61.10 / 90.80 | 63.34 / 108.88 |
| Chat 1 MiB, c4, 64 frames | 74.002 / 58.544 | 44.95 / 60.16 | 38.76 / 57.49 | 40.46 / 60.30 |
| Responses 1 MiB, c4, 64 frames | 67.047 / 61.016 | 51.21 / 57.09 | 40.97 / 63.30 | 41.56 / 64.84 |

### Ordinary user text without system instructions

| Case | TTFT ms | Requests/s | Median peak RSS MiB | Highest peak RSS MiB |
|---|---:|---:|---:|---:|
| Messages 1 MiB, c4, 64 frames | 74.068 / 46.017 | 46.65 / 72.83 | 39.82 / 60.67 | 42.90 / 62.11 |
| Messages 4 MiB, c1, 64 frames | 70.148 / 37.788 | 13.64 / 24.75 | 54.73 / 84.76 | 63.23 / 102.46 |
| Chat 1 MiB, c4, 64 frames | 72.696 / 53.628 | 45.41 / 65.21 | 40.48 / 60.92 | 42.00 / 64.25 |
| Responses 1 MiB, c4, 64 frames | 66.884 / 62.419 | 50.97 / 56.23 | 40.44 / 62.64 | 42.81 / 64.85 |

Across these CI cases, candidate TTFT is 6.6–46.1% lower and throughput is 9.8–81.5% higher than Go. The ordinary-user Responses case that previously regressed now measures 61.016 vs Go 67.047 ms, with 57.09 vs 51.21 requests/s. Removing system instructions also remains covered; this exercises actual prompt injection rather than assuming all ordinary-user fixtures need normalization.

Against the previous AOT reference, tool-history 4 MiB Messages median peak RSS falls from 117.30 to 89.49 MiB; bare 4 MiB Messages completes all requests at a median 84.76 MiB. Small tool-history throughput is 2.6% below the previous AOT reference, although it remains above Go. The change does not improve every metric against every baseline.

## Production-machine isolated comparison

The exact formal PR artifact, SHA-256 `fce1a7f256a8f44ee0d8c566174099acf0bc5a34cb75f194f25fa6161be435f6`, was compared with the installed Go `cacf5a0` executable on the one-CPU, 768 MiB production machine. This uses three repetitions of 16 timed requests, rotated Go/candidate order, the same fixtures and nominal GC budgets, private mount/network namespaces and lower scheduling priority. Only loopback is up/routed in the test network namespace. Hosts mappings are private; the real `/etc/hosts` hash stayed unchanged. Test accounts, keys and ledger are synthetic.

**All 1,440 timed requests passed**, including output/termination/upstream-count checks; no model requests were sent. Production PID 34296 remained on the same `b28ac2a` executable and readiness stayed HTTP 200. These shorter samples corroborate the CI direction, but share the host with the live service and are not a dedicated-load-machine result.

### Tool history

| Case | TTFT ms | Requests/s | Median peak RSS MiB | Highest peak RSS MiB |
|---|---:|---:|---:|---:|
| Messages 1 KiB, c1, 64 frames | 3.535 / 4.967 | 65.04 / 74.65 | 17.25 / 42.38 | 17.38 / 42.88 |
| Messages 1 MiB, c1, 64 frames | 28.763 / 27.188 | 17.59 / 22.52 | 25.88 / 53.25 | 26.12 / 53.50 |
| Messages 1 MiB, c4, 64 frames | 193.275 / 146.335 | 17.76 / 20.03 | 33.11 / 60.18 | 37.04 / 60.47 |
| Messages 4 MiB, c1, 64 frames | 184.371 / 119.316 | 5.39 / 7.18 | 51.09 / 66.05 | 52.25 / 71.83 |
| Chat 1 MiB, c4, 64 frames | 187.686 / 138.805 | 17.78 / 22.73 | 32.90 / 55.64 | 35.67 / 56.39 |
| Responses 1 MiB, c4, 64 frames | 188.092 / 182.671 | 19.56 / 20.61 | 33.14 / 58.88 | 33.68 / 59.52 |
| Messages 1 KiB, c4, 1024 frames | 77.488 / 57.040 | 11.21 / 13.61 | 17.88 / 43.50 | 17.98 / 43.50 |

### Ordinary user text

| Case | TTFT ms | Requests/s | Median peak RSS MiB | Highest peak RSS MiB |
|---|---:|---:|---:|---:|
| Messages 1 MiB, c4, 64 frames | 208.112 / 177.790 | 14.82 / 18.38 | 36.63 / 60.62 | 38.67 / 60.70 |
| Messages 4 MiB, c1, 64 frames | 200.103 / 134.799 | 4.59 / 6.30 | 52.99 / 65.87 | 57.79 / 66.88 |
| Chat 1 MiB, c4, 64 frames | 206.375 / 181.120 | 15.86 / 20.08 | 35.24 / 55.40 | 35.95 / 56.50 |
| Responses 1 MiB, c4, 64 frames | 194.633 / 184.822 | 17.22 / 18.14 | 35.50 / 57.86 | 39.64 / 58.66 |

### Ordinary user text without system instructions

| Case | TTFT ms | Requests/s | Median peak RSS MiB | Highest peak RSS MiB |
|---|---:|---:|---:|---:|
| Messages 1 MiB, c4, 64 frames | 212.720 / 131.407 | 14.74 / 20.94 | 37.38 / 57.43 | 39.43 / 57.63 |
| Messages 4 MiB, c1, 64 frames | 204.496 / 109.046 | 4.58 / 7.95 | 52.16 / 67.75 | 63.17 / 71.96 |
| Chat 1 MiB, c4, 64 frames | 214.078 / 182.468 | 14.51 / 20.04 | 37.54 / 56.35 | 41.68 / 56.62 |
| Responses 1 MiB, c4, 64 frames | 199.346 / 193.417 | 17.46 / 18.09 | 37.70 / 57.70 | 40.95 / 59.04 |

On this machine all 15 throughput results exceed Go by 3.6–73.6%. Fourteen TTFT results improve; small 1 KiB Messages is 4.967 vs Go 3.535 ms. Tool-history 4 MiB Messages TTFT falls 35.3% (184.371 to 119.316 ms); without system instructions it falls 46.7% (204.496 to 109.046 ms). Ordinary-user Responses improves more modestly, from 194.633 to 184.822 ms.

**Memory is still a limitation.** The production-machine tool-history 4 MiB peak is 66.05 vs Go 51.09 MiB, and the small-request peak is 42.38 vs 17.25 MiB. CI tool-history 4 MiB median peak is 89.49 vs 52.43 MiB, with highest candidate peak 124.83 MiB. These tests do not justify removing existing memory admission or increasing production concurrency.

## Compatibility and free HY3 validation

Final source checks passed: 467 OpenAI encoding/ownership checks, 15 streaming and 12 HTTP checks; 1,026 normalization/byte/prefix comparisons; 791 affinity, 574 operations, 46 body-lifetime and 26 Messages HTTP checks; 40 UTF-8 tool-result and 15 UTF-8 user-text cases. Rebuilding the differential harness against the final assemblies again reproduced all 552 three-protocol output/error fingerprints exactly.

The final code revision passed full-gateway Native AOT and native component/JIT regressions in [42357432771](https://github.com/GMagisk9527/workbuddy-gateway/actions/runs/42357432771), Go vet/test/JSON fallback/race in [42357432735](https://github.com/GMagisk9527/workbuddy-gateway/actions/runs/42357432735), and TCP fault tests in [42357432743](https://github.com/GMagisk9527/workbuddy-gateway/actions/runs/42357432743). Native acceptance retains API key, synthetic billing/WAL and restart-recovery checks.

The user authorized real free `hy3` tests. The live catalog was checked before each request for `fromLive`, a nonexpired multiplier of 0 and exact model ID `hy3`; priced `hy3-x` and paid fallback were not used. The exact formal candidate above ran in a temporary loopback-only instance with separate keys/ledger and a temporary copy of one enabled, unexpired CN credential. Credential refresh, resource/catalog polling, check-in, keepalive and price probes were disabled.

**All 10 real HY3 requests passed with HTTP 200 and independently recorded `credit=0`:** three-protocol SSE, three-protocol tool-call/result round trips and summary/compaction continuation. Responses replayed returned reasoning/function items with the matching `call_id`; SSE had terminal events. The summary request normally returned `OK.`. Test processes and temporary credential directories were removed. No production deployment/configuration/credential writes were made.

These are synthetic conversations against a real model, not a running Codex/DSH client or a captured DSH fixture. International upstream behavior, long client sessions, all possible payload shapes and account restrictions are not established by these checks. Real model/network timing is not used to claim gateway speed gains.

## Evidence and rollout

- [Machine-readable paired summaries](benchmarks/performance-20261107.json) retain all 30 Go/candidate case comparisons, reference failure accounting, binary/source identities and redacted HY3 outcomes. Raw local evidence is under `csharp/out/perf-final-c3cfe47/`, including all timed-request rows, host metrics archive and synthetic HY3 records.
- CI artifacts in run 42357069574: `alpine-native-aot-vs-go-benchmark`, `alpine-native-aot-vs-go-user-benchmark`, `alpine-native-aot-vs-go-bare-benchmark`, `benchmark-native-candidate`. Their `metadata.json` records binary hashes; `results.jsonl` retains individual outcomes and times.
- The benchmark candidate includes the GC CLI probe and has SHA-256 `c507b6da35f04cfcef406bbdf823e6d7d1657a52f97d6fff035ffbbf0d37e96f`. The formal PR artifact used on the production machine and for HY3 has SHA-256 `fce1a7f256a8f44ee0d8c566174099acf0bc5a34cb75f194f25fa6161be435f6`.
- This report does not authorize deployment. Keep the existing production account scheduling, API keys, credits/WAL, port, OpenRC service and admission settings. No production configuration adjustment is needed for the compiled optimizations; merge/deployment remains a separate user decision.
