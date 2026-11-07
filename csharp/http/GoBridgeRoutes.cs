using System.Net.Http.Headers;
using KeySecurity;

namespace HttpMigration;

internal static class GoBridgeRoutes
{
    public static void Map(WebApplication app, LocalGatewayOptions options)
    {
        foreach (string path in new[] { "/v1/models", "/v1/usage", "/v1/messages/count_tokens", "/v1/search", "/v1/fetch" })
        {
            string endpoint = path;
            app.Map(endpoint, (HttpContext c, HttpClient client, SemaphoreSlim gate, PreparationAdmission resources) => Forward(c, client, gate, resources, options, endpoint));
        }
    }
    public static async Task<IResult> Ready(HttpClient client, LocalGatewayOptions options)
    {
        using var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(3));
        try
        {
            using var response = await client.GetAsync(new Uri(options.Upstream, "/readyz"), timeout.Token);
            return response.IsSuccessStatusCode ? GatewayJson.Result(new Dictionary<string, object?> { ["status"] = "ready", ["implementation"] = "csharp-protocols-go-accounts" }) : Results.StatusCode(503);
        }
        catch (Exception e) when (e is HttpRequestException or OperationCanceledException) { return Results.StatusCode(503); }
    }
    static async Task Forward(HttpContext c, HttpClient client, SemaphoreSlim gate, PreparationAdmission resources, LocalGatewayOptions options, string endpoint)
    {
        using var memory = resources.Acquire();
        bool admitted = false;
        try
        {
            c.Response.Headers["X-Gateway-Implementation"] = "csharp-protocols";
            bool get = endpoint is "/v1/models" or "/v1/usage";
            if (!(get ? HttpMethods.IsGet(c.Request.Method) : HttpMethods.IsPost(c.Request.Method))) { c.Response.StatusCode = 405; return; }
            string token = KeyRules.Token(c.Request.Headers.Authorization.ToString(), c.Request.Headers["x-api-key"].ToString());
            if (token == "") { c.Response.StatusCode = 401; return; }
            if (!await gate.WaitAsync(0, c.RequestAborted)) { c.Response.StatusCode = 503; return; }
            admitted = true;
            using var request = new HttpRequestMessage(get ? HttpMethod.Get : HttpMethod.Post, new Uri(options.Upstream, endpoint));
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token);
            if (!get)
            {
                if (c.Request.ContentLength > options.MaxRequestBytes) throw new RequestFailure("请求体超过大小限制", 413);
                var body = await LocalGateway.ReadBounded(c.Request.Body, options.MaxRequestBytes, c.RequestAborted, memory.Wire(c.Request.ContentLength));
                request.Content = new ByteArrayContent(body); request.Content.Headers.ContentType = new("application/json");
                if (c.Request.Headers.ContentEncoding.Count > 0) request.Content.Headers.ContentEncoding.Add(c.Request.Headers.ContentEncoding.ToString());
            }
            using var deadline = CancellationTokenSource.CreateLinkedTokenSource(c.RequestAborted); deadline.CancelAfter(TimeSpan.FromSeconds(60));
            using var response = await client.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, deadline.Token);
            byte[] output = await LocalGateway.ReadBounded(await response.Content.ReadAsStreamAsync(deadline.Token), 8 << 20, deadline.Token);
            c.Response.StatusCode = (int)response.StatusCode; c.Response.ContentType = response.Content.Headers.ContentType?.ToString() ?? "application/json";
            c.Response.Headers.CacheControl = "no-store"; await c.Response.Body.WriteAsync(output, c.RequestAborted);
        }
        catch (RequestFailure e) { c.Response.StatusCode = e.Status; }
        catch (OperationCanceledException) when (c.RequestAborted.IsCancellationRequested) { c.Abort(); }
        catch (Exception e) when (e is HttpRequestException or IOException or OperationCanceledException) { c.Response.StatusCode = 502; }
        finally { if (admitted) gate.Release(); }
    }
}
