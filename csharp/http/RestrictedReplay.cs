using System.Text;
using System.Text.Json;

namespace HttpMigration;

// One incoming request owns the delay; there is no persisted body or replay queue.
internal sealed class RestrictedReplay(HttpContext context,string model,bool stream,TimeSpan? interval=null):IAsyncDisposable
{
    internal static readonly TimeSpan DefaultDelay=TimeSpan.FromMinutes(2);
    static readonly object Key=new();
    readonly string modelName=model;
    SseHeartbeat? heartbeat;
    CancellationTokenSource? waiting;Task? pendingStart;
    internal void StartWaiting()
    {
        context.Items[Key]=this;if(!stream||pendingStart is not null)return;
        waiting=CancellationTokenSource.CreateLinkedTokenSource(context.RequestAborted);var token=waiting.Token;
        pendingStart=StartDelayed(token);
    }
    async Task StartDelayed(CancellationToken token)
    {
        try{await Task.Delay(interval??TimeSpan.FromSeconds(15),token);await StartHeartbeat(token);}
        catch(OperationCanceledException)when(token.IsCancellationRequested){}
        catch(IOException){context.Abort();}
    }
    async Task StartHeartbeat(CancellationToken token)
    {
        if(heartbeat is not null)return;
        if(!context.Response.HasStarted){context.Response.ContentType="text/event-stream";context.Response.Headers.CacheControl="no-cache";context.Response.Headers["X-Accel-Buffering"]="no";}
        await context.Response.StartAsync(token);heartbeat=new(context,interval);
    }
    internal async Task WaitAsync(TimeSpan delay,CancellationToken cancellation=default)
    {
        context.Items[Key]=this;
        if(stream)
        {
            await StopAsync();await StartHeartbeat(context.RequestAborted);
        }
        using var linked=CancellationTokenSource.CreateLinkedTokenSource(context.RequestAborted,cancellation);
        await Task.Delay(delay,linked.Token);
    }
    internal async Task StopAsync()
    {
        var cancel=Interlocked.Exchange(ref waiting,null);var start=Interlocked.Exchange(ref pendingStart,null);
        if(cancel is not null){cancel.Cancel();try{if(start is not null)await start;}finally{cancel.Dispose();}}
        var pending=Interlocked.Exchange(ref heartbeat,null);if(pending is not null)await pending.DisposeAsync();
    }
    internal static async Task<bool> WriteError(HttpContext context,string type,string message,string? code)
    {
        if(!context.Items.TryGetValue(Key,out var value)||value is not RestrictedReplay replay)return false;
        await replay.StopAsync();if(!context.Response.HasStarted)return false;
        if(context.RequestAborted.IsCancellationRequested){context.Abort();return true;}
        string? eventName=null;object error=new Dictionary<string, object?> { ["type"] = type, ["code"] = code??type, ["message"] = message };object payload=new Dictionary<string, object?> { ["error"] = error };
        if(context.Request.Path.Value?.EndsWith("/responses",StringComparison.Ordinal)==true)
        {
            eventName="response.failed";payload=new Dictionary<string, object?> { ["type"] = eventName, ["sequence_number"] = 0, ["response"] = new Dictionary<string, object?> { ["id"] = "resp_"+Guid.NewGuid().ToString("N"), ["object"] = "response", ["created_at"] = DateTimeOffset.UtcNow.ToUnixTimeSeconds(), ["status"] = "failed", ["model"] = replay.modelName, ["output"] = Array.Empty<object>(), ["error"] = new Dictionary<string, object?> { ["code"] = code??type, ["message"] = message } } };
        }
        else if(context.Request.Path.Value?.EndsWith("/messages",StringComparison.Ordinal)==true){eventName="error";payload=new Dictionary<string, object?> { ["type"] = "error", ["error"] = error };}
        string frame=(eventName is null?"":"event: "+eventName+"\n")+"data: "+JsonSerializer.Serialize(payload, GatewayJson.Default.Object)+"\n\n";
        await context.Response.Body.WriteAsync(Encoding.UTF8.GetBytes(frame),context.RequestAborted);await context.Response.Body.FlushAsync(context.RequestAborted);return true;
    }
    public async ValueTask DisposeAsync(){await StopAsync();context.Items.Remove(Key);}
}
