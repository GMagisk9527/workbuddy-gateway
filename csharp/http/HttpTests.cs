using System.Collections.Concurrent;
using System.Net;
using System.IO.Compression;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using StreamingMigration;

namespace HttpMigration;

internal static class HttpTests
{
    const string Text = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n";
    const string Finish = "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3,\"cached_tokens\":5}}\n\n";
    static readonly object Request = new { model = "test-model", stream = true, messages = new[] { new { role = "user", content = "hi" } } };
    static void Check(bool condition, string name) { if (!condition) throw new InvalidOperationException(name); }
    static async Task<JsonElement> Json(HttpResponseMessage response) => JsonSerializer.Deserialize<JsonElement>(await response.Content.ReadAsByteArrayAsync());
    static async Task Normal(HttpContext c) { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text + Finish); }
    public static async Task Run()
    {
        var passed = new List<string>();
        async Task Test(string name, Func<Task> test) { await test(); passed.Add(name); }
        await Test("HTTP stream request conversion and SSE completion", async () =>
        {
            await using var f = await Fixture.Create(Normal); using var r = await f.Post(Request);
            string body = await r.Content.ReadAsStringAsync(); var captured = f.Requests.Single();
            Check(r.StatusCode == HttpStatusCode.OK && r.Content.Headers.ContentType?.MediaType == "text/event-stream" && body.Contains("message_stop", StringComparison.Ordinal), "stream complete");
            Check(captured.GetProperty("stream").GetBoolean() && captured.GetProperty("messages")[0].GetProperty("content").GetString() == "hi", "upstream conversion");
            Check(r.Headers.Contains("X-Trace-ID") && r.Headers.TryGetValues("X-Accel-Buffering", out var values) && values.Single() == "no", "stream headers");
        });
        await Test("HTTP aggregate output and cache token accounting", async () =>
        {
            await using var f = await Fixture.Create(Normal); var request = JsonSerializer.SerializeToNode(Request)!; request["stream"] = false;
            using var r = await f.Post(request); var body = await Json(r);
            Check(r.StatusCode == HttpStatusCode.OK && body.GetProperty("content")[0].GetProperty("text").GetString() == "hello", "aggregate body");
            Check(body.GetProperty("usage").GetProperty("input_tokens").GetInt32() == 15 && body.GetProperty("usage").GetProperty("cache_read_input_tokens").GetInt32() == 5, "cache usage");
        });
        await Test("invalid JSON, role, messages and capability never reach upstream", async () =>
        {
            await using var f = await Fixture.Create(Normal);
            foreach (string input in new[] { "{bad", "null", "{}", "{\"messages\":[]}", "{\"messages\":[{\"role\":\"tool\",\"content\":\"x\"}]}", "{\"messages\":[{\"role\":\"user\",\"content\":\"x\"}],\"mcp_servers\":[{}]}" })
            { using var r = await f.PostRaw(input); var body = await Json(r); Check((int)r.StatusCode == 400 && body.GetProperty("type").GetString() == "error" && body.GetProperty("error").GetProperty("type").GetString() == "invalid_request_error", "bad request envelope"); }
            Check(f.Requests.IsEmpty, "zero upstream calls");
        });
        await Test("GET Messages returns Anthropic 405 without upstream", async () =>
        {
            await using var f = await Fixture.Create(Normal); using var r = await f.Client.GetAsync("/v1/messages");
            Check((int)r.StatusCode == 405 && (await Json(r)).GetProperty("type").GetString() == "error" && f.Requests.IsEmpty, "method envelope");
        });
        await Test("caller credentials and cookies are not forwarded", async () =>
        {
            bool leaked = true; await using var f = await Fixture.Create(async c => { leaked = c.Request.Headers.ContainsKey("Authorization") || c.Request.Headers.ContainsKey("x-api-key") || c.Request.Headers.ContainsKey("Cookie"); await Normal(c); });
            f.Client.DefaultRequestHeaders.Add("Authorization", "Bearer synthetic-caller"); f.Client.DefaultRequestHeaders.Add("x-api-key", "synthetic-caller"); f.Client.DefaultRequestHeaders.Add("Cookie", "synthetic=cookie");
            using var r = await f.Post(Request); Check(r.IsSuccessStatusCode && !leaked, "credential boundary");
        });
        await Test("gzip request decoding", async () =>
        {
            await using var f = await Fixture.Create(Normal); using var bytes = new MemoryStream();
            using (var gzip = new GZipStream(bytes, CompressionMode.Compress, true)) gzip.Write(JsonSerializer.SerializeToUtf8Bytes(Request));
            using var content = new ByteArrayContent(bytes.ToArray()); content.Headers.ContentEncoding.Add("gzip");
            using var r = await f.Client.PostAsync("/v1/messages", content); Check(r.IsSuccessStatusCode && f.Requests.Count == 1, "gzip success");
        });
        await Test("compressed expansion is bounded before upstream", async () =>
        {
            await using var f = await Fixture.Create(Normal, maxRequest: 512); using var bytes = new MemoryStream();
            using (var gzip = new GZipStream(bytes, CompressionMode.Compress, true)) gzip.Write(Encoding.UTF8.GetBytes(new string('x', 1024)));
            using var content = new ByteArrayContent(bytes.ToArray()); content.Headers.ContentEncoding.Add("gzip");
            using var r = await f.Client.PostAsync("/v1/messages", content); Check((int)r.StatusCode == 413 && f.Requests.IsEmpty, "gzip bound");
        });
        await Test("invalid gzip and unsupported coding fail before upstream", async () =>
        {
            await using var f = await Fixture.Create(Normal);
            foreach (var pair in new[] { ("gzip", 400), ("br", 415), ("gzip, gzip", 415) })
            { using var content = new StringContent("invalid"); content.Headers.TryAddWithoutValidation("Content-Encoding", pair.Item1); using var r = await f.Client.PostAsync("/v1/messages", content); Check((int)r.StatusCode == pair.Item2, "coding status"); }
            Check(f.Requests.IsEmpty, "coding zero calls");
        });
        await Test("declared oversized upload has Anthropic 413", async () =>
        {
            await using var f = await Fixture.Create(Normal, maxRequest: 512); using var r = await f.PostRaw(new string('x', 1024));
            Check((int)r.StatusCode == 413 && (await Json(r)).GetProperty("error").GetProperty("type").GetString() == "request_too_large" && f.Requests.IsEmpty, "body limit");
        });
        await Test("request JSON escape expansion is rejected before allocation", () =>
        {
            byte[] input = Encoding.UTF8.GetBytes("{\"messages\":[{\"role\":\"user\",\"content\":\"" + new string('<', 200) + "\"}]}");
            int rejected = 0;
            try { MessagesRequestAdapter.Prepare(input, Program.TestKey, 512); } catch (RequestFailure e) when (e.Status == 413) { rejected++; }
            Check(rejected == 1, "converted request size"); return Task.CompletedTask;
        });
        await Test("tool reference expansion shares request budget", () =>
        {
            var input = new { messages = new[] { new { role = "user", content = new[] { new { type = "tool_result", tool_use_id = "t", content = new[] { new { type = "tool_reference", tool_name = "run" } } } } } }, tools = new[] { new { name = "run", description = new string('x', 250), input_schema = new { type = "object" } } } };
            byte[] bytes = JsonSerializer.SerializeToUtf8Bytes(input); int rejected = 0;
            Check(bytes.Length < 700, "unexpanded input fits");
            try { MessagesRequestAdapter.Prepare(bytes, Program.TestKey, 700); } catch (RequestFailure e) when (e.Status == 413) { rejected++; }
            Check(rejected == 1, "reference expansion bound"); return Task.CompletedTask;
        });
        await Test("upstream 11128 and 11140 statuses and codes survive", async () =>
        {
            foreach (int code in new[] { 11128, 11140 })
            {
                await using var f = await Fixture.Create(async c => { c.Response.StatusCode = 403; await c.Response.WriteAsJsonAsync(new { code, msg = "blocked", requestId = "synthetic-private-id" }); });
                using var r = await f.Post(Request); var body = await Json(r);
                Check((int)r.StatusCode == 403 && body.GetProperty("error").GetProperty("code").GetString() == code.ToString(System.Globalization.CultureInfo.InvariantCulture), "business code");
                Check(!(await r.Content.ReadAsStringAsync()).Contains("synthetic-private-id", StringComparison.Ordinal) && f.Requests.Count == 1, "no retry or identifier leak");
            }
        });
        await Test("redirect is not followed", async () =>
        {
            await using var f = await Fixture.Create(c => { c.Response.StatusCode = 302; c.Response.Headers.Location = "/upstream"; return Task.CompletedTask; });
            using var r = await f.Post(Request); Check((int)r.StatusCode == 502 && f.Requests.Count == 1, "redirect");
        });
        await Test("oversized upstream error is a gateway failure", async () =>
        {
            await using var f = await Fixture.Create(async c => { c.Response.StatusCode = 400; await c.Response.WriteAsync(new string('x', 65537)); });
            using var r = await f.Post(Request); Check((int)r.StatusCode == 502 && (await Json(r)).GetProperty("error").GetProperty("type").GetString() == "api_error", "bounded upstream error");
        });
        await Test("wrong upstream content type fails before SSE start", async () =>
        {
            await using var f = await Fixture.Create(c => c.Response.WriteAsJsonAsync(new { choices = new object[0] }));
            using var r = await f.Post(Request); Check((int)r.StatusCode == 502 && r.Content.Headers.ContentType?.MediaType == "application/json", "bad content type");
        });
        await Test("SSE rejection after output has error and no message_stop", async () =>
        {
            await using var f = await Fixture.Create(async c => { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text + "data: {\"code\":11128}\n\n"); });
            using var r = await f.Post(Request); string body = await r.Content.ReadAsStringAsync();
            Check(body.Contains("11128", StringComparison.Ordinal) && body.Contains("event: error", StringComparison.Ordinal) && !body.Contains("message_stop", StringComparison.Ordinal) && f.Requests.Count == 1, "SSE reject");
        });
        await Test("aggregate malformed tail does not return partial success", async () =>
        {
            await using var f = await Fixture.Create(async c => { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text + Finish + "data: {oops}\n\n"); });
            var input = JsonSerializer.SerializeToNode(Request)!; input["stream"] = false;
            using var r = await f.Post(input); Check((int)r.StatusCode == 502 && (await Json(r)).GetProperty("type").GetString() == "error", "aggregate incomplete");
        });
        await Test("aggregate output budget rejects without sending a partial message", async () =>
        {
            await using var f = await Fixture.Create(Normal, maxOutput: 512); var input = JsonSerializer.SerializeToNode(Request)!; input["stream"] = false;
            using var r = await f.Post(input); Check((int)r.StatusCode == 502 && (await Json(r)).GetProperty("error").GetProperty("code").GetString() == "output_budget_exceeded", "aggregate budget");
        });
        await Test("aggregate encoding expansion stays within output budget", async () =>
        {
            string chunk = "data: " + JsonSerializer.Serialize(new { choices = new[] { new { delta = new { content = new string('\u0001', 1000) } } } }) + "\n\n";
            await using var f = await Fixture.Create(async c => { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(chunk + Finish); }, maxOutput: 2000);
            var input = JsonSerializer.SerializeToNode(Request)!; input["stream"] = false;
            using var r = await f.Post(input); Check((int)r.StatusCode == 502 && (await Json(r)).GetProperty("type").GetString() == "error", "encoded output bound");
        });
        await Test("hidden reasoning survives HTTP roundtrip and rejects another model", async () =>
        {
            await using var f = await Fixture.Create(async c => { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"PRIVATE_THOUGHT\"}}]}\n\n" + Text + Finish); });
            var input = JsonSerializer.SerializeToNode(Request)!; input["stream"] = false; input["thinking"] = new JsonObject { ["type"] = "adaptive", ["display"] = "omitted" };
            using var r = await f.Post(input); var body = await Json(r); var thought = body.GetProperty("content")[0];
            Check(thought.GetProperty("thinking").GetString() == "" && !(await r.Content.ReadAsStringAsync()).Contains("PRIVATE_THOUGHT", StringComparison.Ordinal), "hidden output");
            input["messages"] = new JsonArray(new JsonObject { ["role"] = "assistant", ["content"] = JsonNode.Parse("[" + thought.GetRawText() + "]") }, new JsonObject { ["role"] = "user", ["content"] = "continue" });
            using var replay = await f.Post(input); Check(replay.IsSuccessStatusCode && f.Requests.Last().GetProperty("messages")[0].GetProperty("reasoning_content").GetString() == "PRIVATE_THOUGHT", "restore reasoning");
            input["model"] = "other-model"; using var invalid = await f.Post(input); Check((int)invalid.StatusCode == 400 && f.Requests.Count == 2, "model authentication");
        });
        await Test("headers timeout is bounded without retry", async () =>
        {
            await using var f = await Fixture.Create(async c => { try { await Task.Delay(Timeout.InfiniteTimeSpan, c.RequestAborted); } catch (OperationCanceledException) { } }, headerTimeout: TimeSpan.FromMilliseconds(100));
            using var r = await f.Post(Request); Check((int)r.StatusCode == 504 && f.Requests.Count == 1, "header timeout");
        });
        await Test("idle body timeout yields incomplete SSE", async () =>
        {
            await using var f = await Fixture.Create(async c => { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text); await c.Response.Body.FlushAsync(); try { await Task.Delay(Timeout.InfiniteTimeSpan, c.RequestAborted); } catch (OperationCanceledException) { } }, idleTimeout: TimeSpan.FromMilliseconds(100));
            using var r = await f.Post(Request); string wire = await r.Content.ReadAsStringAsync(); Check(wire.Contains("event: error", StringComparison.Ordinal) && !wire.Contains("message_stop", StringComparison.Ordinal), "idle timeout");
        });
        await Test("header deadline does not truncate a healthy response body", async () =>
        {
            await using var f = await Fixture.Create(async c => { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text); await c.Response.Body.FlushAsync(); await Task.Delay(250, c.RequestAborted); await c.Response.WriteAsync(Finish); }, headerTimeout: TimeSpan.FromMilliseconds(100), idleTimeout: TimeSpan.FromSeconds(2));
            using var r = await f.Post(Request); Check((await r.Content.ReadAsStringAsync()).Contains("message_stop", StringComparison.Ordinal), "separate response deadlines");
        });
        await Test("client disconnect aborts upstream and releases admission", async () =>
        {
            var cancelled = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously); int count = 0;
            await using var f = await Fixture.Create(async c =>
            {
                if (Interlocked.Increment(ref count) > 1) { await Normal(c); return; }
                c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text); await c.Response.Body.FlushAsync();
                try { await Task.Delay(Timeout.InfiniteTimeSpan, c.RequestAborted); } catch (OperationCanceledException) { cancelled.TrySetResult(); }
            }, maxConcurrent: 1);
            using (var message = new HttpRequestMessage(HttpMethod.Post, "/v1/messages") { Content = JsonContent.Create(Request) })
            using (var r = await f.Client.SendAsync(message, HttpCompletionOption.ResponseHeadersRead))
            { var stream = await r.Content.ReadAsStreamAsync(); byte[] b = new byte[1]; Check(await stream.ReadAsync(b) == 1, "first output"); }
            await cancelled.Task.WaitAsync(TimeSpan.FromSeconds(5)); using var next = await f.Post(Request); Check(next.IsSuccessStatusCode, "admission released");
        });
        await Test("bounded admission rejects extra request before upstream", async () =>
        {
            var entered = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously); var release = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            await using var f = await Fixture.Create(async c => { entered.TrySetResult(); await release.Task.WaitAsync(c.RequestAborted); await Normal(c); }, maxConcurrent: 1);
            var first = f.Post(Request);
            try { await entered.Task.WaitAsync(TimeSpan.FromSeconds(5)); using var second = await f.Post(Request); Check((int)second.StatusCode == 503 && f.Requests.Count == 1, "admission bound"); }
            finally { release.TrySetResult(); using var response = await first; }
        });
        await Test("preview refuses external upstream and invalid startup limits", () =>
        {
            int rejected = 0;
            try { LocalGateway.Build(new(new Uri("https://example.invalid/upstream"), Program.TestKey)); } catch (ArgumentException) { rejected++; }
            try { LocalGateway.Build(new(new Uri("http://127.0.0.1:1/upstream"), Program.TestKey, HeaderTimeout: TimeSpan.Zero)); } catch (ArgumentException) { rejected++; }
            Check(rejected == 2, "local preview boundary"); return Task.CompletedTask;
        });
        Console.WriteLine(JsonSerializer.Serialize(new { passed = passed.Count, tests = passed }));
    }
    sealed class Fixture(WebApplication upstream, WebApplication gateway, HttpClient client, ConcurrentQueue<JsonElement> requests) : IAsyncDisposable
    {
        public HttpClient Client { get; } = client;
        public ConcurrentQueue<JsonElement> Requests { get; } = requests;
        public Task<HttpResponseMessage> Post(object body) => Client.PostAsync("/v1/messages", JsonContent.Create(body));
        public Task<HttpResponseMessage> PostRaw(string body) => Client.PostAsync("/v1/messages", new StringContent(body, Encoding.UTF8, "application/json"));
        public static async Task<Fixture> Create(Func<HttpContext, Task> respond, int maxRequest = 32 << 20, long maxOutput = 64 << 20, int maxConcurrent = 4, TimeSpan? headerTimeout = null, TimeSpan? idleTimeout = null)
        {
            var builder = WebApplication.CreateSlimBuilder(); builder.Logging.ClearProviders(); builder.WebHost.ConfigureKestrel(k => k.Listen(IPAddress.Loopback, 0));
            var upstream = builder.Build(); var requests = new ConcurrentQueue<JsonElement>();
            upstream.MapPost("/upstream", async c => { using var parsed = await JsonDocument.ParseAsync(c.Request.Body, cancellationToken: c.RequestAborted); requests.Enqueue(parsed.RootElement.Clone()); await respond(c); });
            await upstream.StartAsync();
            var gateway = LocalGateway.Build(new(new Uri(LocalGateway.Address(upstream) + "/upstream"), Program.TestKey, MaxRequestBytes: maxRequest, MaxOutputBytes: maxOutput, MaxConcurrent: maxConcurrent, HeaderTimeout: headerTimeout, IdleTimeout: idleTimeout));
            await gateway.StartAsync();
            return new(upstream, gateway, new HttpClient(new SocketsHttpHandler { UseProxy = false }) { BaseAddress = new Uri(LocalGateway.Address(gateway)), Timeout = TimeSpan.FromSeconds(10) }, requests);
        }
        public async ValueTask DisposeAsync() { Client.Dispose(); await gateway.StopAsync(); await gateway.DisposeAsync(); await upstream.StopAsync(); await upstream.DisposeAsync(); }
    }
}
