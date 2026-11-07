using AccountMaintenance;
namespace HttpMigration;
public sealed record AccountResourceOptions(TimeSpan Interval,int BatchSize=1024,bool ExpiringFirst=false)
{
    internal void Validate(){if(Interval<TimeSpan.FromSeconds(30)||Interval>TimeSpan.FromHours(24)||BatchSize is <1 or >1024)throw new ArgumentException("Invalid resource polling settings");}
}
public sealed record ResourceWorkerStatus(long Runs,int Attempted,int Succeeded,int Failed,bool CacheWriteFailed=false);
public sealed class AccountResourceWorker:BackgroundService
{
    readonly CredentialManager credentials;readonly AccountResourceOptions options;int running;int cursor;
    readonly ResourceSnapshotCache? cache;
    readonly SemaphoreSlim requested=new(0,1);
    readonly StandaloneAccounts? owner;
    ResourceWorkerStatus status=new(0,0,0,0);public ResourceWorkerStatus Status=>Volatile.Read(ref status);
    public ScheduledTaskState TaskState{get;}=new();
    public AccountResourceWorker(CredentialManager credentials,AccountResourceOptions options,ResourceSnapshotCache? cache=null,StandaloneAccounts? owner=null){options.Validate();this.credentials=credentials;this.options=options;this.cache=cache;this.owner=owner;if(owner is not null)owner.Changed+=RequestScan;}
    public void RequestScan(){try{requested.Release();}catch(SemaphoreFullException){}}
    public async Task RunOnceAsync(CancellationToken cancellation=default)
    {
        if(Interlocked.CompareExchange(ref running,1,0)!=0)return;
        string trace=TaskState.Begin();int succeeded=0,failed=0,attempted=0;
        try
        {
            var candidates=credentials.ResourceCandidates;
            int start=candidates.Count==0?0:cursor%candidates.Count;
            for(int i=0;i<Math.Min(candidates.Count,options.BatchSize);i++)
            {
                cancellation.ThrowIfCancellationRequested();var id=candidates[(start+i)%candidates.Count];attempted++;
                using var deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);deadline.CancelAfter(TimeSpan.FromSeconds(10));
                try{await credentials.FetchResourcesAsync(id,deadline.Token);succeeded++;}
                catch(OperationCanceledException) when(!cancellation.IsCancellationRequested){failed++;}
                catch(Exception e) when(e is CredentialFailure or HttpRequestException or IOException){failed++;}
            }
            cursor=candidates.Count==0?0:(start+attempted)%candidates.Count;cancellation.ThrowIfCancellationRequested();if(succeeded>0)cache?.TrySave(credentials);Volatile.Write(ref status,new(Status.Runs+1,attempted,succeeded,failed,cache?.Status.WriteFailed??false));
            TaskState.Finish(trace,attempted,succeeded,failed,options.Interval,cache?.Status.WriteFailed??false);
        }
        catch(OperationCanceledException){TaskState.Finish(trace,attempted,succeeded,failed,options.Interval,outcome:"cancelled");throw;}
        catch{TaskState.Finish(trace,attempted,succeeded,failed,options.Interval,outcome:"failed");throw;}
        finally{Volatile.Write(ref running,0);}
    }
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        using var timer=new PeriodicTimer(options.Interval);
        Task tick=timer.WaitForNextTickAsync(stoppingToken).AsTask(),trigger=requested.WaitAsync(stoppingToken);
        var runCancellation=CancellationTokenSource.CreateLinkedTokenSource(stoppingToken);Task scan=RunOnceAsync(runCancellation.Token);
        try
        {
            while(!stoppingToken.IsCancellationRequested)
            {
                await Task.WhenAny(tick,trigger);stoppingToken.ThrowIfCancellationRequested();
                runCancellation.Cancel();
                try{await scan;}catch(OperationCanceledException)when(runCancellation.IsCancellationRequested){}
                if(tick.IsCompleted){await tick;tick=timer.WaitForNextTickAsync(stoppingToken).AsTask();}
                if(trigger.IsCompleted){await trigger;trigger=requested.WaitAsync(stoppingToken);}
                runCancellation.Dispose();runCancellation=CancellationTokenSource.CreateLinkedTokenSource(stoppingToken);scan=RunOnceAsync(runCancellation.Token);
            }
        }
        finally
        {
            runCancellation.Cancel();try{await scan;}catch(OperationCanceledException)when(runCancellation.IsCancellationRequested){}finally{runCancellation.Dispose();}
        }
    }
    public override void Dispose(){if(owner is not null)owner.Changed-=RequestScan;base.Dispose();}
}
