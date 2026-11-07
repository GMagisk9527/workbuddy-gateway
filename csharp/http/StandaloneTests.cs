using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class StandaloneTests
{
    static void Check(bool condition,string message) { if(!condition) throw new InvalidOperationException(message); }
    sealed class Fake(AccountSite site) : HttpMessageHandler
    {
        internal int Models, Refreshes, Verifications;
        internal string Mode="ok";
        internal string ExpectedSystem="You are a helpful assistant.";
        internal string? ExpectedUser;
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage r,CancellationToken cancellation)
        {
            Check(!r.Headers.Contains("Cookie"),"No caller cookie");
            if(r.RequestUri==SiteEndpoints.Refresh(site))
            {
                Refreshes++; Check(r.Headers.GetValues("X-Refresh-Token").Single()=="synthetic-refresh" && r.Headers.Authorization is null,"Refresh credential only");
                return Json("{\"code\":0,\"data\":{\"accessToken\":\"fresh-access\",\"refreshToken\":\"fresh-refresh\",\"expiresIn\":3600}}");
            }
            if(r.RequestUri==SiteEndpoints.Verify(site))
            {
                Verifications++; Check(r.Headers.Authorization?.Parameter=="fresh-access","Verify candidate");
                return Json("{\"code\":0,\"data\":{}}");
            }
            Check(r.RequestUri==SiteEndpoints.Model(site),"Pinned model endpoint"); Models++;
            Check(r.Headers.Authorization?.Parameter=="fresh-access" && r.Headers.GetValues("X-User-Id").Single()=="synthetic-user","Account authentication");
            Check(r.Headers.UserAgent.ToString()=="WorkBuddy-Gateway-CSharp/0.1" && !r.Headers.Contains("X-Client-ID"),"Honest gateway identity");
            using var doc=JsonDocument.Parse(await r.Content!.ReadAsByteArrayAsync(cancellation));
            Check(doc.RootElement.GetProperty("messages")[0].GetProperty("role").GetString()=="system"&&doc.RootElement.GetProperty("messages")[0].GetProperty("content").GetString()==ExpectedSystem,"Shared system preparation applied before account send");
            if(ExpectedUser is not null)Check(doc.RootElement.GetProperty("messages").EnumerateArray().Last(m=>m.GetProperty("role").GetString()=="user").GetProperty("content").GetString()==ExpectedUser,"User text remains unchanged with configured prompt names");
            Check(doc.RootElement.GetProperty("stream").GetBoolean(),"SSE upstream");
            if(Mode=="timeout") { await Task.Delay(Timeout.InfiniteTimeSpan,cancellation); }
            if(Mode=="sse11140") return new(HttpStatusCode.OK){Content=new StringContent("data: {\"code\":11140,\"msg\":\"synthetic restricted\"}\n\n",Encoding.UTF8,"text/event-stream")};
            if(Mode=="403") return Json("forbidden",HttpStatusCode.Forbidden);
            if(Mode!="ok") return Json("{\"code\":"+Mode+",\"msg\":\"synthetic rejection\"}",HttpStatusCode.BadRequest);
            return new(HttpStatusCode.OK) {Content=new StringContent("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":2,\"prompt_tokens\":10,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")};
        }
        static HttpResponseMessage Json(string body,HttpStatusCode status=HttpStatusCode.OK)=>new(status){Content=new StringContent(body,Encoding.UTF8,"application/json")};
    }
    internal static async Task Run(string scratch)
    {
        string root=Path.GetFullPath(scratch); Directory.CreateDirectory(root);
        Check(!Directory.EnumerateFileSystemEntries(root).Any(),"Requires empty scratch directory"); int checks=0;
        foreach(var site in new[]{AccountSite.Cn,AccountSite.Intl})
        {
            foreach(string bad in new[]{"https://example.invalid/v2/chat/completions",SiteEndpoints.Model(site)+"?other=1","http://www.workbuddy.cn/v2/chat/completions"})
            { Check(!SiteEndpoints.Allowed(new(bad),site),"Reject arbitrary credential target"); checks++; }
            Check(!SiteEndpoints.Allowed(SiteEndpoints.Model(site==AccountSite.Cn?AccountSite.Intl:AccountSite.Cn),site),"Reject wrong site"); checks++;
            string dir=Path.Combine(root,site.ToString()); Directory.CreateDirectory(dir);
            var initial=new AccountCredential(new("synthetic-user",Site:site),"old-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+60);
            using var files=CredentialFiles.Create(Path.Combine(dir,"account"),initial);
            var endpoint=SiteEndpoints.Model(site); var id=new AccountId("synthetic-account"); var pool=new AccountPool([new(id,endpoint,site)]);
            var controlHandler=new Fake(site); using var control=new WorkBuddyControlPlane(site,controlHandler);
            var manager=new CredentialManager(pool,[new(id,endpoint,files,control)]);
            using var keys=PreviewKeyDirectory.Open(Path.Combine(dir,"keys")); using var ledger=CreditLedger.Open(Path.Combine(dir,"keys"),keys.List().Select(x=>x.Id)); keys.AttachAccounting(ledger);
            var issued=keys.Create(new("synthetic",QuotaTotal:1000,MaxConcurrent:8)); string token=issued.TakeSecret();
            var handler=new Fake(site){ExpectedSystem="offline-fallback\n\noffline-force"};
            await using var gateway=LocalGateway.Build(new(endpoint,new byte[32],Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,UpstreamHandler:handler,HeaderTimeout:TimeSpan.FromMilliseconds(300),SystemPrompts:new("offline-fallback","offline-force")));
            await gateway.StartAsync(); using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(gateway)),Timeout=TimeSpan.FromSeconds(10)};
            client.DefaultRequestHeaders.Authorization=new AuthenticationHeaderValue("Bearer",token); client.DefaultRequestHeaders.Add("Cookie","caller-cookie=synthetic");
            foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"}) foreach(bool stream in new[]{false,true})
            {
                object body=path=="/v1/responses"?new{model="test-model",input="hello",stream}:(object)new{model="test-model",messages=new[]{new{role="user",content="hello"}},stream};
                using var response=await client.PostAsJsonAsync(path,body); string raw=await response.Content.ReadAsStringAsync();
                Check(response.IsSuccessStatusCode && (stream?(path=="/v1/messages"?raw.Contains("message_stop"):raw.Contains("[DONE]")):raw.Contains("ok")),"Three protocols complete"); checks++;
            }
            Check(handler.Models==6 && controlHandler.Refreshes==1 && controlHandler.Verifications==1 && ledger.GetUsage(issued.Item.Id).Total==12 && ledger.Health.PendingAttempts==0,"Exactly one refresh/verification and one account call/settlement per request"); checks++;
            handler.Mode="11128";
            using(var refused=await client.PostAsJsonAsync("/v1/messages",new{model="test-model",messages=new[]{new{role="user",content="hello"}}})) {string refusal=await refused.Content.ReadAsStringAsync();Check((int)refused.StatusCode==400 && refusal.Contains("11128")&&refusal.Contains("渠道校验未通过"),"Channel rejection preserves code and gives an actionable explanation");}
            Check(handler.Models==7 && ledger.GetUsage(issued.Item.Id).Total==12 && !manager.Status.Single().Disabled,"11128 no retry and no charge"); checks++;
            byte[] retained=CredentialCodec.Export(files.Load().Credential); handler.Mode="11140";
            using(var refused=await client.PostAsJsonAsync("/v1/messages",new{model="test-model",messages=new[]{new{role="user",content="hello"}}})) Check((int)refused.StatusCode==400,"Restriction returned");
            Check(files.Load().Disabled && retained.SequenceEqual(CredentialCodec.Export(files.Load().Credential)),"Restriction persists without deleting tokens"); checks++;
            using(var stopped=await client.PostAsJsonAsync("/v1/messages",new{model="test-model",messages=new[]{new{role="user",content="hello"}}})) Check((int)stopped.StatusCode==503 && handler.Models==8,"Restricted account no longer scheduled"); checks++;
            foreach(string mode in new[]{"403","timeout","sse11140"})
            {
                await FailureScenario(Path.Combine(dir,mode),site,mode); checks++;
            }
            checks+=await PromptScenario(Path.Combine(dir,"prompt-text"),site);
        }
        Console.WriteLine(JsonSerializer.Serialize(new{standaloneChecks=checks,realNetworkCalls=0,modelCreditsUsed=0}));
    }
    static async Task<int> PromptScenario(string dir,AccountSite site)
    {
        using var files=CredentialFiles.Create(Path.Combine(dir,"account"),new(new("synthetic-user",Site:site),"fresh-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600));
        var id=new AccountId("synthetic");var endpoint=SiteEndpoints.Model(site);var pool=new AccountPool([new(id,endpoint,site)]);
        using var control=new WorkBuddyControlPlane(site,new Fake(site));var manager=new CredentialManager(pool,[new(id,endpoint,files,control)]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(dir,"keys"));using var ledger=CreditLedger.Open(Path.Combine(dir,"keys"),keys.List().Select(x=>x.Id));keys.AttachAccounting(ledger);
        var issued=keys.Create(new("prompt-text",QuotaTotal:1000,MaxConcurrent:8));var handler=new Fake(site){ExpectedUser="Claude Code user"};
        await using var app=LocalGateway.Build(new(endpoint,new byte[32],Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,UpstreamHandler:handler,SystemPrompts:new("Claude Code fallback","Claude Code force",new(Target:"Acme"))));
        await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(10)};client.DefaultRequestHeaders.Authorization=new("Bearer",issued.TakeSecret());
        int checks=0;
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})foreach(bool stream in new[]{false,true})
        {
            object body=path.EndsWith("responses",StringComparison.Ordinal)?new{model="test-model",stream,instructions="Claude Code rules",input="Claude Code user"}:
                path.EndsWith("messages",StringComparison.Ordinal)?(object)new{model="test-model",stream,system=new[]{new{type="text",text="Keep rules."},new{type="text",text="Claude Code rules"}},messages=new[]{new{role="user",content="Claude Code user"}}}:
                new{model="test-model",stream,messages=new[]{new{role="system",content="Claude Code rules"},new{role="user",content="Claude Code user"}}};
            handler.ExpectedSystem=(path.EndsWith("messages",StringComparison.Ordinal)?"Keep rules.\n":"")+"Acme rules\n\nAcme force";
            using var response=await client.PostAsJsonAsync(path,body);string text=await response.Content.ReadAsStringAsync();
            Check(response.IsSuccessStatusCode&&(stream?text.Contains(path.EndsWith("messages",StringComparison.Ordinal)?"message_stop":"[DONE]"):text.Contains("ok")),"Configured text works through all real HTTP protocol routes");checks++;
        }
        Check(handler.Models==6&&ledger.GetUsage(issued.Item.Id).Total==12&&ledger.Health.PendingAttempts==0,"Configured text retains exactly-once account calls and settlement");
        return checks+1;
    }
    static async Task FailureScenario(string dir,AccountSite site,string mode)
    {
        var credential=new AccountCredential(new("synthetic-user",Site:site),"fresh-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600);
        using var files=CredentialFiles.Create(Path.Combine(dir,"account"),credential);
        var id=new AccountId("synthetic"); var endpoint=SiteEndpoints.Model(site); var pool=new AccountPool([new(id,endpoint,site)]);
        using var control=new WorkBuddyControlPlane(site,new Fake(site)); var manager=new CredentialManager(pool,[new(id,endpoint,files,control)]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(dir,"keys")); using var ledger=CreditLedger.Open(Path.Combine(dir,"keys"),keys.List().Select(x=>x.Id)); keys.AttachAccounting(ledger);
        var issued=keys.Create(new("failure-scenario",QuotaTotal:100,MaxConcurrent:1)); var handler=new Fake(site){Mode=mode};
        await using var app=LocalGateway.Build(new(endpoint,new byte[32],Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,UpstreamHandler:handler,HeaderTimeout:TimeSpan.FromMilliseconds(100)));
        await app.StartAsync(); using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(5)};
        client.DefaultRequestHeaders.Authorization=new AuthenticationHeaderValue("Bearer",issued.TakeSecret());
        using var response=await client.PostAsJsonAsync("/v1/messages",new{model="test-model",stream=true,messages=new[]{new{role="user",content="hello"}}});
        string raw=await response.Content.ReadAsStringAsync();
        if(mode=="403") Check((int)response.StatusCode==403 && !manager.Status.Single().Disabled,"Plain 403 never stops account");
        if(mode=="timeout") Check((int)response.StatusCode==504 && !manager.Status.Single().Disabled,"Timeout bounded without stopping account");
        if(mode=="sse11140") Check(raw.Contains("11140") && !raw.Contains("message_stop") && manager.Status.Single().Disabled && files.Load().Disabled,"SSE restriction is durable and never completed");
        Check(handler.Models==1 && ledger.Health.PendingAttempts==0 && pool.Status.Active==0,"One call and all ownership released");
    }
}
