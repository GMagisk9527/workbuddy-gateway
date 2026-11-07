using System.Net;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;
using StreamingMigration;

namespace HttpMigration;

internal static class FirstTokenTests
{
    static int checks;static void Check(bool ok,string why){if(!ok)throw new InvalidOperationException(why);checks++;}
    internal static async Task<int> Run(string root)
    {
        checks=0;
        using(var budget=new FirstTokenBudget(TimeSpan.FromMilliseconds(100),default))
        {
            await Task.Delay(120);Check(!budget.Token.IsCancellationRequested,"Unarmed request has no first-token deadline");
            budget.Arm();await Task.Delay(60);budget.Arm();await Task.Delay(70);Check(budget.Expired,"Re-arming does not reset the logical request budget");
            try{budget.Finish();throw new InvalidOperationException("Late finish accepted");}catch(ConversionFailure e){Check(e.Code==FirstTokenBudget.ErrorCode,"Late output cannot turn an expired request into success");}
        }
        using(var disabled=new FirstTokenBudget(TimeSpan.Zero,default)){disabled.Arm();await Task.Delay(30);Check(!disabled.Expired,"Explicit zero disables first-token timeout");}
        using(var settings=new ConsoleSettings(new()))
        {
            Check(settings.Upstream==UpstreamTimeouts.Default,"Go upstream timeout defaults");
            settings.AttachUpstream(new(TimeSpan.FromSeconds(42),TimeSpan.FromSeconds(43),TimeSpan.FromSeconds(44)));
            settings.Update("""{"upstream":{"firstTokenTimeoutSeconds":0}}""",settings.View().Revision);
            Check(settings.Upstream.FirstToken==TimeSpan.Zero&&settings.Upstream.Header==TimeSpan.FromSeconds(42),"Partial timeout override keeps explicit startup value");
            settings.Update("{}",settings.View().Revision);Check(settings.Upstream.FirstToken==TimeSpan.FromSeconds(44),"Removing timeout override restores startup baseline");
        }
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})foreach(bool stream in new[]{false,true})
            foreach(string mode in new[]{"header","comments","roles","text","reasoning","tool","finished","disabled","dns"})
                await Http(Path.Combine(root,Guid.NewGuid().ToString("N")),path,stream,mode);
        return checks;
    }
    sealed class Control:IAccountControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken token)=>throw new InvalidOperationException();
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken token)=>Task.FromResult(CredentialVerification.Usable);
    }
    sealed class Backend(string mode):HttpMessageHandler
    {
        public int Calls;
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken token)
        {
            Calls++;if(mode=="dns"&&Calls==1)throw new HttpRequestException(HttpRequestError.NameResolutionError,"synthetic DNS failure");if(mode=="header")await Task.Delay(Timeout.Infinite,token);
            var response=new HttpResponseMessage(HttpStatusCode.OK){Content=new StreamContent(new Frames(mode))};response.Content.Headers.ContentType=new("text/event-stream");return response;
        }
    }
    sealed class Frames:Stream
    {
        readonly List<(byte[] Data,int Delay)> frames=[];int index,offset;bool delayed;
        public Frames(string mode)
        {
            void Add(string s,int delay=0)=>frames.Add((Encoding.UTF8.GetBytes(s),delay));
            const string role="data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n";
            const string finish="data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":1}}\n\n";
            if(mode=="roles")Add(string.Concat(Enumerable.Repeat(role,70)));
            if(mode is "roles" or "comments"){for(int i=0;i<100;i++)Add(": keepalive\n\n",20);return;}
            if(mode=="finished"){Add(string.Concat(Enumerable.Repeat(role,70)));Add(finish);Add("data: [DONE]\n\n",280);return;}
            string delta=mode switch
            {
                "reasoning"=>"\"reasoning_content\":\"synthetic reasoning\"",
                "tool"=>"\"tool_calls\":[{\"index\":0,\"id\":\"call_synthetic\",\"type\":\"function\",\"function\":{\"name\":\"synthetic\",\"arguments\":\"{}\"}}]",
                _=>"\"content\":\"synthetic text\""
            };
            Add("data: {\"choices\":[{\"delta\":{"+delta+"}}]}\n\n",mode=="disabled"?280:0);
            Add(finish,280);Add("data: [DONE]\n\n");
        }
        public override async ValueTask<int> ReadAsync(Memory<byte> buffer,CancellationToken cancellationToken=default)
        {
            if(index==frames.Count)return 0;var frame=frames[index];if(!delayed){delayed=true;await Task.Delay(frame.Delay,cancellationToken);}
            int n=Math.Min(buffer.Length,frame.Data.Length-offset);frame.Data.AsMemory(offset,n).CopyTo(buffer);offset+=n;
            if(offset==frame.Data.Length){index++;offset=0;delayed=false;}return n;
        }
        public override bool CanRead=>true;public override bool CanSeek=>false;public override bool CanWrite=>false;
        public override long Length=>throw new NotSupportedException();public override long Position{get=>throw new NotSupportedException();set=>throw new NotSupportedException();}
        public override int Read(byte[] b,int o,int n)=>throw new NotSupportedException();public override void Flush()=>throw new NotSupportedException();public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long v)=>throw new NotSupportedException();public override void Write(byte[] b,int o,int n)=>throw new NotSupportedException();
    }
    static async Task Http(string root,string path,bool stream,string mode)
    {
        Directory.CreateDirectory(root);var endpoint=SiteEndpoints.Model(AccountSite.Cn);var id=new AccountId("a");
        using var files=CredentialFiles.Create(Path.Combine(root,"credential"),new(new("synthetic-user"),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.AddHours(2).ToUnixTimeSeconds()));
        var pool=new AccountPool([new AccountSnapshot(id,endpoint)]);var credentials=new CredentialManager(pool,[new(id,endpoint,files,new Control())]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(root,"keys"));using var ledger=CreditLedger.Open(Path.Combine(root,"keys"),[]);keys.AttachAccounting(ledger);var key=keys.Create(new("synthetic",QuotaTotal:100));
        var backend=new Backend(mode);var options=new LocalGatewayOptions(endpoint,new byte[32],Security:new(keys,new("admin"),ledger),Accounts:pool,Credentials:credentials,DirectUpstream:true,UpstreamHandler:backend,HeaderTimeout:TimeSpan.FromSeconds(2),IdleTimeout:TimeSpan.FromSeconds(2)){FirstTokenTimeout=TimeSpan.FromMilliseconds(mode=="disabled"?0:160)};
        await using var app=LocalGateway.Build(options);await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(4)};
        // Fast synthetic heartbeat exercises committed-SSE errors while header
        // and first-output deadlines are still pending, without real model calls.
        var settings=app.Services.GetRequiredService<ConsoleRuntime>().Settings;
        settings.AttachUpstream(settings.Upstream with{Keepalive=TimeSpan.FromMilliseconds(25)});
        if(mode=="dns")app.Services.GetRequiredService<RetryAdmission>().Configure(new(BackoffMaxMs:0));
        using var request=new HttpRequestMessage(HttpMethod.Post,path);request.Headers.Authorization=new("Bearer",key.TakeSecret());
        request.Content=new StringContent(JsonSerializer.Serialize(path.EndsWith("responses",StringComparison.Ordinal)?(object)new{model="m",input="synthetic",stream}:new{model="m",messages=new[]{new{role="user",content="synthetic"}},stream}),Encoding.UTF8,"application/json");
        using var response=await client.SendAsync(request);string body=await response.Content.ReadAsStringAsync();bool timeout=mode is "header" or "comments" or "roles";
        Check(stream?body.Contains(": keepalive"):!body.Contains(": keepalive"),"Heartbeat covers waiting/output only for streaming clients");
        if(timeout)
        {
            Check(body.Contains(FirstTokenBudget.ErrorCode)&&!body.Contains("[DONE]")&&!body.Contains("response.completed")&&!body.Contains("message_stop"),$"Timeout is an honest terminal error: {path} {stream} {mode}");
            Check((int)response.StatusCode==504||stream&&response.IsSuccessStatusCode,$"Timeout HTTP status before/after streaming begins: {path} {stream} {mode}");
        }
        else Check(response.IsSuccessStatusCode&&!body.Contains(FirstTokenBudget.ErrorCode),$"Valid increment/completion disarms timeout: {path} {stream} {mode}: {body}");
        for(int i=0;i<100&&app.Services.GetRequiredService<RequestAdmission>().Status.Active>0;i++)await Task.Delay(5);
        Check(backend.Calls==(mode=="dns"?2:1)&&ledger.Health.PendingAttempts==0&&pool.Status.Active==0&&app.Services.GetRequiredService<RequestAdmission>().Status.Active==0,"Only definitely-unsent DNS failure retries; accounting/admission released");
        if(!timeout)Check(ledger.GetUsage(key.Item.Id).Total==1,"Successful long response settles actual usage once");
    }
}
