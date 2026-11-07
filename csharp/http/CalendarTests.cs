using System.Net;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class CalendarTests
{
    static int checks;
    static void Check(bool value,string message){if(!value)throw new InvalidOperationException(message);checks++;}
    sealed class Clock:TimeProvider{public DateTimeOffset Now=DateTimeOffset.Parse("2026-11-06T07:50:00+08:00");public override DateTimeOffset GetUtcNow()=>Now;}
    static CheckinTarget Target(char key,string id="a",bool supported=true,bool eligible=true)=>new(new string(key,64),new(id),supported,eligible);
    static DateTimeOffset At(string time)=>DateTimeOffset.Parse("2026-11-06T"+time+"+08:00");
    sealed class Store(AccountCredential c,bool disabled=false):ICredentialPersistence
    {public PersistedCredential Saved=new(c,disabled);public PersistedCredential Load()=>Saved;public void Save(PersistedCredential value)=>Saved=value;public void Dispose(){}}
    sealed class Control(Clock time):IAccountControlPlane,IAccountCheckinControlPlane
    {
        public int Refreshes,Checkins;public bool FailCheckin;public TaskCompletionSource? Entered,Release;
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken ct){Refreshes++;return Task.FromResult(new AccountCredential(c.Identity,"new-access","new-refresh",time.Now.ToUnixTimeSeconds()+7200,lastRefreshTime:time.Now.ToUnixTimeSeconds()));}
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken ct)=>Task.FromResult(CredentialVerification.Usable);
        public async Task<CheckinResult> CheckinAsync(AccountCredential c,CancellationToken ct){Checkins++;Entered?.TrySetResult();if(Release is not null)await Release.Task.WaitAsync(ct);if(FailCheckin)throw new CredentialFailure("checkin_rejected");return CheckinResult.Already;}
    }
    sealed class Handler:HttpMessageHandler
    {
        internal int Calls;
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage r,CancellationToken ct)
        {Calls++;Check(r.RequestUri?.ToString()=="https://www.codebuddy.cn/v2/billing/meter/daily-checkin"&&r.Method==HttpMethod.Post,"Pinned check-in endpoint");Check(r.Headers.Authorization?.Parameter=="synthetic-access"&&await r.Content!.ReadAsStringAsync(ct)=="{}","Only credential headers and empty object");Check(r.Headers.UserAgent.ToString()=="WorkBuddy-Gateway-CSharp/0.1"&&!r.Headers.Contains("X-Client-Platform"),"Honest gateway identity");return new(HttpStatusCode.OK){Content=new StringContent("{\"code\":0}")};}
    }
    static AccountCredential Cred(Clock clock,string uid,long last=0,AccountSite site=AccountSite.Cn,string enterprise="")=>new(new(uid,enterprise,Site:site),"synthetic-access","synthetic-refresh",clock.Now.ToUnixTimeSeconds()+7200,lastRefreshTime:last);
    internal static async Task Run(string scratch)
    {
        checks=0;Directory.CreateDirectory(scratch);Check(!Directory.EnumerateFileSystemEntries(scratch).Any(),"Empty calendar scratch");var clock=new Clock();var target=Target('a');string path=Path.Combine(scratch,"restart");
        using(var s=new DailyCheckinScheduler(path,clock,_=>0))
        {Check(s.Claim([target],true).Count==0,"Before 08:00 only plan");Check(s.View(target).NextAt==At("08:00:00").ToUnixTimeSeconds(),"First CST boundary");clock.Now=At("08:00:00");var job=s.Claim([target]).Single();Check(job.Attempt==1&&s.View(target).Status=="running","Persisted first claim");Check(s.Claim([target]).Count==0,"No duplicate in-flight claim");}
        clock.Now=At("08:01:00");
        using(var s=new DailyCheckinScheduler(path,clock,_=>throw new Exception("No re-randomization")))
        {Check(s.Claim([target],true).Count==0&&s.View(target).Status=="failed","Restart consumes uncertain first attempt");clock.Now=At("09:30:00");var job=s.Claim([target]).Single();Check(job.Attempt==2,"Second attempt in retry window");s.Complete(job,true);Check(s.View(target).Status=="success"&&s.Claim([target]).Count==0,"Success not resent");}
        using(var s=new DailyCheckinScheduler(path,clock))Check(s.Claim([target],true).Count==0&&s.View(target).Attempts==2,"Success and attempts survive restart");
        clock.Now=At("08:00:00");using(var s=new DailyCheckinScheduler(Path.Combine(scratch,"retry"),clock,_=>0))
        {var first=s.Claim([target],true).Single();s.Complete(first,false);clock.Now=At("09:29:59");Check(s.Claim([target]).Count==0,"No early retry");clock.Now=At("09:30:00");var second=s.Claim([target]).Single();s.Complete(second,false);clock.Now=At("09:59:59");Check(s.Claim([target]).Count==0&&s.View(target).Attempts==2,"At most two attempts per day");}
        clock.Now=At("15:00:00");using(var s=new DailyCheckinScheduler(Path.Combine(scratch,"startup"),clock,_=>0))
        {var first=s.Claim([target,Target('b',"b")],true).Single();clock.Now=At("15:00:10");s.Complete(first,true);clock.Now=At("15:01:00");Check(s.Claim([target,Target('b',"b")]).Count==0,"Completion-based minute gap");clock.Now=At("15:01:10");var second=s.Claim([target,Target('b',"b")]).Single();Check(second.Key!=first.Key,"Persisted startup queue order");s.Complete(second,true);Check(s.View(target with{Id=new("renamed")}).Status=="success","Identity survives account rename");}
        clock.Now=At("23:59:50");using(var s=new DailyCheckinScheduler(Path.Combine(scratch,"midnight"),clock,_=>0))
        {Check(s.Claim([target,Target('b',"b")],true).Count==1&&s.View(Target('b',"b")).Status=="missed","Startup queue never crosses midnight");}
        clock.Now=At("08:00:00");using(var s=new DailyCheckinScheduler(Path.Combine(scratch,"write-fail"),clock,_=>0,_=>throw new IOException("synthetic")))
        {Check(s.Claim([target],true).Count==0&&s.Blocked,"Failed durable claim never returns jobs");Check(s.Claim([target],true).Count==0&&s.View(target).Status=="unknown","Failure stays blocked");}
        string corrupt=Path.Combine(scratch,"corrupt");using(var s=new DailyCheckinScheduler(corrupt,clock)){}File.WriteAllText(Path.Combine(corrupt,"checkin-state.json"),"{bad");
        using(var s=new DailyCheckinScheduler(corrupt,clock))Check(s.Blocked&&s.Claim([target],true).Count==0&&File.ReadAllText(Path.Combine(corrupt,"checkin-state.json"))=="{bad","Corrupt state preserved without RPC");
        using(var s=new DailyCheckinScheduler(Path.Combine(scratch,"eligibility"),clock,_=>0))Check(s.Claim([target with{Eligible=false},Target('b',"b",false)],true).Count==0,"Disabled, international, enterprise targets excluded");
        foreach(var (status,json,ok) in new[]{(200,"{\"code\":0}",true),(500,"{\"code\":0}",false),(409,"{\"code\":10001}",true),(400,"{\"msg\":\"already checked in\"}",true),(200,"{\"code\":11128,\"msg\":\"already checked in\"}",false),(302,"{\"code\":10001}",false),(200,"{}",false),(200,"null",false)})
        {bool accepted=true;try{_=CheckinResponse.Parse(status,Encoding.UTF8.GetBytes(json));}catch(CredentialFailure){accepted=false;}Check(accepted==ok,"Strict check-in transport result "+status+json);}
        using(var h=new Handler())using(var control=new WorkBuddyControlPlane(AccountSite.Cn,h))
        {Check(await control.CheckinAsync(Cred(clock,"uid"),CancellationToken.None)==CheckinResult.Success,"Real adapter with synthetic transport");bool skipped=false;try{await control.CheckinAsync(Cred(clock,"uid",enterprise:"enterprise"),CancellationToken.None);}catch(CredentialFailure){skipped=true;}Check(skipped&&h.Calls==1,"Enterprise sends no check-in request");}
        var id=new AccountId("a");var c=new Control(clock);var store=new Store(Cred(clock,"uid"));var pool=new AccountPool([new(id,SiteEndpoints.Model(AccountSite.Cn))],time:clock);var manager=new CredentialManager(pool,[new(id,SiteEndpoints.Model(AccountSite.Cn),store,c)],clock);
        string key=manager.CheckinTargets.Single().Key;Check(key==Convert.ToHexStringLower(System.Security.Cryptography.SHA256.HashData(Encoding.UTF8.GetBytes("cn:uid:uid"))),"Go account digest compatible");
        c.Entered=new(TaskCreationOptions.RunContinuationsAsynchronously);c.Release=new(TaskCreationOptions.RunContinuationsAsynchronously);Task request=manager.CheckinAsync(id,key);await c.Entered.Task;await manager.RefreshAsync(id).WaitAsync(TimeSpan.FromSeconds(2));c.Release.SetResult();await request;Check(c.Refreshes==1,"Check-in does not hold credential writer lock during network I/O");
        using(var s=new DailyCheckinScheduler(Path.Combine(scratch,"worker"),clock,_=>0))
        {using var worker=new DailyCheckinWorker(s,manager,clock);c.Entered=null;c.Release=null;c.FailCheckin=true;await worker.RunOnceAsync(true);Check(worker.TaskState.Snapshot.Status=="failed"&&s.View(manager.CheckinTargets.Single()).Attempts==1,"Worker failures preserved");}
        await KeepaliveChecks(scratch,clock);await Host(Path.Combine(scratch,"host"));Console.WriteLine(JsonSerializer.Serialize(new{calendarChecks=checks,realModelCalls=0}));
    }
    static async Task Host(string scratch)
    {
        var clock=new Clock{Now=DateTimeOffset.UtcNow};var id=new AccountId("host");var endpoint=SiteEndpoints.Model(AccountSite.Cn);var c=new Control(clock);using var file=new Store(Cred(clock,"host-uid"));var pool=new AccountPool([new(id,endpoint)]);var manager=new CredentialManager(pool,[new(id,endpoint,file,c)]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(scratch,"keys"));using var ledger=CreditLedger.Open(Path.Combine(scratch,"keys"),[]);
        await using var app=LocalGateway.Build(new(endpoint,new byte[32],Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,Calendar:new(Path.Combine(scratch,"calendar"))));await app.StartAsync();
        using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app))};client.DefaultRequestHeaders.Authorization=new("Bearer","synthetic-admin");
        using var response=await client.GetAsync("/admin/api/tasks");using var doc=JsonDocument.Parse(await response.Content.ReadAsStringAsync());var rows=doc.RootElement.GetProperty("tasks").EnumerateArray().ToArray();
        Check(rows.Length==11&&new[]{"daily_checkin","keepalive","keepalive_catchup"}.All(id=>rows.Single(r=>r.GetProperty("id").GetString()==id).GetProperty("enabled").GetBoolean()),"Hosted task registrations enabled in admin view");
        Check(rows.Single(r=>r.GetProperty("id").GetString()=="keepalive_catchup").GetProperty("nextRunAt").GetInt64()>0,"Startup 90s scheduled before execution");
        Check(c.Refreshes==0&&ledger.Health.Seq==0&&ledger.Health.PendingAttempts==0,"No eager keepalive or model billing on startup");await app.StopAsync();
    }
    static async Task KeepaliveChecks(string scratch,Clock clock)
    {
        clock.Now=At("12:00:00");var ids=new[]{new AccountId("old"),new AccountId("new"),new AccountId("disabled")};var endpoint=SiteEndpoints.Model(AccountSite.Cn);var pool=new AccountPool(ids.Select(id=>new AccountSnapshot(id,endpoint)),time:clock);
        var controls=ids.Select(_=>new Control(clock)).ToArray();var stores=ids.Select((id,i)=>new Store(Cred(clock,id.Value,i==1?clock.Now.AddHours(-1).ToUnixTimeSeconds():0),i==2)).ToArray();
        var manager=new CredentialManager(pool,ids.Select((id,i)=>new CredentialRegistration(id,endpoint,stores[i],controls[i])),clock);var gaps=new List<TimeSpan>();var options=new CalendarMaintenanceOptions(Path.Combine(scratch,"unused"),false,[22,10,22]);
        using var worker=new KeepaliveWorker(manager,options,clock,TimeZoneInfo.Utc,(gap,ct)=>{ct.ThrowIfCancellationRequested();gaps.Add(gap);return Task.CompletedTask;});
        await worker.RunOnceAsync(true);Check(controls[0].Refreshes==1&&controls[1].Refreshes==0&&controls[2].Refreshes==0,"Catchup only overdue usable accounts");Check(worker.CatchupState.Snapshot.NextRunAt==0,"Catchup is one-shot");
        await worker.RunOnceAsync(true);Check(worker.CatchupState.Snapshot.Status=="skipped"&&controls[0].Refreshes==1,"Fresh catchup not repeated");
        await worker.RunOnceAsync(false);Check(controls[0].Refreshes==2&&controls[1].Refreshes==1&&controls[2].Refreshes==0&&gaps.SequenceEqual(new[]{TimeSpan.FromSeconds(1)}),"Daily refresh is serial with one-second gap");
        Check(worker.TaskState.Snapshot.Status=="success"&&worker.TaskState.Snapshot.NextRunAt>clock.Now.ToUnixTimeSeconds(),"Next daily run recorded");
        Check(KeepaliveWorker.Next(At("22:00:00"),[22],TimeZoneInfo.CreateCustomTimeZone("CST",TimeSpan.FromHours(8),"CST","CST"))==At("22:00:00").AddDays(1),"Exact hour schedules next day");
        Check(KeepaliveWorker.Next(clock.Now,[],TimeZoneInfo.Utc) is null&&new CalendarMaintenanceOptions(scratch,false,[]).KeepaliveHours.Count==0,"Empty hours disable keepalive and catchup");
        using var cancelled=new CancellationTokenSource();cancelled.Cancel();bool ended=false;try{await worker.RunOnceAsync(false,cancelled.Token);}catch(OperationCanceledException){ended=true;}Check(ended,"Cancelled wait does not execute refresh");
    }
}
