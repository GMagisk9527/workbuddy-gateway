using System.Text.Json;
using StreamingMigration;

namespace HttpMigration;

internal sealed record OutputOptions(long MaxRequestBytes=0,long MaxSharedBytes=0)
{internal void Validate(){if(MaxRequestBytes<0||MaxSharedBytes<0)throw new ArgumentException("Invalid output budget");}}
internal sealed class OutputAdmission(Func<MemorySample> sample)
{
    readonly object sync=new();OutputOptions options=new();long used,rejected;
    internal OutputOptions Options{get{lock(sync)return options;}}
    internal void Configure(OutputOptions next){next.Validate();lock(sync)options=next;}
    (long Request,long Shared) Limits()
    {
        long request=16L<<20,shared=32L<<20;var current=sample();
        if(current.Runtime.Limit>0){request=Math.Min(request,current.Runtime.Limit/4);shared=Math.Min(shared,current.Runtime.Limit/2);}
        foreach(var group in current.Cgroups)if(group.Limit>0){request=Math.Min(request,group.Limit/8);shared=Math.Min(shared,group.Limit/4);}
        return(Math.Max(1,options.MaxRequestBytes>0?options.MaxRequestBytes:request),Math.Max(1,options.MaxSharedBytes>0?options.MaxSharedBytes:shared));
    }
    internal (long Used,long Rejected,long Request,long Shared) Status{get{lock(sync){var limits=Limits();return(used,rejected,limits.Request,limits.Shared);}}}
    internal Lease Acquire()=>new(this);
    internal sealed class Lease(OutputAdmission owner):IRetainedOutputBudget,IDisposable
    {
        long used,reserved,limit,scratch;bool closed;
        public void Add(long bytes)
        {
            if(bytes<=0)return;lock(owner.sync)
            {
                if(closed)throw new ObjectDisposedException(nameof(Lease));var limits=owner.Limits();if(limit==0)limit=limits.Request;
                if(bytes>limit-used){owner.rejected++;throw new ConversionFailure("output_budget_exceeded");}
                if(bytes>reserved-used)
                {
                    long need=used+bytes-reserved,available=Math.Min(limit-reserved,limits.Shared-owner.used);
                    if(need>available){owner.rejected++;throw new ConversionFailure("output_budget_exceeded");}
                    long block=Math.Min(64L<<10,Math.Max(limit/16,1)),reserve=Math.Min(Math.Max(need,block),available);
                    owner.used+=reserve;reserved+=reserve;
                }
                used+=bytes;
            }
        }
        public void Remove(long bytes){if(bytes<0)throw new ArgumentOutOfRangeException(nameof(bytes));lock(owner.sync)used-=Math.Min(bytes,used-scratch);}
        public void Scratch(long bytes){lock(owner.sync){if(bytes<=scratch)return;Add(bytes-scratch);scratch=bytes;}}
        public void Dispose(){lock(owner.sync){if(closed)return;closed=true;owner.used-=reserved;reserved=0;}}
    }
}
public sealed partial class ConsoleSettings
{
    OutputAdmission? outputAdmission;OutputOptions outputBaseline=new();
    internal void AttachOutput(OutputAdmission output){lock(sync){outputAdmission=output;outputBaseline=output.Options;ApplyRuntime();}}
    static OutputOptions ReadOutput(JsonElement root,OutputOptions baseline)
    {
        if(!root.TryGetProperty("output",out var o))return baseline;Fields(o,"maxRequestBytes","maxSharedBytes");
        var next=new OutputOptions(o.TryGetProperty("maxRequestBytes",out var per)?per.GetInt64():baseline.MaxRequestBytes,o.TryGetProperty("maxSharedBytes",out var shared)?shared.GetInt64():baseline.MaxSharedBytes);next.Validate();return next;
    }
}
