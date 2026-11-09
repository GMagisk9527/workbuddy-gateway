using System.Collections.Concurrent;
using System.Net;
using System.Net.Http.Headers;
using System.Runtime.InteropServices;
using System.Text;
using System.Text.Json.Nodes;
using HttpMigration;
using KeySecurity;
using LedgerMigration;
using static OpenAiChecks.StreamTests;

namespace OpenAiChecks;

internal static class HttpTests
{
    const string Secret = "synthetic-openai-key";
    static readonly string[] Paths = ["/v1/messages", "/v1/chat/completions", "/v1/responses"];
    static string Body(string path, bool stream = false) => path == "/v1/responses" ? "{\"model\":\"m\",\"input\":\"hi\",\"stream\":" + (stream ? "true" : "false") + "}" : "{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":" + (stream ? "true" : "false") + "}";
    public static async Task<List<string>> Run()
    {
        var tests = new List<string>(); async Task Test(string name, Func<Task> action) { await action(); tests.Add(name); }
        await Test("bridge translates all protocols and modes with exactly one backend call each", async () =>
        {
            await using var f = await Fixture.Create();
            foreach (var path in Paths) foreach (bool stream in new[] { false, true })
            {
                using var response = await f.Post(path, Body(path, stream)); var data = await response.Content.ReadAsStringAsync();
                Check(response.IsSuccessStatusCode && data.Contains("hello", StringComparison.Ordinal) && response.Headers.GetValues("X-Gateway-Implementation").Single() == "csharp-protocols", "three protocols");
            }
            Check(f.Calls.Count == 6 && f.Committed == 6 && f.Calls.All(c => c["stream"]!.GetValue<bool>() && c["messages"] is JsonArray), "one backend settlement");
        });
        await Test("bridge requires a key before parsing a request", async () =>
        {
            await using var f = await Fixture.Create(); f.Client.DefaultRequestHeaders.Authorization = null;
            foreach (var path in Paths) { using var response = await f.Post(path, "{bad"); Check((int)response.StatusCode == 401, "authentication first"); }
            Check(f.Calls.IsEmpty, "no unauthenticated backend calls");
        });
        await Test("backend invalid-key refusal is propagated without retries", async () =>
        {
            await using var f = await Fixture.Create(); f.Client.DefaultRequestHeaders.Authorization = new("Bearer", "wrong-key");
            foreach (var path in Paths) { using var response = await f.Post(path, Body(path)); Check((int)response.StatusCode == 401, "invalid key"); }
            Check(f.Calls.IsEmpty && f.Committed == 0, "no account attempt");
        });
        await Test("input validation happens before the backend and uses protocol error envelopes", async () =>
        {
            await using var f = await Fixture.Create();
            foreach (var path in Paths)
            {
                using var response = await f.Post(path, "{}"); var json = JsonNode.Parse(await response.Content.ReadAsByteArrayAsync())!;
                Check((int)response.StatusCode == 400 && json["error"] is JsonObject && (path == "/v1/messages" ? json["type"] is not null : json["type"] is null), "error shape");
            }
            Check(f.Calls.IsEmpty, "no invalid upstream requests");
        });
        await Test("Go bridge forwards caller credential and session hint but no cookies", async () =>
        {
            await using var f = await Fixture.Create(c =>
            {
                Check(c.Request.Headers.Authorization == "Bearer " + Secret && c.Request.Headers["X-Session-ID"] == "synthetic-session" && !c.Request.Headers.ContainsKey("Cookie"), "forwarding allowlist");
                c.Response.ContentType = "text/event-stream"; return c.Response.WriteAsync(Text + Finish);
            });
            f.Client.DefaultRequestHeaders.Add("X-Session-ID", "synthetic-session"); f.Client.DefaultRequestHeaders.Add("Cookie", "private=must-not-forward");
            using var response = await f.Post("/v1/responses", Body("/v1/responses")); Check(response.IsSuccessStatusCode, "allowlisted header forwarding");
        });
        await Test("streaming bridge starts keepalive before a slow first token", async () =>
        {
            var release = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            await using var f = await Fixture.Create(async c => { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(": backend wait\n\n"); await c.Response.Body.FlushAsync(); await release.Task.WaitAsync(c.RequestAborted); await c.Response.WriteAsync(Text + Finish); });
            foreach (var path in Paths)
            {
                using var message = new HttpRequestMessage(HttpMethod.Post, path) { Content = new StringContent(Body(path, true), Encoding.UTF8, "application/json") };
                using var response = await f.Client.SendAsync(message, HttpCompletionOption.ResponseHeadersRead);
                await using var body = await response.Content.ReadAsStreamAsync(); var first = new byte[13]; await body.ReadExactlyAsync(first);
                Check(Encoding.UTF8.GetString(first) == ": keepalive\n\n", "early heartbeat"); release.TrySetResult();
                using var reader = new StreamReader(body); await reader.ReadToEndAsync();
            }
        });
        await Test("upstream channel rejection keeps its code and never cascades", async () =>
        {
            int rejected = 0; await using var f = await Fixture.Create(async c => { Interlocked.Increment(ref rejected); c.Response.StatusCode = 400; await c.Response.WriteAsJsonAsync(new { code = 11128 }); });
            foreach (var path in Paths)
            {
                using var response = await f.Post(path, Body(path)); var body = await response.Content.ReadAsStringAsync();
                Check((int)response.StatusCode == 400 && body.Contains("11128", StringComparison.Ordinal), "business code");
            }
            Check(rejected == 3, "one send per logical request");
        });
        await Test("standalone mode settles each OpenAI response once before completion", async () =>
        {
            await using var f = await Fixture.Create(bridge: false);
            foreach (var path in Paths) foreach (bool stream in new[] { false, true })
            {
                using var response = await f.Post(path, Body(path, stream)); Check(response.IsSuccessStatusCode, "standalone completion");
            }
            Check(f.Ledger!.GetUsage(f.KeyId!) is { Total: 12, Requests: 6 } && f.Ledger.Health.PendingAttempts == 0, "one WAL settlement per call");
        });
        await Test("upstream error finish fails every protocol while retaining measured usage", async () =>
        {
            const string failed = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"error\"}]}\n\n";
            foreach (string usage in new[] { "finish", "trailer", "absent" })
            {
                string end = usage switch
                {
                    "finish" => Finish.Replace("\"stop\"", "\"error\"", StringComparison.Ordinal),
                    "trailer" => failed + "data: {\"choices\":[],\"usage\":{\"credit\":2}}\n\n",
                    _ => failed
                };
                await using var f = await Fixture.Create(c =>
                {
                    c.Response.ContentType = "text/event-stream";
                    return c.Response.WriteAsync(Text + end + "data: [DONE]\n\n");
                }, bridge: false);
                foreach (var path in Paths) foreach (bool stream in new[] { false, true })
                {
                    using var response = await f.Post(path, Body(path, stream));
                    string output = await response.Content.ReadAsStringAsync();
                    Check((int)response.StatusCode == (stream ? 200 : 502) && output.Contains("upstream_finish_error", StringComparison.Ordinal), "explicit upstream failure: " + path);
                    if (stream)
                    {
                        string failure = path == "/v1/responses" ? "event: response.failed" : "\"error\":";
                        Check(output.Contains("hello", StringComparison.Ordinal) && output.Contains(failure, StringComparison.Ordinal), "partial output followed by protocol error");
                        Check(!output.Contains("[DONE]", StringComparison.Ordinal) && !output.Contains("message_stop", StringComparison.Ordinal) && !output.Contains("response.completed", StringComparison.Ordinal), "no success terminator");
                    }
                }
                var settled = f.Ledger!.GetUsage(f.KeyId!);
                Check(f.Calls.Count == 6 && settled.Requests == 6 && f.Ledger.Health.PendingAttempts == 0, "one settlement per failed request");
                Check(usage == "absent" ? settled is { Total: 0, Unknown: 6 } : settled is { Total: 12, Unknown: 0 }, "only charge measured usage");
            }
        });
        await Test("all protocols reject a late generic Go error rather than forge success", async () =>
        {
            await using var f = await Fixture.Create(c => { c.Response.ContentType = "text/event-stream"; return c.Response.WriteAsync(Text + Finish + "data: {\"error\":{\"code\":\"stream_interrupted\"}}\n\n"); });
            foreach (var path in Paths)
            {
                using var response = await f.Post(path, Body(path, true)); var output = await response.Content.ReadAsStringAsync();
                Check(!output.Contains("[DONE]", StringComparison.Ordinal) && !output.Contains("message_stop", StringComparison.Ordinal) && !output.Contains("response.completed", StringComparison.Ordinal), "late error prevents terminal success");
            }
        });
        await Test("bridge exposes backend model and usage helpers without admin routes", async () =>
        {
            await using var f = await Fixture.Create(); using var ready = await f.Client.GetAsync("/readyz"); using var models = await f.Client.GetAsync("/v1/models"); using var usage = await f.Client.GetAsync("/v1/usage"); using var admin = await f.Client.GetAsync("/admin/api/keys");
            Check(ready.IsSuccessStatusCode && models.IsSuccessStatusCode && usage.IsSuccessStatusCode && (int)admin.StatusCode == 404, "delegated helper routes");
        });
        await Test("native zstd accepts real compressed requests for every protocol", async () =>
        {
            await using var f = await Fixture.Create();
            foreach (var path in Paths)
            {
                byte[] compressed = Compress(Encoding.UTF8.GetBytes(Body(path))); using var content = new ByteArrayContent(compressed); content.Headers.ContentType = new("application/json"); content.Headers.ContentEncoding.Add("zstd");
                using var response = await f.Client.PostAsync(path, content); Check(response.IsSuccessStatusCode, "zstd request");
            }
        });
        await Test("zstd corrupt input and expansion beyond body limit never reaches backend", async () =>
        {
            await using var f = await Fixture.Create(limit: 1024);
            foreach (byte[] bytes in new[] { new byte[] { 1, 2, 3 }, Compress(Encoding.UTF8.GetBytes(new string('x', 4096))) })
            {
                using var content = new ByteArrayContent(bytes); content.Headers.ContentEncoding.Add("zstd"); using var response = await f.Client.PostAsync("/v1/chat/completions", content);
                Check((int)response.StatusCode is 400 or 413, "bad compressed body rejected");
            }
            Check(f.Calls.IsEmpty, "no decoded overflow requests");
        });
        return tests;
    }
    [UnmanagedFunctionPointer(CallingConvention.Cdecl)] delegate nuint CompressDelegate(byte[] target, nuint targetSize, byte[] source, nuint sourceSize, int level);
    internal static byte[] Compress(byte[] input)
    {
        IntPtr lib = NativeLibrary.Load(OperatingSystem.IsWindows() ? "zstd.dll" : "libzstd.so.1");
        try
        {
            var compress = Marshal.GetDelegateForFunctionPointer<CompressDelegate>(NativeLibrary.GetExport(lib, "ZSTD_compress"));
            var output = new byte[input.Length + 65536]; nuint size = compress(output, (nuint)output.Length, input, (nuint)input.Length, 3);
            Check(size <= (nuint)output.Length, "native compression"); return output.AsSpan(0, (int)size).ToArray();
        }
        finally { NativeLibrary.Free(lib); }
    }
    sealed class Fixture(WebApplication backend, WebApplication gateway, HttpClient client, KeyStore? keys, CreditLedger? ledger, string? keyId) : IAsyncDisposable
    {
        public HttpClient Client { get; } = client;
        public ConcurrentQueue<JsonNode> Calls { get; } = new();
        public int Committed;
        public CreditLedger? Ledger { get; } = ledger;
        public string? KeyId { get; } = keyId;
        public Task<HttpResponseMessage> Post(string path, string body) => Client.PostAsync(path, new StringContent(body, Encoding.UTF8, "application/json"));
        public static async Task<Fixture> Create(Func<HttpContext, Task>? respond = null, bool bridge = true, int limit = 32 << 20)
        {
            var builder = WebApplication.CreateSlimBuilder(); builder.Logging.ClearProviders(); builder.WebHost.ConfigureKestrel(k => k.Listen(IPAddress.Loopback, 0)); var backend = builder.Build(); Fixture? f = null;
            backend.MapGet("/readyz", () => Results.Json(new { status = "ready" }));
            backend.MapGet("/v1/models", () => Results.Json(new { data = new[] { new { id = "m" } } }));
            backend.MapGet("/v1/usage", () => Results.Json(new { used = 0 }));
            backend.MapPost("/v1/chat/completions", async c =>
            {
                if (bridge && c.Request.Headers.Authorization != "Bearer " + Secret) { c.Response.StatusCode = 401; await c.Response.WriteAsJsonAsync(new { error = new { code = "invalid_api_key" } }); return; }
                f!.Calls.Enqueue((await JsonNode.ParseAsync(c.Request.Body))!);
                if (respond is not null) { await respond(c); return; }
                Interlocked.Increment(ref f.Committed); c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text + Finish);
            });
            await backend.StartAsync(); var endpoint = new Uri(LocalGateway.Address(backend) + "/v1/chat/completions");
            KeyStore? keys = null; CreditLedger? ledger = null; string? id = null; string token = Secret; LocalKeySecurity? security = null;
            if (!bridge)
            {
                string path = Path.Combine(Path.GetTempPath(), "synthetic-openai-" + Guid.NewGuid().ToString("N")); Directory.CreateDirectory(path);
                keys = new(new KeyFiles(path)); ledger = new(new LedgerFiles(path)); keys.AttachAccounting(ledger);
                var key = keys.Create(new("synthetic", QuotaTotal: 100, MaxConcurrent: 8)); id = key.Item.Id; token = key.TakeSecret(); security = new(keys, new("synthetic-admin"), ledger);
            }
            var gateway = LocalGateway.Build(new(endpoint, Program.Key, MaxRequestBytes: limit, GoBridge: bridge, Security: security)); await gateway.StartAsync();
            var client = new HttpClient(new SocketsHttpHandler { UseProxy = false }) { BaseAddress = new(LocalGateway.Address(gateway)), Timeout = TimeSpan.FromSeconds(8) };
            client.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("Bearer", token); f = new(backend, gateway, client, keys, ledger, id); return f;
        }
        public async ValueTask DisposeAsync() { Client.Dispose(); await gateway.StopAsync(); await gateway.DisposeAsync(); await backend.StopAsync(); await backend.DisposeAsync(); Ledger?.Dispose(); keys?.Dispose(); }
    }
}
