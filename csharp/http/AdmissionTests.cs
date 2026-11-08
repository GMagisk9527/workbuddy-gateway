using System.Net;
using System.Net.Http.Json;
using System.Text;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class AdmissionTests
{
    static int checks;
    static void Check(bool ok,string reason){if(!ok)throw new InvalidOperationException(reason);checks++;}
    static async Task Until(Func<bool> ready)
    {using var deadline=new CancellationTokenSource(TimeSpan.FromSeconds(5));while(!ready())await Task.Delay(5,deadline.Token);}
    static async Task Reject(Func<Task> action,int status,string code)
    {try{await action();throw new InvalidOperationException("Expected admission refusal");}catch(RequestFailure e){Check(e.Status==status&&e.ErrorType==code,"Expected refusal code");}}
    internal static async Task<int> Run(string root)
    {
        checks=0;await Queue();await Preparing();await Memory();
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses","/v1/search","/v1/fetch","/v1/messages/count_tokens"})
            await Http(Path.Combine(root,Guid.NewGuid().ToString("N")),path);
        return checks;
    }
    static async Task Memory()
    {
        var sample=new MemorySample(new(0,128L<<20),[new(0,64L<<20),new(0,32L<<20)]);
        using var memory=new PreparationAdmission(0,1,()=>sample);
        Check(memory.Status.Limit==4L<<20,"Implicit body budget respects smallest parent cgroup at one eighth");
        sample=sample with{Cgroups=[]};Check(memory.Status.Limit==32L<<20,"Runtime one-quarter budget applied");
        sample=MemorySample.Empty;Check(memory.Status.Limit==64L<<20,"No finite memory limit uses 64 MiB budget");
        memory.Configure(new(MaxBodyBytes:1024));using var retained=memory.Acquire();retained.Reserve(1024);
        await Reject(()=>{memory.CheckAdmission();return Task.CompletedTask;},503,"overloaded_error");
        memory.Configure(new(MaxBodyBytes:512));Check(memory.Used==1024,"Hot budget shrink retains bytes for existing request");
        retained.Retain(256);memory.CheckAdmission();Check(memory.Status.Used==256,"Released bytes permit new requests below shrunk budget");
        sample=new(new(85,100),[]);await Reject(()=>{memory.CheckAdmission();return Task.CompletedTask;},503,"memory_pressure");
        sample=new(new(80,100),[]);await Reject(()=>{memory.CheckAdmission();return Task.CompletedTask;},503,"memory_pressure");
        sample=new(new(75,100),[]);memory.CheckAdmission();Check(!memory.Status.Pressured,"Runtime low watermark clears pressure");
        sample=new(new(10,100),[new(85,100)]);await Reject(()=>{memory.CheckAdmission();return Task.CompletedTask;},503,"memory_pressure");
        sample=new(new(80,100),[new(10,100)]);await Reject(()=>{memory.CheckAdmission();return Task.CompletedTask;},503,"memory_pressure");
        sample=new(new(75,100),[new(75,100)]);memory.CheckAdmission();Check(!memory.Status.Pressured,"Every scope must reach low watermark before reopening");
        memory.Configure(new(Enabled:false,MaxBodyBytes:1));sample=new(new(100,100),[new(100,100)]);memory.CheckAdmission();retained.Reserve(1024);
        Check(memory.Status.Limit==long.MaxValue&&!memory.Status.Pressured&&memory.Used==1280,"Explicit memory protection disable removes pressure and aggregate body gate");
        retained.Dispose();Check(memory.Used==0,"Budget counters remain valid while protection disabled");
        var gate=new RequestAdmission(new(),memory.CheckAdmission);using var settings=new ConsoleSettings(new());settings.AttachAdmission(gate,memory);
        settings.Update("""{"admission":{"memoryProtection":true,"maxBodyBytes":2048,"memoryHighWatermarkPercent":90,"memoryLowWatermarkPercent":70}}""",settings.View().Revision);
        Check(memory.Options==new MemoryAdmissionOptions(true,2048,90,70),"Memory protection reload applies all Go controls");
        string basis=Path.Combine(Path.GetTempPath(),"synthetic-cgroup");var paths=SystemMemorySampler.CgroupPaths(basis,"0::/a/b\n4:cpu,memory:/nested\n0::/../../escape");
        Check(paths.Any(p=>p.Used==Path.Combine(basis,"a","b","memory.current"))&&paths.Any(p=>p.Used==Path.Combine(basis,"a","memory.current"))&&paths.Any(p=>p.Used==Path.Combine(basis,"memory.current")),"Nested v2 and ancestor memory scopes sampled");
        Check(paths.Any(p=>p.Used==Path.Combine(basis,"memory","nested","memory.usage_in_bytes"))&&paths.All(p=>p.Used.StartsWith(basis+Path.DirectorySeparatorChar,StringComparison.Ordinal))&&paths.Distinct().Count()==paths.Length,"v1 paths included, traversal rejected, scopes deduplicated");
        var large=new MemoryUsage(long.MaxValue,long.MaxValue);Check(large.Threshold(85)>0&&large.Threshold(100)==long.MaxValue,"Threshold arithmetic cannot overflow");
        var sampler=new SystemMemorySampler();var current=sampler.Snapshot();Check(ReferenceEquals(current,sampler.Snapshot())&&current.Runtime.Limit>0,"System sampler bounded to one sample per 250 ms");
    }
    static async Task Queue()
    {
        var options=new AdmissionOptions(MaxActive:1,MaxWaiting:4,MaxWaitingPerKey:2);
        var gate=new AdmissionQueue(options,()=>{});using var first=await gate.AcquireAsync("a",TimeSpan.FromSeconds(2),default);
        var a1=gate.AcquireAsync("a",TimeSpan.FromSeconds(2),default);var a2=gate.AcquireAsync("a",TimeSpan.FromSeconds(2),default);
        await Reject(async()=>{using var ignored=await gate.AcquireAsync("a",TimeSpan.FromSeconds(2),default);},429,"request_queue_full");
        var b1=gate.AcquireAsync("b",TimeSpan.FromSeconds(2),default);
        first.Dispose();using var turnA=await a1;
        Check(!a2.IsCompleted&&!b1.IsCompleted,"Only one active permit");turnA.Dispose();using var turnB=await b1;
        Check(!a2.IsCompleted,"Equal-load flows rotate without violating per-flow FIFO");turnB.Dispose();(await a2).Dispose();
        Check(gate.Status==(0,0,0),"All flow entries and permits released");

        gate.Configure(options with{MaxActive=2});var holdA=await gate.AcquireAsync("a",TimeSpan.FromSeconds(2),default);
        var holdB=await gate.AcquireAsync("a",TimeSpan.FromSeconds(2),default);
        var waitA=gate.AcquireAsync("a",TimeSpan.FromSeconds(2),default);var waitB=gate.AcquireAsync("b",TimeSpan.FromSeconds(2),default);
        holdA.Dispose();using var least=await waitB;Check(!waitA.IsCompleted,"Least-active flow wins even behind another flow");
        gate.Configure(options);holdB.Dispose();Check(!waitA.IsCompleted&&gate.Status.Active==1,"Hot shrink preserves active request and waits below new limit");
        least.Dispose();(await waitA).Dispose();

        using var busy=await gate.AcquireAsync("a",TimeSpan.FromSeconds(1),default);
        using var cancellation=new CancellationTokenSource();var canceled=gate.AcquireAsync("b",TimeSpan.FromSeconds(2),cancellation.Token);cancellation.Cancel();
        try{(await canceled).Dispose();throw new InvalidOperationException("Cancellation ignored");}catch(OperationCanceledException){}
        Check(gate.Status.Waiting==0,"Cancellation removes ticket");
        await Reject(async()=>{using var ignored=await gate.AcquireAsync("b",TimeSpan.FromMilliseconds(30),default);},429,"request_queue_timeout");
        Check(gate.Status.Waiting==0,"Timeout removes ticket");
        gate.Configure(options with{MaxWaiting=0});await Reject(async()=>{using var ignored=await gate.AcquireAsync("b",TimeSpan.FromSeconds(1),default);},429,"request_queue_full");
        busy.Dispose();
        bool pressured=false;var memory=new AdmissionQueue(options,()=>{if(pressured)throw new RequestFailure("memory",503,"overloaded_error");});
        using var prior=await memory.AcquireAsync("a",TimeSpan.FromSeconds(2),default);var queued=memory.AcquireAsync("b",TimeSpan.FromSeconds(2),default);
        pressured=true;prior.Dispose();await Reject(async()=>{using var ignored=await queued;},503,"overloaded_error");
        Check(memory.Status==(0,0,0),"Memory pressure checked on queued grant without permit leak");
    }
    static async Task Preparing()
    {
        var options=new AdmissionOptions(MaxActive:3,MaxPreparing:1,WaitTimeoutMs:150);
        var gate=new RequestAdmission(options,()=>{});
        using var first=await gate.AcquireAsync("a",default);var second=gate.AcquireAsync("b",default);
        Check(gate.Status==(2,0,1,1),"Preparing wait holds global admission");first.FinishPreparing();using var ready=await second;
        Check(gate.Status==(2,0,1,0),"Preparation releases early while response stays admitted");
        var third=gate.AcquireAsync("c",default);gate.Configure(options with{MaxPreparing=0});using var disabled=await third;
        Check(gate.Status.PreparingWaiting==0,"Disabling preparation drains existing waiters");
        first.Dispose();ready.Dispose();disabled.Dispose();Check(gate.Status==(0,0,0,0),"Preparation leases idempotently return both permits");

        gate.Configure(options with{MaxPreparing=1,MaxActive=2});using var one=await gate.AcquireAsync("a",default);
        var blocked=gate.AcquireAsync("b",default);await Reject(async()=>{using var ignored=await blocked;},429,"request_prepare_queue_full");
        Check(gate.Status==(1,0,1,0),"Preparation timeout returns global permit");
        using var settings=new ConsoleSettings(new());settings.AttachAdmission(gate);
        settings.Update("""{"admission":{"maxActive":1,"maxPreparing":0,"maxWaiting":0,"waitTimeoutMs":0,"fairQueue":false}}""",settings.View().Revision);
        Check(gate.Options.MaxActive==1&&gate.Options.MaxPreparing==0&&gate.Options.MaxWaiting==0&&gate.Options.WaitTimeoutMs==15000&&!gate.Options.FairQueue,"Partial Go admission configuration applied");
        settings.Update("{}",settings.View().Revision);Check(gate.Options==options with{MaxPreparing=1,MaxActive=2},"Removing config restores startup limits");
        foreach(string invalid in new[]{"""{"admission":{"maxWaiting":-1}}""","""{"admission":{"waitTimeoutMs":-1}}""","""{"admission":{"maxPreparing":1,"maxPreparing":2}}"""})
        {var prior=settings.View();try{settings.Update(invalid,prior.Revision);throw new InvalidOperationException("Invalid config accepted");}catch(ArgumentException){}Check(settings.View()==prior,"Invalid admission edit preserves active configuration");}
    }
    sealed class Control:IAccountControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken token)=>throw new InvalidOperationException("No real authentication");
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken token)=>Task.FromResult(CredentialVerification.Usable);
    }
    sealed class Backend:HttpMessageHandler
    {
        public int Calls;public TaskCompletionSource Entered=new(TaskCreationOptions.RunContinuationsAsynchronously),Release=new(TaskCreationOptions.RunContinuationsAsynchronously);
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken token)
        {Interlocked.Increment(ref Calls);Entered.TrySetResult();await Release.Task.WaitAsync(token);return new(HttpStatusCode.OK){Content=new StringContent("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":1}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")};}
    }
    static async Task Http(string root,string path)
    {
        Directory.CreateDirectory(root);var endpoint=SiteEndpoints.Model(AccountSite.Cn);var id=new AccountId("a");
        using var files=CredentialFiles.Create(Path.Combine(root,"credential"),new(new("synthetic-user"),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.AddHours(2).ToUnixTimeSeconds()));
        var pool=new AccountPool([new AccountSnapshot(id,endpoint)]);var credentials=new CredentialManager(pool,[new(id,endpoint,files,new Control())]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(root,"keys"));using var ledger=CreditLedger.Open(Path.Combine(root,"keys"),[]);keys.AttachAccounting(ledger);
        var a=keys.Create(new("a",QuotaTotal:100,MaxConcurrent:20));var b=keys.Create(new("b",QuotaTotal:100,MaxConcurrent:20));string secretA=a.TakeSecret(),secretB=b.TakeSecret();
        var backend=new Backend();var options=new LocalGatewayOptions(endpoint,new byte[32],Security:new(keys,new("admin"),ledger),Accounts:pool,Credentials:credentials,DirectUpstream:true,UpstreamHandler:backend){Admission=new(MaxActive:1,MaxWaiting:2,MaxWaitingPerKey:1,WaitTimeoutMs:1500)};
        await using var app=LocalGateway.Build(options);await app.StartAsync();var gate=app.Services.GetRequiredService<RequestAdmission>();
        using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false,MaxResponseDrainSize=0}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(5)};
        async Task<HttpResponseMessage> Send(string target,string secret,string body,CancellationToken token=default)
        {using var request=new HttpRequestMessage(HttpMethod.Post,target){Content=new StringContent(body,Encoding.UTF8,"application/json")};request.Headers.Authorization=new("Bearer",secret);request.Headers.Add("x-session-id",Guid.NewGuid().ToString("N"));return await client.SendAsync(request,token);}
        const string chat="""{"model":"m","messages":[{"role":"user","content":"synthetic"}],"stream":false}""";
        var first=Send("/v1/chat/completions",secretA,chat);await backend.Entered.Task.WaitAsync(TimeSpan.FromSeconds(3));
        long bytes=app.Services.GetRequiredService<PreparationAdmission>().Used;
        using var cancellation=new CancellationTokenSource();var waiting=Send(path,secretB,"invalid-unread-json",cancellation.Token);await Until(()=>gate.Status.Waiting==1);
        using(var full=await Send(path,secretB,"invalid-unread-json"))Check((int)full.StatusCode==429&&full.Headers.RetryAfter?.Delta==TimeSpan.FromSeconds(1),"Per-key queue limit and Retry-After enforced before parsing: "+path);
        Check(backend.Calls==1&&ledger.Health.PendingAttempts==1&&ledger.GetUsage(b.Item.Id).Total==0&&app.Services.GetRequiredService<PreparationAdmission>().Used==bytes,"Queued and rejected requests neither read body nor enter WAL/upstream: "+path);
        cancellation.Cancel();try{using var ignored=await waiting;}catch(OperationCanceledException){}await Until(()=>gate.Status.Waiting==0);
        gate.Configure(gate.Options with{WaitTimeoutMs=50});using(var timeout=await Send(path,secretB,"invalid-unread-json"))Check((int)timeout.StatusCode==429&&(await timeout.Content.ReadAsStringAsync()).Contains("超时"),"Queue timeout before malformed body: "+path);
        Check(gate.Status.Waiting==0&&backend.Calls==1,"Canceled and timed-out requests never run later");
        gate.Configure(gate.Options with{WaitTimeoutMs=1500});
        var revoked=Send(path,secretB,path.EndsWith("responses",StringComparison.Ordinal)?"""{"model":"m","input":"synthetic"}""":path.EndsWith("search",StringComparison.Ordinal)?"""{"query":"synthetic"}""":path.EndsWith("fetch",StringComparison.Ordinal)?"""{"url":"https://example.com"}""":chat);
        await Until(()=>gate.Status.Waiting==1);keys.SetEnabled(b.Item.Id,false);backend.Release.TrySetResult();
        using(var success=await first)Check(success.IsSuccessStatusCode,"Already admitted request completes");
        using(var denied=await revoked)Check((int)denied.StatusCode==403,"Key policy rechecked after queue: "+path);
        await Until(()=>gate.Status.Active==0);
        Check(backend.Calls==1&&ledger.Health.PendingAttempts==0&&ledger.GetUsage(a.Item.Id).Total==1&&ledger.GetUsage(b.Item.Id).Total==0,"Exact ledger settlement and no queued revoked dispatch: "+path);
        var memory=app.Services.GetRequiredService<PreparationAdmission>();memory.Configure(new(MaxBodyBytes:1));using var retained=memory.Acquire();retained.Reserve(1);
        using(var pressure=await Send(path,secretA,"invalid-unread-json"))Check((int)pressure.StatusCode==503&&pressure.Headers.RetryAfter?.Delta==TimeSpan.FromSeconds(1),"Aggregate memory admission rejects before body or upstream: "+path);
        Check(backend.Calls==1&&ledger.Health.PendingAttempts==0,"Memory refusal creates no ledger attempt");
    }
}
