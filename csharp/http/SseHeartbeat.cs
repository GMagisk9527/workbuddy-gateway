namespace HttpMigration;

internal sealed class SseHeartbeat : Stream
{
    readonly HttpContext context;
    readonly SemaphoreSlim write = new(1);
    readonly CancellationTokenSource stop;
    readonly Task heartbeat;
    readonly TimeSpan interval;
    public SseHeartbeat(HttpContext context,TimeSpan? interval=null)
    {
        this.context = context;this.interval=interval??TimeSpan.FromSeconds(15); stop = CancellationTokenSource.CreateLinkedTokenSource(context.RequestAborted);
        heartbeat = Run();
    }
    async Task Run()
    {
        try
        {
            while (true)
            {
                await WriteAsync(": keepalive\n\n"u8.ToArray(), stop.Token); await FlushAsync(stop.Token);
                await Task.Delay(interval, stop.Token);
            }
        }
        catch (OperationCanceledException) { }
        catch (Exception e) when (e is IOException or ObjectDisposedException) { context.Abort(); }
    }
    public override async ValueTask WriteAsync(ReadOnlyMemory<byte> buffer, CancellationToken cancellationToken = default)
    {
        await write.WaitAsync(cancellationToken);
        try { await context.Response.Body.WriteAsync(buffer, cancellationToken); } finally { write.Release(); }
    }
    public override async Task FlushAsync(CancellationToken cancellationToken)
    {
        await write.WaitAsync(cancellationToken);
        try { await context.Response.Body.FlushAsync(cancellationToken); } finally { write.Release(); }
    }
    public override async ValueTask DisposeAsync() { stop.Cancel(); await heartbeat; stop.Dispose(); write.Dispose(); GC.SuppressFinalize(this); }
    public override bool CanRead => false; public override bool CanSeek => false; public override bool CanWrite => true;
    public override long Length => throw new NotSupportedException(); public override long Position { get => throw new NotSupportedException(); set => throw new NotSupportedException(); }
    public override void Flush() => throw new NotSupportedException(); public override int Read(byte[] b, int o, int c) => throw new NotSupportedException();
    public override long Seek(long o, SeekOrigin s) => throw new NotSupportedException(); public override void SetLength(long v) => throw new NotSupportedException();
    public override void Write(byte[] b, int o, int c) => throw new NotSupportedException();
}
