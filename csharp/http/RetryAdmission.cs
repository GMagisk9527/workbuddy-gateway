using System.Text.Json;

namespace HttpMigration;

internal sealed record RetryOptions(int MaxExtraAttempts=4,int MaxConcurrent=2,int RatioPercent=20,int MinAllowance=2,int BackoffMaxMs=2000)
{
    internal void Validate()
    {if(MaxExtraAttempts is <0 or >64||MaxConcurrent is <0 or >1024||RatioPercent is <1 or >100||MinAllowance is <1 or >1024||BackoffMaxMs is <0 or >60000)throw new ArgumentException("Invalid retry budget");}
}
internal sealed class RetryAdmission
{
    readonly object sync=new();RetryOptions options=new();int originals,active;long denied;
    internal RetryOptions Options{get{lock(sync)return options;}}
    internal void Configure(RetryOptions next){next.Validate();lock(sync)options=next;}
    internal (int Originals,int Active,long Denied) Status{get{lock(sync)return(originals,active,denied);}}
    internal Scope Acquire(){lock(sync)return new(this,options);}
    internal static TimeSpan Backoff(int ordinal,int maximum,double random)
    {
        long delay=Math.Min(300,maximum);for(int i=1;i<ordinal&&delay<maximum;i++)delay=Math.Min(delay*2,maximum);
        return TimeSpan.FromMilliseconds(delay*(0.5+0.5*Math.Clamp(random,0,1)));
    }
    internal sealed class Scope(RetryAdmission owner,RetryOptions initial):IDisposable
    {
        bool begun,closed;int extras;
        internal async Task<IDisposable?> BeginAsync(CancellationToken cancellation)
        {
            IDisposable permit;int ordinal;
            lock(owner.sync)
            {
                cancellation.ThrowIfCancellationRequested();if(closed)throw new ObjectDisposedException(nameof(Scope));
                if(!begun){begun=true;owner.originals++;return null;}
                var current=owner.options;
                if(extras>=initial.MaxExtraAttempts||owner.active>=Math.Min(current.MaxConcurrent,current.MinAllowance+(long)owner.originals*current.RatioPercent/100))
                {owner.denied++;throw new RequestFailure("额外上游尝试预算已用尽，请稍后重试",503,"retry_budget_exhausted");}
                ordinal=++extras;owner.active++;permit=new Permit(()=>{lock(owner.sync)owner.active--;});
            }
            try{await Task.Delay(Backoff(ordinal,initial.BackoffMaxMs,Random.Shared.NextDouble()),cancellation);return permit;}
            catch{permit.Dispose();throw;}
        }
        public void Dispose(){lock(owner.sync){if(closed)return;closed=true;if(begun)owner.originals--;}}
    }
    sealed class Permit(Action release):IDisposable
    {Action? action=release;public void Dispose()=>Interlocked.Exchange(ref action,null)?.Invoke();}
}
public sealed partial class ConsoleSettings
{
    RetryAdmission? retryAdmission;RetryOptions retryBaseline=new();
    internal void AttachRetries(RetryAdmission retries){lock(sync){retryAdmission=retries;retryBaseline=retries.Options;ApplyRuntime();}}
    static RetryOptions ReadRetries(JsonElement root,RetryOptions baseline)
    {
        if(!root.TryGetProperty("retry",out var retry))return baseline;
        Fields(retry,"maxExtraAttempts","maxConcurrent","ratioPercent","minAllowance","backoffMaxMs");
        int Read(string name,int fallback,bool zeroDefault=false){if(!retry.TryGetProperty(name,out var v))return fallback;int n=v.GetInt32();return n==0&&zeroDefault?fallback:n;}
        var result=new RetryOptions(Read("maxExtraAttempts",baseline.MaxExtraAttempts),Read("maxConcurrent",baseline.MaxConcurrent),Read("ratioPercent",baseline.RatioPercent,true),Read("minAllowance",baseline.MinAllowance,true),Read("backoffMaxMs",baseline.BackoffMaxMs,true));result.Validate();return result;
    }
}
