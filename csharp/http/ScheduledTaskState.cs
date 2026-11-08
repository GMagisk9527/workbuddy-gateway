namespace HttpMigration;

public sealed record ScheduledTaskSnapshot(string Status="unknown",bool? Success=null,long LastRunAt=0,long LastFinishedAt=0,long NextRunAt=0,string TraceId="",long Runs=0,int Attempted=0,int Succeeded=0,int Failed=0,bool CacheWriteFailed=false);

// Process-local execution evidence; never infer success from an empty counter.
public sealed class ScheduledTaskState(TimeProvider? clock=null)
{
    readonly object sync=new();readonly TimeProvider time=clock??TimeProvider.System;
    ScheduledTaskSnapshot state=new();
    public event Action<ScheduledTaskSnapshot>? Completed;
    public ScheduledTaskSnapshot Snapshot{get{lock(sync)return state;}}
    public void Schedule(long next){lock(sync)state=state with{NextRunAt=next};}
    public string Begin()
    {
        lock(sync)
        {
            string trace=Guid.NewGuid().ToString("N");
            state=state with{Status="running",Success=null,LastRunAt=time.GetUtcNow().ToUnixTimeSeconds(),LastFinishedAt=0,NextRunAt=0,TraceId=trace,Attempted=0,Succeeded=0,Failed=0,CacheWriteFailed=false};return trace;
        }
    }
    public void Finish(string trace,int attempted,int succeeded,int failed,TimeSpan interval,bool cacheFailed=false,string? outcome=null)
    {
        lock(sync)
        {
            if(state.TraceId!=trace||state.Status!="running")return;
            string result=outcome??(attempted==0?"skipped":failed==0&&!cacheFailed?"success":succeeded==0?"failed":"partial");
            bool? success=result=="success"?true:result is "failed" or "partial"?false:null;
            long now=time.GetUtcNow().ToUnixTimeSeconds();
            state=state with{Status=result,Success=success,LastFinishedAt=now,NextRunAt=result=="cancelled"?0:time.GetUtcNow().Add(interval).ToUnixTimeSeconds(),Runs=state.Runs+1,Attempted=attempted,Succeeded=succeeded,Failed=failed,CacheWriteFailed=cacheFailed};
            Completed?.Invoke(state);
        }
    }
}
