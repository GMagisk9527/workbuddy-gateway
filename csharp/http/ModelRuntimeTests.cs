using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using KeySecurity;
using LedgerMigration;

namespace HttpMigration;

internal static class ModelRuntimeTests
{
    static int checks;
    static void Check(bool condition,string name){if(!condition)throw new InvalidOperationException(name);checks++;}
    sealed class Clock:TimeProvider{internal DateTimeOffset Now=DateTimeOffset.FromUnixTimeSeconds(1800000000);public override DateTimeOffset GetUtcNow()=>Now;}
    static async Task None(AccountPool pool,string model)
    {try{using var lease=await pool.AcquireAsync(model);throw new InvalidOperationException("Unexpected eligible account");}catch(SchedulingFailure e){Check(e.Code=="no_eligible_account","Ineligible state is rejected immediately");}}
    static async Task Rules()
    {
        var id=new AccountId("a");var clock=new Clock();var endpoint=SiteEndpoints.Model(AccountSite.Cn);
        AccountPool New()=>new([new(id,endpoint)],time:clock);
        foreach(var raw in new string?[]{null,"null","{}","bad","{\"credit\":-1,\"total_tokens\":100}","{\"credit\":\"NaN\"}","{\"credit\":0,\"total_tokens\":99}","{\"credit\":0}"})Check(ModelObservations.Cost(raw)==ModelCost.Unknown,"No unsupported free inference");
        foreach(string raw in new[]{"{\"credit\":0,\"total_tokens\":100}","{\"credit\":\"0\",\"prompt_tokens\":\"80\",\"completion_tokens\":20}","{\"credit\":0,\"total_tokens\":100.9}"})Check(ModelObservations.Cost(raw)==ModelCost.Free,"Sufficient zero-credit usage is free");
        Check(ModelObservations.Cost("{\"credit\":0.01}")==ModelCost.Paid,"Positive credit needs no token threshold");
        foreach(string code in new[]{"11128","11140"})Check(ModelObservations.Failure(429,code,"{\"message\":\"rate limit switch to another model\"}",clock.Now).Kind==ModelRejection.None,"Channel/restriction code cannot be reclassified as rate limit");
        Check(ModelObservations.Failure(403,null,"{\"request\":\"rate limit\"}",clock.Now).Kind==ModelRejection.None,"Echoed request cannot cause cooldown");
        Check(ModelObservations.Failure(429,"11128","{\"code\":6004}",clock.Now).Kind==ModelRejection.None,"Existing channel code wins conflicting body");
        Check(ModelObservations.Failure(429,null,"{\"code\":11140,\"error\":{\"code\":6004}}",clock.Now).Kind==ModelRejection.None,"Root restriction wins conflicting nested error");
        Check(ModelObservations.Failure(400,null,"{\"code\": 14018}",clock.Now).Kind==ModelRejection.Quota,"Structured spaced business code");
        Check(ModelObservations.Failure(400,null,"{\"error\":{\"code\":\"6004\"}}",clock.Now).Kind==ModelRejection.ModelRate,"Nested business code");
        Check(ModelObservations.Reset("reset at 2027-01-15 08:00:00 UTC-3:30")==new DateTimeOffset(2027,1,15,11,30,0,TimeSpan.Zero),"Signed minute timezone");
        foreach(string text in new[]{"2027-02-30 08:00:00","2027-01-15 08:00:00 UTC+99","2027-01-15 08:00:00 UTC+14:01"})Check(ModelObservations.Reset(text) is null,"Reject impossible reset date/offset");
        Check(ModelObservations.Failure(429,null,"{\"message\":\"reset 2099-01-01 00:00:00 UTC+8\"}",clock.Now).Until==clock.Now.AddDays(1),"Bound future cooldown");
        Check(ModelObservations.Failure(429,null,"{\"message\":\"reset 2000-01-01 00:00:00 UTC+8\"}",clock.Now).Until==clock.Now.AddSeconds(60),"Expired reset falls back to 60 seconds");
        var pool=New();pool.ObserveModelUsage(id,"m","{\"credit\":0,\"total_tokens\":100}");pool.ObserveQuota(id,true,default,0);
        using(var lease=await pool.AcquireAsync("m"))Check(lease.Kind==SelectionKind.FreeExhausted,"Learned free model remains eligible at zero balance");
        pool.ObserveModelFailure(id,"m",400,"14018");var state=pool.Snapshot(id)!.Models!["m"];
        Check(state.Cost==ModelCost.Free&&state.QuotaBlocked,"Quota rejection preserves free classification");await None(pool,"m");
        pool.ObserveQuota(id,false,default,0);using(var lease=await pool.AcquireAsync("m"))Check(!lease.Account.Models!["m"].QuotaBlocked,"Positive quota releases model block");
        pool.ObserveModelFailure(id,"m",400,"6004");await None(pool,"m");using(var lease=await pool.AcquireAsync("other"))Check(true,"Model cooldown leaves other models eligible");
        pool.ObserveQuota(id,false,default,0);await None(pool,"m");clock.Now=clock.Now.AddSeconds(61);using(var lease=await pool.AcquireAsync("m"))Check(true,"Model cooldown recovers by time without polling");
        pool.ObserveModelFailure(id,"m",429,null);pool.RestoreCredentialAvailability(id,true);await None(pool,"other");
        pool.Replace([new(id,endpoint)]);await None(pool,"other");
        Check(!pool.Snapshot(id)!.Disabled,"Rate limit never disables credentials");clock.Now=clock.Now.AddSeconds(61);using(var lease=await pool.AcquireAsync("other"))Check(true,"Account cooldown recovers by time");
        pool.ObserveModelUsage(id,"m","{\"credit\":1}");pool.ObserveQuota(id,true,default,0);await None(pool,"m");
        Check(pool.Snapshot(id)!.Models!["m"].Cost==ModelCost.Paid,"Actual charge replaces old free evidence");
        pool=New();pool.ObserveQuota(id,true,default,0);using(var probe=await pool.AcquireAsync("m"))Check(probe.Kind==SelectionKind.ProbeExhausted,"Unknown exhausted model reserves one probe");await None(pool,"m");
        pool.ObserveModelUsage(id,"m","{\"credit\":0,\"total_tokens\":1}");await None(pool,"m");clock.Now=clock.Now.AddMinutes(6);using(var probe=await pool.AcquireAsync("m"))Check(probe.Kind==SelectionKind.ProbeExhausted,"Probe window expires normally");
        var protectedStates=Enumerable.Range(0,1024).ToDictionary(i=>"m"+i,_=>new AccountModelState(QuotaBlocked:true));
        pool=new([new(id,endpoint,Models:protectedStates)],time:clock);pool.ObserveModelUsage(id,"extra","{\"credit\":1}");pool.ObserveModelFailure(id,"extra",400,"14018");Check(pool.Snapshot(id)!.Models!.Count==1024&&!pool.Snapshot(id)!.Models!.ContainsKey("extra"),"Never evict protected state or exceed cap");
        pool.ObserveModelUsage(id,"m0","{\"credit\":0,\"total_tokens\":100}");pool.ObserveModelFailure(id,"extra",400,"14018");Check(pool.Snapshot(id)!.Models!.Count==1024&&pool.Snapshot(id)!.Models!.ContainsKey("extra"),"Only inactive state is evicted");
        pool=New();var concurrent=pool;await Task.WhenAll(Enumerable.Range(0,1200).Select(i=>Task.Run(()=>concurrent.ObserveModelUsage(id,"m"+i,"{\"credit\":1}"))));Check(pool.Snapshot(id)!.Models!.Count==1024,"Concurrent arbitrary names remain bounded");
        pool=New();pool.SetCredentialAvailability(id,true,false,clock.Now.AddHours(1));pool.ObserveModelFailure(id,"m",429,null);pool.RestoreCredentialAvailability(id,true);Check(pool.Snapshot(id)!.CooldownUntil==clock.Now.AddSeconds(60),"Restoring credentials does not extend independent rate cooldown");
        var intl=new AccountId("intl");pool=new([new(id,endpoint),new(intl,SiteEndpoints.Model(AccountSite.Intl),AccountSite.Intl)],policies:new Dictionary<string,ModelPolicy>{{"m",new(PaidSites:new HashSet<AccountSite>{AccountSite.Cn,AccountSite.Intl})}},time:clock);
        pool.ObserveModelUsage(intl,"m","{\"credit\":0,\"total_tokens\":100}");using(var lease=await pool.AcquireAffinityAsync("m",id))Check(lease.Account.Id==intl,"Learned free site outranks paid affinity");
    }
    sealed class Fake(string mode):HttpMessageHandler
    {
        internal int Calls;
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage r,CancellationToken ct)
        {
            Calls++;string? code=mode switch{"quota" or "ssequota"=>"14018","model" or "ssemodel" or "partial" or "late"=>"6004","channel"=>"11128",_=>null};
            if(mode is "model" or "quota" or "rate" or "forbidden" or "channel")return Task.FromResult(new HttpResponseMessage(mode=="rate"?HttpStatusCode.TooManyRequests:mode=="forbidden"?HttpStatusCode.Forbidden:HttpStatusCode.BadRequest){Content=new StringContent(JsonSerializer.Serialize(new{code=code??"ordinary",message="synthetic rejection"}),Encoding.UTF8,"application/json")});
            string text=mode=="late"?string.Concat(Enumerable.Repeat("data: {\"choices\":[]}\n\n",65)):mode=="partial"?"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n":"";
            if(code is not null)text+="data: {\"code\":"+code+",\"usage\":{\"credit\":2}}\n\n";
            else text+="data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":"+(mode=="paid"?"2":"0")+",\"total_tokens\":"+(mode=="tiny"?"99":"100")+"}}\n\ndata: [DONE]\n\n";
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK){Content=new StringContent(text,Encoding.UTF8,"text/event-stream")});
        }
    }
    internal static async Task Run(string scratch)
    {
        checks=0;Directory.CreateDirectory(scratch);Check(!Directory.EnumerateFileSystemEntries(scratch).Any(),"Empty scratch required");await Rules();int scenarios=0;
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})foreach(bool stream in new[]{false,true})foreach(string mode in new[]{"model","quota","rate","forbidden","channel","ssemodel","ssequota","partial","late","free","paid","tiny"})
        {
            string root=Path.Combine(scratch,Guid.NewGuid().ToString("N"));var id=new AccountId("first");var endpoint=SiteEndpoints.Model(AccountSite.Cn);var pool=new AccountPool([new(id,endpoint),new(new("second"),endpoint)]);
            using var files=CredentialFiles.Create(Path.Combine(root,"account"),new(new("synthetic-user"),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+7200));
            using var second=CredentialFiles.Create(Path.Combine(root,"second"),new(new("synthetic-second"),"synthetic-access2","synthetic-refresh2",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+7200));
            using var control=new WorkBuddyControlPlane(AccountSite.Cn,new Fake(mode));var credentials=new CredentialManager(pool,[new(id,endpoint,files,control),new(new("second"),endpoint,second,control)]);
            using var keys=PreviewKeyDirectory.Open(Path.Combine(root,"keys"));using var ledger=CreditLedger.Open(Path.Combine(root,"keys"),keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);var issued=keys.Create(new("test"));var fake=new Fake(mode);
            await using var app=LocalGateway.Build(new(endpoint,new byte[32],Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:credentials,DirectUpstream:true,UpstreamHandler:fake));await app.StartAsync();
            using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(10)};client.DefaultRequestHeaders.Authorization=new AuthenticationHeaderValue("Bearer",issued.TakeSecret());
            object body=path=="/v1/responses"?new{model="m",input="hello",stream}:(object)new{model="m",messages=new[]{new{role="user",content="hello"}},stream};
            using var response=await client.PostAsJsonAsync(path,body);string text=await response.Content.ReadAsStringAsync();var snapshot=pool.Snapshot(id)!;var model=snapshot.Models!.GetValueOrDefault("m")??new();
            Check(fake.Calls==1,"No rate/quota/channel/403 replay");Check(!files.Load().Disabled&&!second.Load().Disabled,"Non-restriction never disables either account");
            if(mode is "model" or "ssemodel" or "partial" or "late")Check(model.CooldownUntil>DateTimeOffset.UtcNow&&snapshot.CooldownUntil==default,"Model-only cooldown applied");
            if(mode is "quota" or "ssequota")Check(model.QuotaBlocked&&model.Cost==ModelCost.Unknown,"Quota blocks without paid inference");
            if(mode=="rate")Check(snapshot.CooldownUntil>DateTimeOffset.UtcNow&&!model.QuotaBlocked,"Account rate cooldown applied");
            if(mode is "forbidden" or "channel")Check(snapshot.CooldownUntil==default&&snapshot.Models!.Count==0,"Unrelated refusal never changes scheduling");
            if(mode is "free" or "paid" or "tiny")Check(model.Cost==(mode=="free"?ModelCost.Free:mode=="paid"?ModelCost.Paid:ModelCost.Unknown)&&response.IsSuccessStatusCode,"Cost learned through all protocols");
            if(mode is "ssemodel" or "ssequota" or "partial" or "late")Check(text.Contains(mode=="ssequota"?"14018":"6004")&&!text.Contains("message_stop")&&!text.Contains("[DONE]")&&!text.Contains("response.completed"),"SSE refusal is never successful completion");
            double charge=mode is "ssemodel" or "ssequota" or "partial" or "late" or "paid"?2:0;
            Check(ledger.GetUsage(issued.Item.Id).Total==charge&&ledger.Health.PendingAttempts==0&&pool.Status.Active==0,"Measured rejection usage settled exactly once, leases drained");scenarios++;
            using var admin=new HttpRequestMessage(HttpMethod.Get,"/admin/api/accounts");admin.Headers.Authorization=new AuthenticationHeaderValue("Bearer","synthetic-admin");using var stateResponse=await client.SendAsync(admin);using var stateDoc=JsonDocument.Parse(await stateResponse.Content.ReadAsStringAsync());var publicState=stateDoc.RootElement.GetProperty("items")[0].GetProperty("modelScheduling");Check(stateResponse.IsSuccessStatusCode&&publicState.GetProperty("trackedModels").GetInt32()==snapshot.Models!.Count,"Admin exposes bounded scheduling counts");
        }
        Console.WriteLine(JsonSerializer.Serialize(new{modelRuntimeChecks=checks,httpScenarios=scenarios,realNetworkCalls=0,realModelCalls=0}));
    }
}
