namespace HttpMigration;

internal static class RetryAdmissionTests
{
    internal static async Task<int> Run()
    {
        int checks=0;void Check(bool ok,string reason){if(!ok)throw new InvalidOperationException(reason);checks++;}
        async Task Refuse(RetryAdmission.Scope scope)
        {try{using var ignored=await scope.BeginAsync(default);throw new InvalidOperationException("Retry accepted without budget");}catch(RequestFailure e){Check(e.Status==503&&e.ErrorType=="retry_budget_exhausted","Retry refusal is explicit");}}
        var gate=new RetryAdmission();gate.Configure(new(MaxExtraAttempts:1,MaxConcurrent:1,BackoffMaxMs:0));
        using var a=gate.Acquire();using var b=gate.Acquire();
        Check(await a.BeginAsync(default) is null&&await b.BeginAsync(default) is null&&gate.Status==(2,0,0),"Original attempts are not constrained by extra-attempt slots");
        using var extra=await a.BeginAsync(default);await Refuse(b);Check(gate.Status.Active==1,"Shared retry concurrency enforced");
        extra!.Dispose();extra.Dispose();Check(gate.Status.Active==0,"Retry permit releases idempotently");await Refuse(a);
        using(var next=await b.BeginAsync(default))Check(gate.Status.Active==1,"Released shared slot reusable by another logical request");
        gate.Configure(new(MaxConcurrent:0,BackoffMaxMs:0));using var c=gate.Acquire();await c.BeginAsync(default);await Refuse(c);
        Check(gate.Status.Originals==3,"Disabling retries preserves original requests");
        a.Dispose();b.Dispose();c.Dispose();Check(gate.Status.Originals==0&&gate.Status.Active==0,"All scopes release original accounting");
        gate.Configure(new(MaxConcurrent:10,RatioPercent:20,MinAllowance:1,BackoffMaxMs:0));
        var scopes=Enumerable.Range(0,5).Select(_=>gate.Acquire()).ToArray();foreach(var scope in scopes)await scope.BeginAsync(default);
        using(var one=await scopes[0].BeginAsync(default))using(var two=await scopes[1].BeginAsync(default))
        {await Refuse(scopes[2]);Check(gate.Status.Active==2,"Ratio allowance is minimum plus active originals times percentage");}
        foreach(var scope in scopes)scope.Dispose();
        gate.Configure(new(BackoffMaxMs:2000));using var canceled=gate.Acquire();await canceled.BeginAsync(default);
        using var cancellation=new CancellationTokenSource();var pending=canceled.BeginAsync(cancellation.Token);cancellation.Cancel();
        try{using var ignored=await pending;throw new InvalidOperationException("Canceled backoff proceeded");}catch(OperationCanceledException){}
        Check(gate.Status.Active==0,"Cancellation during backoff releases extra slot");
        Check(RetryAdmission.Backoff(1,2000,0)==TimeSpan.FromMilliseconds(150)&&RetryAdmission.Backoff(4,2000,1)==TimeSpan.FromMilliseconds(2000)&&RetryAdmission.Backoff(64,2000,1)==TimeSpan.FromMilliseconds(2000),"Exponential jittered backoff caps without overflow");
        using var settings=new ConsoleSettings(new());settings.AttachRetries(gate);
        settings.Update("""{"retry":{"maxExtraAttempts":0,"maxConcurrent":0,"ratioPercent":0,"minAllowance":0,"backoffMaxMs":0}}""",settings.View().Revision);
        Check(gate.Options==new RetryOptions(0,0,20,2,2000),"Explicit zero disables attempts/concurrency but uses Go defaults for scalar options");
        return checks;
    }
}
