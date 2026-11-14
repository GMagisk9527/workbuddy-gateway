using System.Buffers;

namespace StreamingMigration;

// A complete frame is borrowed until the next Reset. Callers must await both
// WriteAsync and FlushAsync before reuse; deferred terminal frames need a copy.
// Large buffers are request-local and discarded after that frame, never pooled.
internal sealed class SseFrameBuffer(Action<long>? reserve = null, Action? overflow = null) : IBufferWriter<byte>
{
    const int RetainCapacity = 64 << 10;
    byte[] bytes = [];
    int written;
    long limit = int.MaxValue;
    internal ReadOnlyMemory<byte> WrittenMemory => bytes.AsMemory(0, written);
    internal void Reset(long nextLimit = int.MaxValue)
    {
        written = 0; limit = nextLimit;
        if (bytes.Length > RetainCapacity) bytes = [];
    }
    void Fail()
    {
        overflow?.Invoke();
        throw new InvalidOperationException("SSE frame exceeds its size limit");
    }
    public void Advance(int count)
    {
        if (count < 0 || count > bytes.Length - written) throw new ArgumentOutOfRangeException(nameof(count));
        if (count > limit - written) Fail();
        written += count;
    }
    public Memory<byte> GetMemory(int sizeHint = 0)
    {
        if (sizeHint < 0) throw new ArgumentOutOfRangeException(nameof(sizeHint));
        long required = (long)written + Math.Max(1, sizeHint);
        // Utf8JsonWriter requests pessimistic escaping space, even when the
        // eventual JSON fits. Bound that allowance separately from actual bytes.
        long capacityLimit = Math.Min(int.MaxValue, 6 * Math.Min(limit, int.MaxValue) + 4096);
        if (required > capacityLimit) Fail();
        if (required > bytes.Length)
        {
            int capacity = (int)Math.Min(capacityLimit, Math.Max(required, Math.Max(256L, 2L * bytes.Length)));
            // Admit before allocating; includes simultaneous old/new arrays on growth.
            reserve?.Invoke(2L * capacity);
            var next = new byte[capacity];
            bytes.AsSpan(0, written).CopyTo(next); bytes = next;
        }
        return bytes.AsMemory(written);
    }
    public Span<byte> GetSpan(int sizeHint = 0) => GetMemory(sizeHint).Span;
    internal void Write(ReadOnlySpan<byte> value)
    {
        if (value.Length > limit - written) Fail();
        value.CopyTo(GetSpan(value.Length)); Advance(value.Length);
    }
}
