# Fragmented tool arguments, 2026-11-13

`ChatState.Feed` previously converted the entire accumulated tool argument
`StringBuilder` into a new string for every fragment, including Chat and
non-streaming requests. With fixed-size fragments, cumulative copying grew
quadratically. Responses only needed that prefix once, when an identity first
became usable.

Each change now captures the accumulated character count. Responses materializes
that exact prefix when it opens the tool, and uses individual fragments thereafter.
Capturing the count also preserves the boundary when the same tool appears more
than once in one upstream event. The complete arguments remain available for
final output and aggregation. Admission and accounting behavior are unchanged.

## Component measurement

Baseline: `bd9ce5ad6da157d01ec0bb26d55fc85e04ce58fb`. Candidate: this change.
Windows x64, .NET SDK 11.0.100/runtime 11.0.0, Release JIT. The same synchronous
Feed harness used a single synthetic write_file call, valid JSON arguments and
256-byte ASCII fragments. Inputs were created before measurement, each case was
warmed up, and each size was measured three times. The candidate suite ran before
the baseline suite; these are component observations, not a randomized load test.
Final argument JSON and content length were checked outside the measured scope.

| Argument content | Cumulative allocation, baseline / candidate | Median Feed time, baseline / candidate |
|---|---:|---:|
| 64 KiB | 18.67 / 2.47 MiB | 7.642 / 2.827 ms |
| 128 KiB | 69.31 / 4.91 MiB | 23.809 / 5.635 ms |
| 256 KiB | 266.77 / 9.78 MiB | 44.204 / 11.688 ms |
| 512 KiB | 1045.46 / 19.53 MiB | 190.504 / 22.327 ms |

The 512 KiB case reduced cumulative managed allocation by about 98.1%.
Cumulative allocation is not peak RSS. These numbers do not measure the full
HTTP/SSE pipeline, production Linux Native AOT or actual model latency.

## Regression coverage

The OpenAI self-test includes a 512 KiB/256-byte-fragment allocation guard with a
deliberately broad 64 MiB ceiling and complete output verification. Replacing the
candidate protocol assembly with the baseline assembly makes that test fail at
the intended allocation assertion. Time is never asserted.

Offline pump and loopback HTTP tests cover late ID/name arrival, interleaved
tools, repeated same-tool entries in one event, escaped Unicode/JSON, namespace
custom tools, streaming and aggregate output, cancellation and output limits.
HTTP tests check all three protocols, one upstream invocation and one measured
WAL settlement per request.

To run those tests with the repository SDK and native zstd dependency:

```sh
dotnet build openai-tests/OpenAiChecks.csproj -c Release --configfile NuGet.Config -p:GatewayLibraryBuild=true -p:NuGetAudit=false -o out/tool-arguments
dotnet out/tool-arguments/OpenAiChecks.dll --self-test
```

The returned stream test list includes the actual cumulative allocation. The
existing `ci/verify.sh` invokes this self-test in Linux CI. No real model request
or production configuration change is needed.
