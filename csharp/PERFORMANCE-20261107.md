# Native C# request preparation, 2026-11-07

This follow-up compares the deployed Native AOT gateway (`b28ac2a`) with the
request preparation changes in this PR. Go remains the `cacf5a0` baseline.
The candidate was tested in CI; these results do not describe a new production deployment.

## Changes

- Known-length request bodies now use `HttpRequest.BodyReader` to fill one exact
  UTF-8 array. Admission reserves the declared length before reading. Allocation
  waits until the first chunk is available. The reader consumes and advances each
  pipe buffer, checks both overflow and premature completion, and returns the body
  only after the request ends. Unknown-length and decompression limits are preserved.
- Prompt preparation compiles `IdentitySettings` once per preparation pass and
  reuses the immutable rules for message blocks and force-prompt comparison.
  Rule order, matching and code protection are unchanged.
- Real loopback HTTP tests cover fixed-length and chunked requests in all three
  protocols, plus truncated and disconnected uploads. Interrupted uploads must
  release admission and memory, make no upstream call and create no charge.

The local 4 MiB fixed-length read check allocates 12,583,688 bytes before this
change and 4,195,200 bytes after it (66.7% less). This is the body-reading stage,
not total request allocation or resident memory. The existing 1 MiB conversion
allocation check remains approximately 3.29 MB.

## Complete-gateway Linux comparison

Both binaries run against the same synthetic HTTPS upstream in an Alpine 3.21
container without external networking. The runner pins the container to one CPU;
each gateway runs alone, with four synthetic accounts, a synthetic legacy key,
and a declared 512 MiB GC budget. The fixture uses TCP_NODELAY and cached identical
SSE bytes. Each fresh gateway gets 24 small warmup requests and four case warmups.
Modes rotate order. Readiness, actual output length, terminal events and the exact
upstream call count must all match. No real model calls are made.

The initial matrix has seven cases, three modes and three repeats: 63 rounds,
1,008 timed requests. A separate recheck uses seven repeats for three large-context
cases: another 63 rounds and 1,008 timed requests. All 2,016 timed requests passed.
Runner hardware differs from the production machine; use only within-run comparisons.

Recheck results below show Go / deployed AOT / candidate AOT. TTFT is the median
time to first text; throughput is the median round throughput. RSS columns report
the median of per-process peaks and the highest observed peak separately.

| Case | TTFT (ms) | Requests/s | Median peak RSS (MiB) | Highest peak RSS (MiB) |
|---|---:|---:|---:|---:|
| Messages 1 MiB, concurrency 4 | 64.705 / 95.239 / 90.653 | 52.54 / 38.12 / 39.62 | 33.41 / 107.94 / 82.70 | 36.73 / 135.00 / 92.14 |
| Messages 4 MiB, concurrency 1 | 58.797 / 81.380 / 79.814 | 15.99 / 11.75 / 12.16 | 50.39 / 144.02 / 127.12 | 56.49 / 175.49 / 194.75 |
| Responses 1 MiB, concurrency 4 | 59.183 / 95.433 / 93.914 | 56.34 / 37.61 / 38.88 | 33.11 / 177.53 / 152.75 | 38.42 / 221.90 / 161.81 |

Messages 1 MiB concurrency 4 improves TTFT by 4.8% and median peak RSS by 23.4%.
The 4 MiB TTFT gain is only 1.9%; its worst RSS rises by 11.0%, despite a lower
median peak. Do not claim uniformly lower memory or a major throughput improvement.
Go remains faster and uses less memory for these large-context fixtures.
In the initial small-request/long-SSE case, candidate throughput is slightly lower
than deployed AOT (38.24 versus 39.15 requests/s); it was not separately rechecked.

## Evidence and reproduction

- Initial matrix: [Actions run 42338101071](https://github.com/GMagisk9527/workbuddy-gateway/actions/runs/42338101071), benchmark source `806e3399912945bf97581e7539a0436c39218c3e`.
- Recheck: [Actions run 42338493781](https://github.com/GMagisk9527/workbuddy-gateway/actions/runs/42338493781), source `307a6febc6056fb9123ab2168c8e50cf083f2baa`.
- Both runs include strict full-gateway Native AOT compilation, native smoke tests
  and Linux JIT regressions. The benchmark artifact contains `results.jsonl`,
  `metadata.json` and `summary.json`. The recheck metadata identifies each binary
  by SHA-256. These commits are on `codex/aot-go-bench-b28ac2a`; their production
  changes are identical to this PR's candidate, with additional framing tests here.
- The standard `csharp-native-aot.yml` workflow validates the final PR revision and
  produces a fresh native deployment artifact. It does not deploy it.

Microsoft documents the PipeReader lifetime and `AdvanceTo` requirements in
[System.IO.Pipelines](https://learn.microsoft.com/dotnet/standard/io/pipelines),
and warns about repeated large-object allocations in
[ASP.NET Core performance guidance](https://learn.microsoft.com/aspnet/core/fundamentals/best-practices).
The current change addresses a measured extra body copy. Remaining candidates are
the repeated DOM transformations in large-context protocol conversion; those need
separate differential tests and measurements before changing the representation.
