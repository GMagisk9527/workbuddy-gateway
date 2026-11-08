using System.Collections.Concurrent;
using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using HttpMigration;
using KeySecurity;
using LedgerMigration;
using static MaintenanceChecks.Fixtures;

namespace MaintenanceChecks;

internal static class HttpMaintenanceTests
{
    const string Text = "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n";
    const string End = "data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":2,\"prompt_tokens\":20}}\n\n";
    internal sealed class Scenario
    {
        public Func<HttpContext, Task>? Refresh, Verify, Model;
        public int RefreshCount, VerifyCount, ModelCount;
        public bool FailDisk;
        public long Expires = 1800000100;
        public AccountSite Site = AccountSite.Cn;
        public Action<AccountPool, AccountSnapshot>? BeforeModel;
        public readonly ConcurrentQueue<string> Access = new();
    }
    public static async Task<List<string>> Run()
    {
        var passed = new List<string>(); async Task Test(string name, Func<Task> action) { await action(); passed.Add(name); }
        await Test("refresh and verification finish durably before WAL or model send", async () =>
        {
            var scenario = new Scenario(); await using var f = await Fixture.Create(scenario);
            scenario.Model = async c =>
            {
                Check(f.Files.Saves == 1 && Access(f.Files.Saved.Credential) == "fresh-access" && f.Ledger.Health.PendingAttempts == 1, "durability before model");
                Check(c.Request.Headers.Authorization == "Bearer fresh-access" && c.Request.Headers["X-User-Id"] == "user" && !c.Request.Headers.ContainsKey("Cookie"), "selected account headers");
                c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text + End);
            };
            using var result = await f.Post(false); Check(result.IsSuccessStatusCode && scenario.RefreshCount == 1 && scenario.VerifyCount == 1 && scenario.ModelCount == 1 && f.Ledger.GetUsage(f.KeyId).Total == 2, "full chain");
        });
        await Test("valid token bypasses refresh and caller authorization is never forwarded", async () =>
        {
            var scenario = new Scenario { Expires = 1800003600 }; await using var f = await Fixture.Create(scenario); using var result = await f.Post();
            Check(result.IsSuccessStatusCode && scenario.RefreshCount == 0 && scenario.Access.Single() == "Bearer old-access", "account token only");
        });
        await Test("credential site mismatch fails before refresh or model send", async () =>
        {
            var scenario = new Scenario { Site = AccountSite.Intl }; await using var f = await Fixture.Create(scenario); using var result = await f.Post();
            Check((int)result.StatusCode == 503 && scenario.RefreshCount == 0 && scenario.ModelCount == 0 && f.Ledger.Health.Seq == 0, "site binding");
        });
        await Test("plain refresh 403 is transient and does not disable on first failure", async () =>
        {
            var scenario = new Scenario { Refresh = c => { c.Response.StatusCode = 403; return c.Response.WriteAsync("forbidden"); } };
            await using var f = await Fixture.Create(scenario); using var result = await f.Post();
            Check((int)result.StatusCode == 503 && f.Manager.Status.Single() is { Failures: 1, Disabled: false } && f.Ledger.Health.Seq == 0 && scenario.ModelCount == 0, "transient no model");
        });
        await Test("refreshed token rejected by verification leaves old data and no WAL", async () =>
        {
            var scenario = new Scenario { Verify = c => { c.Response.StatusCode = 401; return Task.CompletedTask; } };
            await using var f = await Fixture.Create(scenario); using var result = await f.Post();
            Check((int)result.StatusCode == 503 && f.Files.Saves == 0 && scenario.ModelCount == 0 && f.Ledger.Health.Seq == 0, "rejected candidate");
        });
        await Test("ordinary verification 403 is unknown and preserves successful rotation", async () =>
        {
            var scenario = new Scenario { Verify = c => { c.Response.StatusCode = 403; return Task.CompletedTask; } };
            await using var f = await Fixture.Create(scenario); using var result = await f.Post();
            Check(result.IsSuccessStatusCode && f.Files.Saves == 1 && scenario.ModelCount == 1, "unknown verification accepts");
        });
        await Test("verification 403 with explicit invalid-token evidence rejects the candidate", async () =>
        {
            var scenario = new Scenario { Verify = c => { c.Response.StatusCode = 403; return c.Response.WriteAsync("invalid token"); } };
            await using var f = await Fixture.Create(scenario); using var result = await f.Post();
            Check((int)result.StatusCode == 503 && f.Files.Saves == 0 && scenario.ModelCount == 0, "explicit authentication failure");
        });
        await Test("persistence failure prevents model send and ledger begin", async () =>
        {
            var scenario = new Scenario { FailDisk = true }; await using var f = await Fixture.Create(scenario); using var result = await f.Post();
            Check((int)result.StatusCode == 503 && scenario.ModelCount == 0 && f.Ledger.Health.Seq == 0 && f.Manager.Status.Single().Degraded, "write failure boundary");
        });
        await Test("account disabled during maintenance is rechecked before send", async () =>
        {
            var scenario = new Scenario { BeforeModel = (pool, account) => pool.Replace([account with { Disabled = true }]) };
            await using var f = await Fixture.Create(scenario); using var result = await f.Post();
            Check((int)result.StatusCode == 503 && scenario.ModelCount == 0 && f.Ledger.Health.Seq == 0, "post-maintenance revalidation");
        });
        await Test("credential read lock ends after headers while SSE keeps account permit", async () =>
        {
            var release = Signal(); var scenario = new Scenario
            {
                Model = async c => { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text); await c.Response.Body.FlushAsync(); await release.Task.WaitAsync(c.RequestAborted); await c.Response.WriteAsync(End); }
            };
            await using var f = await Fixture.Create(scenario); using var result = await f.Post(headers: true);
            Check(f.Pool.Status.Active == 1, "stream still active"); await f.Manager.RefreshAsync(Id).WaitAsync(TimeSpan.FromSeconds(5));
            Check(scenario.RefreshCount == 2 && f.Pool.Status.Active == 1, "refresh not blocked for entire SSE");
            release.SetResult(); await result.Content.ReadAsStringAsync(); await Wait(() => f.Pool.Status.Active == 0);
        });
        await Test("control redirects and malformed refresh responses cannot send model requests", async () =>
        {
            foreach (int mode in new[] { 0, 1, 2 })
            {
                var scenario = new Scenario
                {
                    Refresh = c =>
                {
                    if (mode == 0) { c.Response.StatusCode = 307; c.Response.Headers.Location = "/model"; return Task.CompletedTask; }
                    return c.Response.WriteAsync(mode == 1 ? "{bad" : "{\"code\":0,\"data\":{}}");
                }
                };
                await using var f = await Fixture.Create(scenario); using var result = await f.Post();
                Check((int)result.StatusCode == 503 && scenario.ModelCount == 0 && scenario.RefreshCount == 1 && f.Ledger.Health.Seq == 0, "invalid control response");
            }
        });
        await Test("bounded oversized control response and control timeout release account", async () =>
        {
            foreach (bool timeout in new[] { false, true })
            {
                var scenario = new Scenario
                {
                    Refresh = async c =>
                {
                    if (timeout) { try { await Task.Delay(Timeout.Infinite, c.RequestAborted); } catch (OperationCanceledException) { } }
                    else await c.Response.WriteAsync(new string('x', (1 << 20) + 1));
                }
                };
                await using var f = await Fixture.Create(scenario, timeout ? TimeSpan.FromMilliseconds(80) : null); using var result = await f.Post();
                await Wait(() => f.Pool.Status.Active == 0); Check((int)result.StatusCode == 503 && scenario.ModelCount == 0 && f.Ledger.Health.Seq == 0, "bounded control cleanup");
            }
        });
        await Test("new upstream rejection remains single-send with no manufactured charge", async () =>
        {
            var scenario = new Scenario { Model = c => { c.Response.StatusCode = 400; return c.Response.WriteAsJsonAsync(new { code = 11128 }); } };
            await using var f = await Fixture.Create(scenario); using var result = await f.Post();
            Check((int)result.StatusCode == 400 && scenario.ModelCount == 1 && f.Ledger.GetUsage(f.KeyId).Requests == 0 && f.Pool.Status.Active == 0, "no retry after refresh");
        });
        return passed;
    }
    static async Task Wait(Func<bool> predicate)
    {
        using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5)); while (!predicate()) await Task.Delay(5, timeout.Token);
    }
    sealed class Fixture(WebApplication upstream, WebApplication gateway, HttpClient client, AccountPool pool, CredentialManager manager,
        MemoryPersistence files, LoopbackControlPlane control, KeyStore keys, CreditLedger ledger, string keyId) : IAsyncDisposable
    {
        public HttpClient Client { get; } = client;
        public AccountPool Pool { get; } = pool;
        public CredentialManager Manager { get; } = manager;
        public MemoryPersistence Files { get; } = files;
        public CreditLedger Ledger { get; } = ledger;
        public string KeyId { get; } = keyId;
        public async Task<HttpResponseMessage> Post(bool stream = true, bool headers = false)
        {
            using var request = new HttpRequestMessage(HttpMethod.Post, "/v1/messages") { Content = new StringContent("{\"model\":\"m\",\"stream\":" + (stream ? "true" : "false") + ",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}", Encoding.UTF8, "application/json") };
            return await Client.SendAsync(request, headers ? HttpCompletionOption.ResponseHeadersRead : HttpCompletionOption.ResponseContentRead);
        }
        public static async Task<Fixture> Create(Scenario scenario, TimeSpan? timeout = null)
        {
            var clock = new Clock(); var builder = WebApplication.CreateSlimBuilder(); builder.Logging.ClearProviders(); builder.WebHost.ConfigureKestrel(k => k.Listen(IPAddress.Loopback, 0));
            var upstream = builder.Build(); Fixture? f = null; AccountSnapshot? account = null;
            upstream.MapPost("/v2/plugin/auth/token/refresh", async c =>
            {
                Interlocked.Increment(ref scenario.RefreshCount); Check(c.Request.Headers["X-Refresh-Token"].ToString() is "old-refresh" or "fresh-refresh", "refresh token");
                Check(!c.Request.Headers.ContainsKey("Cookie") && !c.Request.Headers.ContainsKey("Authorization"), "no caller headers on refresh");
                if (scenario.Refresh is not null) await scenario.Refresh(c);
                else await c.Response.WriteAsJsonAsync(new { code = 0, data = new { accessToken = "fresh-access", refreshToken = "fresh-refresh", expiresIn = 3600 } });
            });
            upstream.MapPost("/billing/meter/get-user-resource-summary", async c =>
            {
                Interlocked.Increment(ref scenario.VerifyCount); Check(c.Request.Headers.Authorization == "Bearer fresh-access", "verify candidate");
                if (scenario.Verify is not null) await scenario.Verify(c); else await c.Response.WriteAsJsonAsync(new { code = 0, data = new { } });
                scenario.BeforeModel?.Invoke(f!.Pool, account!);
            });
            upstream.MapPost("/model", async c =>
            {
                Interlocked.Increment(ref scenario.ModelCount); scenario.Access.Enqueue(c.Request.Headers.Authorization.ToString());
                if (scenario.Model is not null) await scenario.Model(c); else { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Text + End); }
            });
            await upstream.StartAsync(); var origin = new Uri(LocalGateway.Address(upstream)); var endpoint = new Uri(origin, "/model");
            account = new(Id, endpoint, scenario.Site); var pool = new AccountPool([account], time: clock); var files = new MemoryPersistence(Cred(scenario.Expires)) { After = scenario.FailDisk };
            var control = new LoopbackControlPlane(origin, timeout, clock); var manager = new CredentialManager(pool, [new(Id, endpoint, files, control)], clock);
            string path = Path.Combine(Path.GetTempPath(), "synthetic-maintenance-http-" + Guid.NewGuid().ToString("N")); Directory.CreateDirectory(path);
            var keys = new KeyStore(new KeyFiles(path)); var ledger = new CreditLedger(new LedgerFiles(path)); keys.AttachAccounting(ledger);
            var key = keys.Create(new("synthetic", QuotaTotal: 1000, MaxConcurrent: 8));
            var gateway = LocalGateway.Build(new(endpoint, new byte[32], Security: new(keys, new("synthetic-admin"), ledger), Accounts: pool, Credentials: manager));
            await gateway.StartAsync(); var client = new HttpClient(new SocketsHttpHandler { UseProxy = false }) { BaseAddress = new(LocalGateway.Address(gateway)), Timeout = TimeSpan.FromSeconds(8) };
            client.DefaultRequestHeaders.Authorization = new AuthenticationHeaderValue("Bearer", key.TakeSecret());
            f = new(upstream, gateway, client, pool, manager, files, control, keys, ledger, key.Item.Id); return f;
        }
        public async ValueTask DisposeAsync()
        {
            Client.Dispose(); await gateway.StopAsync(); await gateway.DisposeAsync(); await upstream.StopAsync(); await upstream.DisposeAsync(); control.Dispose(); Ledger.Dispose(); keys.Dispose();
        }
    }
}
