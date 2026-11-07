namespace HttpMigration;

// Retained request bytes survive the response; temporary preparation bytes do not.
internal sealed class PreparationAdmission(long limit, int preparing,Func<MemorySample>? sample=null) : IDisposable
{
    MemoryAdmissionOptions options = new(MaxBodyBytes:limit);
    readonly Func<MemorySample> sample=sample??(()=>MemorySample.Empty);
    bool pressured;
    readonly object sync = new();
    long used;
    internal SemaphoreSlim Preparing { get; } = new(preparing);
    internal long Used { get { lock (sync) return used; } }
    internal Lease Acquire() => new(this);
    internal MemoryAdmissionOptions Options{get{lock(sync)return options;}}
    internal void Configure(MemoryAdmissionOptions next){next.Validate();lock(sync){options=next;if(!next.Enabled)pressured=false;}}
    internal (long Used,long Limit,bool Pressured) Status{get{lock(sync)return(used,sample().BodyLimit(options),pressured);}}
    internal void CheckAdmission()
    {
        lock(sync)
        {
            if(!options.Enabled)return;var current=sample();var marks=current.Watermarks(options.High,options.Low);
            if(marks.High)pressured=true;else if(marks.Low)pressured=false;
            if(pressured)throw new RequestFailure("内存水位过高，请稍后重试",503,"memory_pressure");
            if(used>=current.BodyLimit(options))throw new RequestFailure("在途请求体预算已满，请稍后重试",503,"overloaded_error");
        }
    }
    public void Dispose() => Preparing.Dispose();
    internal sealed class Lease(PreparationAdmission owner) : IDisposable
    {
        long held;
        bool closed;
        internal void Reserve(int count)
        {
            lock (owner.sync)
            {
                if (closed) throw new ObjectDisposedException(nameof(Lease));
                if (count < 0 || count > owner.sample().BodyLimit(owner.options) - owner.used) throw new RequestFailure("在途请求体预算已满，请稍后重试", 503, "overloaded_error");
                owner.used += count; held += count;
            }
        }
        internal Action<int> Wire(long? length)
        {
            int credit = length is > 0 ? checked((int)length.Value) : 0;
            Reserve(credit);
            return count => { int covered = Math.Min(credit, count); credit -= covered; Reserve(count-covered); };
        }
        // Call only after references to discarded buffers have been dropped.
        internal void Retain(int count)
        {
            lock(owner.sync)
            {
                if(closed)throw new ObjectDisposedException(nameof(Lease));
                if(count<0||count>held)throw new ArgumentOutOfRangeException(nameof(count));
                owner.used-=held-count;held=count;
            }
        }
        public void Dispose()
        {
            lock (owner.sync) { if (closed) return; closed = true; owner.used -= held; held = 0; }
        }
    }
}
