# UTF-8 SSE and Responses state, 2026-11-13

The SSE reader previously decoded every line into UTF-16, joined data lines into
a string, and handed it to consumers that encoded or parsed it again. It now
frames data in reusable, bounded byte buffers. `NextPayloadUtf8Async` returns
borrowed memory valid until the next reader call. Both protocol pumps parse it
before advancing the reader; retained strings and usage evidence own their data.
The string-returning reader remains available for existing callers.

`ChatState.FeedResponses` accumulates state directly from the parsed document,
without cloning every scalar into a full `JsonObject`. Chat still materializes
that object to preserve unknown upstream fields. Active response identity
restoration retains its existing DOM transformation and reparses the restored
output before accumulation. Usage alone is materialized as an owned, deduplicated
object so the complete accounting evidence survives input-buffer reuse.

Framing preserves existing BOM handling, CR/LF/split CRLF, Unicode whitespace,
multiline data, repeated data prefixes, bare JSON, malformed UTF-8 replacement,
strict event bounds, incomplete-event rejection, `[DONE]`, and read errors.
No output batching or delayed flushing was introduced. Scheduling, affinity,
admission, cancellation, terminal settlement barriers and WAL durability are
unchanged.

## Synthetic component observations

Baseline: `529baa3` (main including PR #59). Windows x64, .NET SDK 11.0.100,
runtime 11.0.0, Release JIT, `DOTNET_TieredCompilation=0` to avoid tier promotion
during these short cases. Each input contained 512 KiB of ASCII text in 2,048
256-byte fragments. Inputs were built before measurement; each case had two
warmups and five measured rounds. The table reports medians. Baseline ran before
candidate; this is not a randomized load test.

Framing measured reading all payloads; state measured feeding all fragments.
Pump cases included framing, JSON parsing, protocol conversion and serialization
to `Stream.Null` or the aggregate response body, with a synthetic finish/usage
event. Requests were prepared before measurement. Input consumption, output
length in the state case, and successful pump completion were checked.

| Case | Cumulative allocation, baseline / candidate | Time, baseline / candidate |
|---|---:|---:|
| SSE framing | 3.635 / 0.010 MiB | 0.335 / 0.213 ms |
| Responses state | 12.655 / 4.639 MiB | 8.071 / 5.601 ms |
| Responses aggregate pump | 19.940 / 7.670 MiB | 9.113 / 6.076 ms |
| Responses streaming pump | 42.448 / 30.178 MiB | 15.510 / 11.626 ms |

Aggregate and streaming pump allocation fell about 61.5% and 28.9%, respectively.
Cumulative managed allocation is not peak RSS. These measurements exclude real
HTTP, proxy buffering, production concurrency, upstream generation latency and
Linux Native AOT. They must not be interpreted as production latency guarantees.

## Regression evidence

- Streaming self-tests cover byte-by-byte UTF-8/BOM/CRLF, Unicode trimming,
  multiline/empty data, repeated prefixes, mixed reader APIs, invalid UTF-8,
  metadata-inclusive bounds, partial events, cancellation and TCP failures.
  A 512-event, roughly half-MiB framing case must allocate below 256 KiB.
- OpenAI tests verify owned values after overwriting the input buffer, last-key
  wins, metadata, tool association, raw usage, identity restoration across chunks
  and channel changes, and Chat unknown-field forwarding. A fragmented 512 KiB
  Responses state case must allocate below 8 MiB. Time is never asserted.
- Loopback HTTP tests stream UTF-8 one byte at a time for all three protocols
  and both response modes. Success and late 11128 cases verify exact content,
  truthful failure, twelve upstream sends and twelve measured WAL settlements.
- A deterministic baseline comparison used 1,400 Chat/Responses pump cases and
  350 Messages streaming cases, including optional-field type mismatches,
  duplicate keys, tools, reasoning/refusal, line delimiters, errors and partial
  tails. Outputs matched after normalizing generated IDs and timestamps.

Run the permanent tests from `csharp/`:

```sh
dotnet build streaming/StreamingMigration.csproj -c Release --configfile NuGet.Config -p:NuGetAudit=false -o out/utf8-streaming
dotnet out/utf8-streaming/StreamingMigration.dll --self-test
dotnet build openai-tests/OpenAiChecks.csproj -c Release --configfile NuGet.Config -p:NuGetAudit=false -o out/utf8-openai
dotnet out/utf8-openai/OpenAiChecks.dll --self-test
```

The HTTP tests require the existing native zstd dependency. Linux CI runs these
tests through `ci/verify.sh` and checks strict Native AOT compilation separately.
No real model request or production configuration change is required.

## References

- [System.Text.Json UTF-8 reader](https://learn.microsoft.com/en-us/dotnet/standard/serialization/system-text-json/use-utf8jsonreader)
- [ASP.NET request and response pipelines](https://learn.microsoft.com/en-us/aspnet/core/fundamentals/middleware/request-response?view=aspnetcore-10.0)
