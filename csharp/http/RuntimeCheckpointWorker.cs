using AccountMaintenance;
using AccountScheduling;

namespace HttpMigration;

public sealed class RuntimeCheckpointWorker:BackgroundService,IHostedLifecycleService
{
    readonly AccountPool pool;readonly CredentialManager credentials;readonly RuntimeSnapshotCache cache;readonly TimeSpan interval;
    readonly SemaphoreSlim checkpoint=new(1,1);
    internal Func<bool>? SaveStatus{get;set;}
    public ScheduledTaskState TaskState{get;}=new();
    public RuntimeCheckpointWorker(AccountPool pool,CredentialManager credentials,RuntimeSnapshotCache cache,TimeSpan? interval=null)
    {
        this.pool=pool;this.credentials=credentials;this.cache=cache;this.interval=interval??TimeSpan.FromSeconds(3);
        if(this.interval<TimeSpan.FromMilliseconds(100)||this.interval>TimeSpan.FromMinutes(1))throw new ArgumentException("Invalid runtime checkpoint interval");
    }
    public async Task RunOnceAsync(CancellationToken cancellation=default)
    {
        if(!await checkpoint.WaitAsync(0,cancellation))return;
        string trace=TaskState.Begin();long before=cache.Status.Saves;
        try{await Task.Run(()=>cache.TrySave(pool,credentials),CancellationToken.None);bool failed=cache.Status.WriteFailed;if(SaveStatus is{} save)failed|=!save();TaskState.Finish(trace,1,failed?0:1,failed?1:0,interval,outcome:!failed&&cache.Status.Saves==before&&SaveStatus is null?"skipped":null);}
        catch{TaskState.Finish(trace,1,0,1,interval,outcome:"failed");throw;}
        finally{checkpoint.Release();}
    }
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        using var timer=new PeriodicTimer(interval);
        do{await RunOnceAsync(stoppingToken);}while(await timer.WaitForNextTickAsync(stoppingToken));
    }
    public Task StartingAsync(CancellationToken cancellation)=>Task.CompletedTask;
    public Task StartedAsync(CancellationToken cancellation)=>Task.CompletedTask;
    public Task StoppingAsync(CancellationToken cancellation)=>Task.CompletedTask;
    // Lifecycle Stopped runs after every hosted service (including Kestrel) has stopped.
    // Drain any pending checkpoint before capturing the final state of in-flight requests.
    public async Task StoppedAsync(CancellationToken cancellation)
    {
        await checkpoint.WaitAsync(CancellationToken.None);
        try{await Task.Run(()=>cache.TrySave(pool,credentials),CancellationToken.None);SaveStatus?.Invoke();}
        finally{checkpoint.Release();}
    }
}
