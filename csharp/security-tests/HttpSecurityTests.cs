using System.Collections.Concurrent;
using System.Net;
using System.Text.Json;
using System.Text.Json.Nodes;
using HttpMigration;
using KeySecurity;
using static SecurityChecks.Fixtures;

namespace SecurityChecks;

internal static class HttpSecurityTests
{
    const string Admin = "synthetic-admin-key";
    const string Response = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n\n";
    static readonly object Request = new { model = "test-model", stream = true, messages = new[] { new { role = "user", content = "hi" } } };
    static async Task<JsonElement> Read(HttpResponseMessage r) => JsonSerializer.Deserialize<JsonElement>(await r.Content.ReadAsByteArrayAsync());
    static async Task Normal(HttpContext c) { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync(Response); }
    internal static async Task<List<string>> Run()
    {
        var passed = new List<string>();
        async Task Test(string name, Func<Task> action) { await action(); passed.Add(name); }
        await Test("HTTP invalid or absent key rejects before body and upstream", async () =>
        {
            await using var f = await Fixture.Create();
            foreach (string? token in new[] { null, "invalid", Admin })
            { using var r = await f.Model(token); var error = await Read(r); Check((int)r.StatusCode == 401 && error.GetProperty("error").GetProperty("type").GetString() == "authentication_error", "authentication error"); }
            Check(f.Requests.IsEmpty, "no upstream");
        });
        await Test("HTTP Bearer x-api-key raw-header and fallback compatibility", async () =>
        {
            await using var f = await Fixture.Create();
            foreach (var pair in new[] { ("Bearer " + Secret, ""), ("", Secret), (Secret, "") })
            { using var r = await f.Headers(pair.Item1, pair.Item2); Check(r.IsSuccessStatusCode, "accepted header"); }
            using var invalid = await f.Headers("Bearer invalid", Secret); Check((int)invalid.StatusCode == 401 && f.Requests.Count == 3, "no alternate fallback from invalid header");
        });
        await Test("admin and model credentials have separate roles", async () =>
        {
            await using var f = await Fixture.Create(); using var wrong = await f.Admin(new { action = "create" }, Secret); Check((int)wrong.StatusCode == 401, "model key cannot create");
            using var query = await f.Client.GetAsync("/admin/api/keys?key=" + Admin); Check((int)query.StatusCode == 401, "no query admin credential");
            Check(f.Store.List().Length == 1 && f.Requests.IsEmpty, "no side effects");
        });
        await Test("admin create list and update never reveal secret or hash in metadata", async () =>
        {
            await using var f = await Fixture.Create(); using var created = await f.Admin(new { action = "create", name = "created", models = new[] { " TEST-MODEL " }, maxConcurrent = 2 }); var payload = await Read(created);
            string secret = payload.GetProperty("key").GetString()!, id = payload.GetProperty("item").GetProperty("id").GetString()!;
            using var response = await f.Model(secret); Check(response.IsSuccessStatusCode, "created usable");
            using var list = await f.AdminGet(); string listed = await list.Content.ReadAsStringAsync(); Check(!listed.Contains(secret, StringComparison.Ordinal) && !listed.Contains(KeyRules.Hash(secret), StringComparison.Ordinal) && list.Headers.CacheControl?.NoStore == true, "no metadata disclosure");
            using var updated = await f.Admin(new { action = "update", id, name = "updated", models = new[] { "another" } }); Check(updated.IsSuccessStatusCode, "update");
            using var denied = await f.Model(secret); Check((int)denied.StatusCode == 403 && f.Requests.Count == 1, "updated model permission");
        });
        await Test("admin rotation and deletion require confirmation and invalidate keys", async () =>
        {
            await using var f = await Fixture.Create(); using var noConfirm = await f.Admin(new { action = "rotate", id = "fixture" }); Check((int)noConfirm.StatusCode == 400, "rotate confirmation");
            using var rotate = await f.Admin(new { action = "rotate", id = "fixture", confirm = true }); string next = (await Read(rotate)).GetProperty("key").GetString()!;
            using var old = await f.Model(Secret); using var success = await f.Model(next); Check((int)old.StatusCode == 401 && success.IsSuccessStatusCode, "rotated keys");
            using var deleteNo = await f.Admin(new { action = "delete", id = "fixture" }); Check((int)deleteNo.StatusCode == 400, "delete confirmation");
            using var deleted = await f.Admin(new { action = "delete", id = "fixture", confirm = true }); using var nowDenied = await f.Model(next);
            Check(deleted.IsSuccessStatusCode && (int)nowDenied.StatusCode == 401 && f.Store.RequireAuth && f.Store.List().IsEmpty, "last delete stays closed");
        });
        await Test("HTTP expiry disable model policy and unimplemented quotas preserve error classes", async () =>
        {
            foreach (var spec in new[] { (Store(enabled: false), 403, "key_disabled"), (Store(expires: 1), 403, "key_expired"), (Store(models: ["other"]), 403, "model_not_allowed"), (Store(quota: 1), 503, "ledger_unavailable") })
            {
                await using var f = await Fixture.Create(spec.Item1); using var r = await f.Model(Secret); var body = await Read(r);
                Check((int)r.StatusCode == spec.Item2 && body.GetProperty("error").GetProperty("code").GetString() == spec.Item3 && f.Requests.IsEmpty, "policy denial");
            }
        });
        await Test("invalid body and method release per-key permit", async () =>
        {
            await using var f = await Fixture.Create(); using var invalid = await f.Headers("Bearer " + Secret, "", "{bad"); Check((int)invalid.StatusCode == 400, "invalid body");
            using var get = new HttpRequestMessage(HttpMethod.Get, "/v1/messages"); get.Headers.TryAddWithoutValidation("Authorization", "Bearer " + Secret); using var unsupported = await f.Client.SendAsync(get); Check((int)unsupported.StatusCode == 405, "method");
            using var valid = await f.Model(Secret); Check(valid.IsSuccessStatusCode && f.Requests.Count == 1, "released permits");
        });
        await Test("HTTP key concurrency rejects while request is active and recovers on disconnect", async () =>
        {
            var cancelled = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously); int calls = 0;
            await using var f = await Fixture.Create(respond: async c =>
            {
                if (Interlocked.Increment(ref calls) > 1) { await Normal(c); return; }
                c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"); await c.Response.Body.FlushAsync();
                try { await Task.Delay(Timeout.InfiniteTimeSpan, c.RequestAborted); } catch (OperationCanceledException) { cancelled.TrySetResult(); }
            });
            using (var first = await f.Model(Secret, headersOnly: true))
            { using var second = await f.Model(Secret); Check((int)second.StatusCode == 429 && (await Read(second)).GetProperty("error").GetProperty("type").GetString() == "rate_limit_error", "key slot bound"); }
            await cancelled.Task.WaitAsync(TimeSpan.FromSeconds(5)); await f.WaitPermit(Secret); using var recovered = await f.Model(Secret); Check(recovered.IsSuccessStatusCode && f.Requests.Count == 2, "disconnect releases");
        });
        await Test("global admission failure releases the already-acquired key permit", async () =>
        {
            var cancelled = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously); int calls = 0;
            await using var f = await Fixture.Create(maxConcurrent: 1, respond: async c =>
            {
                if (Interlocked.Increment(ref calls) > 1) { await Normal(c); return; }
                c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"); await c.Response.Body.FlushAsync();
                try { await Task.Delay(Timeout.InfiniteTimeSpan, c.RequestAborted); } catch (OperationCanceledException) { cancelled.TrySetResult(); }
            });
            string other = f.Store.Create(new()).TakeSecret();
            using (var held = await f.Model(Secret, headersOnly: true))
                for (int i = 0; i < 2; i++) { using var denied = await f.Model(other); Check((int)denied.StatusCode == 503, "global full not leaked key slot"); }
            await cancelled.Task.WaitAsync(TimeSpan.FromSeconds(5)); await f.WaitPermit(Secret); using var after = await f.Model(other); Check(after.IsSuccessStatusCode && f.Requests.Count == 2, "global released");
        });
        await Test("HTTP opaque reasoning cannot cross managed credentials", async () =>
        {
            await using var f = await Fixture.Create(respond: async c => { c.Response.ContentType = "text/event-stream"; await c.Response.WriteAsync("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"PRIVATE_THOUGHT\"}}]}\n\n" + Response); });
            string other = f.Store.Create(new()).TakeSecret(); var request = JsonSerializer.SerializeToNode(Request)!; request["stream"] = false;
            request["thinking"] = new JsonObject { ["type"] = "adaptive", ["display"] = "omitted" };
            using var response = await f.Model(Secret, body: request); var thought = (await Read(response)).GetProperty("content")[0]; Check(thought.GetProperty("thinking").GetString() == "", "hidden text");
            request["messages"] = new JsonArray(new JsonObject { ["role"] = "assistant", ["content"] = JsonNode.Parse("[" + thought.GetRawText() + "]") }, new JsonObject { ["role"] = "user", ["content"] = "continue" });
            using var replay = await f.Model(Secret, body: request); Check(replay.IsSuccessStatusCode && f.Requests.Last().GetProperty("messages")[0].GetProperty("reasoning_content").GetString() == "PRIVATE_THOUGHT", "same credential replay");
            using var cross = await f.Model(other, body: request); Check((int)cross.StatusCode == 400 && f.Requests.Count == 2, "different credential blocked");
        });
        await Test("HTTP persistence failure returns no secret and keeps old key metadata", async () =>
        {
            var backend = Store(); await using var f = await Fixture.Create(backend); backend.FailOnce("after-replace", "apikeys.json");
            using var failed = await f.Admin(new { action = "rotate", id = "fixture", confirm = true }); var payload = await Read(failed);
            Check((int)failed.StatusCode == 503 && !payload.TryGetProperty("key", out _) && f.Store.Authenticate(Secret) is not null, "no uncommitted response");
            using var blocked = await f.Model(Secret); Check((int)blocked.StatusCode == 503 && f.Requests.IsEmpty, "storage degraded");
        });
        await Test("admin rejects malformed IDs unknown fields and trailing JSON", async () =>
        {
            await using var f = await Fixture.Create();
            foreach (string raw in new[] { "{\"action\":\"delete\",\"id\":null,\"confirm\":true}", "{\"action\":\"create\",\"extra\":true}", "{\"action\":\"create\"} {}" })
            { using var r = await f.AdminRaw(raw); Check((int)r.StatusCode == 400, "malformed admin request"); }
            Check(f.Store.List().Length == 1, "admin input no side effects");
        });
        return passed;
    }
    sealed class Fixture(WebApplication upstream, WebApplication gateway, HttpClient client, KeyStore store, ConcurrentQueue<JsonElement> requests) : IAsyncDisposable
    {
        internal HttpClient Client { get; } = client;
        internal KeyStore Store { get; } = store;
        internal ConcurrentQueue<JsonElement> Requests { get; } = requests;
        internal Task<HttpResponseMessage> Model(string? token, bool headersOnly = false, object? body = null)
        {
            var message = new HttpRequestMessage(HttpMethod.Post, "/v1/messages") { Content = JsonContent.Create(body ?? Request) };
            if (token is not null) message.Headers.TryAddWithoutValidation("Authorization", "Bearer " + token);
            return Send(message, headersOnly);
        }
        async Task<HttpResponseMessage> Send(HttpRequestMessage message, bool headersOnly = false)
        { using (message) return await Client.SendAsync(message, headersOnly ? HttpCompletionOption.ResponseHeadersRead : HttpCompletionOption.ResponseContentRead); }
        internal Task<HttpResponseMessage> Headers(string auth, string alternate, string? raw = null)
        {
            var message = new HttpRequestMessage(HttpMethod.Post, "/v1/messages") { Content = raw is null ? JsonContent.Create(Request) : new StringContent(raw) };
            message.Headers.TryAddWithoutValidation("Authorization", auth); message.Headers.TryAddWithoutValidation("x-api-key", alternate); return Send(message);
        }
        internal Task<HttpResponseMessage> Admin(object body, string token = HttpSecurityTests.Admin) => AdminRaw(JsonSerializer.Serialize(body), token);
        internal Task<HttpResponseMessage> AdminRaw(string raw, string token = HttpSecurityTests.Admin)
        {
            var request = new HttpRequestMessage(HttpMethod.Post, "/admin/api/keys") { Content = new StringContent(raw) }; request.Headers.TryAddWithoutValidation("Authorization", "Bearer " + token); return Send(request);
        }
        internal Task<HttpResponseMessage> AdminGet()
        { var message = new HttpRequestMessage(HttpMethod.Get, "/admin/api/keys"); message.Headers.TryAddWithoutValidation("Authorization", "Bearer " + HttpSecurityTests.Admin); return Send(message); }
        internal async Task WaitPermit(string token)
        {
            using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(5));
            while (true)
            {
                try { using var permit = Store.Acquire(token); return; } catch (KeyFailure e) when (e.Code == "key_concurrency_exceeded") { await Task.Delay(10, timeout.Token); }
            }
        }
        internal static async Task<Fixture> Create(MemoryBackend? backend = null, Func<HttpContext, Task>? respond = null, int maxConcurrent = 4)
        {
            var builder = WebApplication.CreateSlimBuilder(); builder.Logging.ClearProviders(); builder.WebHost.ConfigureKestrel(k => k.Listen(IPAddress.Loopback, 0));
            var upstream = builder.Build(); var requests = new ConcurrentQueue<JsonElement>();
            upstream.MapPost("/upstream", async c => { using var body = await JsonDocument.ParseAsync(c.Request.Body, cancellationToken: c.RequestAborted); requests.Enqueue(body.RootElement.Clone()); await (respond ?? Normal)(c); });
            await upstream.StartAsync();
            var store = new KeyStore((backend ?? Fixtures.Store()).Open());
            var gateway = LocalGateway.Build(new(new Uri(LocalGateway.Address(upstream) + "/upstream"), new byte[32], MaxConcurrent: maxConcurrent, Security: new(store, new Credential(HttpSecurityTests.Admin))));
            await gateway.StartAsync();
            return new(upstream, gateway, new HttpClient(new SocketsHttpHandler { UseProxy = false }) { BaseAddress = new Uri(LocalGateway.Address(gateway)), Timeout = TimeSpan.FromSeconds(10) }, store, requests);
        }
        public async ValueTask DisposeAsync() { Client.Dispose(); await gateway.StopAsync(); await gateway.DisposeAsync(); await upstream.StopAsync(); await upstream.DisposeAsync(); Store.Dispose(); }
    }
}
