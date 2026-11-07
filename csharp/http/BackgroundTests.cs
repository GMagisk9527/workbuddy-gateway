using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;
using System.Net;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
namespace HttpMigration;
internal static class BackgroundTests
{
    static int checks;
    static void Check(bool b,string why){if(!b)throw new InvalidOperationException(why);checks++;}
    sealed class Clock:TimeProvider{internal DateTimeOffset Now=DateTimeOffset.Parse("2026-11-06T00:00:00Z");public override DateTimeOffset GetUtcNow()=>Now;}
    sealed class Store(AccountCredential c):ICredentialPersistence
    {
        internal PersistedCredential Saved=new(c);internal int Saves;internal bool Fail;
        public PersistedCredential Load()=>Saved;public void Save(PersistedCredential next){if(Fail)throw new IOException("synthetic failure");Saved=next;Saves++;}public void Dispose(){ }
    }
    sealed class Control(Clock clock):IAccountControlPlane,IAccountResourcesControlPlane
    {
        internal int Refreshes,Resources,Verifies;internal bool FailRefresh,FailResources;
        internal TaskCompletionSource? RefreshEntered,RefreshRelease,ResourceEntered,ResourceRelease;
        internal AccountResourceObservation Observation=new(new(100,20,80),[new("valid",20,clock.Now.AddDays(2).ToUnixTimeSeconds(),true)],true);
        public async Task<AccountCredential> RefreshAsync(AccountCredential current,CancellationToken ct)
        {Interlocked.Increment(ref Refreshes);RefreshEntered?.TrySetResult();if(RefreshRelease is not null)await RefreshRelease.Task.WaitAsync(ct);if(FailRefresh)throw new CredentialFailure("synthetic_refresh_failed");return new(current.Identity,"fresh-access","fresh-refresh",clock.Now.ToUnixTimeSeconds()+3600);}
        public Task<CredentialVerification> VerifyAsync(AccountCredential current,CancellationToken ct){Interlocked.Increment(ref Verifies);return Task.FromResult(CredentialVerification.Usable);}
        public async Task<AccountResourceObservation> FetchResourcesAsync(AccountCredential current,CancellationToken ct,bool fetchPackages=true)
        {Interlocked.Increment(ref Resources);var observation=Observation;ResourceEntered?.TrySetResult();if(ResourceRelease is not null)await ResourceRelease.Task.WaitAsync(ct);if(FailResources)throw new CredentialFailure("synthetic_resource_failure");return observation;}
    }
    static TaskCompletionSource Signal()=>new(TaskCreationOptions.RunContinuationsAsynchronously);
    static AccountCredential Cred(Clock clock,int expiry=60,string access="old-access",string refresh="old-refresh",AccountSite site=AccountSite.Cn)=>new(new("synthetic-user",Site:site),access,refresh,clock.Now.ToUnixTimeSeconds()+expiry);
    static (AccountId,AccountPool,CredentialManager) Manager(Clock clock,Store store,Control control,SchedulingOptions? options=null,AccountSnapshot? account=null)
    {var id=new AccountId("a");var endpoint=SiteEndpoints.Model(AccountSite.Cn);var pool=new AccountPool([account??new(id,endpoint)],options,time:clock);return(id,pool,new(pool,[new(id,endpoint,store,control)],clock));}
    internal static async Task Run(string scratch)
    {
        checks=0;Directory.CreateDirectory(scratch);Check(!Directory.EnumerateFileSystemEntries(scratch).Any(),"Empty scratch");
        var clock=new Clock();
        foreach(var c in new[]{Cred(clock,3600),Cred(clock,access:""),Cred(clock,refresh:"")})
        {var state=Manager(clock,new(c),new(clock));using var worker=new CredentialMaintenanceWorker(state.Item3,new(TimeSpan.FromMinutes(1)));await worker.RunOnceAsync();Check(worker.Status.Attempted==0,"Only usable near-expiry refreshable credentials are scheduled");}
        {
            var store=new Store(Cred(clock));var control=new Control(clock){RefreshEntered=Signal(),RefreshRelease=Signal()};var(id,pool,manager)=Manager(clock,store,control);using var worker=new CredentialMaintenanceWorker(manager,new(TimeSpan.FromMinutes(1)));
            Task run=worker.RunOnceAsync();await control.RefreshEntered.Task;await Task.WhenAll(Enumerable.Range(0,32).Select(_=>worker.RunOnceAsync()));
            Check(worker.TaskState.Snapshot.Status=="running"&&worker.TaskState.Snapshot.LastRunAt>0&&worker.TaskState.Snapshot.Success is null,"Task page observes real in-flight refresh");
            var acquisitions=Enumerable.Range(0,16).Select(async _=>{using var lease=await manager.AcquireForHeadersAsync(id,SiteEndpoints.Model(AccountSite.Cn));}).ToArray();
            control.RefreshRelease.SetResult();await Task.WhenAll(acquisitions.Append(run));Check(control.Refreshes==1&&control.Verifies==1&&store.Saves==1,"Background and demand refresh coalesce under credential gate");
            await worker.RunOnceAsync();Check(worker.Status.Attempted==0&&!manager.Status.Single().Disabled,"Refreshed credentials are not refreshed again");
            Check(worker.TaskState.Snapshot.Status=="skipped"&&worker.TaskState.Snapshot.Success is null&&worker.TaskState.Snapshot.NextRunAt>0,"No eligible credentials is skipped, not fabricated success");
        }
        {
            var store=new Store(Cred(clock)){Fail=true};var(id,pool,manager)=Manager(clock,store,new(clock));using var worker=new CredentialMaintenanceWorker(manager,new(TimeSpan.FromMinutes(1)));await worker.RunOnceAsync();Check(worker.Status.Failed==1&&manager.Status.Single().Degraded,"Refresh write failure degrades rather than publishing rotated credential");Check(manager.MaintenanceCandidates.Count==0,"Degraded store excluded from background retry");
        }
        {
            var control=new Control(clock){FailRefresh=true};var(id,pool,manager)=Manager(clock,new(Cred(clock)),control);using var worker=new CredentialMaintenanceWorker(manager,new(TimeSpan.FromMinutes(1)));
            for(int i=0;i<3;i++){await worker.RunOnceAsync();clock.Now=clock.Now.AddSeconds(61);}
            Check(control.Refreshes==3&&manager.Status.Single().Disabled,"Refresh failure cooldown and stop policy retained");Check(manager.MaintenanceCandidates.Count==0,"Stopped accounts are not actively refreshed");
        }
        {
            var control=new Control(clock){RefreshEntered=Signal(),RefreshRelease=Signal()};var store=new Store(Cred(clock));var(id,pool,manager)=Manager(clock,store,control);using var worker=new CredentialMaintenanceWorker(manager,new(TimeSpan.FromMinutes(1)),startImmediately:true);
            await worker.StartAsync(CancellationToken.None);await control.RefreshEntered.Task.WaitAsync(TimeSpan.FromSeconds(2));Task stop=worker.StopAsync(CancellationToken.None);control.RefreshRelease.SetResult();await stop.WaitAsync(TimeSpan.FromSeconds(2));Check(store.Saves==1&&store.Saved.Credential.ExpiresAt>clock.Now.ToUnixTimeSeconds()+900,"Shutdown finishes an already-started token rotation");
        }
        {
            var control=new Control(clock){RefreshEntered=Signal(),RefreshRelease=Signal()};var store=new Store(Cred(clock));var(id,pool,manager)=Manager(clock,store,control);using var worker=new CredentialMaintenanceWorker(manager,new(TimeSpan.FromMinutes(1)));
            Task run=worker.RunOnceAsync();await control.RefreshEntered.Task;Task disable=manager.DisableAsync(id);control.RefreshRelease.SetResult();await Task.WhenAll(run,disable);Check(store.Saved.Disabled&&manager.Status.Single().Disabled&&store.Saved.Credential.ExpiresAt>clock.Now.ToUnixTimeSeconds()+900,"Concurrent stop retains new token while disabling account");
        }
        await Resources(clock);await Transport(clock);await Cache(clock,Path.Combine(scratch,"cache"));await Host(clock,Path.Combine(scratch,"host"));
        Console.WriteLine(JsonSerializer.Serialize(new{backgroundChecks=checks,realNetworkCalls=0,realModelCalls=0}));
    }
    static async Task Resources(Clock clock)
    {
        var control=new Control(clock);var store=new Store(Cred(clock,3600));var model=new Dictionary<string,AccountModelState>{{"m",new(ModelCost.Paid,QuotaBlocked:true,NextProbeAt:clock.Now.AddHours(1))}};
        var(id,pool,manager)=Manager(clock,store,control,account:new(new("a"),SiteEndpoints.Model(AccountSite.Cn),Models:model));
        using var worker=new AccountResourceWorker(manager,new(TimeSpan.FromMinutes(1)));
        await worker.RunOnceAsync();Check(worker.Status.Succeeded==1&&manager.ResourceStatus.Single().Remaining==80&&manager.ResourceStatus.Single().ExpiringCredit==20,"Resource observations reach account view");
        using(var lease=await pool.AcquireAsync("m")){Check(lease.Kind==SelectionKind.Normal,"Restored upstream balance clears quota block");}
        control.Observation=new(new(100,100,0),[],false);await worker.RunOnceAsync();
        bool rejected=false;try{using var ignored=await pool.AcquireAsync("m");}catch(SchedulingFailure){rejected=true;}
        Check(rejected&&manager.ResourceStatus.Single().Remaining==0,"Verified zero balance blocks paid-model account");
        control.FailResources=true;await worker.RunOnceAsync();Check(manager.ResourceStatus.Single().Known&&manager.ResourceStatus.Single().Stale&&manager.ResourceStatus.Single().Remaining==0&&!manager.Status.Single().Disabled&&manager.Status.Single().Failures==0,"Read-only failure retains previous balance without credential cooling");
        Check(worker.TaskState.Snapshot.Status=="failed"&&worker.TaskState.Snapshot.Success==false,"All resource failures are failed, not partial");
        control.FailResources=false;control.Observation=new(new(-1,500,-1,true),[],false);await worker.RunOnceAsync();using(var lease=await pool.AcquireAsync("m"))Check(lease.Kind==SelectionKind.Normal&&manager.ResourceStatus.Single().Unlimited,"Unlimited enterprise quota restores scheduling");
        control.Observation=new(new(double.NaN,0,0),[],false);await worker.RunOnceAsync();Check(worker.Status.Failed==1&&manager.ResourceStatus.Single().Unlimited,"Invalid resource data cannot replace good observation");
        control.Observation=new(new(-1,0,0,true),[],false);await worker.RunOnceAsync();Check(worker.Status.Failed==1&&manager.ResourceStatus.Single().Remaining==-1,"Inconsistent unlimited quota sentinels are rejected");
        await manager.DisableAsync(id);int calls=control.Resources;await worker.RunOnceAsync();Check(control.Resources==calls&&worker.Status.Attempted==0,"Disabled accounts excluded from resource polling");
        Check(worker.TaskState.Snapshot.Status=="skipped"&&worker.TaskState.Snapshot.Success is null,"No resource candidates not successful work");
        var ctl2=new Control(clock){ResourceEntered=Signal(),ResourceRelease=Signal()};var state=Manager(clock,new(Cred(clock,3600)),ctl2);using var worker2=new AccountResourceWorker(state.Item3,new(TimeSpan.FromMinutes(1)));
        Task running=worker2.RunOnceAsync();await ctl2.ResourceEntered.Task;await Task.WhenAll(Enumerable.Range(0,20).Select(_=>worker2.RunOnceAsync()));Check(ctl2.Resources==1,"Resource ticks do not build duplicate queues");state.Item3.Block(state.Item1);ctl2.ResourceRelease.SetResult();await running;Check(!state.Item3.ResourceStatus.Single().Known&&state.Item3.Status.Single().Disabled,"Observation cannot republish state after account block");
        AccountId p1=new("a"),p2=new("b");var endpoint=SiteEndpoints.Model(AccountSite.Cn);var pool2=new AccountPool([new(p1,endpoint),new(p2,endpoint)],new(ExpiringFirst:true),time:clock);
        var s1=new Store(Cred(clock,3600));var s2=new Store(new(new("other-user"),"a","r",clock.Now.ToUnixTimeSeconds()+3600));var c1=new Control(clock){Observation=new(new(100,0,100),[new("later",10,clock.Now.AddDays(5).ToUnixTimeSeconds(),true)],true)};var c2=new Control(clock){Observation=new(new(100,0,100),[new("sooner",10,clock.Now.AddDays(1).ToUnixTimeSeconds(),true)],true)};
        var manager2=new CredentialManager(pool2,[new(p1,endpoint,s1,c1),new(p2,endpoint,s2,c2)],clock);using var worker3=new AccountResourceWorker(manager2,new(TimeSpan.FromMinutes(1),1));await worker3.RunOnceAsync();await worker3.RunOnceAsync();Check(c1.Resources==1&&c2.Resources==1,"Bounded resource batches rotate across all accounts");
        using(var lease=await pool2.AcquireAsync("m"))Check(lease.Account.Id==p2,"Earliest known positive credit drives existing priority policy");
        c2.FailResources=true;await manager2.FetchResourcesAsync(p1);manager2.MarkResourceFailure(p2);using(var lease=await pool2.AcquireAsync("m"))Check(lease.Account.Id==p1,"Failed package observation removes stale credit priority");
        var ordered=new Control(clock){ResourceEntered=Signal(),ResourceRelease=Signal(),Observation=new(new(100,100,0),[],false)};var state3=Manager(clock,new(Cred(clock,3600)),ordered);
        Task first=state3.Item3.FetchResourcesAsync(state3.Item1);await ordered.ResourceEntered.Task;ordered.Observation=new(new(100,0,100),[],false);Task second=state3.Item3.FetchResourcesAsync(state3.Item1);
        Check(ordered.Resources==1,"Manual and background queries of one account serialize before sending");ordered.ResourceRelease.SetResult();await Task.WhenAll(first,second);Check(ordered.Resources==2&&state3.Item3.ResourceStatus.Single().Remaining==100,"Older response cannot arrive after and overwrite newer query");
    }
    static async Task Cache(Clock clock,string directory)
    {
        var control=new Control(clock){Observation=new(new(100,100,0),[],false)};var state=Manager(clock,new(Cred(clock,3600)),control);
        AccountSnapshot Paid()=>new(new("a"),SiteEndpoints.Model(AccountSite.Cn),Models:new Dictionary<string,AccountModelState>{{"m",new(ModelCost.Paid)}});
        var source=state.Item3;await source.FetchResourcesAsync(state.Item1);
        using(var cache=ResourceSnapshotCache.Open(directory))
        {
            Check(cache.TrySave(source)&&cache.Status.Saves==1,"Upstream quota snapshot saved");
            string primary=Path.Combine(directory,"resources.json"),backup=Path.Combine(directory,"resources.backup.json");string text=File.ReadAllText(primary);
            Check(!text.Contains("synthetic-user")&&!text.Contains("old-access")&&!text.Contains("old-refresh"),"Snapshot contains identity digest and numeric observations, not credentials or raw UID");
            var cold=Manager(clock,new(Cred(clock,3600,access:"rotated-access")),new(clock),account:Paid());
            Check(cache.TryLoad(cold.Item3)&&cold.Item3.ResourceStatus.Single().Known&&cold.Item3.ResourceStatus.Single().Stale&&cold.Item3.ResourceStatus.Single().Remaining==0,"Known zero balance survives restart and access-token rotation");
            bool unavailable=false;try{using var ignored=await cold.Item2.AcquireAsync("m");}catch(SchedulingFailure){unavailable=true;}Check(unavailable,"Restored zero balance still blocks known-paid model");
            var changed=Manager(clock,new(new(new("different-user"),"a","r",clock.Now.ToUnixTimeSeconds()+3600)),new(clock),account:Paid());
            Check(cache.TryLoad(changed.Item3)&&!changed.Item3.ResourceStatus.Single().Known,"Reusing account ID for a different identity does not replay quota");
            using(var lease=await changed.Item2.AcquireAsync("m"))Check(lease.Kind==SelectionKind.Normal,"Different identity does not inherit cached exhaustion");
            var liveControl=new Control(clock);var live=Manager(clock,new(Cred(clock,3600)),liveControl,account:Paid());await live.Item3.FetchResourcesAsync(live.Item1);cache.TryLoad(live.Item3);
            Check(live.Item3.ResourceStatus.Single().Remaining==80&&!live.Item3.ResourceStatus.Single().Stale,"Restoring a cache never replaces a live observation");
            control.Observation=new(new(100,20,80),[new("soon",20,clock.Now.AddDays(1).ToUnixTimeSeconds(),true)],true);await source.FetchResourcesAsync(state.Item1);Check(cache.TrySave(source),"Second quota snapshot saved");
            var restored=Manager(clock,new(Cred(clock,3600)),new(clock));cache.TryLoad(restored.Item3);Check(restored.Item3.ResourceStatus.Single().ExpiringCredit==0&&!restored.Item3.ResourceStatus.Single().PackagesKnown,"Cached package priority is not restored as fresh scheduling data");
            File.WriteAllText(primary,"{broken");var fromBackup=Manager(clock,new(Cred(clock,3600)),new(clock));Check(cache.TryLoad(fromBackup.Item3)&&cache.Status.LoadedFrom=="backup"&&fromBackup.Item3.ResourceStatus.Single().Remaining==0,"Corrupt primary falls back to valid previous quota");
            Check(cache.TrySave(source),"Fresh observation repairs invalid primary without overwriting good backup");File.WriteAllText(primary,"{broken again");var stillGood=Manager(clock,new(Cred(clock,3600)),new(clock));Check(cache.TryLoad(stillGood.Item3)&&stillGood.Item3.ResourceStatus.Single().Remaining==0,"Invalid primary is not copied over the backup");
            Check(cache.TrySave(source),"Primary repaired for fault test");byte[] retained=File.ReadAllBytes(primary);Directory.CreateDirectory(primary+".tmp");Check(!cache.TrySave(source)&&cache.Status.WriteFailed&&retained.SequenceEqual(File.ReadAllBytes(primary)),"Quota snapshot write failure keeps complete old file");Directory.Delete(primary+".tmp");Check(cache.TrySave(source)&&!cache.Status.WriteFailed,"Later quota snapshot write recovers");
            var snapshot=JsonNode.Parse(source.ExportResources())!.AsObject();var rows=snapshot["accounts"]!.AsArray();var bad=rows[0]!.DeepClone();bad["observation"]!["id"]=" ";rows.Add(bad);
            var atomic=Manager(clock,new(Cred(clock,3600)),new(clock));bool rejected=false;try{atomic.Item3.ImportResources(Encoding.UTF8.GetBytes(snapshot.ToJsonString()));}catch(ArgumentException){rejected=true;}Check(rejected&&!atomic.Item3.ResourceStatus.Single().Known,"Invalid later row cannot partially import earlier quota data");
            File.WriteAllText(primary,"bad");File.WriteAllText(backup,"also bad");var none=Manager(clock,new(Cred(clock,3600)),new(clock));Check(!cache.TryLoad(none.Item3)&&cache.Status.LoadedFrom=="invalid"&&!none.Item3.ResourceStatus.Single().Known,"Two invalid files remain unknown rather than becoming zero balance");
            bool conflict=false;try{using var ignored=ResourceSnapshotCache.Open(directory);}catch(IOException){conflict=true;}Check(conflict,"Quota snapshot has one writer");
            control.Observation=new(new(100,100,0),[],false);await source.FetchResourcesAsync(state.Item1);Check(cache.TrySave(source),"Zero restored as bootstrap for host test");
        }
        using(var cache=ResourceSnapshotCache.Open(directory))
        {
            string keyPath=Path.Combine(directory,"keys");using var keys=PreviewKeyDirectory.Open(keyPath);using var ledger=CreditLedger.Open(keyPath,[]);keys.AttachAccounting(ledger);var key=keys.Create(new("synthetic",QuotaTotal:10));
            var host=Manager(clock,new(Cred(clock,3600)),new(clock),account:Paid());var handler=new NoModel();
            await using var app=LocalGateway.Build(new(SiteEndpoints.Model(AccountSite.Cn),Program.TestKey,Security:new(keys,new("synthetic-admin"),ledger),Accounts:host.Item2,Credentials:host.Item3,DirectUpstream:true,ResourceCache:cache,UpstreamHandler:handler));await app.StartAsync();
            using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(5)};client.DefaultRequestHeaders.Authorization=new("Bearer",key.TakeSecret());
            using var response=await client.PostAsJsonAsync("/v1/messages",new{model="m",messages=new[]{new{role="user",content="hello"}}});Check((int)response.StatusCode==503&&handler.Calls==0&&ledger.Health.PendingAttempts==0,"Host restores paid-model zero-balance block before serving any request");await app.StopAsync();
        }
    }
    sealed class NoModel:HttpMessageHandler
    {internal int Calls;protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage r,CancellationToken ct){Calls++;throw new HttpRequestException("No real network permitted");}}
    sealed class TransportHandler(AccountSite site,bool enterprise):HttpMessageHandler
    {
        internal int Calls;internal string Mode="ok";
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage r,CancellationToken ct)
        {
            Calls++;Check(r.Method==HttpMethod.Post&&r.Headers.Authorization?.Parameter=="old-access"&&!r.Headers.Contains("X-Refresh-Token")&&!r.Headers.Contains("Cookie")&&!r.Headers.Contains("X-Client-ID")&&r.Headers.UserAgent.ToString()=="WorkBuddy-Gateway-CSharp/0.1","Resource RPC authenticates honestly without refresh secret or client impersonation");
            if(Mode=="timeout")await Task.Delay(Timeout.InfiniteTimeSpan,ct);
            if(Mode=="http")return new(HttpStatusCode.Forbidden){Content=new StringContent("synthetic-private-response")};
            if(Mode=="oversize")return new(HttpStatusCode.OK){Content=new ByteArrayContent(new byte[(1<<20)+1])};
            if(Mode=="business")return Json("{\"code\":11128,\"data\":{}}");
            if(enterprise){Check(r.RequestUri==SiteEndpoints.EnterpriseUsage(site)&&r.Headers.GetValues("X-Tenant-Id").Single()=="enterprise-secret","Enterprise uses assigned quota endpoint");return Json("{\"code\":0,\"data\":{\"limitNum\":-1,\"credit\":20}}");}
            if(r.RequestUri==SiteEndpoints.Verify(site))return Json(Mode=="empty"?"{\"code\":0,\"data\":{\"Packages\":[]}}":"{\"code\":0,\"data\":{\"IsPaidUser\":true,\"Packages\":[{\"PackageCode\":\"pkg\",\"CycleTotalCapacity\":\"100\",\"CycleUsedCapacity\":\"20\",\"CycleRemainCapacity\":\"80\"}]}}");
            Check(r.RequestUri==SiteEndpoints.CreditPackages(site,false)||r.RequestUri==SiteEndpoints.CreditPackages(site,true),"Package endpoint fixed to account site");
            using var body=JsonDocument.Parse(await r.Content!.ReadAsByteArrayAsync(ct));Check(body.RootElement.GetProperty("PackageCodes")[0].GetString()=="pkg"&&body.RootElement.GetProperty("PageNumber").GetInt32()==1,"Bounded package query");
            if(Mode=="package-fail")return new(HttpStatusCode.BadGateway){Content=new StringContent("private error")};
            return Json("{\"code\":0,\"data\":{\"Accounts\":[{\"AccountId\":\"same\",\"PackageName\":\"package\",\"CycleCapacityRemainPrecise\":\"80\",\"DeductionEndTime\":1794010000000}]}}");
        }
        static HttpResponseMessage Json(string s)=>new(HttpStatusCode.OK){Content=new StringContent(s,Encoding.UTF8,"application/json")};
    }
    static async Task Transport(Clock clock)
    {
        foreach(var site in new[]{AccountSite.Cn,AccountSite.Intl})foreach(bool enterprise in new[]{false,true})
        {
            var handler=new TransportHandler(site,enterprise);using var control=new WorkBuddyControlPlane(site,handler,TimeSpan.FromMilliseconds(100));var credential=new AccountCredential(new("user",enterprise?"enterprise-secret":"",Site:site),"old-access","private-refresh",clock.Now.ToUnixTimeSeconds()+3600);
            var result=await control.FetchResourcesAsync(credential,CancellationToken.None);Check(enterprise?result.Quota.Unlimited&&handler.Calls==1:result.Quota.Remaining==80&&result.Packages.Count==1&&handler.Calls==3,"Personal packages deduplicate; enterprise does not query personal packages");
            int beforeDisabledFetch=handler.Calls;
            var quotaOnly=await control.FetchResourcesAsync(credential,CancellationToken.None,fetchPackages:false);
            Check(handler.Calls==beforeDisabledFetch+1&&!quotaOnly.PackagesKnown&&quotaOnly.Packages.Count==0&&(enterprise?quotaOnly.Quota.Unlimited:quotaOnly.Quota.Remaining==80),"Disabling credit details still fetches quota, without package requests on either site");
            foreach(string mode in new[]{"http","business","oversize","timeout","empty"})
            {
                if(enterprise&&mode=="empty")continue;handler.Mode=mode;bool rejected=false;try{await control.FetchResourcesAsync(credential,CancellationToken.None);}catch(Exception e) when(e is CredentialFailure or OperationCanceledException){rejected=true;}Check(rejected,"Bad resource query is unavailable, not zero: "+mode);
            }
            if(!enterprise){handler.Mode="package-fail";var partial=await control.FetchResourcesAsync(credential,CancellationToken.None);Check(partial.Quota.Remaining==80&&!partial.PackagesKnown&&partial.Packages.Count==0,"Package failure preserves independently verified balance");}
        }
        foreach(string malformed in new[]{"{}","{\"Packages\":[]}","{\"Packages\":[{}]}","{\"Packages\":[{\"CycleRemainCapacity\":\"NaN\"}]}","{\"Packages\":[{\"CycleRemainCapacity\":12}]}"})
        {using var doc=JsonDocument.Parse(malformed);bool rejected=false;try{ResourceParser.Quota(doc.RootElement,false);}catch(CredentialFailure){rejected=true;}Check(rejected,"Incomplete personal summary is not authoritative zero");}
    }
    static async Task Host(Clock clock,string directory)
    {
        using var keys=PreviewKeyDirectory.Open(Path.Combine(directory,"keys"));using var ledger=CreditLedger.Open(Path.Combine(directory,"keys"),[]);
        var control=new Control(clock);var store=new Store(Cred(clock));var(id,pool,manager)=Manager(clock,store,control);
        using var resourceCache=ResourceSnapshotCache.Open(Path.Combine(directory,"resources"));
        await using var app=LocalGateway.Build(new(SiteEndpoints.Model(AccountSite.Cn),Program.TestKey,Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,Maintenance:new(TimeSpan.FromMinutes(1)),Resources:new(TimeSpan.FromMinutes(1),ExpiringFirst:true),ResourceCache:resourceCache));
        await app.StartAsync();using var deadline=new CancellationTokenSource(TimeSpan.FromSeconds(5));
        while(app.Services.GetRequiredService<AccountResourceWorker>().Status.Runs==0)await Task.Delay(10,deadline.Token);
        Check(control.Refreshes==1&&control.Resources==1&&store.Saves==1&&ledger.Health.PendingAttempts==0,"Hosted workers coalesce credential refresh and do not create billing attempts");
        using var admin=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(5)};admin.DefaultRequestHeaders.Authorization=new("Bearer","synthetic-admin");
        using(var response=await admin.GetAsync("/admin/api/accounts"))
        {string text=await response.Content.ReadAsStringAsync();using var json=JsonDocument.Parse(text);Check(response.IsSuccessStatusCode&&json.RootElement.GetProperty("resourcePolling").GetProperty("runs").GetInt64()>=1&&json.RootElement.GetProperty("items")[0].GetProperty("upstreamQuota").GetProperty("remaining").GetDouble()==80&&!text.Contains("fresh-access")&&!text.Contains("fresh-refresh")&&!text.Contains("synthetic-user"),"Admin exposes bounded background status without credential material");}
        foreach(string action in new[]{"refresh_resources","refresh_credentials"})
        {using var response=await admin.PostAsJsonAsync("/admin/api/accounts",new{action,id="a"});Check(response.IsSuccessStatusCode,"Authenticated manual maintenance action: "+action);}
        Check(control.Refreshes==2&&control.Resources==2&&ledger.Health.PendingAttempts==0,"Manual operations use control plane without model billing");
        Check(resourceCache.Status.Saves==2&&!resourceCache.Status.WriteFailed,"Background and manual observations both persist through registered cache");
        await app.StopAsync();
        var cold=Manager(clock,new(Cred(clock,3600)),new(clock));Check(resourceCache.TryLoad(cold.Item3)&&cold.Item3.ResourceStatus.Single().Stale&&cold.Item3.ResourceStatus.Single().Remaining==80,"Hosted resource cache restores observation as stale after restart");
        bool wrongMode=false;try{await using var invalid=LocalGateway.Build(new(new("http://127.0.0.1/"),Program.TestKey,GoBridge:true,Maintenance:new(TimeSpan.FromMinutes(1))));}catch(ArgumentException){wrongMode=true;}Check(wrongMode,"Bridge cannot own independent maintenance workers");
    }
}
