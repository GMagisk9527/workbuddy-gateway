namespace HttpMigration;

// Go's one-second config poll and hourly cleanup, with early cleanup on a
// retention change. Never touches credentials, billing or request history.
public sealed class ConsoleMaintenanceWorker(ConsoleRuntime console,TimeProvider? clock=null):BackgroundService
{
    readonly TimeProvider time=clock??TimeProvider.System;
    readonly object sync=new();
    long? lastCleanup;int lastDays=-1;
    public ScheduledTaskState ConfigState{get;}=new(clock);
    public ScheduledTaskState CleanupState{get;}=new(clock);
    public void RunOnce()
    {
        lock(sync)
        {
            string trace=ConfigState.Begin();
            try{bool changed=console.Settings.Reload();console.Settings.RefreshAccountPolicies();ConfigState.Finish(trace,1,1,0,TimeSpan.FromSeconds(1),outcome:changed?"success":"skipped");}
            catch(Exception e)when(e is IOException or UnauthorizedAccessException or System.Text.Json.JsonException or ArgumentException or InvalidOperationException or KeyNotFoundException or OverflowException)
            {ConfigState.Finish(trace,1,0,1,TimeSpan.FromSeconds(1));}
            int days=console.Settings.RetentionDays;
            if(days==0){lastDays=days;CleanupState.Schedule(0);return;}
            if(days==lastDays&&lastCleanup is {} last&&time.GetElapsedTime(last)<TimeSpan.FromHours(1))return;
            CleanupCore(days);
        }
    }
    public int Cleanup(){lock(sync)return CleanupCore(console.Settings.RetentionDays);}
    int CleanupCore(int days)
    {
        string trace=CleanupState.Begin();
        try
        {
            int removed=console.Telemetry.Cleanup(time.GetUtcNow());removed+=console.TaskJournal.Cleanup(days,time.GetUtcNow());lastDays=days;lastCleanup=time.GetTimestamp();
            CleanupState.Finish(trace,days==0?0:1,days==0?0:1,0,TimeSpan.FromHours(1));
            if(days==0)CleanupState.Schedule(0);return removed;
        }
        catch{CleanupState.Finish(trace,1,0,1,TimeSpan.FromHours(1));throw;}
    }
    protected override async Task ExecuteAsync(CancellationToken token)
    {using var timer=new PeriodicTimer(TimeSpan.FromSeconds(1),time);do{RunOnce();}while(await timer.WaitForNextTickAsync(token));}
}
