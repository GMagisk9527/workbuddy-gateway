using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class OperationsParityTests
{
    static int checks;
    static void Check(bool value,string why){if(!value)throw new InvalidOperationException(why);checks++;}
    sealed class Control:IAccountControlPlane,IAccountResourcesControlPlane,IModelCatalogControlPlane
    {
        public int ResourceCalls;public bool LastFetchPackages=true;
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken token)=>throw new InvalidOperationException("No refresh expected");
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken token)=>Task.FromResult(CredentialVerification.Usable);
        public Task<byte[]> FetchCatalogAsync(AccountCredential c,CancellationToken token)=>Task.FromResult("{\"code\":0,\"data\":{\"models\":[{\"id\":\"m\"}]}}"u8.ToArray());
        public Task<AccountResourceObservation> FetchResourcesAsync(AccountCredential c,CancellationToken token,bool fetchPackages=true)
        {ResourceCalls++;LastFetchPackages=fetchPackages;return Task.FromResult(new AccountResourceObservation(new(100,0,100),[],false));}
    }
    sealed class Clock:TimeProvider
    {
        public DateTimeOffset Now=DateTimeOffset.UtcNow;
        public override DateTimeOffset GetUtcNow()=>Now;
        public override long GetTimestamp()=>Now.UtcTicks;
        public override long TimestampFrequency=>TimeSpan.TicksPerSecond;
    }
    static void Create(string root,string id,bool disabled=false)
    {using var files=CredentialFiles.Create(Path.Combine(root,id),new(new("uid-"+id),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.AddHours(3).ToUnixTimeSeconds()));if(disabled)files.Save(files.Load() with{Disabled=true});}
    static void Manifest(string root,params string[] ids)=>File.WriteAllBytes(Path.Combine(root,"accounts.json"),JsonSerializer.SerializeToUtf8Bytes(new{version=1,accounts=ids.Select(id=>new{id,directory=id})}));
    internal static async Task Run(string root)
    {
        Directory.CreateDirectory(root);Check(!Directory.EnumerateFileSystemEntries(root).Any(),"Empty synthetic scratch");
        Create(root,"a");Create(root,"b",true);Manifest(root,"a","b");var control=new Control();
        using var owner=StandaloneAccounts.OpenManifest(Path.Combine(root,"accounts.json"),_=>control);
        Check(!await owner.ReloadAsync(),"Unchanged disk does not reset live state");
        owner.Pool.ObserveModelFailure(new("a"),"m",400,"6004");
        Create(root,"c");Manifest(root,"a","b","c");
        Check(await owner.ReloadAsync()&&owner.Pool.Count==3,"New manifest entry admitted without restart");
        Check(owner.Pool.Snapshot(new("a"))!.Models!["m"].CooldownUntil>DateTimeOffset.UtcNow,"Reload preserves existing model cooldown");
        Check(owner.Credentials.Status.Single(x=>x.Id.Value=="b").Disabled,"Reload never re-enables stopped account");
        string good=File.ReadAllText(Path.Combine(root,"accounts.json"));File.WriteAllText(Path.Combine(root,"accounts.json"),"{broken");
        using(var worker=new CredentialReloadWorker(owner,TimeSpan.FromSeconds(5)))
        {await worker.RunOnceAsync();Check(worker.TaskState.Snapshot.Status=="failed"&&owner.Pool.Count==3,"Malformed manifest preserves last good pool");}
        File.WriteAllText(Path.Combine(root,"accounts.json"),good);
        using(var held=await owner.Credentials.AcquireForHeadersAsync(new("a"),SiteEndpoints.Model(AccountSite.Cn)))
        {
            using var cancel=new CancellationTokenSource(TimeSpan.FromMilliseconds(80));bool stopped=false;
            try{await owner.DeleteAsync("a",cancel.Token);}catch(OperationCanceledException){stopped=true;}
            Check(stopped&&File.Exists(Path.Combine(root,"a/credential.json"))&&owner.Pool.Count==3,"Deletion waits for active credential reader; cancellation commits nothing");
        }
        using var keys=PreviewKeyDirectory.Open(Path.Combine(root,"keys"),new("synthetic-key"));using var ledger=CreditLedger.Open(Path.Combine(root,"keys"),keys.List().Select(x=>x.Id));keys.AttachAccounting(ledger);
        var options=new LocalGatewayOptions(owner.Upstream,new byte[32],Security:new(keys,new("synthetic-admin"),ledger),Accounts:owner.Pool,Credentials:owner.Credentials,DirectUpstream:true,AccountOwner:owner,ConsoleDirectory:Path.Combine(root,"keys/console"));
        await using var app=LocalGateway.Build(options);await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app))};
        var settings=app.Services.GetRequiredService<ConsoleRuntime>().Settings;
        foreach(bool fetch in new[]{false,true})
        {
            string content=JsonSerializer.Serialize(new{models=new{blocklist=Array.Empty<string>()},logs=new{retentionDays=7},credits=new{fetch}});
            using var edit=new HttpRequestMessage(HttpMethod.Post,"/admin/api/config-edit"){Content=new StringContent(JsonSerializer.Serialize(new{acknowledged=true,content,revision=settings.View().Revision}),Encoding.UTF8,"application/json")};edit.Headers.Authorization=new("Bearer","synthetic-admin");
            using var edited=await client.SendAsync(edit);Check(edited.IsSuccessStatusCode,"Credit detail setting accepted through admin API");
            await owner.Credentials.FetchResourcesAsync(new("a"));Check(control.LastFetchPackages==fetch&&owner.Credentials.ResourceStatus.Single(x=>x.Id=="a").Known,"Hot-reloaded credit switch reaches control plane while quota remains known");
        }
        settings.Update("""{"models":{"blocklist":[]},"logs":{"retentionDays":7},"credits":{"fetch":false}}""",settings.View().Revision);
        settings.Update("""{"models":{"blocklist":[]},"logs":{"retentionDays":7}}""",settings.View().Revision);
        Check(owner.Credentials.FetchCreditPackages,"Removing credit override restores Go default enabled");
        async Task<int> Delete(string id,string auth="synthetic-admin")
        {using var request=new HttpRequestMessage(HttpMethod.Post,"/admin/api/account-delete"){Content=new StringContent(JsonSerializer.Serialize(new{file=id}),Encoding.UTF8,"application/json")};request.Headers.Authorization=new("Bearer",auth);using var response=await client.SendAsync(request);return (int)response.StatusCode;}
        Check(await Delete("b","wrong")==401&&owner.Pool.Count==3,"Delete requires admin authorization");
        Check(await Delete("../a")==404&&File.Exists(Path.Combine(root,"a/credential.json")),"Unknown or traversal input cannot delete a path");
        Check(await Delete("b")==200&&owner.Pool.Count==2&&!File.Exists(Path.Combine(root,"b/credential.json")),"Stopped account deletion removes only explicit credential");
        Check(await Delete("b")==404,"Repeated deletion reports missing account");
        Check(await Delete("a")==200&&await Delete("c")==200&&owner.Pool.Count==0,"Last account may be deleted without restarting server");
        Check(ledger.Health.Healthy&&ledger.Health.PendingAttempts==0,"Account mutations leave ledger healthy and empty");
        Check(!await owner.ReloadAsync()&&owner.Pool.Count==0,"Deleted accounts do not reappear during scan");
        await app.StopAsync();await app.DisposeAsync();
        using(var restored=StandaloneAccounts.OpenManifest(Path.Combine(root,"accounts.json"),_=>control))Check(restored.Pool.Count==0,"Empty pool survives restart");
        Maintenance(options with{ConsoleDirectory=Path.Combine(root,"isolated-console")});
        Policies();RuntimeConfig();PartialConfig();Defaults();Journal(Path.Combine(root,"journal"));
        await Probes(Path.Combine(root,"probe"));
        await RestrictedProbes(Path.Combine(root,"restricted-probes"));
        checks+=await CredentialEditingTests.Run(Path.Combine(root,"credential-edit"));
        checks+=await AdmissionTests.Run(Path.Combine(root,"admission"));
        checks+=await FirstTokenTests.Run(Path.Combine(root,"first-token"));
        checks+=await ServiceTimeoutTests.Run(Path.Combine(root,"service-timeouts"));
        checks+=await OutputAdmissionTests.Run(Path.Combine(root,"output-budget"));
        checks+=await RetryAdmissionTests.Run();
        checks+=RequestLogTests.Run(Path.Combine(root,"request-logs"));
        checks+=await SafeUpstreamTests.Run();
        Console.WriteLine(JsonSerializer.Serialize(new{operationsParityChecks=checks,realNetworkCalls=0,modelCreditsUsed=0}));
    }
    sealed class FakeProbe:HttpMessageHandler
    {
        public int Calls;public bool Reject;public string? OverrideBody;public HttpStatusCode OverrideStatus=HttpStatusCode.OK;
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken token)
        {
            Calls++;Check(request.RequestUri==SiteEndpoints.Model(AccountSite.Cn),"Probe uses pinned site endpoint");
            if(OverrideBody is not null)return Task.FromResult(new HttpResponseMessage(OverrideStatus){Content=new StringContent(OverrideBody,Encoding.UTF8,"text/event-stream")});
            return Task.FromResult(new HttpResponseMessage(Reject?HttpStatusCode.BadRequest:HttpStatusCode.OK){Content=new StringContent(Reject?"{\"code\":11128}":"data: {\"choices\":[{\"delta\":{\"content\":\"synthetic\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"total_tokens\":200,\"prompt_tokens\":100,\"completion_tokens\":100}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")});
        }
    }
    static void PartialConfig()
    {
        var catalog=new ModelCatalog();using var settings=new ConsoleSettings(catalog);
        foreach(string content in new[]{"{}","{\"models\":{}}","{\"logs\":{}}","{\"credits\":{\"fetch\":false}}"})
        {settings.Update(content,settings.View().Revision);Check(settings.RetentionDays==7,"Omitted Go configuration sections use requested seven-day log default");}
        settings.Update("""{"models":{"allowlist":["m"]},"logs":{"retentionDays":0}}""",settings.View().Revision);
        Check(settings.RetentionDays==0&&catalog.Allows("m")&&!catalog.Allows("n"),"Allow-only config accepted and explicit log disable preserved");
        foreach(string invalid in new[]{"{\"models\":null}","{\"logs\":null}","{\"logs\":{\"retentionDays\":0,\"retentionDays\":7}}","{\"models\":{\"blocklist\":null}}"})
        {
            var before=settings.View();bool rejected=false;
            try{settings.Update(invalid,before.Revision);}catch(ArgumentException){rejected=true;}
            Check(rejected&&settings.View()==before,"Invalid or duplicate optional section preserves configuration");
        }
    }
    static async Task RestrictedProbes(string root)
    {
        foreach(string mode in new[]{"http","sse","late-sse"})
        {
            string dir=Path.Combine(root,mode);Directory.CreateDirectory(dir);Create(dir,"a");Manifest(dir,"a");
            using(var owner=StandaloneAccounts.OpenManifest(Path.Combine(dir,"accounts.json"),_=>new Control()))
            {
                var catalog=new ModelCatalog();catalog.Load("{\"schema\":5,\"catalogs\":{\"cn\":[{\"id\":\"m\"},{\"id\":\"n\"}]}}"u8.ToArray());
                using var console=new ConsoleRuntime(new(owner.Upstream,new byte[32],Credentials:owner.Credentials,Catalog:catalog,ConsoleDirectory:Path.Combine(dir,"console")));
                foreach(string model in new[]{"m","n"})console.Telemetry.Complete(new("a",model,DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),200,true,null,null,null,null,null,1,null),[]);
                string rejection="{\"code\":11140}";
                var handler=new FakeProbe{OverrideStatus=mode=="http"?HttpStatusCode.BadRequest:HttpStatusCode.OK,OverrideBody=mode=="http"?rejection:(mode=="late-sse"?"data: {\"choices\":[{\"delta\":{\"content\":\"synthetic\"}}]}\n\n":"")+"data: "+rejection+"\n\n"};
                using var client=new HttpClient(handler);using var probe=new ModelPriceProbeWorker(owner.Credentials,catalog,console,client);
                using var deadline=new CancellationTokenSource(TimeSpan.FromSeconds(3));
                await probe.RunOnceAsync(deadline.Token);
                Check(owner.Credentials.Status.Single().Disabled,"Probe restriction disables account for "+mode);
                Check(handler.Calls==1&&probe.TaskState.Snapshot.Status=="failed","Restricted account sends no further model probes for "+mode);
                await probe.RunOnceAsync(deadline.Token);Check(handler.Calls==1,"Restriction is not replayed for "+mode);
            }
            using(var restored=StandaloneAccounts.OpenManifest(Path.Combine(dir,"accounts.json"),_=>new Control()))
                Check(restored.Credentials.Status.Single().Disabled,"Probe restriction survives restart for "+mode);
        }
    }
    static async Task Probes(string root)
    {
        Directory.CreateDirectory(root);Create(root,"a");Manifest(root,"a");using var owner=StandaloneAccounts.OpenManifest(Path.Combine(root,"accounts.json"),_=>new Control());
        var catalog=new ModelCatalog();catalog.Load("{\"schema\":5,\"catalogs\":{\"cn\":[{\"id\":\"m\"}]}}"u8.ToArray());
        var options=new LocalGatewayOptions(owner.Upstream,new byte[32],Credentials:owner.Credentials,Catalog:catalog,ConsoleDirectory:Path.Combine(root,"console"));
        using var console=new ConsoleRuntime(options);var handler=new FakeProbe();using var client=new HttpClient(handler);var clock=new Clock();
        using(var probe=new ModelPriceProbeWorker(owner.Credentials,catalog,console,client,clock))
        {
            await probe.RunOnceAsync();Check(handler.Calls==0&&probe.TaskState.Snapshot.Status=="skipped","No user-observed or expired-promotion model means no probe");
            console.Telemetry.Complete(new("a","m",DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),200,true,null,null,null,null,null,1,null),[]);
            await probe.RunOnceAsync();Check(handler.Calls==1&&probe.Results.Single().Cost==ModelCost.Free,"Synthetic complete usage establishes probe verdict");
            await probe.RunOnceAsync();Check(handler.Calls==1,"Twelve-hour window prevents repeat");
        }
        using(var restarted=new ModelPriceProbeWorker(owner.Credentials,catalog,console,client,clock))
        {
            await restarted.RunOnceAsync();Check(handler.Calls==1,"Probe window persists across restart");clock.Now=clock.Now.AddHours(12);handler.Reject=true;
            await restarted.RunOnceAsync();Check(handler.Calls==2&&restarted.TaskState.Snapshot.Status=="failed"&&restarted.Results.Single().Cost==ModelCost.Free,"Rejection is failed and preserves earlier verdict without replay");
            Check(!owner.Credentials.Status.Single().Disabled,"Price observation does not modify account rejection state");
        }
        var restoredCatalog=new ModelCatalog();restoredCatalog.Load(catalog.Export());
        // The saved synthetic clock is ahead, so use a separate current-time fixture.
        string offline=Path.Combine(root,"readonly");Directory.CreateDirectory(offline);
        File.WriteAllBytes(Path.Combine(offline,"price-probes.json"),JsonSerializer.SerializeToUtf8Bytes(new[]{new ModelPriceProbeWorker.Probe("cn","m",DateTimeOffset.UtcNow.ToUnixTimeSeconds(),ModelCost.Free)}));
        Check(PriceProbeState.Restore(offline,restoredCatalog)&&restoredCatalog.PriceLabel(AccountSite.Cn,"m")=="免费（实测）"&&handler.Calls==2,"Restoring price history never enables a probe or sends a request");
        using(var detail=JsonDocument.Parse(JsonSerializer.Serialize(restoredCatalog.PriceDetails())))
            Check(detail.RootElement.GetProperty("rates").GetProperty("cn|m").GetProperty("hasLog").GetBoolean(),"Rate detail uses original UI site/model key and real saved evidence");
        string path=Path.Combine(root,"console/price-probes.json");File.WriteAllText(path,"invalid");using(var broken=new ModelPriceProbeWorker(owner.Credentials,catalog,console,client,clock))
        {await broken.RunOnceAsync();Check(broken.Blocked&&handler.Calls==2&&File.ReadAllText(path)=="invalid","Corrupt probe state blocks sends and is never overwritten");}
    }
    static void Maintenance(LocalGatewayOptions options)
    {
        using var console=new ConsoleRuntime(options with{Catalog=new()});var clock=new Clock();using var worker=new ConsoleMaintenanceWorker(console,clock);
        Check(console.Settings.RetentionDays==7,"User override: log cleanup defaults to enabled with seven-day retention");
        worker.RunOnce();Check(worker.CleanupState.Snapshot.Runs==1,"Cleanup runs once at startup");worker.RunOnce();Check(worker.CleanupState.Snapshot.Runs==1,"One-second config polling does not repeatedly clean logs");
        string config=Path.Combine(options.ConsoleDirectory!,"config.json");
        File.WriteAllText(config,"{\"models\":{\"blocklist\":[\"m\"]},\"logs\":{\"retentionDays\":1}}");worker.RunOnce();
        Check(console.Settings.RetentionDays==1&&worker.ConfigState.Snapshot.Status=="success"&&worker.CleanupState.Snapshot.Runs==2,"Disk config applied and retention change triggers cleanup");
        var good=console.Settings.View();File.WriteAllText(config,"invalid");worker.RunOnce();Check(console.Settings.View()==good&&worker.ConfigState.Snapshot.Status=="failed","Invalid external config preserves valid memory");
        File.WriteAllText(config,good.Content);clock.Now=clock.Now.AddHours(1);worker.RunOnce();Check(worker.CleanupState.Snapshot.Runs==3,"Hourly cleanup runs with no model traffic");
        console.Settings.SetRetention(0);worker.RunOnce();Check(worker.CleanupState.Snapshot.NextRunAt==0,"Zero retention disables automatic cleanup");
        console.Telemetry.Complete(new("a","m",DateTimeOffset.UtcNow.AddDays(-2).ToUnixTimeMilliseconds(),200,true,null,null,null,null,null,1,null),[]);
        Check(worker.Cleanup()==0,"Zero retention manual cleanup also keeps logs");
        console.Settings.SetRetention(1);Check(worker.Cleanup()==1&&console.Telemetry.Rows().Length==1,"Log cleanup retains request statistics");
    }
    static void Policies()
    {
        var baseline=new SchedulingOptions(MaxAccount:3);
        var pool=new AccountPool(new[]{new AccountSnapshot(new("a"),SiteEndpoints.Model(AccountSite.Cn)),new AccountSnapshot(new("b"),SiteEndpoints.Model(AccountSite.Cn),Disabled:true),new AccountSnapshot(new("c"),SiteEndpoints.Model(AccountSite.Cn))},baseline);
        using var settings=new ConsoleSettings(new ModelCatalog());settings.AttachAccountPolicies(pool,new Dictionary<string,string>());
        settings.Update("""{"models":{"blocklist":[],"accounts":{"all":{"allowlist":["a"]},"m":{"blocklist":["c"]}}},"logs":{"retentionDays":7},"concurrent":{"MaxAcc":2}}""",settings.View().Revision);
        Check(pool.Options.MaxAccount==2&&!pool.SchedulingEnabled(new("b"))&&!pool.SchedulingEnabled(new("c")),"Config applies whitelist and concurrency");
        settings.ToggleAccount("b",true);
        Check(pool.SchedulingEnabled(new("b"))&&pool.Snapshot(new("b"))!.Disabled,"UI enable never clears upstream restriction");
        Check(pool.SchedulingEnabled(new("a"))&&!pool.SchedulingEnabled(new("c")),"Whitelist conversion preserves other accounts");
        settings.ToggleAccount("c",true);Check(pool.SchedulingEnabled(new("c"))&&!pool.Allows(new("c"),"m"),"Global toggle preserves per-model rules");
        settings.Update("""{"models":{"blocklist":[]},"logs":{"retentionDays":7}}""",settings.View().Revision);
        Check(pool.Options.MaxAccount==baseline.MaxAccount&&pool.Allows(new("c"),"m"),"Removing optional config restores baseline and clears old policies");
        var calendar=new CalendarMaintenanceOptions(Path.GetTempPath());
        Check(calendar.CheckinEnabled&&calendar.KeepaliveHours.SequenceEqual(new[]{22}),"Go calendar defaults: checkin on, local hour 22");
        Check(new CredentialMaintenanceOptions(TimeSpan.FromMinutes(5)).BatchSize==1024&&new AccountResourceOptions(TimeSpan.FromMinutes(5)).BatchSize==1024,"Default sweeps cover full supported pool");
        var catalog=new ModelCatalog();catalog.Load("""{"schema":5,"catalogs":{"cn":[{"id":"one"},{"id":"two"},{"id":"three"}]}}"""u8.ToArray());
        using var modelSettings=new ConsoleSettings(catalog);
        modelSettings.Update("""{"models":{"blocklist":[],"allowlist":["one"]},"logs":{"retentionDays":7}}""",modelSettings.View().Revision);
        Check(catalog.Allows("one")&&!catalog.Allows("two")&&!catalog.Allows("three"),"Model whitelist applies before routing");
        modelSettings.Toggle("two",true);
        Check(catalog.Allows("one")&&catalog.Allows("two")&&!catalog.Allows("three"),"Model toggle preserves other whitelist exclusions");
    }
    static void Journal(string path)
    {
        Directory.CreateDirectory(path);var clock=new Clock();var state=new ScheduledTaskState(clock);var journal=new ScheduledTaskJournal(path);journal.Attach("token_check",state);
        var trace=state.Begin();state.Finish(trace,1,1,0,TimeSpan.FromMinutes(5));Check(journal.Save(),"Completed task is persisted");
        var restored=new ScheduledTaskJournal(path);Check(restored.Lines(10,0).Length==1,"Completed task history survives restart");
        Check(restored.Cleanup(0,clock.Now.AddDays(8))==0&&restored.Cleanup(7,clock.Now.AddDays(8))==1,"Task history honors disabled and seven-day cleanup");
        Check(restored.Save()&&new ScheduledTaskJournal(path).Lines(10,0).Length==0,"Task cleanup is durable");
    }
    static void Defaults()
    {
        string[] names=["WORKBUDDY_CATALOG_REFRESH_SECONDS","WORKBUDDY_MAINTENANCE_SECONDS","WORKBUDDY_RESOURCES_SECONDS","WORKBUDDY_RELOAD_SECONDS","WORKBUDDY_PRICE_PROBE","WORKBUDDY_AFFINITY","WORKBUDDY_EXPIRING_FIRST"];
        var saved=names.ToDictionary(n=>n,Environment.GetEnvironmentVariable);
        try
        {
            foreach(string name in names)Environment.SetEnvironmentVariable(name,null);
            var tasks=StandaloneTaskOptions.FromEnvironment();
            Check(tasks.Catalog==TimeSpan.FromHours(1)&&tasks.Maintenance?.Interval==TimeSpan.FromMinutes(5)&&tasks.Resources?.Interval==TimeSpan.FromMinutes(5)&&tasks.Reload==TimeSpan.FromSeconds(5),"Actual startup defaults match Go periodic tasks");
            Check(!tasks.PriceProbe&&tasks.Affinity.Enabled&&tasks.Affinity.Mode==AffinityMode.Hybrid,"No paid probing by default; Go hybrid affinity enabled");
            foreach(string name in names.Take(4))Environment.SetEnvironmentVariable(name,"0");
            Environment.SetEnvironmentVariable("WORKBUDDY_AFFINITY","prefix");tasks=StandaloneTaskOptions.FromEnvironment();
            Check(tasks.Catalog is null&&tasks.Maintenance is null&&tasks.Resources is null&&tasks.Reload is null&&tasks.Affinity.Mode==AffinityMode.Prefix,"Explicit disable and deployed prefix affinity overrides survive");
            Environment.SetEnvironmentVariable("WORKBUDDY_PRICE_PROBE","invalid");bool rejected=false;try{StandaloneTaskOptions.FromEnvironment();}catch(ArgumentException){rejected=true;}Check(rejected,"Invalid price probe switch fails startup instead of enabling requests");
        }
        finally{foreach(var item in saved)Environment.SetEnvironmentVariable(item.Key,item.Value);}
    }
    static void RuntimeConfig()
    {
        var store=new SessionAffinity(new(true));using var settings=new ConsoleSettings(new());
        settings.AttachRuntime(new("original","",new(true,"Original")),new(true),store);
        store.Remember("key",new("a"));
        const string config="""{"models":{"blocklist":[]},"logs":{"retentionDays":7},"systemPrompt":{"fallback":"changed","force":"suffix"},"identity":{"enabled":false},"affinity":{"enabled":false},"tokenEstimation":{"modelMultipliers":{"m":2}}}""";
        settings.Update(config,settings.View().Revision);
        Check(settings.Prompts.Fallback=="changed"&&settings.Prompts.Force=="suffix"&&settings.Prompts.Identity?.Enabled==false,"Prompt and text rewrite settings apply together");
        Check(store.Lookup("key") is null&&!store.UsesSession&&settings.TokenMultiplier("m")==2&&settings.TokenMultiplier("other")==1,"Affinity disable clears bindings and token multiplier uses explicit model");
        var before=settings.View();bool rejected=false;
        try{settings.Update(config.Replace("\"m\":2","\"m\":5"),before.Revision);}catch(ArgumentException){rejected=true;}
        Check(rejected&&settings.View()==before&&settings.TokenMultiplier("m")==2,"Invalid Go multiplier bounds cannot partially apply config");
        settings.Update("""{"models":{"blocklist":[]},"logs":{"retentionDays":7}}""",before.Revision);
        store.Remember("new",new("a"));Check(store.UsesSession&&store.Lookup("new")==new AccountId("a")&&settings.Prompts.Fallback=="original"&&settings.Prompts.Identity?.Target=="Original"&&settings.TokenMultiplier("m")==1,"Removing runtime overrides restores deployment defaults");
    }
}
