using AccountMaintenance;
namespace HttpMigration;
public sealed record CredentialMaintenanceOptions(TimeSpan Interval,int BatchSize=1024)
{
    internal void Validate(){if(Interval<TimeSpan.FromSeconds(10)||Interval>TimeSpan.FromHours(1)||BatchSize is <1 or >1024)throw new ArgumentException("Invalid credential maintenance settings");}
}
public sealed record CredentialMaintenanceStatus(long Runs,int Attempted,int Succeeded,int Failed);
public sealed class CredentialMaintenanceWorker:BackgroundService
{
    readonly CredentialManager credentials;readonly CredentialMaintenanceOptions options;int running;
    readonly bool startImmediately;
    CredentialMaintenanceStatus status=new(0,0,0,0);
    public CredentialMaintenanceStatus Status=>Volatile.Read(ref status);
    public ScheduledTaskState TaskState{get;}=new();
    public CredentialMaintenanceWorker(CredentialManager credentials,CredentialMaintenanceOptions options,bool startImmediately=false){options.Validate();this.credentials=credentials;this.options=options;this.startImmediately=startImmediately;}
    public async Task RunOnceAsync(CancellationToken cancellation=default)
    {
        if(Interlocked.CompareExchange(ref running,1,0)!=0)return;
        string trace=TaskState.Begin();int attempted=0,succeeded=0,failed=0;
        try
        {
            foreach(var id in credentials.MaintenanceCandidates.Take(options.BatchSize))
            {
                cancellation.ThrowIfCancellationRequested();attempted++;
                using var deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);deadline.CancelAfter(TimeSpan.FromSeconds(35));
                try{await credentials.RefreshIfDueAsync(id,deadline.Token);succeeded++;}
                catch(OperationCanceledException) when(!cancellation.IsCancellationRequested){failed++;}
                catch(Exception e) when(e is CredentialFailure or HttpRequestException or IOException){failed++;}
            }
            cancellation.ThrowIfCancellationRequested();Volatile.Write(ref status,new(Status.Runs+1,attempted,succeeded,failed));
            TaskState.Finish(trace,attempted,succeeded,failed,options.Interval);
        }
        catch(OperationCanceledException){TaskState.Finish(trace,attempted,succeeded,failed,options.Interval,outcome:"cancelled");throw;}
        catch{TaskState.Finish(trace,attempted,succeeded,failed,options.Interval,outcome:"failed");throw;}
        finally{Volatile.Write(ref running,0);}
    }
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        using var timer=new PeriodicTimer(options.Interval);
        TaskState.Schedule(DateTimeOffset.UtcNow.Add(options.Interval).ToUnixTimeSeconds());
        if(startImmediately)await RunOnceAsync(stoppingToken);
        while(await timer.WaitForNextTickAsync(stoppingToken))await RunOnceAsync(stoppingToken);
    }
}
