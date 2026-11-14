# Outbound SSE and telemetry lock checks

Baseline: `d46454e`. SDK/runtime: 11.0.100 / 11.0.0. Local measurements
use Windows Release JIT with `DOTNET_TieredCompilation=0`, two warmups and
five measured iterations. Input is 512 KiB ASCII in 2,048 upstream deltas;
the output counts bytes/writes/flushes without retaining response bodies.
Numbers are medians of process-wide managed allocations, not RSS or production
latency. There are no model calls or production data in these checks.

| Path | Baseline allocated bytes | Updated allocated bytes | Reduction | Baseline ms | Updated ms |
|---|---:|---:|---:|---:|---:|
| Chat stream | 26,642,128 | 15,075,000 | 43.42% | 11.616 | 11.181 |
| Responses stream | 31,644,120 | 16,990,968 | 46.31% | 11.894 | 9.501 |
| Messages stream | 3,538,480 | 2,718,352 | 23.18% | 4.908 | 5.081 |
| Chat aggregate (control) | 16,076,568 | 16,076,632 | ~0% | 9.392 | 9.575 |
| Responses aggregate (control) | 8,043,016 | 8,043,080 | ~0% | 6.067 | 6.081 |

This is evidence of fewer allocations, not a guarantee that all requests become
faster. Messages timings slightly increased in this short sample. Output byte,
write and flush counts match the baseline for each path. Deferred terminal frames
still have owned storage and wait for upstream validation and ledger settlement.
Every visible frame still flushes immediately, using the heartbeat-aware Stream.
Buffers over 64 KiB are discarded on the next frame; no shared pool retains them.
Scratch growth is admitted before allocating, including old/new buffer overlap.

The benchmark can link either revision's separately built assemblies:

```sh
dotnet build bench-outbound/BenchOutbound.csproj -c Release --configfile NuGet.Config \
  -p:GatewayAssemblies=/absolute/path/to/gateway-assemblies -o out/outbound-bench
DOTNET_TieredCompilation=0 dotnet out/outbound-bench/BenchOutbound.dll
```

`ci/verify.sh` also builds and runs this synthetic benchmark on Linux. Its JSON
is retained with the verification artifacts; timings are not CI thresholds.

Offline validation includes 1,400 Chat/Responses and 350 Messages differential
cases versus the baseline (generated IDs/times normalized), 2,508 byte-level
event variants, delayed downstream writes/flushes, retained terminal events,
partial-frame rejection and exhausted-budget errors. Integration tests retain
the existing exactly-once WAL and failed-tail settlement checks.

Request-log append, tail and cleanup use a separate ordering gate. They remain
synchronous and serialized: this change removes their I/O from the statistics
lock, without an asynchronous queue or dropped-log policy. Deterministic stalled
I/O tests verify rows, traces, account summaries and history flushes remain
available, with concurrent in-memory/disk log order preserved. Log retention,
rotation and the billing WAL format/durability policy are unchanged.

## Stream state, Responses deltas and output admission (2026-11-14)

This iteration uses main `79acdc42cc67296897077bdf6c9951d978081b83` as its
baseline, with the same benchmark, SDK/runtime, warmups and five-round median
method described above. No production configuration or model requests are used.

| Path | Baseline allocated bytes | Updated allocated bytes | Reduction | Baseline ms | Updated ms |
|---|---:|---:|---:|---:|---:|
| Chat stream | 15,075,000 | 14,013,536 | 7.04% | 10.199 | 10.468 |
| Responses stream | 16,990,968 | 14,861,544 | 12.53% | 9.251 | 7.995 |
| Messages stream (control) | 2,718,352 | 2,718,352 | 0% | 4.767 | 4.665 |
| Chat aggregate (control) | 16,076,632 | 16,076,632 | 0% | 9.004 | 10.959 |
| Responses aggregate (control) | 8,043,080 | 8,043,120 | ~0% | 6.226 | 5.990 |

Allocation reductions are repeatable; this short timing sample is noisy,
including the unchanged aggregate control. It does not establish production
latency or AOT throughput gains. Output bytes, write counts and flush counts
match the baseline for every path. The benchmark does not use the HTTP memory
admission lease, so its timings do not measure that lock optimization.

Streamed Chat keeps cumulative logical output accounting but no longer retains
content/reasoning/refusal builders or reserves shared retained-memory budget for
already forwarded text. Tools, usage evidence, restoration buffers, frame scratch
and held terminal frames remain accounted for. Aggregated Chat and all Responses
paths still accumulate the text needed for their final bodies/events. A
streaming-only ChatState explicitly refuses aggregate completion.

Responses text/refusal/reasoning/function-argument deltas use value events and
Utf8JsonWriter with cached property names. Complex lifecycle events keep the
existing DOM path. A request-local event list is borrowed only until the next
Feed; the pump awaits each write and flush before reusing it. Deferred success
events still own their bytes and wait for validation and settlement.

Output admission keeps local used/scratch accounting under a lease lock. It only
takes the shared lock and samples memory when reserving another block (up to
64 KiB), or when releasing a lease. An offline regression verifies 4,096 one-byte
appends use one sample/refill instead of 4,096. Existing reserved blocks and pinned
request limits survive a hot shrink; refills and new requests consult current
shared limits. Concurrent refills, request-limit failures, scratch/remove and
idempotent cancellation disposal verify exact rejection counts and no leaks.
The shared budget remains authoritative for every reservation.

Regressions cover byte-exact typed-event field order/escaping, contiguous event
sequences, delayed downstream writes, split-prefix response restoration, late
tool identities, custom tools, measured usage, failed tails and settlement
barriers. Emitting 64 Chat text chunks reserves the same memory as one chunk of
the same size, while cumulative logical overflow still fails. Differential
checks against the baseline compare 1,400 Chat/Responses results (successful
SSE also compared as raw text after normalizing generated IDs/times) and 350
Messages results. Existing HTTP output-budget/WAL checks continue to exercise all
three protocols and both streaming and aggregate modes without real model calls.
