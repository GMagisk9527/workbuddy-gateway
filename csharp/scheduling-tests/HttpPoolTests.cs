using System.Collections.Concurrent;
using System.Net;
using System.Net.Http.Headers;
using System.Text;
using HttpMigration;
using KeySecurity;
using LedgerMigration;
using AccountScheduling;
using static SchedulingChecks.PoolTests;

namespace SchedulingChecks;

internal static class HttpPoolTests
{
    const string Text = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n";
    const string End = "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":2,\"prompt_tokens\":20,\"completion_tokens\":3}}\n\n";
    static Task Reply(HttpContext c, string body = Text + End) { c.Response.ContentType = "text/event-stream"; return c.Response.WriteAsync(body); }
    public static async Task<List<string>> Run()
    {
        var passed = new List<string>(); async Task Test(string name, Func<Task> test) { await test(); passed.Add(name); }
        await Test("both HTTP response modes select accounts and retain measured usage", async () =>
        {
            await using var f = await Fixture.Create();
            foreach (bool stream in new[] { false, true }) { using var response = await f.Post(stream); Check(response.IsSuccessStatusCode, "HTTP success"); }
            Check(f.Requests.SequenceEqual(["/a", "/b"]) && f.Ledger!.GetUsage(f.KeyId!) is { Total: 4, Requests: 2 }, "routing and accounting");
            await Until(() => f.Pool.Status.Active == 0);
        });
        await Test("no eligible account rejects before WAL begin or HTTP send", async () =>
        {
            await using var f = await Fixture.Create(configure: p => p.Replace([])); using var response = await f.Post();
            Check((int)response.StatusCode == 503 && f.Requests.IsEmpty && f.Ledger!.Health.Seq == 0, "pre-send scheduling");
        });
        await Test("stream slot stays occupied until body and settlement finish", async () =>
        {
            var release = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            await using var f = await Fixture.Create(async c => { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text); await c.Response.Body.FlushAsync(); await release.Task.WaitAsync(c.RequestAborted); await c.Response.WriteAsync(End); }, single: true);
            using var first = await f.Post(headersOnly: true); Check(f.Pool.Status.Active == 1 && f.Ledger!.Health.PendingAttempts == 1, "held stream");
            var pending = f.Post(); await Until(() => f.Pool.Status.Waiting == 1); Check(f.Requests.Count == 1, "queued not sent");
            release.SetResult(); await first.Content.ReadAsStringAsync(); using var second = await pending;
            await Until(() => f.Pool.Status.Active == 0); Check(second.IsSuccessStatusCode && f.Ledger.GetUsage(f.KeyId!).Total == 4, "completed then released");
        });
        await Test("downstream cancellation releases account and settles ambiguous output", async () =>
        {
            await using var f = await Fixture.Create(async c => { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text); await c.Response.Body.FlushAsync(); try { await Task.Delay(Timeout.Infinite, c.RequestAborted); } catch (OperationCanceledException) { } }, single: true);
            using var cancellation = new CancellationTokenSource(); using var response = await f.Post(headersOnly: true, cancellation: cancellation.Token);
            Check(f.Pool.Status.Active == 1, "held"); cancellation.Cancel(); response.Dispose();
            await Until(() => f.Pool.Status.Active == 0 && f.Ledger!.Health.PendingAttempts == 0);
            Check(f.Ledger!.GetUsage(f.KeyId!).Unknown == 1, "unknown consumption");
        });
        await Test("upstream header timeout releases all permits", async () =>
        {
            await using var f = await Fixture.Create(async c => { try { await Task.Delay(Timeout.Infinite, c.RequestAborted); } catch (OperationCanceledException) { } }, headerTimeout: TimeSpan.FromMilliseconds(80));
            using var response = await f.Post(); await Until(() => f.Pool.Status.Active == 0);
            Check((int)response.StatusCode == 504 && f.Ledger!.GetUsage(f.KeyId!).Unknown == 1 && f.Requests.Count == 1, "timeout cleanup");
        });
        await Test("explicit channel rejection releases without retrying another account", async () =>
        {
            await using var f = await Fixture.Create(async c => { c.Response.StatusCode = 400; await c.Response.WriteAsJsonAsync(new { code = 11128 }); });
            using var response = await f.Post(); await Until(() => f.Pool.Status.Active == 0);
            Check((int)response.StatusCode == 400 && f.Requests.Count == 1 && f.Ledger!.GetUsage(f.KeyId!).Requests == 0 && f.Ledger.Health.PendingAttempts == 0, "no replay");
        });
        await Test("malformed SSE and early EOF release streaming and aggregate leases", async () =>
        {
            foreach (bool stream in new[] { false, true })
            {
                await using var f = await Fixture.Create(c => Reply(c, Text + "data: {bad}\n\n"));
                using var response = await f.Post(stream); await Until(() => f.Pool.Status.Active == 0);
                Check(f.Ledger!.GetUsage(f.KeyId!).Unknown == 1 && f.Requests.Count == 1, "bad stream cleanup");
            }
        });
        await Test("queue full and timeout create no new ledger attempts", async () =>
        {
            var release = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            await using var f = await Fixture.Create(async c => { await release.Task.WaitAsync(c.RequestAborted); await Reply(c); },
                single: true, scheduling: new(MaxAccount: 1, MaxQueued: 1, WaitTimeout: TimeSpan.FromMilliseconds(500)));
            var first = f.Post(); await Until(() => f.Requests.Count == 1); var queued = f.Post(); await Until(() => f.Pool.Status.Waiting == 1);
            using var full = await f.Post(); using var timeout = await queued;
            Check((int)full.StatusCode == 503 && (int)timeout.StatusCode == 503 && f.Ledger!.Health.Seq == 1 && f.Requests.Count == 1, "no attempts while queued");
            release.SetResult(); using var success = await first; Check(success.IsSuccessStatusCode, "first survives");
        });
        await Test("invalid requests reject before account queue", async () =>
        {
            await using var f = await Fixture.Create(single: true); using var held = await f.Pool.AcquireAsync("m");
            using var response = await f.Client.PostAsync("/v1/messages", new StringContent("{bad", Encoding.UTF8, "application/json"));
            Check((int)response.StatusCode == 400 && f.Pool.Status.Waiting == 0 && f.Requests.IsEmpty && f.Ledger!.Health.Seq == 0, "validation first");
        });
        await Test("anonymous request is denied before acquiring an account", async () =>
        {
            await using var f = await Fixture.Create(); using var client = new HttpClient { BaseAddress = f.Client.BaseAddress }; using var response = await client.PostAsync("/v1/messages", new StringContent("{}", Encoding.UTF8, "application/json"));
            Check((int)response.StatusCode == 401 && f.Pool.Status.Active == 0 && f.Requests.IsEmpty, "auth before scheduling");
        });
        return passed;
    }

    sealed class Fixture(WebApplication upstream, WebApplication gateway, HttpClient client, AccountPool pool, KeyStore store, CreditLedger ledger, string keyId) : IAsyncDisposable
    {
        public HttpClient Client { get; } = client;
        public AccountPool Pool { get; } = pool;
        public CreditLedger Ledger { get; } = ledger;
        public string KeyId { get; } = keyId;
        public ConcurrentQueue<string> Requests { get; } = new();
        public async Task<HttpResponseMessage> Post(bool stream = true, bool headersOnly = false, CancellationToken cancellation = default)
        {
            using var request = new HttpRequestMessage(HttpMethod.Post, "/v1/messages") { Content = new StringContent("{\"model\":\"m\",\"stream\":" + (stream ? "true" : "false") + ",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}", Encoding.UTF8, "application/json") };
            return await Client.SendAsync(request, headersOnly ? HttpCompletionOption.ResponseHeadersRead : HttpCompletionOption.ResponseContentRead, cancellation);
        }
        public static async Task<Fixture> Create(Func<HttpContext, Task>? respond = null, bool single = false,
            SchedulingOptions? scheduling = null, Action<AccountPool>? configure = null, TimeSpan? headerTimeout = null)
        {
            var builder = WebApplication.CreateSlimBuilder(); builder.Logging.ClearProviders(); builder.WebHost.ConfigureKestrel(k => k.Listen(IPAddress.Loopback, 0));
            var upstream = builder.Build(); Fixture? fixture = null;
            upstream.MapPost("/{account}", async c => { fixture!.Requests.Enqueue(c.Request.Path); await (respond?.Invoke(c) ?? Reply(c)); }); await upstream.StartAsync();
            var endpoint = new Uri(LocalGateway.Address(upstream)); var a = A() with { Upstream = new(endpoint, "/a") }; var b = A("b") with { Upstream = new(endpoint, "/b") };
            var pool = new AccountPool(single ? [a] : [a, b], scheduling ?? new(MaxAccount: 1)); configure?.Invoke(pool);
            string directory = Path.Combine(Path.GetTempPath(), "synthetic-scheduling-" + Guid.NewGuid().ToString("N")); Directory.CreateDirectory(directory);
            var store = new KeyStore(new KeyFiles(directory)); var ledger = new CreditLedger(new LedgerFiles(directory)); store.AttachAccounting(ledger);
            var key = store.Create(new("synthetic", QuotaTotal: 1000, MaxConcurrent: 16));
            var app = LocalGateway.Build(new(new(endpoint, "/fallback"), new byte[32], MaxConcurrent: 16, HeaderTimeout: headerTimeout,
                Security: new(store, new("synthetic-admin"), ledger), Accounts: pool)); await app.StartAsync();
            var client = new HttpClient(new SocketsHttpHandler { UseProxy = false }) { BaseAddress = new(LocalGateway.Address(app)), Timeout = TimeSpan.FromSeconds(8) };
            client.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("Bearer", key.TakeSecret());
            fixture = new(upstream, app, client, pool, store, ledger, key.Item.Id); return fixture;
        }
        public async ValueTask DisposeAsync()
        {
            Client.Dispose(); await gateway.StopAsync(); await gateway.DisposeAsync(); await upstream.StopAsync(); await upstream.DisposeAsync(); Ledger.Dispose(); store.Dispose();
        }
    }
}
