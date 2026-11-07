using System.Net;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class AgentToolTests
{
    static int checks;
    static void Check(bool b,string why){if(!b)throw new InvalidOperationException(why);checks++;}
    sealed class Control:IAccountControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken ct)=>throw new InvalidOperationException("Unexpected refresh");
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken ct)=>Task.FromResult(CredentialVerification.Usable);
    }
    sealed class Fake(AccountSite site,CreditLedger ledger):HttpMessageHandler
    {
        internal int Calls;internal int Status=200;internal string Body="";internal bool Disconnect;
        internal string? Payload;internal TaskCompletionSource? Entered,Resume;
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage req,CancellationToken ct)
        {
            Calls++;Check(ledger.Health.PendingAttempts==1,"WAL intent before send");
            Check(req.RequestUri==SiteEndpoints.AgentTool(site,false)||req.RequestUri==SiteEndpoints.AgentTool(site,true),"Pinned tool target");
            Check(req.Headers.Authorization?.Parameter=="synthetic-access","Own credential applied");
            Check(!req.Headers.Contains("Cookie")&&!req.Headers.Contains("x-api-key")&&!req.Headers.Contains("session_id"),"No client credentials/session leakage");
            Payload=await req.Content!.ReadAsStringAsync(ct);
            if(Entered is not null){Entered.TrySetResult();await Resume!.Task.WaitAsync(ct);}
            if(Disconnect)throw new HttpRequestException("synthetic private failure");
            var response=new HttpResponseMessage((HttpStatusCode)Status){Content=new StringContent(Body,Encoding.UTF8,"application/json")};
            if(Status==302)response.Headers.Location=new("https://example.invalid/credential-trap");return response;
        }
    }
    internal static async Task Run(string scratch)
    {
        checks=0;foreach(var site in new[]{AccountSite.Cn,AccountSite.Intl})await HttpChecks(Path.Combine(scratch,site.ToString()),site);
        Console.WriteLine(JsonSerializer.Serialize(new{agentToolChecks=checks,realModelCalls=0,productionChanged=false}));
    }
    static async Task HttpChecks(string dir,AccountSite site)
    {
        using var persisted=CredentialFiles.Create(Path.Combine(dir,"account"),new(new("uid",Site:site),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600));
        var id=new AccountId("tool-account");var endpoint=SiteEndpoints.Model(site);var pool=new AccountPool([new(id,endpoint,site)]);
        var manager=new CredentialManager(pool,[new(id,endpoint,persisted,new Control())]);
        using(var selected=await pool.AcquireAsync("web-search"))
        using(var credential=await manager.AcquireForHeadersAsync(selected,CancellationToken.None))
        {
            foreach(var target in new[]{new Uri("https://example.invalid/agenttool/v1/search"),SiteEndpoints.AgentTool(site==AccountSite.Cn?AccountSite.Intl:AccountSite.Cn,false),new Uri(SiteEndpoints.AgentTool(site,false)+"?redirect=1"),SiteEndpoints.Model(site)})
            {
                using var message=new HttpRequestMessage(HttpMethod.Post,target);
                try{credential.ApplyToAgentTool(message,false);throw new InvalidOperationException("Credential target accepted");}
                catch(CredentialFailure){Check(message.Headers.Authorization is null,"Reject target before attaching secret");}
            }
            using var tool=new HttpRequestMessage(HttpMethod.Post,SiteEndpoints.AgentTool(site,false));
            try{credential.ApplyTo(tool);throw new InvalidOperationException("Model lease target broadened");}catch(CredentialFailure){checks++;}
        }
        using var keys=PreviewKeyDirectory.Open(Path.Combine(dir,"keys"));using var ledger=CreditLedger.Open(Path.Combine(dir,"keys"),keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);
        var issued=keys.Create(new("tools",QuotaTotal:100,MaxConcurrent:2,Models:["web-search","web-fetch"]));string token=issued.TakeSecret();
        var forbidden=keys.Create(new("chat-only",QuotaTotal:100,Models:["m"]));string forbiddenToken=forbidden.TakeSecret();var fake=new Fake(site,ledger);
        await using var app=LocalGateway.Build(new(endpoint,new byte[32],MaxConcurrent:2,MaxRequestBytes:AgentToolProtocol.Limit,MaxInFlightBodyBytes:8<<20,Security:new(keys,new("admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,UpstreamHandler:fake));
        await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(10)};
        const string search="{\"query\":\"  中文 test  \",\"count\":99,\"need_content\":true,\"ori_question\":\"private\"}";
        const string result="{\"results\":[{\"title\":\"title\",\"site\":\"go.dev\",\"snippet\":\"text\"}],\"usage\":{\"credit\":2}}";
        async Task<string> Send(string path,string body,int status,string? auth=null,HttpMethod? method=null,CancellationToken ct=default,bool expectContinue=false)
        {
            using var req=new HttpRequestMessage(method??HttpMethod.Post,path){Content=new StringContent(body,Encoding.UTF8,"application/json")};
            req.Headers.ExpectContinue=expectContinue;req.Headers.Add("x-api-key",auth??token);req.Headers.Add("Cookie","sensitive");req.Headers.Add("session_id","sensitive-session");
            using var res=await client.SendAsync(req,ct);string text=await res.Content.ReadAsStringAsync(ct);
            Check((int)res.StatusCode==status,$"Expected {status}, got {(int)res.StatusCode}: {text}");Check(res.Headers.CacheControl?.NoStore==true,"No cache");
            if(status>=400)Check(!text.Contains("private")&&!text.Contains("synthetic-access"),"No upstream sensitive error echo");return text;
        }
        fake.Body=result;
        var success=await Send("/v1/search",search,200);Check(success.Contains("https://go.dev")&&success.Contains("\"date\":null"),"DSH search projection");
        using(var doc=JsonDocument.Parse(fake.Payload!)){var r=doc.RootElement;Check(r.GetProperty("query").GetString()=="中文 test"&&r.GetProperty("max_results").GetInt32()==20&&r.EnumerateObject().Count()==3,"Search payload normalized");}
        Check(ledger.GetUsage(issued.Item.Id).Total==2,"Measured search charge");
        fake.Body="{\"content\":\"# Page\",\"usage\":{\"credit\":0}}";
        await Send("/v1/fetch","{\"url\":\"https://example.invalid/private\"}",200);
        using(var doc=JsonDocument.Parse(fake.Payload!))Check(doc.RootElement.GetProperty("max_length").GetInt32()==100000,"Fetch upstream format");
        int before=fake.Calls;
        await Send("/v1/search",search,401,auth:"invalid");await Send("/v1/search",search,403,auth:forbiddenToken);await Send("/v1/search",search,405,method:HttpMethod.Get);
        await Send("/v1/search","{",400);await Send("/v1/search","{\"query\":\"x\"}",400);await Send("/v1/search","{\"query\":\"ok\",\"count\":1.5}",400);
        await Send("/v1/fetch","{\"url\":\"file:///etc/passwd\"}",400);
        // Exercise header-level rejection without racing an oversized HTTP/1 upload against connection close.
        // Full uploads and decoded size limits are separately covered by CountTokensTests/HttpTests.
        await Send("/v1/search",new string('a',AgentToolProtocol.Limit+1),413,expectContinue:true);
        Check(fake.Calls==before,"Validation and permissions before upstream");
        long unknown=ledger.GetUsage(issued.Item.Id).Unknown;
        fake.Body="{\"results\":[]}";await Send("/v1/search",search,200);Check(ledger.GetUsage(issued.Item.Id).Unknown==++unknown,"Missing usage unknown");
        fake.Body="{\"content\":\"\",\"usage\":{\"credit\":1}}";await Send("/v1/fetch","{\"url\":\"https://example.invalid\"}",502);Check(ledger.GetUsage(issued.Item.Id).Total==3,"Empty fetch still settles reported usage");
        foreach(int status in new[]{400,401,403,429})
        {fake.Status=status;fake.Body="private upstream rejection";await Send("/v1/search",search,status);Check(ledger.GetUsage(issued.Item.Id).Unknown==unknown,"Complete 4xx safely cancelled");Check(!manager.Status.Single().Disabled,"Ordinary rejection not account ban");}
        fake.Status=200;fake.Body="{\"code\":11128,\"msg\":\"private synthetic-access\"}";before=fake.Calls;
        string rejection=await Send("/v1/search",search,502);Check(rejection.Contains("11128")&&fake.Calls==before+1,"Business rejection preserved without retry");Check(ledger.GetUsage(issued.Item.Id).Unknown==unknown,"11128 safe cancellation");
        fake.Body="{\"code\":11128,\"usage\":{\"credit\":1}}";await Send("/v1/search",search,502);Check(ledger.GetUsage(issued.Item.Id).Total==4,"Rejection cannot erase reported usage");
        fake.Status=302;fake.Body="{}";before=fake.Calls;await Send("/v1/search",search,502);Check(fake.Calls==before+1&&ledger.GetUsage(issued.Item.Id).Unknown==++unknown,"Redirect not replayed and uncertain cost retained");
        fake.Status=200;fake.Body="not json";await Send("/v1/search",search,502);Check(ledger.GetUsage(issued.Item.Id).Unknown==++unknown,"Malformed success unknown");
        fake.Body=new string('x',AgentToolProtocol.Limit+1);await Send("/v1/search",search,502);Check(ledger.GetUsage(issued.Item.Id).Unknown==++unknown,"Oversized response unknown");
        fake.Disconnect=true;await Send("/v1/search",search,502);fake.Disconnect=false;Check(ledger.GetUsage(issued.Item.Id).Unknown==++unknown,"Network failure unknown without retry");
        fake.Body=result;fake.Entered=new(TaskCreationOptions.RunContinuationsAsynchronously);fake.Resume=new(TaskCreationOptions.RunContinuationsAsynchronously);
        using(var cancel=new CancellationTokenSource())
        {
            var pending=Send("/v1/search",search,200,ct:cancel.Token);await fake.Entered.Task.WaitAsync(TimeSpan.FromSeconds(5));cancel.Cancel();
            try{await pending;throw new InvalidOperationException("Expected cancellation");}catch(OperationCanceledException){checks++;}
            for(int i=0;i<100&&ledger.Health.PendingAttempts!=0;i++)await Task.Delay(10);
            Check(ledger.GetUsage(issued.Item.Id).Unknown==++unknown&&ledger.Health.PendingAttempts==0,"Cancelled send settled unknown");
        }
        fake.Entered=null;fake.Resume=null;
        fake.Body="{\"code\":11140,\"msg\":\"private\"}";before=fake.Calls;await Send("/v1/search",search,502);
        Check(fake.Calls==before+1&&manager.Status.Single().Disabled,"11140 blocks once, no account replay");
        await Send("/v1/search",search,503);Check(fake.Calls==before+1,"Blocked account never retried");
        Check(ledger.Health.PendingAttempts==0&&app.Services.GetRequiredService<PreparationAdmission>().Used==0,"No leaked intent or body reservations");
        await app.StopAsync();
    }
}
