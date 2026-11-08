using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;

namespace AccountLogin;

// One flow owns its own cookie jar. Credentials/state never cross flow instances.
public sealed class LoopbackLoginFlow : ILoginFlow
{
    readonly Uri origin;
    readonly AccountSite site;
    readonly TimeProvider time;
    readonly TimeSpan timeout;
    readonly HttpClient client;
    JsonElement? grant;
    long issuedAt;
    LoginChallenge? challenge;
    public LoopbackLoginFlow(Uri origin, AccountSite site, TimeSpan? timeout = null, TimeProvider? time = null)
        :this(origin,site,timeout,time,false,null){}
    public static ILoginFlow ForWorkBuddy(AccountSite site,HttpMessageHandler? transport=null)=>
        new LoopbackLoginFlow(LoginOrigins.Control(site),site,TimeSpan.FromSeconds(30),null,true,transport);
    LoopbackLoginFlow(Uri origin,AccountSite site,TimeSpan? timeout,TimeProvider? time,bool production,HttpMessageHandler? transport)
    {
        if (!origin.IsAbsoluteUri || !(production?origin==LoginOrigins.Control(site):origin.Scheme=="http"&&origin.IsLoopback) || origin.UserInfo.Length != 0 ||
            origin.AbsolutePath != "/" || origin.Query.Length != 0 || origin.Fragment.Length != 0 || !Enum.IsDefined(site))
            throw new ArgumentException("Preview login requires a loopback origin and valid site");
        this.origin = origin; this.site = site; this.time = time ?? TimeProvider.System; this.timeout = timeout ?? TimeSpan.FromSeconds(15);
        if (this.timeout <= TimeSpan.Zero || this.timeout > TimeSpan.FromMinutes(1)) throw new ArgumentException("Invalid login control timeout");
        client = new(transport??new SocketsHttpHandler { UseProxy = false, AllowAutoRedirect = false, UseCookies = true, CookieContainer = new(), AutomaticDecompression = DecompressionMethods.None }) { Timeout = Timeout.InfiniteTimeSpan };
        client.DefaultRequestHeaders.UserAgent.ParseAdd("WorkBuddy-Gateway-CSharp/0.1");
    }
    async Task<JsonElement?> Request(HttpMethod method, string path, string? access, CancellationToken cancellation)
    {
        using var deadline = CancellationTokenSource.CreateLinkedTokenSource(cancellation); deadline.CancelAfter(timeout);
        using var request = new HttpRequestMessage(method, new Uri(origin, path));
        if (method == HttpMethod.Post) { request.Content = new ByteArrayContent("{}"u8.ToArray()); request.Content.Headers.ContentType = new("application/json"); }
        if (access is not null) request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", access);
        using var response = await client.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, deadline.Token);
        await using var stream = await response.Content.ReadAsStreamAsync(deadline.Token);
        using var buffer = new MemoryStream(); byte[] block = new byte[8192];
        while (true)
        {
            int n = await stream.ReadAsync(block, deadline.Token); if (n == 0) break;
            if (buffer.Length + n > 1 << 20) throw new LoginFailure("login_response_too_large");
            buffer.Write(block, 0, n);
        }
        if (!response.IsSuccessStatusCode) return null;
        using var doc = JsonDocument.Parse(buffer.ToArray()); var root = doc.RootElement;
        var code = LoginRules.Field(root, "code"); var data = LoginRules.Field(root, "data");
        if (root.ValueKind != JsonValueKind.Object || code.ValueKind != JsonValueKind.Undefined && (code.ValueKind != JsonValueKind.Number || !code.TryGetInt32(out int c) || c != 0) ||
            data.ValueKind != JsonValueKind.Object) return null;
        return data.Clone();
    }
    public async Task<LoginChallenge> BeginAsync(CancellationToken cancellation)
    {
        if (challenge is not null) throw new LoginFailure("login_flow_already_started");
        try
        {
            var raw = await Request(HttpMethod.Post, "/v2/plugin/auth/state?platform=" + Uri.EscapeDataString(LoginRules.Platform(site)), null, cancellation);
            if (raw is not { } data || LoginRules.Text(data, "state").Length == 0 || !Uri.TryCreate(LoginRules.Text(data, "authUrl"), UriKind.Absolute, out var auth) || !(origin.Scheme=="https"?LoginOrigins.AllowedChallenge(auth,site):auth.Scheme==origin.Scheme&&auth.Authority==origin.Authority) ||
                auth.UserInfo.Length != 0) throw new LoginFailure("invalid_login_challenge");
            return challenge = new(LoginRules.Text(data, "state"), auth);
        }
        catch (OperationCanceledException) when (cancellation.IsCancellationRequested) { throw; }
        catch (Exception e) when (e is HttpRequestException or IOException or JsonException or OperationCanceledException)
        { throw new LoginFailure("login_begin_failed"); }
    }
    public async Task<AccountCredential?> PollAsync(LoginChallenge pending, CancellationToken cancellation)
    {
        if (challenge != pending) throw new LoginFailure("login_challenge_mismatch");
        try
        {
            if (grant is null)
            {
                grant = await Request(HttpMethod.Get, "/v2/plugin/auth/token?state=" + Uri.EscapeDataString(pending.State), null, cancellation);
                if (grant is not { } g || LoginRules.Text(g, "accessToken").Length == 0) { grant = null; return null; }
                issuedAt = time.GetUtcNow().ToUnixTimeSeconds();
            }
            string bearer = LoginRules.Text(grant.Value, "accessToken");
            var account = await Request(HttpMethod.Get, "/v2/plugin/login/account?state=" + Uri.EscapeDataString(pending.State), bearer, cancellation);
            if (account is not { } acct || LoginRules.Text(acct, "uid").Length == 0) return null;
            var candidate = LoginRules.Candidate(grant.Value, acct, site, issuedAt);
            if (candidate.ExpiresAt <= time.GetUtcNow().ToUnixTimeSeconds()) throw new LoginFailure("login_token_expired");
            return candidate;
        }
        catch (OperationCanceledException) when (cancellation.IsCancellationRequested) { throw; }
        catch (Exception e) when (e is HttpRequestException or IOException or JsonException or OperationCanceledException or InvalidOperationException)
        { return null; }
    }
    public void Dispose() { grant = null; client.Dispose(); }
}
