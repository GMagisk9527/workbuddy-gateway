using System.Text.Json;

namespace HttpMigration;

internal sealed record UpstreamTimeouts(TimeSpan Header,TimeSpan Idle,TimeSpan FirstToken)
{internal int TransientRetries{get;init;}=2;internal TimeSpan Keepalive{get;init;}=TimeSpan.FromSeconds(15);internal static UpstreamTimeouts Default{get;}=new(TimeSpan.FromSeconds(300),TimeSpan.FromSeconds(120),TimeSpan.FromSeconds(300));}
public sealed partial class ConsoleSettings
{
    UpstreamTimeouts upstreamBaseline=UpstreamTimeouts.Default,upstream=UpstreamTimeouts.Default;
    internal UpstreamTimeouts Upstream=>Volatile.Read(ref upstream);
    internal void AttachUpstream(UpstreamTimeouts initial){lock(sync){upstreamBaseline=initial;ApplyRuntime();}}
    static UpstreamTimeouts ReadUpstream(JsonElement root,UpstreamTimeouts baseline)
    {
        if(!root.TryGetProperty("upstream",out var u))return baseline;
        Fields(u,"headerTimeoutSeconds","idleTimeoutSeconds","firstTokenTimeoutSeconds","streamKeepaliveSeconds","transientRetries");
        TimeSpan Read(string key,TimeSpan fallback,bool allowZero)
        {
            if(!u.TryGetProperty(key,out var n))return fallback;int seconds=n.GetInt32();
            if(seconds<0||seconds>3600)throw new ArgumentException("Invalid upstream timeout");
            return seconds==0&&!allowZero?fallback:TimeSpan.FromSeconds(seconds);
        }
        int retries=u.TryGetProperty("transientRetries",out var retry)?retry.GetInt32():baseline.TransientRetries;if(retries is <0 or >10)throw new ArgumentException("Invalid transient retries");
        return new(Read("headerTimeoutSeconds",baseline.Header,false),Read("idleTimeoutSeconds",baseline.Idle,false),Read("firstTokenTimeoutSeconds",baseline.FirstToken,true)){Keepalive=Read("streamKeepaliveSeconds",baseline.Keepalive,false),TransientRetries=retries};
    }
}
