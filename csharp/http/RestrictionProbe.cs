using StreamingMigration;

namespace HttpMigration;

// Read-ahead is captured byte-for-byte, including the SSE reader's buffered tail.
internal static class RestrictionProbe
{
    internal sealed record Result(Stream Replay,bool Restricted,string? Usage);
    internal static async Task<Result> Run(Stream source,CancellationToken cancellation,Action<string>? observeUsage=null,Action? firstOutput=null,Action? completed=null)
    {
        var capture=new Capture(source); var reader=new SseReader(capture,65536); string? usage=null;
        try
        {
            for(int i=0;i<64;i++)
            {
                byte[]? payload=await reader.NextPayloadBytesAsync(cancellation);
                if(payload is null) break;
                // Rejection frames may themselves contain measured usage.
                try { using var d=System.Text.Json.JsonDocument.Parse(payload); if(d.RootElement.ValueKind==System.Text.Json.JsonValueKind.Object && d.RootElement.TryGetProperty("usage",out var u) && u.ValueKind==System.Text.Json.JsonValueKind.Object) { usage=u.GetRawText(); observeUsage?.Invoke(usage); } } catch(System.Text.Json.JsonException) { }
                ChatChunk chunk;
                try { chunk=ChunkReader.Parse(payload); }
                catch(ConversionFailure e) when(e.Code=="11140") { return new(Stream.Null,true,usage); }
                if(chunk.HasOutput)firstOutput?.Invoke();
                if(chunk.Choices.Any(c=>c.Finish.Length>0))completed?.Invoke();
                if(chunk.HasOutput || chunk.Choices.Any(c=>c.Finish.Length>0)) break;
            }
        }
        catch(ConversionFailure e)when(e.Code==FirstTokenBudget.ErrorCode){throw;}
        catch(ConversionFailure) { } // The normal pump reports malformed/oversize frames.
        catch(IOException e) { capture.Failure=e; }
        return new(new Replay(capture.Bytes.ToArray(),source,capture.Failure),false,usage);
    }
    sealed class Capture(Stream source) : Stream
    {
        internal MemoryStream Bytes { get; }=new(); internal IOException? Failure;
        public override async ValueTask<int> ReadAsync(Memory<byte> buffer,CancellationToken cancellationToken=default)
        {
            int remaining=65536-(int)Bytes.Length;
            if(remaining==0) throw new ConversionFailure("probe_limit");
            int n=await source.ReadAsync(buffer[..Math.Min(buffer.Length,remaining)],cancellationToken); Bytes.Write(buffer.Span[..n]); return n;
        }
        public override bool CanRead=>true; public override bool CanSeek=>false; public override bool CanWrite=>false;
        public override long Length=>throw new NotSupportedException(); public override long Position{get=>throw new NotSupportedException();set=>throw new NotSupportedException();}
        public override int Read(byte[] b,int o,int n)=>throw new NotSupportedException(); public override void Flush(){} public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long v)=>throw new NotSupportedException();public override void Write(byte[] b,int o,int n)=>throw new NotSupportedException();
    }
    sealed class Replay(byte[] prefix,Stream source,IOException? failure) : Stream
    {
        int offset;
        public override ValueTask<int> ReadAsync(Memory<byte> buffer,CancellationToken cancellationToken=default)
        {
            cancellationToken.ThrowIfCancellationRequested();
            if(offset<prefix.Length) { int n=Math.Min(buffer.Length,prefix.Length-offset); prefix.AsMemory(offset,n).CopyTo(buffer); offset+=n; return ValueTask.FromResult(n); }
            if(failure is not null) throw failure;
            return source.ReadAsync(buffer,cancellationToken);
        }
        public override bool CanRead=>true;public override bool CanSeek=>false;public override bool CanWrite=>false;
        public override long Length=>throw new NotSupportedException();public override long Position{get=>throw new NotSupportedException();set=>throw new NotSupportedException();}
        public override int Read(byte[] b,int o,int n)=>throw new NotSupportedException();public override void Flush(){}public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long v)=>throw new NotSupportedException();public override void Write(byte[] b,int o,int n)=>throw new NotSupportedException();
    }
}
