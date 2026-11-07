using StreamingMigration;

namespace HttpMigration;

// One timer spans headers and permitted retries. SSE comments/role-only frames
// never reset it; the first meaningful increment or completion disarms it.
internal sealed class FirstTokenBudget(TimeSpan timeout,CancellationToken cancellation):IDisposable
{
    internal const string ErrorCode="upstream_first_token_timeout";
    readonly object sync=new();readonly CancellationTokenSource deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);
    Timer? timer;bool armed,finished,expired;
    internal CancellationToken Token=>deadline.Token;
    internal bool Expired{get{lock(sync)return expired;}}
    internal void Arm()
    {
        lock(sync)
        {
            if(armed||finished||timeout<=TimeSpan.Zero||deadline.IsCancellationRequested)return;armed=true;
            timer=new Timer(_=>{lock(sync){if(finished)return;expired=true;deadline.Cancel();}},null,timeout,Timeout.InfiniteTimeSpan);
        }
    }
    internal void Finish()
    {lock(sync){if(expired)throw new ConversionFailure(ErrorCode);finished=true;timer?.Dispose();timer=null;}}
    public void Dispose(){lock(sync){finished=true;timer?.Dispose();timer=null;deadline.Dispose();}}
    internal Stream Wrap(Stream source)=>new BudgetStream(source,this);
    sealed class BudgetStream(Stream inner,FirstTokenBudget owner):Stream
    {
        public override async ValueTask<int> ReadAsync(Memory<byte> buffer,CancellationToken cancellationToken=default)
        {
            using var linked=CancellationTokenSource.CreateLinkedTokenSource(cancellationToken,owner.Token);
            try{return await inner.ReadAsync(buffer,linked.Token);}
            catch(OperationCanceledException)when(owner.Expired){throw new ConversionFailure(ErrorCode);}
        }
        protected override void Dispose(bool disposing){if(disposing)inner.Dispose();base.Dispose(disposing);}
        public override bool CanRead=>true;public override bool CanSeek=>false;public override bool CanWrite=>false;
        public override long Length=>throw new NotSupportedException();public override long Position{get=>throw new NotSupportedException();set=>throw new NotSupportedException();}
        public override int Read(byte[] b,int o,int n)=>throw new NotSupportedException();public override void Flush()=>throw new NotSupportedException();public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long v)=>throw new NotSupportedException();public override void Write(byte[] b,int o,int n)=>throw new NotSupportedException();
    }
}
