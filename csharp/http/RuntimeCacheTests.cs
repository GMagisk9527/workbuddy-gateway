using System.Net;
using System.Net.Http.Headers;
using System.Net.Http.Json;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using AccountMaintenance;
using AccountScheduling;
using KeySecurity;
using LedgerMigration;

namespace HttpMigration;

internal static class RuntimeCacheTests
{
    static int checks;
    static void Check(bool ok,string name){if(!ok)throw new InvalidOperationException(name);checks++;}
    sealed class Clock:TimeProvider{internal DateTimeOffset Now=DateTimeOffset.FromUnixTimeSeconds(1800000000);public override DateTimeOffset GetUtcNow()=>Now;}
    sealed class Store(AccountCredential credential):ICredentialPersistence
    {PersistedCredential saved=new(credential);public PersistedCredential Load()=>saved;public void Save(PersistedCredential s)=>saved=s;public void Dispose(){}}
    sealed class Control:IAccountControlPlane
    {public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken ct)=>throw new InvalidOperationException("No refresh traffic");public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken ct)=>Task.FromResult(CredentialVerification.Usable);}
    static readonly AccountId Id=new("a");static readonly Uri Endpoint=SiteEndpoints.Model(AccountSite.Cn);
    static (AccountPool Pool,CredentialManager Manager) New(Clock clock,string uid="synthetic-private-uid",AccountSite site=AccountSite.Cn,bool disabled=false)
    {
        var endpoint=SiteEndpoints.Model(site);var pool=new AccountPool([new(Id,endpoint,site,Disabled:disabled)],time:clock);var store=new Store(new(new(uid,Site:site),"synthetic-private-access","synthetic-private-refresh",clock.Now.ToUnixTimeSeconds()+7200));
        return(pool,new(pool,[new(Id,endpoint,store,new Control())],clock));
    }
    static byte[] Bytes(JsonNode node)=>JsonSerializer.SerializeToUtf8Bytes(node);
    static void Reject(Action action,string name)
    {try{action();throw new InvalidOperationException("Expected rejection: "+name);}catch(Exception e)when(e is ArgumentException or JsonException or InvalidOperationException or KeyNotFoundException or FormatException or OverflowException or IOException or UnauthorizedAccessException){Check(!e.Message.StartsWith("Expected rejection",StringComparison.Ordinal),name);}}
    static async Task None(AccountPool pool,string model)
    {try{using var lease=await pool.AcquireAsync(model);throw new InvalidOperationException("Unexpected eligible account");}catch(SchedulingFailure e){Check(e.Code=="no_eligible_account","Restored scheduling guard applies");}}
    static async Task SnapshotRules(Clock clock)
    {
        var source=New(clock);source.Pool.ObserveModelUsage(Id,"free","{\"credit\":0,\"total_tokens\":100}");source.Pool.ObserveModelUsage(Id,"paid","{\"credit\":1}");source.Pool.ObserveModelFailure(Id,"blocked",400,"14018");source.Pool.ObserveModelFailure(Id,"cool",429,"6004");source.Pool.ObserveQuota(Id,true,default,0);
        using(var probe=await source.Pool.AcquireAsync("probe"))Check(probe.Kind==SelectionKind.ProbeExhausted,"Reserve probe before checkpoint");
        var export=source.Pool.ExportRuntime(source.Manager.RuntimeIdentities);string raw=Encoding.UTF8.GetString(export.Snapshot);
        Check(!raw.Contains("synthetic-private")&&!raw.Contains("access")&&!raw.Contains("refresh"),"Snapshot contains digests and model state only");
        var cold=New(clock);Check(cold.Pool.ImportRuntime(export.Snapshot,cold.Manager.RuntimeIdentities)==1,"Identity-bound import");
        using(var free=await cold.Pool.AcquireAsync("free"))Check(free.Kind==SelectionKind.FreeExhausted,"Free cost survives restart");await None(cold.Pool,"paid");await None(cold.Pool,"cool");await None(cold.Pool,"blocked");await None(cold.Pool,"probe");
        Check(cold.Pool.Status.Active==0&&cold.Pool.Status.Waiting==0&&!cold.Pool.Snapshot(Id)!.Disabled,"No permits or disabled flags restored from model checkpoint");
        clock.Now=clock.Now.AddMinutes(6);using(var probe=await cold.Pool.AcquireAsync("probe"))Check(probe.Kind==SelectionKind.ProbeExhausted,"Recovered probe window naturally expires");
        var later=New(clock);later.Pool.ImportRuntime(export.Snapshot,later.Manager.RuntimeIdentities);Check(later.Pool.Snapshot(Id)!.Models!["cool"].CooldownUntil==default&&later.Pool.Snapshot(Id)!.Models!["probe"].NextProbeAt==default,"Expired persisted deadlines are cleared");
        later.Pool.ObserveQuota(Id,false,default,0);using(var blocked=await later.Pool.AcquireAsync("blocked"))Check(true,"Fresh balance clears restored quota block");
        var changed=New(clock,"different-uid");Check(changed.Pool.ImportRuntime(export.Snapshot,changed.Manager.RuntimeIdentities)==0&&changed.Pool.Snapshot(Id)!.Models!.Count==0,"Reused account ID cannot inherit another UID");
        var moved=New(clock,site:AccountSite.Intl);Check(moved.Pool.ImportRuntime(export.Snapshot,moved.Manager.RuntimeIdentities)==0,"Different site cannot inherit scheduling evidence");
        var live=New(clock);live.Pool.ObserveQuota(Id,false,default,0);Check(live.Pool.ImportRuntime(export.Snapshot,live.Manager.RuntimeIdentities)==0&&!live.Pool.Snapshot(Id)!.QuotaExhausted,"Old zero balance cannot override fresh positive balance");
        live=New(clock);live.Pool.ObserveModelUsage(Id,"free","{\"credit\":1}");Check(live.Pool.ImportRuntime(export.Snapshot,live.Manager.RuntimeIdentities)==0&&live.Pool.Snapshot(Id)!.Models!["free"].Cost==ModelCost.Paid,"Older free evidence cannot overwrite a live charge");
        var disabled=New(clock,disabled:true);disabled.Pool.ImportRuntime(export.Snapshot,disabled.Manager.RuntimeIdentities);Check(disabled.Pool.Snapshot(Id)!.Disabled,"Runtime restore cannot enable configured-disabled accounts");
        source=New(clock);source.Pool.ObserveModelFailure(Id,"m",429,null);export=source.Pool.ExportRuntime(source.Manager.RuntimeIdentities);cold=New(clock);cold.Pool.ImportRuntime(export.Snapshot,cold.Manager.RuntimeIdentities);cold.Pool.RestoreCredentialAvailability(Id,true);await None(cold.Pool,"other");
        Check(cold.Pool.ImportRuntime(export.Snapshot,cold.Manager.RuntimeIdentities)==0,"Checkpoint is applied at most once per account");
        var valid=JsonNode.Parse(export.Snapshot)!;
        void Bad(Action<JsonNode> mutate,string name)
        {var copy=valid.DeepClone();mutate(copy);var target=New(clock);Reject(()=>target.Pool.ImportRuntime(Bytes(copy),target.Manager.RuntimeIdentities),name);Check(target.Pool.Snapshot(Id)!.Models!.Count==0&&target.Pool.Snapshot(Id)!.CooldownUntil==default,"Invalid input never partially publishes");}
        Bad(n=>n["version"]=2,"Unsupported schema");Bad(n=>n["at"]=clock.Now.ToUnixTimeSeconds()+301,"Future capture");Bad(n=>n["accounts"]![0]!["identity"]="bad","Malformed digest");Bad(n=>n["accounts"]![0]!["site"]=3,"Unknown site");
        Bad(n=>n["accounts"]![0]!["rateUntil"]=clock.Now.ToUnixTimeSeconds()+86401,"Unbounded account cooldown");
        Bad(n=>n["accounts"]!.AsArray().Add(n["accounts"]![0]!.DeepClone()),"Duplicate account");
        var state=new JsonObject{{"cost",1},{"cooldown",0},{"blocked",false},{"probe",0},{"observed",clock.Now.ToUnixTimeSeconds()}};
        foreach(string field in new[]{"cost","cooldown","probe","observed"})Bad(n=>{n["accounts"]![0]!["models"]!["m"]=state.DeepClone();n["accounts"]![0]!["models"]!["m"]![field]=-1;},"Negative model field: "+field);
        Bad(n=>{n["accounts"]![0]!["models"]!["m"]=state.DeepClone();n["accounts"]![0]!["models"]!["m"]!["cost"]=99;},"Unknown cost");
        Bad(n=>{n["accounts"]![0]!["models"]!["m"]=state.DeepClone();n["accounts"]![0]!["models"]!["m"]!["probe"]=clock.Now.ToUnixTimeSeconds()+301;},"Unbounded probe");
        Bad(n=>n["accounts"]![0]!["models"]!["bad\nname"]=state.DeepClone(),"Control character in model");Bad(n=>n["accounts"]![0]!["models"]![new string('a',257)]=state.DeepClone(),"Oversize model");Bad(n=>n["accounts"]![0]!["models"]!["UPPER"]=state.DeepClone(),"Noncanonical model");
        Bad(n=>{for(int i=0;i<1025;i++)n["accounts"]![0]!["models"]!["m"+i]=state.DeepClone();},"Per-account state cap");
        var atomic=valid.DeepClone();atomic["accounts"]!.AsArray().Add(new JsonObject{{"id","bad"}});var untouched=New(clock);Reject(()=>untouched.Pool.ImportRuntime(Bytes(atomic),untouched.Manager.RuntimeIdentities),"Validate all rows before applying first");Check(untouched.Pool.Snapshot(Id)!.CooldownUntil==default,"A bad later row cannot publish a valid earlier row");
        var duplicate=raw="{\"version\":1,\"version\":1,\"at\":"+clock.Now.ToUnixTimeSeconds()+",\"accounts\":[]}";Reject(()=>cold.Pool.ValidateRuntimeSnapshot(Encoding.UTF8.GetBytes(duplicate)),"Duplicate JSON property");Reject(()=>cold.Pool.ValidateRuntimeSnapshot(new byte[AccountPool.RuntimeSnapshotLimit+1]),"Byte bounds before JSON parse");
        var models=Enumerable.Range(0,1024).ToDictionary(i=>"m"+i,_=>new AccountModelState(ModelCost.Paid,ObservedAt:clock.Now));var full=new AccountPool([new(Id,Endpoint,Models:models)],time:clock);var fullExport=full.ExportRuntime(cold.Manager.RuntimeIdentities);var fullCold=New(clock);fullCold.Pool.ImportRuntime(fullExport.Snapshot,fullCold.Manager.RuntimeIdentities);Check(fullCold.Pool.Snapshot(Id)!.Models!.Count==1024,"Full per-account limit round trips");
        var identities=Enumerable.Range(0,17).Select(i=>new RuntimeIdentity(new("a"+i),AccountSite.Cn,new string('a',64))).ToArray();var many=new AccountPool(identities.Select(b=>new AccountSnapshot(b.Id,Endpoint,Models:models)),time:clock);Reject(()=>many.ExportRuntime(identities),"Global entry bound before serialization");
        string prefix=string.Concat(Enumerable.Repeat("模",84));var longModels=Enumerable.Range(0,1024).ToDictionary(i=>prefix+i.ToString("D4"),_=>new AccountModelState());var large=new AccountPool(identities.Take(16).Select(b=>new AccountSnapshot(b.Id,Endpoint,Models:longModels)),time:clock);Reject(()=>large.ExportRuntime(identities),"Escaped UTF-8 snapshot respects hard byte budget");
    }
    static async Task FilesAndWorker(Clock clock,string dir)
    {
        Directory.CreateDirectory(dir);var first=New(clock);string cacheDir=Path.Combine(dir,"cache");using var cache=RuntimeSnapshotCache.Open(cacheDir);
        Reject(()=>RuntimeSnapshotCache.Open(cacheDir),"Exclusive writer");
        using var worker=new RuntimeCheckpointWorker(first.Pool,first.Manager,cache,TimeSpan.FromMilliseconds(100));await worker.RunOnceAsync();Check(cache.Status.Saves==1,"Initial bounded checkpoint");
        await Task.WhenAll(Enumerable.Range(0,32).Select(_=>worker.RunOnceAsync()));Check(cache.Status.Saves==1,"Idle/concurrent ticks do not rewrite unchanged snapshot");
        for(int i=0;i<80;i++)first.Pool.ObserveModelUsage(Id,"m"+i,"{\"credit\":1}");Check(cache.Status.Saves==1,"Foreground observations never fsync");await worker.RunOnceAsync();Check(cache.Status.Saves==2&&cache.Status.SavedVersion==first.Pool.RuntimeVersion,"Batch changes coalesce into one checkpoint");
        var cold=New(clock);Check(cache.TryLoad(cold.Pool,cold.Manager)&&cold.Pool.ModelStatus(Id)!.PaidModels==80,"Read verified primary");
        File.WriteAllText(Path.Combine(cacheDir,"runtime.json"),"broken");var backupCold=New(clock);Check(cache.TryLoad(backupCold.Pool,backupCold.Manager)&&cache.Status.LoadedFrom=="backup","Corrupt primary falls back to valid backup");
        first.Pool.ObserveModelFailure(Id,"m0",400,"14018");Check(cache.TrySave(first.Pool,first.Manager),"Repair corrupt primary");var retained=New(clock);File.WriteAllText(Path.Combine(cacheDir,"runtime.json"),"bad again");Check(cache.TryLoad(retained.Pool,retained.Manager)&&retained.Pool.ModelStatus(Id)!.TrackedModels==0,"Repair never replaces good older backup with corrupt primary");
        Check(cache.TrySave(first.Pool,first.Manager),"Unchanged state remains saved in memory");
        first.Pool.ObserveModelFailure(Id,"m1",400,"14018");Check(cache.TrySave(first.Pool,first.Manager),"Subsequent dirty tick repairs file");
        byte[] stable=File.ReadAllBytes(Path.Combine(cacheDir,"runtime.json"));Directory.CreateDirectory(Path.Combine(cacheDir,"runtime.json.tmp"));first.Pool.ObserveModelFailure(Id,"m2",400,"14018");Check(!cache.TrySave(first.Pool,first.Manager)&&cache.Status.WriteFailed,"Write failure is visible");Check(File.ReadAllBytes(Path.Combine(cacheDir,"runtime.json")).AsSpan().SequenceEqual(stable)&&first.Pool.Snapshot(Id)!.Models!["m2"].QuotaBlocked,"Failed checkpoint preserves old disk and new memory");
        Directory.Delete(Path.Combine(cacheDir,"runtime.json.tmp"));Check(cache.TrySave(first.Pool,first.Manager)&&!cache.Status.WriteFailed,"Dirty failed checkpoint retries after storage recovers");
        await Task.WhenAll(Enumerable.Range(0,100).Select(async i=>{first.Pool.ObserveModelUsage(Id,"live"+i,"{\"credit\":1}");await worker.RunOnceAsync();}));await worker.RunOnceAsync();Check(cache.Status.SavedVersion==first.Pool.RuntimeVersion,"Writes concurrent with new observations cannot mark newer state clean");
        var final=New(clock);Check(cache.TryLoad(final.Pool,final.Manager)&&final.Pool.ModelStatus(Id)!.PaidModels==180,"Latest concurrent observations are durable");
        File.WriteAllText(Path.Combine(cacheDir,"runtime.json"),"bad");File.WriteAllText(Path.Combine(cacheDir,"runtime.backup.json"),"bad");var lost=New(clock);Check(!cache.TryLoad(lost.Pool,lost.Manager)&&cache.Status.LoadedFrom=="invalid"&&lost.Pool.ModelStatus(Id)!.TrackedModels==0,"Both invalid files remain unknown, never publish corrupt state");
        string foreign=Path.Combine(dir,"foreign");Directory.CreateDirectory(foreign);File.WriteAllText(Path.Combine(foreign,"unowned"),"keep");Reject(()=>RuntimeSnapshotCache.Open(foreign),"Unowned directory rejected");Check(File.ReadAllText(Path.Combine(foreign,"unowned"))=="keep","No foreign data modified");
        string marker=Path.Combine(dir,"wrong");Directory.CreateDirectory(marker);File.WriteAllText(Path.Combine(marker,".csharp-model-runtime"),"wrong");Reject(()=>RuntimeSnapshotCache.Open(marker),"Invalid ownership marker rejected");
        using var reused=RuntimeSnapshotCache.Open(Path.Combine(dir,"reused"));var a=New(clock);var b=New(clock,"new-user");Check(reused.TrySave(a.Pool,a.Manager)&&reused.TrySave(b.Pool,b.Manager)&&reused.Status.Saves==2,"Independent pools with equal revisions cannot skip a checkpoint");
    }
    sealed class Fake:HttpMessageHandler
    {
        internal int Calls;internal TaskCompletionSource Entered=new(TaskCreationOptions.RunContinuationsAsynchronously),Release=new(TaskCreationOptions.RunContinuationsAsynchronously);
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage r,CancellationToken ct)
        {Calls++;Entered.TrySetResult();await Release.Task.WaitAsync(ct);return new(HttpStatusCode.OK){Content=new StringContent("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"total_tokens\":100}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")};}
    }
    static async Task HostDrain(Clock clock,string dir,string path,bool stream)
    {
        var first=New(clock);using var cache=RuntimeSnapshotCache.Open(Path.Combine(dir,"cache"));using var keys=PreviewKeyDirectory.Open(Path.Combine(dir,"keys"));using var ledger=CreditLedger.Open(Path.Combine(dir,"keys"),keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);var key=keys.Create(new("synthetic"));var fake=new Fake();
        await using(var app=LocalGateway.Build(new(Endpoint,Program.TestKey,Security:new(keys,new("synthetic-admin"),ledger),Accounts:first.Pool,Credentials:first.Manager,DirectUpstream:true,UpstreamHandler:fake,RuntimeCache:cache)))
        {
            await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(15)};client.DefaultRequestHeaders.Authorization=new AuthenticationHeaderValue("Bearer",key.TakeSecret());
            object body=path=="/v1/responses"?new{model="m",input="hello",stream}:(object)new{model="m",messages=new[]{new{role="user",content="hello"}},stream};var request=client.PostAsJsonAsync(path,body);await fake.Entered.Task.WaitAsync(TimeSpan.FromSeconds(3));
            using var adminRequest=new HttpRequestMessage(HttpMethod.Get,"/admin/api/accounts");adminRequest.Headers.Authorization=new AuthenticationHeaderValue("Bearer","synthetic-admin");using var response=await client.SendAsync(adminRequest);using var state=JsonDocument.Parse(await response.Content.ReadAsStringAsync());Check(state.RootElement.GetProperty("runtimeCache").ValueKind==JsonValueKind.Object,"Admin reports cache health without credential data");
            Task stopped=app.StopAsync();Check(!stopped.IsCompleted,"Shutdown waits for active model request");fake.Release.TrySetResult();using var result=await request;string text=await result.Content.ReadAsStringAsync();await stopped.WaitAsync(TimeSpan.FromSeconds(5));Check(result.IsSuccessStatusCode&&text.Contains("ok")&&fake.Calls==1,"Drain keeps protocol output and never replays");
        }
        var cold=New(clock);Check(cache.TryLoad(cold.Pool,cold.Manager)&&cold.Pool.Snapshot(Id)!.Models!["m"].Cost==ModelCost.Free,"Final checkpoint captures observations produced during shutdown drain");Check(ledger.Health.PendingAttempts==0&&ledger.GetUsage(key.Item.Id).ZeroCost==1&&first.Pool.Status.Active==0,"Shutdown checkpoint never alters WAL settlement or leases");
    }
    internal static async Task Run(string scratch)
    {
        checks=0;Directory.CreateDirectory(scratch);Check(!Directory.EnumerateFileSystemEntries(scratch).Any(),"Empty scratch required");var clock=new Clock();await SnapshotRules(clock);await FilesAndWorker(clock,Path.Combine(scratch,"files"));
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})foreach(bool stream in new[]{false,true})await HostDrain(clock,Path.Combine(scratch,Guid.NewGuid().ToString("N")),path,stream);
        bool rejected=false;using(var cache=RuntimeSnapshotCache.Open(Path.Combine(scratch,"bridge")))try{await using var app=LocalGateway.Build(new(new("http://127.0.0.1/"),Program.TestKey,GoBridge:true,RuntimeCache:cache));}catch(ArgumentException){rejected=true;}Check(rejected,"Go bridge cannot own runtime cache");
        Console.WriteLine(JsonSerializer.Serialize(new{runtimeCacheChecks=checks,shutdownHttpScenarios=6,realModelCalls=0,productionChanged=false}));
    }
}
