using System.Net;
using System.Net.Http.Headers;
using System.Text.Json;

namespace AccountMaintenance;

// Explicit fake-server adapter. No production URL defaults, redirects, proxy or official-client identity.
public sealed class LoopbackControlPlane : IAccountControlPlane, IDisposable
{
    readonly Uri refresh, verify;
    readonly HttpClient client;
    readonly TimeProvider time;
    readonly TimeSpan timeout;
    public LoopbackControlPlane(Uri origin, TimeSpan? timeout = null, TimeProvider? time = null)
        : this(LoopbackEndpoint(origin, "/v2/plugin/auth/token/refresh"), LoopbackEndpoint(origin, "/billing/meter/get-user-resource-summary"), null, timeout, time) { }
    static Uri LoopbackEndpoint(Uri origin, string path)
    {
        if (!origin.IsAbsoluteUri || origin.Scheme != "http" || !origin.IsLoopback || origin.UserInfo.Length != 0 || origin.AbsolutePath != "/" || origin.Query.Length != 0 || origin.Fragment.Length != 0) throw new ArgumentException("Requires a loopback origin");
        return new(origin,path);
    }
    internal LoopbackControlPlane(Uri refresh, Uri verify, HttpMessageHandler? handler, TimeSpan? timeout, TimeProvider? time)
    {
        this.timeout = timeout ?? TimeSpan.FromSeconds(15);
        if (this.timeout <= TimeSpan.Zero || this.timeout > TimeSpan.FromMinutes(1)) throw new ArgumentException("Invalid control timeout");
        this.time = time ?? TimeProvider.System;
        this.refresh = refresh; this.verify = verify;
        client = new(handler ?? new SocketsHttpHandler { UseProxy = false, AllowAutoRedirect = false, UseCookies = false, AutomaticDecompression = DecompressionMethods.GZip | DecompressionMethods.Deflate }) { Timeout = Timeout.InfiniteTimeSpan };
    }
    internal sealed class Envelope
    {
        public int Code { get; set; }
        public string? Msg { get; set; }
        public JsonElement Data { get; set; }
    }
    async Task<(HttpStatusCode Status, byte[] Body)> Send(Uri endpoint, AccountCredential credential, bool refreshing, CancellationToken cancellation)
    {
        using var deadline = CancellationTokenSource.CreateLinkedTokenSource(cancellation); deadline.CancelAfter(timeout);
        using var request = new HttpRequestMessage(HttpMethod.Post, endpoint);
        request.Content = new ByteArrayContent(refreshing ? [] : "{}"u8.ToArray());
        request.Content.Headers.ContentType = new("application/json");
        if (refreshing) request.Headers.Add("X-Refresh-Token", credential.Refresh);
        else request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", credential.Access);
        if (credential.Identity.EnterpriseId.Length != 0) request.Headers.Add("X-Enterprise-Id", credential.Identity.EnterpriseId);
        using var response = await client.SendAsync(request, HttpCompletionOption.ResponseHeadersRead, deadline.Token);
        await using var stream = await response.Content.ReadAsStreamAsync(deadline.Token);
        using var buffer = new MemoryStream(); byte[] block = new byte[8192];
        while (true)
        {
            int n = await stream.ReadAsync(block, deadline.Token); if (n == 0) return (response.StatusCode, buffer.ToArray());
            if (buffer.Length + n > 1 << 20) throw new CredentialFailure("control_response_too_large");
            buffer.Write(block, 0, n);
        }
    }
    public async Task<AccountCredential> RefreshAsync(AccountCredential current, CancellationToken cancellation)
    {
        try
        {
            var result = await Send(refresh, current, true, cancellation);
            if ((int)result.Status is < 200 or >= 300) throw new CredentialFailure("refresh_rejected", CredentialRules.Classify(System.Text.Encoding.UTF8.GetString(result.Body)));
            var envelope = JsonSerializer.Deserialize(result.Body, CredentialJson.Default.Envelope) ?? throw new CredentialFailure("invalid_refresh_response");
            if (envelope.Code != 0) throw new CredentialFailure("refresh_rejected", CredentialRules.Classify(System.Text.Encoding.UTF8.GetString(result.Body)));
            return CredentialRules.Merge(current, System.Text.Encoding.UTF8.GetBytes(envelope.Data.GetRawText()), time.GetUtcNow().ToUnixTimeSeconds());
        }
        catch (OperationCanceledException) { throw new CredentialFailure("refresh_timeout"); }
        catch (HttpRequestException) { throw new CredentialFailure("refresh_transport_failed"); }
        catch (IOException) { throw new CredentialFailure("refresh_transport_failed"); }
        catch (JsonException) { throw new CredentialFailure("invalid_refresh_response"); }
        catch (InvalidOperationException) { throw new CredentialFailure("invalid_refresh_response"); }
    }
    internal async Task<byte[]> FetchCatalogAsync(Uri endpoint, AccountCredential credential, CancellationToken cancellation)
    {
        using var deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);deadline.CancelAfter(timeout);
        using var request=new HttpRequestMessage(HttpMethod.Get,endpoint);
        request.Headers.Authorization=new AuthenticationHeaderValue("Bearer",credential.Access);
        request.Headers.UserAgent.ParseAdd("WorkBuddy-Gateway-CSharp/0.1");
        if(credential.Identity.Uid.Length!=0)request.Headers.Add("X-User-Id",credential.Identity.Uid);
        if(credential.Identity.EnterpriseId.Length!=0)request.Headers.Add("X-Enterprise-Id",credential.Identity.EnterpriseId);
        if(credential.Domain.Length!=0)request.Headers.Add("X-Domain",credential.Domain);
        try
        {
            using var response=await client.SendAsync(request,HttpCompletionOption.ResponseHeadersRead,deadline.Token);
            if(!response.IsSuccessStatusCode)throw new CredentialFailure("catalog_http_rejected");
            if(response.Content.Headers.ContentLength is >8388608)throw new CredentialFailure("catalog_response_too_large");
            await using var stream=await response.Content.ReadAsStreamAsync(deadline.Token);
            using var buffer=new MemoryStream();byte[] block=new byte[8192];
            while(true)
            {
                int n=await stream.ReadAsync(block,deadline.Token);if(n==0)return buffer.ToArray();
                if(buffer.Length+n>8<<20)throw new CredentialFailure("catalog_response_too_large");
                buffer.Write(block,0,n);
            }
        }
        catch(OperationCanceledException) when(!cancellation.IsCancellationRequested){throw new CredentialFailure("catalog_timeout");}
        catch(HttpRequestException){throw new CredentialFailure("catalog_transport_failed");}
        catch(IOException){throw new CredentialFailure("catalog_transport_failed");}
    }
    public async Task<CredentialVerification> VerifyAsync(AccountCredential candidate, CancellationToken cancellation)
    {
        try
        {
            var result = await Send(verify, candidate, false, cancellation);
            if ((int)result.Status is < 200 or >= 300)
                return CredentialRules.AuthenticationRejected((int)result.Status, System.Text.Encoding.UTF8.GetString(result.Body)) ? CredentialVerification.Rejected : CredentialVerification.Unknown;
            var envelope = JsonSerializer.Deserialize(result.Body, CredentialJson.Default.Envelope);
            if (envelope is null) return CredentialVerification.Unknown;
            return envelope.Code == 0 ? CredentialVerification.Usable : CredentialRules.AuthenticationRejected(0, envelope.Msg ?? "") ? CredentialVerification.Rejected : CredentialVerification.Unknown;
        }
        catch (Exception e) when (e is OperationCanceledException or HttpRequestException or IOException or JsonException or CredentialFailure) { return CredentialVerification.Unknown; }
    }
    internal async Task<JsonElement> ResourceDataAsync(Uri endpoint,AccountCredential credential,byte[] payload,bool tenant,CancellationToken cancellation)
    {
        using var deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);deadline.CancelAfter(timeout);
        using var request=new HttpRequestMessage(HttpMethod.Post,endpoint){Content=new ByteArrayContent(payload)};request.Content.Headers.ContentType=new("application/json");
        request.Headers.Authorization=new("Bearer",credential.Access);request.Headers.UserAgent.ParseAdd("WorkBuddy-Gateway-CSharp/0.1");
        if(credential.Identity.Uid.Length>0)request.Headers.Add("X-User-Id",credential.Identity.Uid);
        if(credential.Identity.EnterpriseId.Length>0){request.Headers.Add("X-Enterprise-Id",credential.Identity.EnterpriseId);if(tenant)request.Headers.Add("X-Tenant-Id",credential.Identity.EnterpriseId);}
        if(credential.Domain.Length>0)request.Headers.Add("X-Domain",credential.Domain);
        try
        {
            using var response=await client.SendAsync(request,HttpCompletionOption.ResponseHeadersRead,deadline.Token);
            if(!response.IsSuccessStatusCode||response.Content.Headers.ContentLength is >1048576)throw new CredentialFailure("resource_http_rejected");
            await using var stream=await response.Content.ReadAsStreamAsync(deadline.Token);using var output=new MemoryStream();byte[] buffer=new byte[8192];
            while(true){int n=await stream.ReadAsync(buffer,deadline.Token);if(n==0)break;if(output.Length+n>1<<20)throw new CredentialFailure("resource_response_too_large");output.Write(buffer,0,n);}
            using var doc=JsonDocument.Parse(output.ToArray(),new JsonDocumentOptions{MaxDepth=32});var r=doc.RootElement;
            if(r.GetProperty("code").GetInt32()!=0||r.GetProperty("data").ValueKind!=JsonValueKind.Object)throw new CredentialFailure("resource_business_rejected");return r.GetProperty("data").Clone();
        }
        catch(OperationCanceledException) when(!cancellation.IsCancellationRequested){throw new CredentialFailure("resource_timeout");}
        catch(Exception e) when(e is JsonException or InvalidOperationException or KeyNotFoundException or FormatException or HttpRequestException or IOException){throw new CredentialFailure("resource_response_unavailable");}
    }
    public void Dispose() => client.Dispose();
    internal async Task<CheckinResult> CheckinAsync(AccountCredential credential,CancellationToken cancellation)
    {
        using var deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);deadline.CancelAfter(TimeSpan.FromSeconds(30));
        using var request=new HttpRequestMessage(HttpMethod.Post,new Uri("https://www.codebuddy.cn/v2/billing/meter/daily-checkin")){Content=new ByteArrayContent("{}"u8.ToArray())};
        request.Content.Headers.ContentType=new("application/json");request.Headers.Authorization=new("Bearer",credential.Access);request.Headers.UserAgent.ParseAdd("WorkBuddy-Gateway-CSharp/0.1");
        if(credential.Identity.Uid.Length>0)request.Headers.Add("X-User-Id",credential.Identity.Uid);
        if(credential.Domain.Length>0)request.Headers.Add("X-Domain",credential.Domain);
        using var response=await client.SendAsync(request,HttpCompletionOption.ResponseHeadersRead,deadline.Token);
        if(response.Content.Headers.ContentLength is >1048576)throw new CredentialFailure("checkin_response_too_large");
        await using var stream=await response.Content.ReadAsStreamAsync(deadline.Token);using var output=new MemoryStream();byte[] buffer=new byte[8192];
        while(true){int n=await stream.ReadAsync(buffer,deadline.Token);if(n==0)break;if(output.Length+n>1<<20)throw new CredentialFailure("checkin_response_too_large");output.Write(buffer,0,n);}
        return CheckinResponse.Parse((int)response.StatusCode,output.ToArray());
    }
}
