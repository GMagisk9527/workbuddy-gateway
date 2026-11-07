using System.Diagnostics;

namespace HttpMigration;

public sealed record AdmissionOptions(int MaxActive=2,int MaxPreparing=0,int MaxWaiting=8,int WaitTimeoutMs=15000,bool FairQueue=true,int MaxWaitingPerKey=0)
{
    internal void Validate()
    {if(MaxActive is <1 or >65536||MaxPreparing is <0 or >65536||MaxWaiting is <0 or >65536||MaxWaitingPerKey is <0 or >65536||WaitTimeoutMs is <1 or >900000)throw new ArgumentException("Invalid request admission settings");}
    internal int PerKey=>MaxWaitingPerKey==0?Math.Max(1,(MaxWaiting+1)/2):MaxWaitingPerKey;
}

internal sealed class AdmissionQueue
{
    sealed class Ticket(string flow){internal string Flow{get;}=flow;}
    readonly object sync=new();readonly List<Ticket> waiters=[];readonly Dictionary<string,int> flows=new(StringComparer.Ordinal);
    readonly Action checkMemory;AdmissionOptions options;int active;
    TaskCompletionSource changed=Signal();static TaskCompletionSource Signal()=>new(TaskCreationOptions.RunContinuationsAsynchronously);
    internal AdmissionQueue(AdmissionOptions options,Action checkMemory){options.Validate();this.options=options;this.checkMemory=checkMemory;}
    internal (int Active,int Waiting,int Flows) Status{get{lock(sync)return(active,waiters.Count,flows.Count);}}
    void Notify(){var previous=changed;changed=Signal();previous.TrySetResult();}
    internal void Configure(AdmissionOptions next){next.Validate();lock(sync){options=next;Notify();}}
    Ticket? Next()
    {
        if(waiters.Count==0)return null;var next=waiters[0];
        if(options.FairQueue)foreach(var ticket in waiters.Skip(1))if(flows.GetValueOrDefault(ticket.Flow)<flows.GetValueOrDefault(next.Flow))next=ticket;
        return next;
    }
    IDisposable Reserve(string flow)
    {
        active++;flows[flow]=flows.GetValueOrDefault(flow)+1;
        return new Permit(()=>{lock(sync){active--;if(--flows[flow]==0)flows.Remove(flow);Notify();}});
    }
    internal async Task<IDisposable> AcquireAsync(string flow,TimeSpan timeout,CancellationToken cancellation)
    {
        Ticket ticket;long start=Stopwatch.GetTimestamp();
        lock(sync)
        {
            cancellation.ThrowIfCancellationRequested();checkMemory();
            if(active<options.MaxActive&&waiters.Count==0)return Reserve(flow);
            if(waiters.Count>=options.MaxWaiting||options.FairQueue&&flow.Length>0&&waiters.Count(t=>t.Flow==flow)>=options.PerKey)
                throw new RequestFailure("请求队列已满，请稍后重试",429,"request_queue_full");
            ticket=new(flow);waiters.Add(ticket);
        }
        try
        {
            while(true)
            {
                Task signal;TimeSpan remaining;
                lock(sync)
                {
                    cancellation.ThrowIfCancellationRequested();remaining=timeout-Stopwatch.GetElapsedTime(start);
                    if(remaining<=TimeSpan.Zero)throw new RequestFailure("等待请求处理名额超时，请稍后重试",429,"request_queue_timeout");
                    if(active<options.MaxActive&&ReferenceEquals(Next(),ticket))
                    {
                        checkMemory();waiters.Remove(ticket);
                        if(options.FairQueue)
                        {var same=waiters.Where(t=>t.Flow==flow).ToArray();waiters.RemoveAll(t=>t.Flow==flow);waiters.AddRange(same);}
                        var permit=Reserve(flow);Notify();return permit;
                    }
                    signal=changed.Task;
                }
                try{await signal.WaitAsync(remaining,cancellation);}
                catch(TimeoutException){throw new RequestFailure("等待请求处理名额超时，请稍后重试",429,"request_queue_timeout");}
            }
        }
        finally{lock(sync)if(waiters.Remove(ticket))Notify();}
    }
    sealed class Permit(Action release):IDisposable{Action? action=release;public void Dispose()=>Interlocked.Exchange(ref action,null)?.Invoke();}
}

internal sealed class RequestAdmission
{
    readonly AdmissionQueue global,preparing;readonly object sync=new();AdmissionOptions options;
    internal RequestAdmission(AdmissionOptions initial,Action checkMemory)
    {initial.Validate();options=initial;global=new(initial,checkMemory);preparing=new(initial with{MaxActive=initial.MaxPreparing==0?1024:initial.MaxPreparing},checkMemory);}
    internal AdmissionOptions Options{get{lock(sync)return options;}}
    internal (int Active,int Waiting,int Preparing,int PreparingWaiting) Status
    {get{var g=global.Status;var p=preparing.Status;return(g.Active,g.Waiting,p.Active,p.Waiting);}}
    internal void Configure(AdmissionOptions next)
    {next.Validate();lock(sync){global.Configure(next);preparing.Configure(next with{MaxActive=next.MaxPreparing==0?1024:next.MaxPreparing});options=next;}}
    internal async Task<Lease> AcquireAsync(string flow,CancellationToken cancellation)
    {
        var limits=Options;long start=Stopwatch.GetTimestamp();var wait=TimeSpan.FromMilliseconds(limits.WaitTimeoutMs);
        var admitted=await global.AcquireAsync(flow,wait,cancellation);
        try
        {
            if(Options.MaxPreparing==0)return new(admitted,null);
            var remaining=wait-Stopwatch.GetElapsedTime(start);
            if(remaining<=TimeSpan.Zero)throw new RequestFailure("等待前处理名额超时",429,"request_prepare_queue_full");
            try{return new(admitted,await preparing.AcquireAsync(flow,remaining,cancellation));}
            catch(RequestFailure e)when(e.Status==429){throw new RequestFailure("等待前处理名额超时或队列已满",429,"request_prepare_queue_full");}
        }
        catch{admitted.Dispose();throw;}
    }
    internal sealed class Lease(IDisposable admitted,IDisposable? preparing):IDisposable
    {
        IDisposable? global=admitted,preparation=preparing;
        internal void FinishPreparing()=>Interlocked.Exchange(ref preparation,null)?.Dispose();
        public void Dispose(){FinishPreparing();Interlocked.Exchange(ref global,null)?.Dispose();}
    }
}
