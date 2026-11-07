using System.Collections.Concurrent;
using System.Net;
using System.Text.Json;
using AccountLogin;
using AccountMaintenance;
using AccountScheduling;
using HttpMigration;
using static LoginChecks.Fixtures;

namespace LoginChecks;

internal static class HttpLoginTests
{
    public static async Task<List<string>> Run()
    {
        var tests = new List<string>(); async Task Test(string name, Func<Task> action) { await action(); tests.Add(name); }
        await Test("pending polls complete and account bearer never leaks to unauthenticated endpoints", async () =>
        {
            await using var server = await Server.Create();
            server.Token = c => server.Tokens < 3 ? c.Response.WriteAsJsonAsync(new { code = 1, data = new { } }) : server.Grant(c);
            var dest = new Destination(); await using var coordinator = new LoginCoordinator(site => new LoopbackLoginFlow(server.Origin, site, time: server.Clock), Fast, server.Clock);
            var op = await coordinator.StartAsync(AccountSite.Cn, dest); var result = await Done(op);
            Check(result.Status == "done" && server.Tokens == 3 && server.Accounts == 1 && dest.Writes == 1 && dest.Saved!.Identity.Uid == "user-1", "pending then save");
        });
        await Test("account lookup retry retains a one-time token grant", async () =>
        {
            await using var server = await Server.Create(); server.Account = c =>
            {
                if (server.Accounts == 1) { c.Response.StatusCode = 503; return Task.CompletedTask; }
                return server.User(c);
            };
            await using var coordinator = new LoginCoordinator(site => new LoopbackLoginFlow(server.Origin, site, time: server.Clock), Fast, server.Clock);
            var op = await coordinator.StartAsync(AccountSite.Cn, new Destination()); Check((await Done(op)).Status == "done" && server.Tokens == 1 && server.Accounts == 2, "grant not consumed twice");
        });
        await Test("parallel sessions have independent cookie jars and correctly escaped states", async () =>
        {
            await using var server = await Server.Create(); var one = new Destination("one"); var two = new Destination("two");
            await using var coordinator = new LoginCoordinator(site => new LoopbackLoginFlow(server.Origin, site, time: server.Clock), Fast, server.Clock);
            var first = await coordinator.StartAsync(AccountSite.Cn, one); var second = await coordinator.StartAsync(AccountSite.Intl, two);
            var both = await Task.WhenAll(Done(first), Done(second));
            Check(both.All(x => x.Status == "done") && one.Saved!.Identity.Uid != two.Saved!.Identity.Uid &&
                two.Saved.Identity.Site == AccountSite.Intl && server.Tokens == 2 && server.Platforms.Contains("VSCode") && server.Platforms.Contains("workbuddy-ai"), "flow isolation");
        });
        await Test("challenge URLs cannot switch authority or use executable schemes", async () =>
        {
            foreach (string url in new[] { "javascript:alert(1)", "https://example.invalid/login", "http://user@127.0.0.1/login" })
            {
                await using var server = await Server.Create(); server.State = c => c.Response.WriteAsJsonAsync(new { code = 0, data = new { state = "state", authUrl = url } });
                await using var coordinator = new LoginCoordinator(site => new LoopbackLoginFlow(server.Origin, site), Fast);
                await Fail(async () => { await coordinator.StartAsync(AccountSite.Cn, new Destination()); }, "invalid_login_challenge"); Check(server.Tokens == 0, "no unsafe flow");
            }
        });
        await Test("redirected and missing-state challenges never begin token polling", async () =>
        {
            foreach (bool redirect in new[] { false, true })
            {
                await using var server = await Server.Create(); server.State = c =>
                {
                    if (redirect) { c.Response.StatusCode = 307; c.Response.Headers.Location = "/v2/plugin/auth/token"; return Task.CompletedTask; }
                    return c.Response.WriteAsJsonAsync(new { code = 0, data = new { authUrl = server.Origin + "login" } });
                };
                await using var coordinator = new LoginCoordinator(site => new LoopbackLoginFlow(server.Origin, site), Fast);
                await Fail(async () => { await coordinator.StartAsync(AccountSite.Cn, new Destination()); }, "invalid_login_challenge"); Check(server.Tokens == 0 && coordinator.Active == 0, "challenge refused");
            }
        });
        await Test("oversized challenge body is bounded and does not retain an active slot", async () =>
        {
            await using var server = await Server.Create(); server.State = c => c.Response.WriteAsync(new string('x', (1 << 20) + 1));
            await using var coordinator = new LoginCoordinator(site => new LoopbackLoginFlow(server.Origin, site), Fast);
            await Fail(async () => { await coordinator.StartAsync(AccountSite.Cn, new Destination()); }, "login_response_too_large"); Check(coordinator.Active == 0, "bounded response");
        });
        await Test("challenge control timeout releases an unpublished flow", async () =>
        {
            await using var server = await Server.Create(); server.State = async c => { try { await Task.Delay(Timeout.Infinite, c.RequestAborted); } catch (OperationCanceledException) { } };
            await using var coordinator = new LoginCoordinator(site => new LoopbackLoginFlow(server.Origin, site, TimeSpan.FromMilliseconds(80)), Fast);
            await Fail(async () => { await coordinator.StartAsync(AccountSite.Cn, new Destination()); }, "login_begin_failed"); Check(coordinator.Active == 0, "begin timeout");
        });
        await Test("overall login deadline cancels a hung HTTP token poll", async () =>
        {
            await using var server = await Server.Create(); server.Token = async c => { try { await Task.Delay(Timeout.Infinite, c.RequestAborted); } catch (OperationCanceledException) { } };
            var dest = new Destination(); await using var coordinator = new LoginCoordinator(site => new LoopbackLoginFlow(server.Origin, site), Fast with { Timeout = TimeSpan.FromMilliseconds(100) });
            var op = await coordinator.StartAsync(AccountSite.Cn, dest); Check((await Done(op)).Status == "expired" && dest.Writes == 0, "deadline controls HTTP");
        });
        await Test("missing account identity never creates a credential with an empty owner", async () =>
        {
            await using var server = await Server.Create(); server.Account = c => c.Response.WriteAsJsonAsync(new { code = 0, data = new { nickname = "unknown" } });
            var dest = new Destination(); await using var coordinator = new LoginCoordinator(site => new LoopbackLoginFlow(server.Origin, site), Fast with { Timeout = TimeSpan.FromMilliseconds(100) });
            var op = await coordinator.StartAsync(AccountSite.Cn, dest); Check((await Done(op)).Status == "expired" && dest.Writes == 0 && server.Tokens == 1, "incomplete owner");
        });
        await Test("invalid token expiry fails before committing a credential", async () =>
        {
            await using var server = await Server.Create(); server.Token = c => c.Response.WriteAsJsonAsync(new { code = 0, data = new { accessToken = "access-1", expiresIn = 0 } });
            var dest = new Destination(); await using var coordinator = new LoginCoordinator(site => new LoopbackLoginFlow(server.Origin, site, time: server.Clock), Fast, server.Clock);
            var op = await coordinator.StartAsync(AccountSite.Cn, dest); Check((await Done(op)).Status == "failed" && dest.Writes == 0, "expiry required");
        });
        await Test("token lifetime is anchored at receipt rather than account lookup retries", async () =>
        {
            await using var server = await Server.Create(); server.Token = c => c.Response.WriteAsJsonAsync(new { code = 0, data = new { accessToken = "access-1", expiresIn = 1 } });
            server.Account = c => { server.Clock.Now += TimeSpan.FromSeconds(2); return server.User(c); };
            var dest = new Destination(); await using var coordinator = new LoginCoordinator(site => new LoopbackLoginFlow(server.Origin, site, time: server.Clock), Fast, server.Clock);
            var op = await coordinator.StartAsync(AccountSite.Cn, dest); Check((await Done(op)).Error == "login_token_expired" && dest.Writes == 0, "no lifetime extension");
        });
        return tests;
    }
    sealed class Server(WebApplication app, Uri origin) : IAsyncDisposable
    {
        public Uri Origin { get; } = origin;
        public Clock Clock { get; } = new();
        public Func<HttpContext, Task>? State, Token, Account;
        public int Begins, Tokens, Accounts;
        public ConcurrentBag<string> Platforms { get; } = new();
        static string StateFor(string id) => "state +/&?汉-" + id;
        string Cookie(HttpContext c) => c.Request.Cookies["login"] ?? throw new InvalidOperationException("missing per-flow cookie");
        public Task Grant(HttpContext c)
        {
            string id = Cookie(c); Check(c.Request.Query["state"] == StateFor(id), "query encoding");
            return c.Response.WriteAsJsonAsync(new { code = 0, data = new { accessToken = "access-" + id, refreshToken = "refresh-" + id, expiresIn = 3600, domain = "synthetic" } });
        }
        public Task User(HttpContext c)
        {
            string id = Cookie(c); Check(c.Request.Headers.Authorization == "Bearer access-" + id, "candidate bearer and cookie isolation");
            return c.Response.WriteAsJsonAsync(new { code = 0, data = new { uid = "user-" + id, enterpriseId = "enterprise", nickname = "test" } });
        }
        public static async Task<Server> Create()
        {
            var builder = WebApplication.CreateSlimBuilder(); builder.Logging.ClearProviders(); builder.WebHost.ConfigureKestrel(k => k.Listen(IPAddress.Loopback, 0));
            var app = builder.Build(); Server? s = null;
            app.MapPost("/v2/plugin/auth/state", async c =>
            {
                string id = Interlocked.Increment(ref s!.Begins).ToString(System.Globalization.CultureInfo.InvariantCulture);
                Check(!c.Request.Headers.ContainsKey("Authorization"), "state unauthenticated"); s.Platforms.Add(c.Request.Query["platform"].ToString()); c.Response.Cookies.Append("login", id);
                if (s.State is not null) await s.State(c);
                else await c.Response.WriteAsJsonAsync(new { code = 0, data = new { state = StateFor(id), authUrl = new Uri(s.Origin, "/authorize?id=" + id).ToString() } });
            });
            app.MapGet("/v2/plugin/auth/token", async c => { Interlocked.Increment(ref s!.Tokens); Check(!c.Request.Headers.ContainsKey("Authorization"), "poll unauthenticated"); await (s.Token?.Invoke(c) ?? s.Grant(c)); });
            app.MapGet("/v2/plugin/login/account", async c => { Interlocked.Increment(ref s!.Accounts); await (s.Account?.Invoke(c) ?? s.User(c)); });
            await app.StartAsync(); s = new(app, new(LocalGateway.Address(app))); return s;
        }
        public async ValueTask DisposeAsync() { await app.StopAsync(); await app.DisposeAsync(); }
    }
}
