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
