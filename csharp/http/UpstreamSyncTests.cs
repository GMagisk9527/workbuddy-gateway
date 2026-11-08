using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;
using OpenAiProtocols;
using StreamingMigration;

namespace HttpMigration;

// Synthetic requests based on the public Go v1.14.44-46 behavior. No client
// installation, production credentials, or real model requests are involved.
internal static class UpstreamSyncTests
{
    static int checks;
    static void Check(bool condition,string message){if(!condition)throw new InvalidOperationException(message);checks++;}
    sealed class Clock:TimeProvider
    {internal DateTimeOffset Now=DateTimeOffset.FromUnixTimeSeconds(1800000000);public override DateTimeOffset GetUtcNow()=>Now;}
    sealed class Control:IAccountControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken ct)=>throw new InvalidOperationException("Unexpected refresh");
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken ct)=>Task.FromResult(CredentialVerification.Usable);
    }
    sealed class Fake:HttpMessageHandler
    {
        internal int Calls;internal string Body="";internal int Status=200;internal string Captured="";
        internal Func<HttpRequestMessage,int,HttpResponseMessage>? Reply;
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage r,CancellationToken ct)
        {Calls++;Captured=await r.Content!.ReadAsStringAsync(ct);return Reply?.Invoke(r,Calls)??new((HttpStatusCode)Status){Content=new StringContent(Body,Encoding.UTF8,"text/event-stream")};}
    }
    static string Frame(object delta,string? finish=null)=>"data: "+JsonSerializer.Serialize(new{choices=new[]{new{index=0,delta,finish_reason=finish}}})+"\n\n";
    static string Sample=>Frame(new{reasoning_content="I am Work"})+Frame(new{reasoning_content="Buddy. "})+
        Frame(new{content="I am Work"})+Frame(new{content="Buddy. https://WorkBuddy.dev C:\\WorkBuddy\\a.cs"})+
        Frame(new{tool_calls=new[]{new{index=0,id="call_WorkBuddy",type="function",function=new{name="WorkBuddy_tool",arguments="{\"agent\":\"WorkBuddy\"}"}}}})+
        "data: {\"choices\":[{\"index\":0,\"finish_reason\":\"tool_calls\"}],\"usage\":{\"credit\":2,\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_cache_hit_tokens\":80}}\n\ndata: [DONE]\n\n";
    internal static async Task Run(string scratch)
    {
        checks=0;Directory.CreateDirectory(scratch);Check(!Directory.EnumerateFileSystemEntries(scratch).Any(),"Empty scratch required");
        IdentityBoundaries();await Pricing(Path.Combine(scratch,"prices"));
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})foreach(bool stream in new[]{false,true})
            foreach(string mode in new[]{"on","off","mention","ambiguous","error"})await IdentityHttp(Path.Combine(scratch,Guid.NewGuid().ToString("N")),path,stream,mode);
        foreach(var site in new[]{AccountSite.Cn,AccountSite.Intl})foreach(string mode in new[]{"429","401","403","500","11128","11140","charged","network","all-reject"})
            await ToolFailover(Path.Combine(scratch,Guid.NewGuid().ToString("N")),site,mode);
        Console.WriteLine(JsonSerializer.Serialize(new{upstreamSyncChecks=checks,httpScenarios=48,realModelCalls=0,productionChanged=false}));
    }
    static void IdentityBoundaries()
    {
        var plan=new ResponseIdentityPlan([new("WorkBuddy","Codex")]);
        foreach(string text in new[]{"WorkBuddy", "I am WorkBuddy.","WorkBuddy_code WorkBuddy2 2WorkBuddy", "WorkBuddy.dev https://WorkBuddy.dev/x email@WorkBuddy.dev", "C:\\WorkBuddy\\foo /tmp/WorkBuddy/file", "\"/tmp/a WorkBuddy/file\" WorkBuddy", "中文 WorkBuddy 你好 😀WorkBuddy。", "WorkBuddy. Hello", "WorkBuddy suffix"})
        {
            string expected=plan.Restore(text);
            for(int i=0;i<=text.Length;i++)
            {
                var restorer=plan.CreateRestorer();string actual=restorer.Write(text[..i])+restorer.Write(text[i..])+restorer.Flush();
                Check(actual==expected,"All split points agree with complete restoration: "+text+" @"+i);
                Check(restorer.PendingLength==0,"Terminal flush releases carry");
            }
            var byCharacter=plan.CreateRestorer();var output=new StringBuilder();foreach(char c in text){output.Append(byCharacter.Write(c.ToString()));Check(byCharacter.PendingLength<=plan.HoldLength+3,"Streaming carry bounded independently of total output");}output.Append(byCharacter.Flush());Check(output.ToString()==expected,"Single character chunks preserve Unicode and boundaries");
        }
        Check(plan.Restore("https://WorkBuddy.dev C:\\WorkBuddy\\foo WorkBuddy_code") == "https://WorkBuddy.dev C:\\WorkBuddy\\foo WorkBuddy_code","URLs paths and identifiers are protected");
        Check(new ResponseIdentityPlan([new("WorkBuddy","Codex"),new("Codex","Other")]).Restore("WorkBuddy") == "Codex","Replacement output is not rescanned");
        var state=new ChatState("m",1<<20,responseRestore:plan);
        state.Feed("{\"choices\":[{\"index\":7,\"delta\":{\"content\":\"Work\"}}]}");
        state.Feed("{\"choices\":[{\"index\":2,\"delta\":{\"content\":\"Buddy\"},\"finish_reason\":\"stop\"}]}");
        var final=state.Feed("{\"choices\":[{\"index\":7,\"delta\":{\"content\":\"Buddy\"},\"finish_reason\":\"stop\"}]}");
        Check(final.Raw["choices"]![0]!["delta"]!["content"]!.GetValue<string>()=="Codex","Restoration buffers use actual choice indexes");
        using var user=JsonDocument.Parse("{\"messages\":[{\"role\":\"user\",\"content\":\"You are Codex.\"}]}");
        Check(ResponseIdentityFactory.Create(user.RootElement,new(Target:"WorkBuddy",ResponseRestore:true)) is null,"User declarations cannot enable restoration");
        using(var custom=JsonDocument.Parse("{\"instructions\":\"You are Custom Agent.\"}"))
        {
            var options=new IdentityOptions(Target:"助手😀",Replacements:new Dictionary<string,string>{{"Custom Agent","助手😀"}},ResponseRestore:true);
            var active=ResponseIdentityFactory.Create(custom.RootElement,options);
            Check(active?.Restore("助手😀") == "Custom Agent","Configured identity names and Unicode target use the request snapshot");
            Check(ResponseIdentityFactory.Create(custom.RootElement,options with{ResponseRestore=false}) is null&&active?.Restore("助手😀")=="Custom Agent","Disabling future requests does not mutate an existing plan");
        }
        foreach(string instructions in new[]{"```\nYou are Codex.\n```","> You are Codex.","You are Codex or Claude.","You are Codex. You are Claude."})
        {using var doc=JsonDocument.Parse(JsonSerializer.Serialize(new{instructions}));Check(ResponseIdentityFactory.Create(doc.RootElement,new(Target:"WorkBuddy",ResponseRestore:true)) is null,"Quoted code and ambiguous identities are ignored");}
    }
    static JsonObject Body(string path,bool stream,string declaration)
    {
        const string user="User code: const agent = 'Codex'; WorkBuddy_tool({agent:'WorkBuddy'});";
        var body=new JsonObject{["model"]="m",["stream"]=stream,["max_tokens"]=200};
        if(path.EndsWith("responses",StringComparison.Ordinal)){body["instructions"]=declaration;body["input"]=user;}
        else if(path.EndsWith("messages",StringComparison.Ordinal)){body["system"]=declaration;body["messages"]=new JsonArray(new JsonObject{["role"]="user",["content"]=user});}
        else body["messages"]=new JsonArray(new JsonObject{["role"]="developer",["content"]=declaration},new JsonObject{["role"]="user",["content"]=user});
        return body;
    }
    static IEnumerable<string> Visible(JsonElement root)
    {
        if(root.ValueKind==JsonValueKind.Object)foreach(var p in root.EnumerateObject())
        {
            if(p.Value.ValueKind==JsonValueKind.String&&p.Name is "text" or "thinking" or "reasoning_content" or "content" or "delta")yield return p.Value.GetString()!;
            else if(p.Value.ValueKind is JsonValueKind.Array or JsonValueKind.Object)foreach(var text in Visible(p.Value))yield return text;
        }
        else if(root.ValueKind==JsonValueKind.Array)foreach(var item in root.EnumerateArray())foreach(var text in Visible(item))yield return text;
    }
    static async Task IdentityHttp(string dir,string path,bool stream,string mode)
    {
        var endpoint=SiteEndpoints.Model(AccountSite.Cn);var id=new AccountId("a");var pool=new AccountPool([new(id,endpoint)]);
        using var files=CredentialFiles.Create(Path.Combine(dir,"account"),new(new("synthetic-user"),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+7200));
        var manager=new CredentialManager(pool,[new(id,endpoint,files,new Control())]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(dir,"keys"));using var ledger=CreditLedger.Open(Path.Combine(dir,"keys"),keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);
        var issued=keys.Create(new("offline"));var catalog=new ModelCatalog();var fake=new Fake{Body=mode=="error"?"{\"code\":11128,\"msg\":\"unapproved channel\"}":Sample,Status=mode=="error"?400:200};
        using var cache=CatalogCache.Open(Path.Combine(dir,"catalog"));
        await using var app=LocalGateway.Build(new(endpoint,Program.TestKey,Security:new(keys,new("admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,Catalog:catalog,CatalogCache:cache,UpstreamHandler:fake,SystemPrompts:new(Identity:new(Target:"WorkBuddy",ResponseRestore:mode!="off"))));
        await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(10)};client.DefaultRequestHeaders.Authorization=new("Bearer",issued.TakeSecret());
        string declaration=mode=="mention"?"Discuss Codex and code carefully.":mode=="ambiguous"?"You are Codex or Claude.":"You are Codex.";
        using var response=await client.PostAsJsonAsync(path,Body(path,stream,declaration));string wire=await response.Content.ReadAsStringAsync();
        Check(fake.Calls==1,"Identity restoration never retries a request");
        using(var captured=JsonDocument.Parse(fake.Captured))
        {
            string user=captured.RootElement.GetProperty("messages").EnumerateArray().First(m=>m.GetProperty("role").GetString()=="user").GetProperty("content").ToString();
            Check(user.Contains("const agent = 'Codex'",StringComparison.Ordinal)&&user.Contains("WorkBuddy_tool",StringComparison.Ordinal),"User instructions/code preserved through conversion");
        }
        if(mode=="error")Check((int)response.StatusCode==400&&wire.Contains("11128",StringComparison.Ordinal)&&!wire.Contains("response.completed",StringComparison.Ordinal)&&ledger.GetUsage(issued.Item.Id).Total==0,"Upstream rejection remains a rejection");
        else
        {
            Check(response.IsSuccessStatusCode,"Protocol request succeeds: "+path+" "+mode+" "+wire);
            var visible=new StringBuilder();
            if(stream){foreach(string line in wire.Split('\n').Where(x=>x.StartsWith("data: {",StringComparison.Ordinal))){using var d=JsonDocument.Parse(line[6..]);foreach(string v in Visible(d.RootElement))visible.Append(v);}}
            else{using var doc=JsonDocument.Parse(wire);foreach(string v in Visible(doc.RootElement))visible.Append(v);}
            string text=visible.ToString();
            Check(text.Contains(mode=="on"?"I am Codex.":"I am WorkBuddy.",StringComparison.Ordinal),"Visible output uses only the selected request identity: "+path+" "+mode+" "+text);
            Check(text.Contains("https://WorkBuddy.dev",StringComparison.Ordinal)&&text.Contains("C:\\WorkBuddy\\a.cs",StringComparison.Ordinal),"Output URLs and paths preserved");
            Check(wire.Contains("WorkBuddy_tool",StringComparison.Ordinal)&&wire.Contains("call_WorkBuddy",StringComparison.Ordinal)&&wire.Contains("WorkBuddy",StringComparison.Ordinal),"Tool name ID and argument identity preserved");
            Check(ledger.GetUsage(issued.Item.Id).Total==2&&catalog.PriceLabel(AccountSite.Cn,"m")=="收费(倍率未知)","Original measured credit settles once and updates price evidence");
            if(stream)Check(wire.Contains(path.EndsWith("responses",StringComparison.Ordinal)?"response.completed":path.EndsWith("messages",StringComparison.Ordinal)?"message_stop":"[DONE]",StringComparison.Ordinal),"Protocol emits expected successful terminal event");
        }
        Check(ledger.Health.PendingAttempts==0&&pool.Status.Active==0&&app.Services.GetRequiredService<PreparationAdmission>().Used==0,"All accounting/scheduling/body leases drained");
        await app.StopAsync();
        if(mode!="error")
        {
            var restored=new ModelCatalog();Check(cache.TryLoad(restored)&&restored.PriceLabel(AccountSite.Cn,"m")=="收费(倍率未知)","Usage arriving before first timer tick is saved on shutdown independently of logs");
            Check(!catalog.NeedsSave,"Successful cache save clears only the captured revision");
        }
    }
    static void LoadPrice(ModelCatalog catalog,AccountSite site,double? credits,DateTimeOffset? from=null,DateTimeOffset? until=null,double factor=0,string model="m")
    {
        var row=new JsonObject{["id"]=model,["maxOutputTokens"]=256};if(credits.HasValue)row["credits"]=credits.Value;
        var data=new JsonObject{["models"]=new JsonArray(row)};
        if(from.HasValue||until.HasValue)data["modelPromotions"]=new JsonArray(new JsonObject{["enabled"]=true,["modelIds"]=new JsonArray(model),["discount"]=new JsonObject{["factor"]=factor},["schedule"]=new JsonObject{["validFrom"]=from?.ToString("O"),["validUntil"]=until?.ToString("O")}});
        catalog.LoadLive(site,Encoding.UTF8.GetBytes(new JsonObject{["code"]=0,["data"]=data}.ToJsonString()));
    }
    static async Task Pricing(string dir)
    {
        var clock=new Clock();var catalog=new ModelCatalog(time:clock);
        LoadPrice(catalog,AccountSite.Cn,2);clock.Now=clock.Now.AddSeconds(1);catalog.ObserveUsage(AccountSite.Cn,"m","{\"credit\":0,\"total_tokens\":100}");
        Check(catalog.PriceLabel(AccountSite.Cn,"m")=="免费（实测）","New valid usage overrides conflicting catalog");
        using(var detail=JsonDocument.Parse(JsonSerializer.Serialize(catalog.PriceDetails())))Check(detail.RootElement.GetProperty("rates").GetProperty("cn|m").GetProperty("conflict").GetBoolean(),"Price detail exposes conflict using same decision as list");
        clock.Now=clock.Now.AddSeconds(1);LoadPrice(catalog,AccountSite.Cn,2);Check(catalog.PriceLabel(AccountSite.Cn,"m")=="免费（实测）","Refreshing same rule does not invalidate actual evidence");
        clock.Now=clock.Now.AddSeconds(1);catalog.ObserveUsage(AccountSite.Cn,"m","{\"credit\":0,\"total_tokens\":200}");
        var snapshot=catalog.Export();var restored=new ModelCatalog(time:clock);restored.Load(snapshot);Check(restored.PriceLabel(AccountSite.Cn,"m")=="免费（实测）","Usage survives snapshot without logs");
        clock.Now=clock.Now.AddSeconds(1);LoadPrice(catalog,AccountSite.Cn,3);Check(catalog.PriceLabel(AccountSite.Cn,"m")=="3x","Substantively changed official rule supersedes old usage");
        catalog.ObserveUsage(AccountSite.Cn,"m","{\"credit\":6,\"total_tokens\":300}");Check(catalog.PriceLabel(AccountSite.Cn,"m")=="3x","Usage never infers numeric multiplier");
        clock.Now=clock.Now.AddSeconds(1);LoadPrice(catalog,AccountSite.Cn,0);clock.Now=clock.Now.AddSeconds(1);catalog.ObserveUsage(AccountSite.Cn,"m","{\"credit\":1}");Check(catalog.PriceLabel(AccountSite.Cn,"m")=="收费(倍率未知)","Actual charge overrides free catalog without inventing rate");
        catalog.ObserveUsage(AccountSite.Cn,"unknown","{\"credit\":0,\"total_tokens\":99}");Check(catalog.PriceLabel(AccountSite.Cn,"unknown") is null,"Tiny sample cannot prove free");
        var promo=new ModelCatalog(time:clock);LoadPrice(promo,AccountSite.Cn,2,until:clock.Now.AddSeconds(10));LoadPrice(promo,AccountSite.Intl,1,model:"m-sg");
        Check(promo.PriceLabel(AccountSite.Intl,"m")=="1x"&&promo.PriceLabel(AccountSite.Intl,"m-sg")=="1x"&&promo.DefaultOutput("m",AccountSite.Intl)==256,"International aliases share capabilities and price");
        Check(promo.List()["data"]!.AsArray().Count==1,"International catalog does not duplicate display IDs");
        var pool=new AccountPool([new(new("cn"),SiteEndpoints.Model(AccountSite.Cn)),new(new("intl"),SiteEndpoints.Model(AccountSite.Intl),AccountSite.Intl)],time:clock);promo.AttachScheduling(pool);
        using(var lease=await pool.AcquireAffinityAsync("m",new("intl")))Check(lease.Account.Site==AccountSite.Cn,"Shared resolver prefers known free site");
        clock.Now=clock.Now.AddSeconds(1);promo.ObserveUsage(AccountSite.Cn,"m","{\"credit\":0,\"total_tokens\":200}");
        string revision=promo.RateRevision;clock.Now=clock.Now.AddSeconds(10);promo.RefreshPricing();
        Check(promo.PriceLabel(AccountSite.Cn,"m")=="2x"&&promo.RateRevision!=revision,"Promotion expires at read time and invalidates revision/evidence");
        using(var lease=await pool.AcquireAffinityAsync("m",new("intl")))Check(lease.Account.Site==AccountSite.Intl,"Expired promo also updates scheduling preference");
        var noBase=new ModelCatalog(time:clock);LoadPrice(noBase,AccountSite.Cn,null,until:clock.Now.AddSeconds(10),factor:.5);Check(noBase.PriceLabel(AccountSite.Cn,"m") is null,"Positive discount without base is unknown");
        LoadPrice(noBase,AccountSite.Cn,null,until:clock.Now.AddSeconds(10));Check(noBase.PriceLabel(AccountSite.Cn,"m")=="0x","Zero discount can explicitly establish free");
        noBase.ObserveUsage(AccountSite.Cn,"m","{\"credit\":0,\"total_tokens\":100}");var removed=new ModelCatalog(time:clock);removed.Load(noBase.Export());removed.LoadLive(AccountSite.Cn,"{\"code\":0,\"data\":{\"models\":[{\"id\":\"other\"}]}}"u8.ToArray());
        clock.Now=clock.Now.AddSeconds(11);Check(noBase.PriceLabel(AccountSite.Cn,"m") is null&&removed.PriceLabel(AccountSite.Cn,"m") is null,"Known free expiry survives model removal and restart");
        var future=new ModelCatalog(time:clock);LoadPrice(future,AccountSite.Cn,4,clock.Now.AddSeconds(10),clock.Now.AddSeconds(20),.5);Check(future.PriceLabel(AccountSite.Cn,"m")=="4x","Future promo uses positive base");clock.Now=clock.Now.AddSeconds(10);Check(future.PriceLabel(AccountSite.Cn,"m")=="2x","Future promo starts without refetch");
        future.ObserveProbeCost(AccountSite.Cn,"m",ModelCost.Free,clock.Now.ToUnixTimeSeconds());var restoredProbe=new ModelCatalog(time:clock);restoredProbe.Load(future.Export());Check(restoredProbe.PriceLabel(AccountSite.Cn,"m")=="免费（实测）","Probe evidence also persists in catalog snapshot");
        using var cache=CatalogCache.Open(dir);Check(cache.TrySave(future),"Extended snapshot checksum saves");var cached=new ModelCatalog(time:clock);Check(cache.TryLoad(cached)&&cached.PriceLabel(AccountSite.Cn,"m")==future.PriceLabel(AccountSite.Cn,"m"),"Extended snapshot checksum reloads");
    }
    static async Task ToolFailover(string dir,AccountSite site,string mode)
    {
        var endpoint=SiteEndpoints.Model(site);var ids=new[]{new AccountId("first"),new AccountId("second")};var pool=new AccountPool(ids.Select(id=>new AccountSnapshot(id,endpoint,site)));
        using var first=CredentialFiles.Create(Path.Combine(dir,"first"),new(new("synthetic-first",Site:site),"first-access","refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+7200));
        using var second=CredentialFiles.Create(Path.Combine(dir,"second"),new(new("synthetic-second",Site:site),"second-access","refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+7200));
        var manager=new CredentialManager(pool,[new(ids[0],endpoint,first,new Control()),new(ids[1],endpoint,second,new Control())]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(dir,"keys"));using var ledger=CreditLedger.Open(Path.Combine(dir,"keys"),keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);var issued=keys.Create(new("offline"));
        var headers=new List<string>();var accounts=new List<string>();var fake=new Fake();
        fake.Reply=(r,n)=>
        {
            Check(ledger.Health.PendingAttempts==1,"Each tool attempt has durable intent");
            headers.Add(string.Join("|",new[]{"X-Conversation-ID","X-Conversation-Request-ID","X-Conversation-Message-ID"}.Select(h=>r.Headers.GetValues(h).Single())));
            accounts.Add(r.Headers.Authorization!.Parameter!);
            if(mode=="network")throw new HttpRequestException("synthetic reset");
            bool reject=n==1||mode=="all-reject";int status=reject?mode is "11128" or "11140" or "charged"?403:mode=="all-reject"?429:int.Parse(mode):200;
            string body=reject?mode=="11128"?"{\"code\":11128}":mode=="11140"?"{\"code\":11140}":mode=="charged"?"{\"code\":403,\"usage\":{\"credit\":2}}":"{\"code\":999,\"message\":\"rejected\"}":"{\"results\":[],\"content\":\"synthetic page\",\"usage\":{\"credit\":2}}";
            return new((HttpStatusCode)status){Content=new StringContent(body,Encoding.UTF8,"application/json")};
        };
        await using var app=LocalGateway.Build(new(endpoint,Program.TestKey,Security:new(keys,new("admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,UpstreamHandler:fake));await app.StartAsync();
        using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(10)};client.DefaultRequestHeaders.Authorization=new("Bearer",issued.TakeSecret());client.DefaultRequestHeaders.Add("session_id","synthetic-session");
        bool retry=mode is "401" or "403" or "429" or "all-reject";
        using var response=await client.PostAsJsonAsync("/v1/"+(site==AccountSite.Intl?"fetch":"search"),site==AccountSite.Intl?(object)new{url="https://example.invalid"}:new{query="synthetic query"});
        string wire=await response.Content.ReadAsStringAsync();Check(fake.Calls==(retry?2:1),"Only proven uncharged rejections fail over: "+mode);
        if(retry){Check(headers.Distinct().Count()==1&&accounts.Distinct().Count()==2,"Failover preserves request metadata but changes credential");Check((int)response.StatusCode==(mode=="all-reject"?429:200),"Failover terminal status preserved");}
        else Check(!response.IsSuccessStatusCode,"Uncertain/restricted response never pretends success");
        Check(ledger.GetUsage(issued.Item.Id).Total==(retry&&mode!="all-reject"||mode=="charged"?2:0)&&ledger.GetUsage(issued.Item.Id).Unknown==(mode is "network" or "500"?1:0),"Tool usage charged exactly once with uncertainty preserved: "+mode);
        Check(first.Load().Disabled==(mode is "401" or "11140")&&!second.Load().Disabled,"Only explicit invalid/restricted credential is disabled");
        Check(ledger.Health.PendingAttempts==0&&pool.Status.Active==0,"No leaked failover permit or WAL intent");await app.StopAsync();
    }
}
