using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Net;
using System.Net.Sockets;

namespace StreamingMigration;

internal static class SelfTests
{
    const string Text = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n";
    const string Finish = "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3}}\n\n";
    static void Check(bool ok, string name) { if (!ok) throw new InvalidOperationException("Assertion failed: " + name); }
    public static async Task RunAsync()
    {
        var passed = new List<string>();
        async Task Run(string name, Func<Task> action) { await action(); passed.Add(name); }
        await Run("borrowed Messages frames remain intact through delayed write and flush",async()=>
        {
            using var output=new DelayedOutput();int settlements=0;
            var result=await StreamPump.RunAsync(new MemoryStream(Encoding.UTF8.GetBytes(Text+Text+Finish)),output,new(),beforeTerminal:_=>
            {settlements++;Check(!Encoding.UTF8.GetString(output.ToArray()).Contains("message_stop",StringComparison.Ordinal),"settlement before success");});
            string wire=Encoding.UTF8.GetString(output.ToArray());
            Check(result.Outcome==StreamOutcome.Complete&&settlements==1&&output.Writes==output.Flushes,"await each complete frame");
            var deltas=wire.Split('\n').Where(l=>l.StartsWith("data: {",StringComparison.Ordinal)).Select(l=>JsonDocument.Parse(l[6..])).ToArray();
            try
            {
                Check(string.Concat(deltas.Where(d=>d.RootElement.GetProperty("type").GetString()=="content_block_delta").Select(d=>d.RootElement.GetProperty("delta").GetProperty("text").GetString()))=="hellohello","ordered deltas");
                Check(deltas[^1].RootElement.GetProperty("type").GetString()=="message_stop","terminal marker");
            }
            finally{foreach(var doc in deltas)doc.Dispose();}
        });
        await Run("Messages writer reuses small frames and discards large scratch",()=>
        {
            var writer=new EventBufferWriter();var delta=new TextDelta(0,new string('x',1024));
            writer.Write(delta);long before=GC.GetAllocatedBytesForCurrentThread();
            for(int i=0;i<1024;i++)Check(writer.Write(delta).Length>1024,"complete borrowed frame");
            Check(GC.GetAllocatedBytesForCurrentThread()-before<4096,"No per-frame byte-array allocation");
            writer.Write(new TextDelta(0,new string('x',128<<10)));
            var small=writer.Write(delta);
            Check(System.Runtime.InteropServices.MemoryMarshal.TryGetArray(small,out var segment)&&segment.Array!.Length<=64<<10,"Large scratch is not retained for subsequent frames");
            var owned=new EventWriter();byte[] first=owned.Write(delta);owned.Write(new MessageStop());
            Check(first.AsSpan().SequenceEqual(writer.Write(delta).Span),"Owned-array facade remains stable");
            return Task.CompletedTask;
        });
        await Run("Messages errors remain writable when shared scratch is exhausted",async()=>
        {
            using var output=new MemoryStream();
            var result=await StreamPump.RunAsync(new MemoryStream(Encoding.UTF8.GetBytes(Text+Finish)),output,new(Budget:new DenyScratch()));
            string wire=Encoding.UTF8.GetString(output.ToArray());
            Check(result.Code=="output_budget_exceeded"&&wire.StartsWith("event: error\n",StringComparison.Ordinal)&&!wire.Contains("message_stop",StringComparison.Ordinal),"Budget error remains visible");
        });
        await Run("UTF-8 payloads preserve legacy framing and Unicode trimming at every byte boundary", async () =>
        {
            const string wire = "\ufeff: heartbeat\r\nid: 1\r\nevent: message\r\ndata: data: \u2003{\"text\":\"中文 😀\",\u00a0\r\ndata\r\ndata: \"n\":1}\u3000\r\n\r\n  {\"bare\":true} \n\ndata: [DONE]\n\ndata: ignored\n\n";
            foreach (int step in new[] { 1, 2, 7, 8192 })
            {
                using var source = new FragmentedInput(Encoding.UTF8.GetBytes(wire), step);
                var reader = new SseReader(source);
                var first = await reader.NextPayloadUtf8Async(default);
                Check(first is not null && Encoding.UTF8.GetString(first.Value.Span) == "{\"text\":\"中文 😀\",\n\n\"n\":1}", "UTF-8 multiline framing");
                Check(await reader.NextPayloadAsync(default) == "{\"bare\":true}", "mixed byte and string consumption");
                Check(await reader.NextPayloadUtf8Async(default) is null && await reader.NextAsync(default) is null, "DONE is terminal");
            }
        });
        await Run("UTF-8 reader keeps malformed input replacement and owns parsed values", async () =>
        {
            byte[] bytes = [.. "data: {\"choices\":[{\"delta\":{\"content\":\""u8.ToArray(), 0xff, 0xe4, 0xb8,
                .. "\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"next\"}}]}\n\n"u8.ToArray()];
            foreach (int step in new[] { 1, 8192 })
            {
                using var source = new FragmentedInput(bytes, step); var reader = new SseReader(source);
                var first = await reader.NextAsync(default); var next = await reader.NextAsync(default);
                Check(first!.Choices.Single().Text == "\ufffd\ufffd" && next!.Choices.Single().Text == "next", "replacement matches UTF-8 decoder and prior chunk survives buffer reuse");
            }
        });
        await Run("UTF-8 reader enforces metadata limits and rejects partial events", async () =>
        {
            foreach (string wire in new[] { "data: {}", "data: {}\n", "data: {}\r", "data: {}\n\rdata: {}" })
            {
                using var source = new FragmentedInput(Encoding.UTF8.GetBytes(wire), 1); var reader = new SseReader(source);
                try { while (await reader.NextPayloadUtf8Async(default) is not null) { } throw new InvalidOperationException("partial event accepted"); }
                catch (ConversionFailure error) when (error.Code == "incomplete_event") { }
            }
            const string metadata = "event: long-metadata\r\ndata: {}\r\n\r\n";
            using var bounded = new FragmentedInput(Encoding.UTF8.GetBytes(metadata), 1);
            try { await new SseReader(bounded, Encoding.UTF8.GetByteCount(metadata) - 1).NextPayloadUtf8Async(default); throw new InvalidOperationException("metadata bypassed limit"); }
            catch (ConversionFailure error) when (error.Code == "event_limit") { }
        });
        await Run("UTF-8 framing allocation stays bounded across many payloads", () =>
        {
            string text = new('x', 1024); byte[] bytes = Encoding.UTF8.GetBytes(string.Concat(Enumerable.Repeat("data: {\"text\":\"" + text + "\"}\n\n", 512)));
            using var warm = new MemoryStream(bytes); new SseReader(warm).NextPayloadUtf8Async(default).GetAwaiter().GetResult();
            long before = GC.GetAllocatedBytesForCurrentThread();
            using var source = new MemoryStream(bytes); var reader = new SseReader(source); int count = 0;
            while (reader.NextPayloadUtf8Async(default).GetAwaiter().GetResult() is { } payload) { Check(payload.Length == 1035, "exact payload length"); count++; }
            long allocated = GC.GetAllocatedBytesForCurrentThread() - before;
            Check(count == 512 && allocated < 256 << 10, "framing half a MiB must allocate less than 256 KiB");
            return Task.CompletedTask;
        });
        await Run("SSE framing preserves split CRLF, UTF-8 and exact event limits", async () =>
        {
            const string wire = "\ufeff: comment\r\nid: 1\r\ndata: {\"text\":\"中文\",\r\ndata: \"n\":1}\r\n\r\ndata: [DONE]\r\n\r\n";
            foreach (int step in new[] { 1, 7, 8192 })
            {
                using var source = new FragmentedInput(Encoding.UTF8.GetBytes(wire), step);
                var reader = new SseReader(source);
                Check(await reader.NextPayloadAsync(default) == "{\"text\":\"中文\",\n\"n\":1}", "fragmented multiline event");
                Check(await reader.NextPayloadAsync(default) is null, "terminal marker");
            }
            byte[] bounded = "data: value\n\n"u8.ToArray();
            using var valid = new MemoryStream(bounded);
            Check(await new SseReader(valid, bounded.Length + 1).NextPayloadAsync(default) == "value", "event just under limit");
            using var invalid = new MemoryStream(bounded);
            try
            {
                await new SseReader(invalid, bounded.Length).NextPayloadAsync(default);
                throw new InvalidOperationException("Event at limit must be rejected");
            }
            catch (ConversionFailure error) when (error.Code == "event_limit") { }
        });
        await Run("cancel blocked read and close upstream", async () =>
        {
            var source = new BlockingInput(); using var output = new MemoryStream(); using var cts = new CancellationTokenSource();
            var task = StreamPump.RunAsync(source, output, new(), cts.Token);
            await source.Entered.Task.WaitAsync(TimeSpan.FromSeconds(5)); cts.Cancel();
            var r = await task.WaitAsync(TimeSpan.FromSeconds(5));
            Check(r.Outcome == StreamOutcome.Cancelled && source.Closed && output.Length == 0, "cancel blocked");
        });
        await Run("write failure stops reading and closes upstream", async () =>
        {
            var source = new FragmentedInput(Encoding.UTF8.GetBytes(Text + Finish), 1);
            using var output = new BrokenOutput(false);
            var r = await StreamPump.RunAsync(source, output, new());
            Check(r.Outcome == StreamOutcome.DownstreamFailed && source.Closed && source.Reads < Encoding.UTF8.GetByteCount(Text + Finish), "write failure");
        });
        await Run("flush failure is downstream failure", async () =>
        {
            var source = new FragmentedInput(Encoding.UTF8.GetBytes(Text + Finish), 1); using var output = new BrokenOutput(true);
            var r = await StreamPump.RunAsync(source, output, new());
            Check(r.Outcome == StreamOutcome.DownstreamFailed && source.Closed, "flush failure");
        });
        await Run("output arrives before upstream EOF", async () =>
        {
            var source = new BlockingInput(Encoding.UTF8.GetBytes(Text)); using var output = new SignallingOutput(); using var cts = new CancellationTokenSource();
            var task = StreamPump.RunAsync(source, output, new(), cts.Token);
            await output.Written.Task.WaitAsync(TimeSpan.FromSeconds(5));
            Check(!task.IsCompleted, "did not aggregate entire response"); cts.Cancel();
            var r = await task.WaitAsync(TimeSpan.FromSeconds(5));
            Check(r.Outcome == StreamOutcome.Cancelled && source.Closed && !Encoding.UTF8.GetString(output.ToArray()).Contains("message_stop", StringComparison.Ordinal), "no fake finish on cancel");
        });
        await Run("read failure after finish is not success; measured usage survives", async () =>
        {
            byte[] bytes = Encoding.UTF8.GetBytes(Text + Finish);
            var source = new FragmentedInput(bytes, 3, bytes.Length); using var output = new MemoryStream();
            var r = await StreamPump.RunAsync(source, output, new()); string sse = Encoding.UTF8.GetString(output.ToArray());
            Check(r.Outcome == StreamOutcome.Failed && r.Code == "read_error" && r.Usage.Output == 3 && source.Closed && !sse.Contains("message_stop", StringComparison.Ordinal), "truncated finish");
        });
        await Run("retained output budget fails closed", async () =>
        {
            var source = new FragmentedInput(Encoding.UTF8.GetBytes(Text + Finish)); using var output = new MemoryStream();
            var r = await StreamPump.RunAsync(source, output, new(RetainedLimit: 511));
            Check(r.Code == "output_budget_exceeded" && source.Closed && !Encoding.UTF8.GetString(output.ToArray()).Contains("message_stop", StringComparison.Ordinal), "budget");
        });
        await Run("terminal state cannot finish or feed twice", () =>
        {
            var state = new MessageStreamState("model", false); state.Feed(ChunkReader.Parse("{\"choices\":[{\"finish_reason\":\"stop\"}]}")); state.Finish();
            int caught = 0;
            try { state.Finish(); } catch (InvalidOperationException) { caught++; }
            try { state.Feed(ChunkReader.Parse("{}")); } catch (InvalidOperationException) { caught++; }
            Check(caught == 2, "terminal state"); return Task.CompletedTask;
        });
        await Run("opaque reasoning authenticates model, key and ciphertext", () =>
        {
            var codec = new ReasoningCodec(Program.TestKey, "model"); string token = codec.Seal("中文 reasoning");
            Check(codec.Open(token) == "中文 reasoning", "roundtrip"); int rejected = 0;
            try { new ReasoningCodec(Program.TestKey, "other").Open(token); } catch (CryptographicException) { rejected++; }
            try { new ReasoningCodec(new byte[32], "model").Open(token); } catch (CryptographicException) { rejected++; }
            string changed = token[..10] + (token[10] == 'a' ? 'b' : 'a') + token[11..];
            try { codec.Open(changed); } catch (CryptographicException) { rejected++; }
            Check(rejected == 3, "authenticated envelope"); return Task.CompletedTask;
        });
        await Run("hidden reasoning never emits plaintext", async () =>
        {
            string s = "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"PRIVATE_REASONING\"}}]}\n\n" + Finish;
            using var output = new MemoryStream(); var source = new FragmentedInput(Encoding.UTF8.GetBytes(s), 1);
            var r = await StreamPump.RunAsync(source, output, new(WantThinking: true, Hidden: new ReasoningCodec(Program.TestKey, "test-model")));
            string wire = Encoding.UTF8.GetString(output.ToArray());
            Check(r.Outcome == StreamOutcome.Complete && wire.Contains("wbg-r1.", StringComparison.Ordinal) && !wire.Contains("PRIVATE_REASONING", StringComparison.Ordinal), "hidden");
        });
        await Run("large normal text is not retained like an aggregate", async () =>
        {
            string chunk = "data: " + JsonSerializer.Serialize(new { choices = new[] { new { delta = new { content = new string('x', 10000) } } } }) + "\n\n";
            using var output = new MemoryStream(); var source = new FragmentedInput(Encoding.UTF8.GetBytes(string.Concat(Enumerable.Repeat(chunk, 100)) + Finish));
            var r = await StreamPump.RunAsync(source, output, new(RetainedLimit: 512));
            Check(r.Outcome == StreamOutcome.Complete, "one MB text with one 512-byte slot charge");
        });
        await Run("raw usage evidence survives a later conversion failure", async () =>
        {
            string wire = "data: {\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"credits\":0.125,\"custom\":{\"source\":\"measured\"}},\"choices\":[]}\n\ndata: {oops}\n\n";
            using var output = new MemoryStream(); var source = new FragmentedInput(Encoding.UTF8.GetBytes(wire));
            var r = await StreamPump.RunAsync(source, output, new());
            using var evidence = JsonDocument.Parse(r.RawUsageJson ?? "null");
            Check(r.Outcome == StreamOutcome.Failed && r.Usage.Output == 3 && evidence.RootElement.GetProperty("credits").GetDecimal() == .125m &&
                evidence.RootElement.GetProperty("custom").GetProperty("source").GetString() == "measured", "raw usage evidence");
        });
        await Run("real HTTP chunked response streams before completion", async () =>
        {
            using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(10));
            var listener = new TcpListener(IPAddress.Loopback, 0); listener.Start();
            try
            {
                var releaseFinish = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
                var server = Serve(listener, async stream =>
                {
                    await Write(stream, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n", timeout.Token);
                    await Chunk(stream, Text, timeout.Token);
                    await releaseFinish.Task.WaitAsync(timeout.Token);
                    await Chunk(stream, Finish, timeout.Token); await Write(stream, "0\r\n\r\n", timeout.Token);
                }, timeout.Token);
                using var client = new HttpClient(new SocketsHttpHandler { UseProxy = false });
                using var response = await client.GetAsync(Address(listener), HttpCompletionOption.ResponseHeadersRead, timeout.Token);
                using var output = new SignallingOutput();
                var pump = StreamPump.RunAsync(await response.Content.ReadAsStreamAsync(timeout.Token), output, new(), timeout.Token);
                await output.Written.Task.WaitAsync(timeout.Token); Check(!pump.IsCompleted, "HTTP streaming before finish"); releaseFinish.SetResult();
                var r = await pump; await server;
                Check(r.Outcome == StreamOutcome.Complete && Encoding.UTF8.GetString(output.ToArray()).Contains("message_stop", StringComparison.Ordinal), "HTTP completed");
            }
            finally { listener.Stop(); }
        });
        await Run("real HTTP truncated content-length fails after finish", async () =>
        {
            using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(10));
            var listener = new TcpListener(IPAddress.Loopback, 0); listener.Start();
            try
            {
                var server = Serve(listener, stream => Write(stream,
                    $"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: {Encoding.UTF8.GetByteCount(Text + Finish) + 100}\r\nConnection: close\r\n\r\n" + Text + Finish, timeout.Token), timeout.Token);
                using var client = new HttpClient(new SocketsHttpHandler { UseProxy = false });
                using var response = await client.GetAsync(Address(listener), HttpCompletionOption.ResponseHeadersRead, timeout.Token);
                using var output = new MemoryStream();
                var r = await StreamPump.RunAsync(await response.Content.ReadAsStreamAsync(timeout.Token), output, new(), timeout.Token); await server;
                Check(r.Outcome == StreamOutcome.Failed && r.Code == "read_error" && r.Usage.Output == 3 && !Encoding.UTF8.GetString(output.ToArray()).Contains("message_stop", StringComparison.Ordinal), "HTTP truncation");
            }
            finally { listener.Stop(); }
        });
        Console.WriteLine(JsonSerializer.Serialize(new { passed = passed.Count, tests = passed }));
    }
    static string Address(TcpListener listener) => $"http://127.0.0.1:{((IPEndPoint)listener.LocalEndpoint).Port}/synthetic";
    static async Task Serve(TcpListener listener, Func<NetworkStream, Task> respond, CancellationToken cancellation)
    {
        using var connection = await listener.AcceptTcpClientAsync(cancellation);
        await using var stream = connection.GetStream(); using var reader = new StreamReader(stream, leaveOpen: true);
        while (await reader.ReadLineAsync(cancellation) is { Length: > 0 }) { }
        await respond(stream);
    }
    static async Task Write(NetworkStream stream, string value, CancellationToken cancellation)
    { await stream.WriteAsync(Encoding.UTF8.GetBytes(value), cancellation); await stream.FlushAsync(cancellation); }
    static Task Chunk(NetworkStream stream, string value, CancellationToken cancellation) =>
        Write(stream, Encoding.UTF8.GetByteCount(value).ToString("x", System.Globalization.CultureInfo.InvariantCulture) + "\r\n" + value + "\r\n", cancellation);
    sealed class BrokenOutput(bool flushFailure) : MemoryStream
    {
        public override ValueTask WriteAsync(ReadOnlyMemory<byte> buffer, CancellationToken cancellationToken = default)
        { if (!flushFailure) throw new IOException("Injected write failure"); return base.WriteAsync(buffer, cancellationToken); }
        public override Task FlushAsync(CancellationToken cancellationToken) => throw new IOException("Injected flush failure");
    }
    sealed class DenyScratch : IRetainedOutputBudget
    {
        public void Add(long bytes){}public void Remove(long bytes){}
        public void Scratch(long bytes)=>throw new ConversionFailure("output_budget_exceeded");
    }
    sealed class DelayedOutput : MemoryStream
    {
        ReadOnlyMemory<byte> borrowed;byte[] snapshot=[];
        internal int Writes,Flushes;
        public override async ValueTask WriteAsync(ReadOnlyMemory<byte> buffer,CancellationToken cancellationToken=default)
        {
            borrowed=buffer;snapshot=buffer.ToArray();await Task.Yield();
            Check(buffer.Span.SequenceEqual(snapshot),"Buffer intact until write completes");
            await base.WriteAsync(buffer,cancellationToken);Writes++;
        }
        public override async Task FlushAsync(CancellationToken cancellationToken)
        {await Task.Yield();Check(borrowed.Span.SequenceEqual(snapshot),"Buffer intact until flush completes");Flushes++;}
    }
    sealed class SignallingOutput : MemoryStream
    {
        public TaskCompletionSource Written { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);
        public override ValueTask WriteAsync(ReadOnlyMemory<byte> buffer, CancellationToken cancellationToken = default)
        { var r = base.WriteAsync(buffer, cancellationToken); Written.TrySetResult(); return r; }
    }
    sealed class BlockingInput(byte[]? prefix = null) : Stream
    {
        bool sent;
        public bool Closed { get; private set; }
        public TaskCompletionSource Entered { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);
        public override async ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default)
        {
            if (!sent && prefix is not null) { sent = true; prefix.CopyTo(buffer); return prefix.Length; }
            Entered.TrySetResult(); await Task.Delay(Timeout.InfiniteTimeSpan, cancellationToken); return 0;
        }
        protected override void Dispose(bool disposing) { Closed = true; base.Dispose(disposing); }
        public override bool CanRead => true; public override bool CanSeek => false; public override bool CanWrite => false;
        public override long Length => throw new NotSupportedException(); public override long Position { get => throw new NotSupportedException(); set => throw new NotSupportedException(); }
        public override int Read(byte[] buffer, int offset, int count) => throw new NotSupportedException(); public override void Flush() { }
        public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException(); public override void SetLength(long value) => throw new NotSupportedException();
        public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
    }
}
