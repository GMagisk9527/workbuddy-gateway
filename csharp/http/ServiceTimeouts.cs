using System.Collections.Concurrent;
using System.Diagnostics;
using System.Text.Json;
using Microsoft.Extensions.Options;

namespace HttpMigration;

internal sealed record ServiceTimeouts(TimeSpan Read,TimeSpan Write,TimeSpan Grace,TimeSpan Cleanup)
{
    internal static ServiceTimeouts Default{get;}=new(TimeSpan.FromSeconds(900),TimeSpan.FromSeconds(30),TimeSpan.FromSeconds(120),TimeSpan.FromSeconds(15));
}
public sealed partial class ConsoleSettings
{
    ServiceTimeouts service=ServiceTimeouts.Default,serviceBaseline=ServiceTimeouts.Default;HostOptions? hostOptions;
    internal ServiceTimeouts Service=>Volatile.Read(ref service);
    internal void AttachService(HostOptions host,ServiceTimeouts? baseline=null)
    {lock(sync){hostOptions=host;serviceBaseline=baseline??ServiceTimeouts.Default;ApplyRuntime();}}
    static ServiceTimeouts ReadService(JsonElement root,ServiceTimeouts baseline)
    {
        TimeSpan read=baseline.Read,write=baseline.Write,grace=baseline.Grace,cleanup=baseline.Cleanup;
        TimeSpan Seconds(JsonElement section,string name,TimeSpan fallback,int maximum)
        {if(!section.TryGetProperty(name,out var n))return fallback;int value=n.GetInt32();if(value<0||value>maximum)throw new ArgumentException("Invalid service timeout");return value==0?fallback:TimeSpan.FromSeconds(value);}
        if(root.TryGetProperty("server",out var server))
        {
            Fields(server,"requestReadTimeoutSeconds","shutdownGraceSeconds","shutdownCleanupSeconds");
            read=Seconds(server,"requestReadTimeoutSeconds",read,3600);grace=Seconds(server,"shutdownGraceSeconds",grace,3600);cleanup=Seconds(server,"shutdownCleanupSeconds",cleanup,300);
        }
        if(root.TryGetProperty("downstream",out var downstream)){Fields(downstream,"writeTimeoutSeconds");write=Seconds(downstream,"writeTimeoutSeconds",write,3600);}
        return new(read,write,grace,cleanup);
    }
}
// A deadline bounds each actual downstream write, not the total model response.
internal sealed class DeadlineWriteStream(Stream inner,TimeSpan timeout,Action abort):Stream
{
    long bytesWritten;
    internal bool TrackBytes { get; set; }
    internal long BytesWritten => Interlocked.Read(ref bytesWritten);

    async Task Run(Func<CancellationToken,Task> action,CancellationToken cancellation,int bytes=0)
    {
        using var deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);deadline.CancelAfter(timeout);
        try
        {
            await action(deadline.Token);
            if(TrackBytes && bytes>0)Interlocked.Add(ref bytesWritten,bytes);
        }
        catch(OperationCanceledException)when(!cancellation.IsCancellationRequested){abort();throw new IOException("Downstream write timeout");}
    }
    public override ValueTask WriteAsync(ReadOnlyMemory<byte> buffer,CancellationToken cancellationToken=default)=>new(Run(token=>inner.WriteAsync(buffer,token).AsTask(),cancellationToken,buffer.Length));
    public override Task WriteAsync(byte[] buffer,int offset,int count,CancellationToken cancellationToken)=>WriteAsync(buffer.AsMemory(offset,count),cancellationToken).AsTask();
    public override Task FlushAsync(CancellationToken cancellationToken)=>Run(inner.FlushAsync,cancellationToken);
    // The response body belongs to Kestrel; wrappers do not dispose it.
    public override bool CanRead=>false;public override bool CanSeek=>false;public override bool CanWrite=>true;
    public override long Length=>throw new NotSupportedException();public override long Position{get=>throw new NotSupportedException();set=>throw new NotSupportedException();}
    public override int Read(byte[] b,int o,int n)=>throw new NotSupportedException();public override void Flush()=>inner.Flush();public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long v)=>throw new NotSupportedException();public override void Write(byte[] b,int o,int n)=>throw new NotSupportedException();
}
internal sealed class GracefulRequests:IDisposable
{
    static readonly object ReadDeadlineKey=new();
    readonly object sync=new();readonly ConcurrentDictionary<HttpContext,byte> requests=new();readonly ConsoleSettings settings;
    readonly CancellationTokenRegistration stopping;Timer? expiration;bool closed;
    internal GracefulRequests(IHostApplicationLifetime lifetime,ConsoleSettings settings)
    {this.settings=settings;stopping=lifetime.ApplicationStopping.Register(BeginStop);}
    void BeginStop(){lock(sync){if(closed)return;closed=true;expiration=new Timer(_=>{foreach(var c in requests.Keys)c.Abort();},null,settings.Service.Grace,Timeout.InfiniteTimeSpan);}}
    internal async Task Handle(HttpContext c,RequestDelegate next)
    {
        lock(sync){if(!closed)requests.TryAdd(c,0);}
        if(!requests.ContainsKey(c)){c.Response.StatusCode=503;c.Response.Headers.RetryAfter="1";return;}
        var original=c.Response.Body;var timeouts=settings.Service;
        c.Items[ReadDeadlineKey]=(Stopwatch.GetTimestamp(),timeouts.Read);
        c.Response.Body=new DeadlineWriteStream(original,timeouts.Write,c.Abort);
        try{await next(c);}
        catch(IOException){c.Abort();}
        finally{c.Response.Body=original;requests.TryRemove(c,out _);}
    }
    internal static TimeSpan RemainingRead(HttpContext c)
    {
        if(c.Items.TryGetValue(ReadDeadlineKey,out var value)&&value is ValueTuple<long,TimeSpan> start)
            return start.Item2-Stopwatch.GetElapsedTime(start.Item1);
        return ServiceTimeouts.Default.Read;
    }
    public void Dispose(){stopping.Dispose();lock(sync){closed=true;expiration?.Dispose();}}
}
