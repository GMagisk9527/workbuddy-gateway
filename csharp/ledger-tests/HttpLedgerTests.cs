using System.Collections.Concurrent;
using System.Net;
using System.Text.Json;
using HttpMigration;
using KeySecurity;
using LedgerMigration;
using static LedgerChecks.Fixtures;

namespace LedgerChecks;

internal static class HttpLedgerTests
{
    const string Admin = "synthetic-ledger-admin";
    const string Text = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n";
    internal static string Finish(double? credit = 2) => "data: " + JsonSerializer.Serialize(new { choices = new[] { new { finish_reason = "stop" } }, usage = credit is null ? (object)new { prompt_tokens = 20 } : new { credit, prompt_tokens = 20, completion_tokens = 3, cached_tokens = 5 } }) + "\n\n";
    static Task Reply(HttpContext c, string value) { c.Response.ContentType = "text/event-stream"; return c.Response.WriteAsync(value); }
    static async Task<JsonElement> Read(HttpResponseMessage r) => JsonSerializer.Deserialize<JsonElement>(await r.Content.ReadAsByteArrayAsync());
    internal static async Task<List<string>> Run()
    {
        var passed = new List<string>(); async Task Test(string name, Func<Task> action) { await action(); passed.Add(name); }
        await Test("finite quota now admits and charges both response modes", async () =>
        {
            foreach (bool streaming in new[] { true, false })
            {
                await using var f = await Fixture.Create(); using var response = await f.Model(streaming);
                var usage = f.Ledger.GetUsage(f.Id); Check(response.IsSuccessStatusCode && usage is { Total: 2, Input: 20, Output: 3, Cached: 5, Requests: 1 } && f.Ledger.Health.PendingAttempts == 0, "measured HTTP usage");
            }
        });
        await Test("quota exhaustion rejects before another upstream invocation", async () =>
        {
            await using var f = await Fixture.Create(quota: 2); using var first = await f.Model(); using var second = await f.Model();
            Check(first.IsSuccessStatusCode && (int)second.StatusCode == 429 && (await Read(second)).GetProperty("error").GetProperty("code").GetString() == "credit_quota_exhausted" && f.Requests.Count == 1, "quota boundary");
        });
        await Test("durable begin exists before the fake upstream receives request", async () =>
        {
            CreditLedger? observed = null; await using var f = await Fixture.Create(respond: c => { Check(observed!.Health.PendingAttempts == 1, "begin before send"); return Reply(c, Text + Finish()); }); observed = f.Ledger;
            using var response = await f.Model(); Check(response.IsSuccessStatusCode, "completed");
        });
        await Test("validation failure has no WAL attempt or charge", async () =>
        {
            await using var f = await Fixture.Create(); using var response = await f.Model(raw: "{bad");
            Check((int)response.StatusCode == 400 && f.Ledger.Health.Seq == 0 && f.Requests.IsEmpty, "pre-send validation");
        });
        await Test("zero credit and missing credit have distinct factual counts", async () =>
        {
            int count = 0; await using var f = await Fixture.Create(respond: c => Reply(c, Text + Finish(Interlocked.Increment(ref count) == 1 ? 0 : null)));
            using var zero = await f.Model(); using var unknown = await f.Model(); Check(f.Ledger.GetUsage(f.Id) is { Total: 0, ZeroCost: 1, Unknown: 1, Requests: 2 }, "zero versus unknown");
        });
        await Test("ten unknowns stop finite key and authenticated usage remains readable", async () =>
        {
            await using var f = await Fixture.Create(respond: c => Reply(c, Text + Finish(null)));
            for (int i = 0; i < 10; i++) { using var response = await f.Model(); Check(response.IsSuccessStatusCode, "unknown response"); }
            using var blocked = await f.Model(); Check((int)blocked.StatusCode == 429 && f.Requests.Count == 10, "unknown tolerance");
            using var usage = await f.GetUsage(); Check(usage.IsSuccessStatusCode && (await Read(usage)).GetProperty("unknownSettlements").GetInt32() == 10, "diagnostic usage");
        });
        await Test("explicit HTTP channel/account rejection cancels uncharged begin", async () =>
        {
            foreach (int code in new[] { 11128, 11140 })
            {
                await using var f = await Fixture.Create(respond: async c => { c.Response.StatusCode = 403; await c.Response.WriteAsJsonAsync(new { code }); });
                using var response = await f.Model(); Check((int)response.StatusCode == 403 && f.Ledger.GetUsage(f.Id).Requests == 0 && f.Ledger.Health.PendingAttempts == 0 && f.Ledger.Health.Healthy, "uncharged rejection");
            }
        });
        await Test("SSE rejection before output cancels; after output settles unknown", async () =>
        {
            foreach (bool partial in new[] { false, true })
            {
                await using var f = await Fixture.Create(respond: c => Reply(c, (partial ? Text : "") + "data: {\"code\":11128}\n\n"));
                using var response = await f.Model(); Check(f.Ledger.GetUsage(f.Id).Unknown == (partial ? 1 : 0) && f.Ledger.Health.PendingAttempts == 0, "SSE rejection accounting");
            }
        });
        await Test("ordinary HTTP failure is unknown rather than fabricated free", async () =>
        {
            await using var f = await Fixture.Create(respond: async c => { c.Response.StatusCode = 500; await c.Response.WriteAsync("failure"); });
            using var response = await f.Model(); Check((int)response.StatusCode == 500 && f.Ledger.GetUsage(f.Id) is { Unknown: 1, Requests: 1 }, "ordinary failure");
        });
        await Test("measured error response credit wins over an uncharged error code", async () =>
        {
            await using var f = await Fixture.Create(respond: async c => { c.Response.StatusCode = 400; await c.Response.WriteAsJsonAsync(new { code = 11128, usage = new { credit = 2 } }); });
            using var response = await f.Model(); Check((int)response.StatusCode == 400 && f.Ledger.GetUsage(f.Id).Total == 2, "error cost retained");
        });
        await Test("malformed tail preserves measured credit in streaming and aggregation", async () =>
        {
            foreach (bool stream in new[] { true, false })
            {
                await using var f = await Fixture.Create(respond: c => Reply(c, Text + Finish() + "data: {oops}\n\n")); using var response = await f.Model(stream);
                Check(f.Ledger.GetUsage(f.Id) is { Total: 2, Requests: 1 } && (stream || (int)response.StatusCode == 502), "conversion failure usage");
            }
        });
        await Test("begin fsync failure prevents upstream send", async () =>
        {
            var files = new MemoryLedger(); await using var f = await Fixture.Create(backend: files); files.Fault = (op, _) => { if (op == "sync") throw new IOException("begin fsync"); };
            using var response = await f.Model(); Check((int)response.StatusCode == 503 && f.Requests.IsEmpty && !f.Ledger.Health.Healthy, "begin write barrier");
        });
        await Test("aggregate settlement failure returns error and never partial success", async () =>
        {
            var files = new MemoryLedger(); await using var f = await Fixture.Create(backend: files); files.Fault = (op, _) => { if (op == "sync" && files.LastKind == "settle") throw new IOException("settle fsync"); };
            using var response = await f.Model(stream: false); Check((int)response.StatusCode == 503 && (await Read(response)).GetProperty("type").GetString() == "error" && f.Ledger.GetUsage(f.Id).Requests == 0, "no uncommitted success");
        });
        await Test("stream settlement failure happens before message_stop", async () =>
        {
            var files = new MemoryLedger(); await using var f = await Fixture.Create(backend: files); files.Fault = (op, _) => { if (op == "sync" && files.LastKind == "settle") throw new IOException("settle fsync"); };
            using var response = await f.Model(headersOnly: true); using var output = new MemoryStream();
            try { await (await response.Content.ReadAsStreamAsync()).CopyToAsync(output); } catch (IOException) { }
            Check(!System.Text.Encoding.UTF8.GetString(output.ToArray()).Contains("message_stop", StringComparison.Ordinal) && !f.Ledger.Health.Healthy && f.Ledger.GetUsage(f.Id).Requests == 0, "no successful terminal marker");
        });
        await Test("client disconnect settles observed usage despite cancellation", async () =>
        {
            var entered = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            await using var f = await Fixture.Create(respond: async c =>
            {
                c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text + "data: {\"choices\":[{\"delta\":{\"content\":\"usage-observed\"}}],\"usage\":{\"credit\":2}}\n\n"); await c.Response.Body.FlushAsync(); entered.TrySetResult();
                try { await Task.Delay(Timeout.InfiniteTimeSpan, c.RequestAborted); } catch (OperationCanceledException) { }
            });
            using (var response = await f.Model(headersOnly: true))
            {
                await entered.Task.WaitAsync(TimeSpan.FromSeconds(5)); var body = await response.Content.ReadAsStreamAsync(); byte[] chunk = new byte[1024]; using var observed = new MemoryStream();
                using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(5));
                while (!System.Text.Encoding.UTF8.GetString(observed.ToArray()).Contains("usage-observed", StringComparison.Ordinal))
                { int n = await body.ReadAsync(chunk, deadline.Token); Check(n > 0, "usage marker before disconnect"); observed.Write(chunk, 0, n); }
            }
            await f.WaitSettled(); Check(f.Ledger.GetUsage(f.Id) is { Total: 2, Requests: 1 }, "cancelled measured cost");
        });
        await Test("aggregate cancellation closes intent conservatively", async () =>
        {
            var entered = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            await using var f = await Fixture.Create(respond: async c =>
            {
                c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text + "data: {\"choices\":[],\"usage\":{\"credit\":2}}\n\n"); await c.Response.Body.FlushAsync(); entered.TrySetResult();
                try { await Task.Delay(Timeout.InfiniteTimeSpan, c.RequestAborted); } catch (OperationCanceledException) { }
            });
            using var cts = new CancellationTokenSource(); var request = f.Model(stream: false, cancellation: cts.Token); await entered.Task.WaitAsync(TimeSpan.FromSeconds(5)); cts.Cancel();
            try { using var response = await request; } catch (OperationCanceledException) { }
            await f.WaitSettled(); var usage = f.Ledger.GetUsage(f.Id); Check(usage.Requests == 1 && (usage.Total == 2 || usage.Unknown == 1), "aggregate cancelled cost or unknown");
        });
        await Test("admin reset preserves lifetime and delete preserves archive summary", async () =>
        {
            await using var f = await Fixture.Create(); using var response = await f.Model();
            using var reset = await f.AdminCall(new { action = "reset", id = f.Id, confirm = true }); Check(reset.IsSuccessStatusCode && f.Ledger.GetUsage(f.Id) is { Total: 0, Lifetime: 2, SpentToday: 2 }, "reset accounting");
            using var delete = await f.AdminCall(new { action = "delete", id = f.Id, confirm = true }); using var summary = await f.AdminGet(); var body = await Read(summary);
            Check(delete.IsSuccessStatusCode && body.GetProperty("summary").GetProperty("allTimeCredits").GetDouble() == 2 && body.GetProperty("summary").GetProperty("archivedCredits").GetDouble() == 2, "deleted history");
        });
        return passed;
    }
    sealed class Fixture(WebApplication upstream, WebApplication gateway, HttpClient client, KeyStore store, CreditLedger ledger, string secret, string id, ConcurrentQueue<JsonElement> requests) : IAsyncDisposable
    {
        internal CreditLedger Ledger { get; } = ledger;
        internal string Id { get; } = id;
        internal ConcurrentQueue<JsonElement> Requests { get; } = requests;
        internal async Task<HttpResponseMessage> Model(bool stream = true, bool headersOnly = false, string? raw = null, CancellationToken cancellation = default)
        {
            using var message = new HttpRequestMessage(HttpMethod.Post, "/v1/messages") { Content = raw is null ? JsonContent.Create(new { model = "test-model", stream, messages = new[] { new { role = "user", content = "hi" } } }) : new StringContent(raw) };
            message.Headers.TryAddWithoutValidation("Authorization", "Bearer " + secret);
            return await client.SendAsync(message, headersOnly ? HttpCompletionOption.ResponseHeadersRead : HttpCompletionOption.ResponseContentRead, cancellation);
        }
        internal async Task<HttpResponseMessage> GetUsage()
        { using var message = new HttpRequestMessage(HttpMethod.Get, "/v1/usage"); message.Headers.TryAddWithoutValidation("Authorization", "Bearer " + secret); return await client.SendAsync(message); }
        internal async Task<HttpResponseMessage> AdminCall(object body)
        { using var message = new HttpRequestMessage(HttpMethod.Post, "/admin/api/keys") { Content = JsonContent.Create(body) }; message.Headers.TryAddWithoutValidation("Authorization", "Bearer " + Admin); return await client.SendAsync(message); }
        internal async Task<HttpResponseMessage> AdminGet()
        { using var message = new HttpRequestMessage(HttpMethod.Get, "/admin/api/keys"); message.Headers.TryAddWithoutValidation("Authorization", "Bearer " + Admin); return await client.SendAsync(message); }
        internal async Task WaitSettled()
        { using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5)); while (Ledger.Health.PendingAttempts > 0) await Task.Delay(10, timeout.Token); }
        internal static async Task<Fixture> Create(double quota = 10, Func<HttpContext, Task>? respond = null, MemoryLedger? backend = null)
        {
            var ledger = new CreditLedger((backend ?? new()).Open()); var store = new KeyStore(new MemoryKeys()); var issued = store.Create(new("quota", QuotaTotal: quota)); string secret = issued.TakeSecret();
            var builder = WebApplication.CreateSlimBuilder(); builder.Logging.ClearProviders(); builder.WebHost.ConfigureKestrel(k => k.Listen(IPAddress.Loopback, 0));
            var upstream = builder.Build(); var requests = new ConcurrentQueue<JsonElement>();
            upstream.MapPost("/upstream", async c => { using var body = await JsonDocument.ParseAsync(c.Request.Body, cancellationToken: c.RequestAborted); requests.Enqueue(body.RootElement.Clone()); await (respond is null ? Reply(c, Text + Finish()) : respond(c)); });
            await upstream.StartAsync();
            var gateway = LocalGateway.Build(new(new Uri(LocalGateway.Address(upstream) + "/upstream"), new byte[32], Security: new(store, new Credential(Admin), ledger))); await gateway.StartAsync();
            return new(upstream, gateway, new HttpClient(new SocketsHttpHandler { UseProxy = false }) { BaseAddress = new Uri(LocalGateway.Address(gateway)), Timeout = TimeSpan.FromSeconds(10) }, store, ledger, secret, issued.Item.Id, requests);
        }
        public async ValueTask DisposeAsync()
        { client.Dispose(); await gateway.StopAsync(); await gateway.DisposeAsync(); await upstream.StopAsync(); await upstream.DisposeAsync(); store.Dispose(); Ledger.Dispose(); }
    }
}
