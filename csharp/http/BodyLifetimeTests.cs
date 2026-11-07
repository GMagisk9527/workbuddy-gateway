using System.Net;
using System.Net.Http.Headers;
using System.Net.Sockets;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class BodyLifetimeTests
{
    static int checks;
    static void Check(bool value,string message){if(!value)throw new InvalidOperationException(message);checks++;}
    sealed class Store(AccountCredential credential):ICredentialPersistence
    {public PersistedCredential Load()=>new(credential);public void Save(PersistedCredential value){}public void Dispose(){}}
    sealed class Control:IAccountControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken ct)=>throw new InvalidOperationException("Unexpected refresh");
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken ct)=>Task.FromResult(CredentialVerification.Usable);
    }
    sealed class Backend:HttpMessageHandler
    {
        internal int Calls;internal int LastBytes;internal TaskCompletionSource? Entered,Release;
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken ct)
        {
            Calls++;byte[] body=await request.Content!.ReadAsByteArrayAsync(ct);LastBytes=body.Length;using var doc=JsonDocument.Parse(body);
            Check(doc.RootElement.GetProperty("max_tokens").GetInt32()==128,"Catalog default still applied");
            Check(doc.RootElement.GetProperty("messages")[0].GetProperty("role").GetString()=="system","System fallback still applied");
            Entered?.TrySetResult();if(Release is not null)await Release.Task.WaitAsync(ct);
            return new(HttpStatusCode.OK){Content=new StringContent("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"prompt_tokens\":11,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")};
        }
    }
    internal static async Task Run(string scratch)
    {
        checks=0;Directory.CreateDirectory(scratch);Check(!Directory.EnumerateFileSystemEntries(scratch).Any(),"Empty scratch");
        var endpoint=SiteEndpoints.Model(AccountSite.Cn);var id=new AccountId("synthetic");var pool=new AccountPool([new(id,endpoint)]);
        using var store=new Store(new(new("synthetic-user"),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+7200));
        var manager=new CredentialManager(pool,[new(id,endpoint,store,new Control())]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(scratch,"keys"));using var ledger=CreditLedger.Open(Path.Combine(scratch,"keys"),keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);var issued=keys.Create(new("synthetic",MaxConcurrent:4));
        var catalog=new ModelCatalog();catalog.Load(Encoding.UTF8.GetBytes("{\"schema\":5,\"catalogs\":{\"cn\":[{\"id\":\"m\",\"maxOutputTokens\":128}]}}"));
        var backend=new Backend();await using var app=LocalGateway.Build(new(endpoint,new byte[32],MaxRequestBytes:1<<20,MaxInFlightBodyBytes:2<<20,Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,UpstreamHandler:backend,Catalog:catalog));await app.StartAsync();
        using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(15)};client.DefaultRequestHeaders.Authorization=new("Bearer",issued.TakeSecret());
        var resources=app.Services.GetRequiredService<PreparationAdmission>();
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})foreach(bool stream in new[]{false,true})
        {
            string content=new('x',750<<10);object input=path.EndsWith("responses",StringComparison.Ordinal)?new{model="m",input=content,stream}:(object)new{model="m",messages=new[]{new{role="user",content}},stream};
            // Exercise both Kestrel body framing paths for every protocol.
            using HttpContent body=stream?new ByteArrayContent(JsonSerializer.SerializeToUtf8Bytes(input)):JsonContent.Create(input);
            body.Headers.ContentType=new MediaTypeHeaderValue("application/json");
            using var response=await client.PostAsync(path,body);string result=await response.Content.ReadAsStringAsync();
            Check(response.IsSuccessStatusCode,path+" should fit the budget after temporary buffers are released; got "+(int)response.StatusCode+" "+result);
            Check(stream?result.Contains(path.EndsWith("messages",StringComparison.Ordinal)?"message_stop":"[DONE]"):result.Contains("ok"),"Correct protocol completion");Check(resources.Used==0,"All reservations released");
        }
        Check(backend.Calls==6&&ledger.GetUsage(issued.Item.Id).Requests==6&&ledger.GetUsage(issued.Item.Id).ZeroCost==6&&ledger.Health.PendingAttempts==0,"One call and settlement per response");
        backend.Entered=new(TaskCreationOptions.RunContinuationsAsynchronously);backend.Release=new(TaskCreationOptions.RunContinuationsAsynchronously);
        var held=client.PostAsJsonAsync("/v1/responses",new{model="m",input=new string('x',750<<10),stream=false});
        try
        {
            await backend.Entered.Task.WaitAsync(TimeSpan.FromSeconds(5));Check(resources.Used>750<<10,"Retained request and catalog output remain budgeted");Check(resources.Used<=2L*backend.LastBytes,"Only retained buffers are charged while waiting");
            using var other=resources.Acquire();bool refused=false;try{other.Reserve(1<<20);}catch(RequestFailure e){refused=e.Status==503;}Check(refused,"Actual budget saturation is still rejected");
        }
        finally{backend.Release.TrySetResult();using var response=await held;Check(response.IsSuccessStatusCode,"Held response finishes");}
        Check(resources.Used==0,"Reservations released after held response");
        await InterruptedBodies(new Uri(LocalGateway.Address(app)),client.DefaultRequestHeaders.Authorization!.Parameter!,app,backend,ledger,issued.Item.Id);
        Console.WriteLine(JsonSerializer.Serialize(new{bodyLifetimeChecks=checks,syntheticCalls=backend.Calls,realNetworkCalls=0,modelCreditsUsed=0}));
    }

    static async Task InterruptedBodies(Uri address,string secret,WebApplication app,Backend backend,CreditLedger ledger,string key)
    {
        var resources=app.Services.GetRequiredService<PreparationAdmission>();
        var admission=app.Services.GetRequiredService<RequestAdmission>();
        int calls=backend.Calls;long requests=ledger.GetUsage(key).Requests;
        foreach(bool disconnect in new[]{false,true})
        {
            using(var tcp=new TcpClient{NoDelay=true})
            {
                await tcp.ConnectAsync(address.Host,address.Port);
                var stream=tcp.GetStream();
                byte[] header=Encoding.ASCII.GetBytes($"POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer {secret}\r\nContent-Type: application/json\r\nContent-Length: 65536\r\nConnection: close\r\n\r\n");
                await stream.WriteAsync(header);
                await stream.WriteAsync(new byte[20<<10]);
                for(int i=0;i<200&&resources.Used==0;i++)await Task.Delay(5);
                Check(resources.Used==65536,"A partial fixed-length upload reserves its declared buffer once");
                if(!disconnect)
                {
                    tcp.Client.Shutdown(SocketShutdown.Send);
                    using var reader=new StreamReader(stream,Encoding.ASCII);
                    string? status=await reader.ReadLineAsync().WaitAsync(TimeSpan.FromSeconds(3));
                    // Kestrel can close a peer's half-closed connection before an
                    // error response is writable; both outcomes reject the upload.
                    Check(status is null||status.Contains("400",StringComparison.Ordinal),"Truncated fixed-length body is rejected: "+status);
                }
            }
            for(int i=0;i<200&&(resources.Used!=0||admission.Status.Active!=0);i++)await Task.Delay(5);
            Check(resources.Used==0&&admission.Status.Active==0,"Interrupted upload releases request and memory admission");
            Check(backend.Calls==calls&&ledger.Health.PendingAttempts==0&&ledger.GetUsage(key).Requests==requests,"Interrupted upload never calls or charges upstream");
        }
    }
}
