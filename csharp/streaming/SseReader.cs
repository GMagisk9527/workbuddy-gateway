using System.Buffers;
using System.Text;

namespace StreamingMigration;

// Byte framing avoids decoding split UTF-8 sequences. CR, LF and split CRLF are
// equivalent delimiters; the entire event, including metadata, has one bound.
// Payloads stay UTF-8 until the JSON parser consumes them, so the hot path does
// not build a string per line and transcode it back for every SSE frame.
public sealed class SseReader(Stream source, int maxEventBytes = 4 << 20)
{
    static ReadOnlySpan<byte> Done => "[DONE]"u8;
    static ReadOnlySpan<byte> DataPrefix => "data:"u8;
    static ReadOnlySpan<byte> DataWord => "data"u8;
    readonly byte[] buffer = new byte[8192];
    readonly ArrayBufferWriter<byte> line = new(256);
    readonly ArrayBufferWriter<byte> data = new(256);
    int offset, available, frameBytes, dataLines;
    bool skipLf, first = true, done, hadLine;
    IOException? readError;
    async ValueTask<bool> FillAsync(CancellationToken cancellation)
    {
        try { available = await source.ReadAsync(buffer, cancellation); }
        catch (IOException e) { readError = e; available = 0; }
        offset = 0;
        return available != 0;
    }
    void ResetEvent() { frameBytes = 0; hadLine = false; data.Clear(); dataLines = 0; }
    void AppendData(ReadOnlySpan<byte> value)
    {
        if (dataLines++ > 0) data.Write("\n"u8);
        data.Write(value);
    }
    static ReadOnlySpan<byte> TrimAscii(ReadOnlySpan<byte> value)
    {
        int start = 0, end = value.Length;
        while (start < end && value[start] <= (byte)' ') start++;
        while (end > start && value[end - 1] <= (byte)' ') end--;
        return value[start..end];
    }
    byte[]? ProcessLine(ReadOnlySpan<byte> raw, bool eof)
    {
        var value = TrimAscii(raw);
        if (first)
        {
            first = false;
            if (value.Length >= 3 && value[0] == 0xEF && value[1] == 0xBB && value[2] == 0xBF) value = value[3..];
        }
        if (!hadLine && (value.Length > 0 && value[0] == (byte)'{' || value.SequenceEqual(Done)))
        {
            var payload = value.ToArray(); ResetEvent(); return payload;
        }
        if (!hadLine && value.Length > 0 && value[0] == (byte)':') { ResetEvent(); return null; }
        if (value.Length == 0 && !eof)
        {
            if (dataLines == 0) { ResetEvent(); return null; }
            var payload = data.WrittenSpan.ToArray(); ResetEvent(); return payload;
        }
        hadLine = true;
        if (value.Length >= DataPrefix.Length && value[..DataPrefix.Length].SequenceEqual(DataPrefix))
        {
            var rest = value[DataPrefix.Length..];
            if (rest.Length > 0 && rest[0] == (byte)' ') rest = rest[1..];
            AppendData(rest);
        }
        else if (value.SequenceEqual(DataWord)) AppendData(default);
        if (eof && dataLines != 0) throw new ConversionFailure("incomplete_event");
        return null;
    }
    byte[]? FinishPayload(byte[] payload)
    {
        var value = TrimAscii(payload);
        // Match stripDataPrefixBytes for repeated prefixes in existing upstreams.
        while (value.Length >= DataPrefix.Length && value[..DataPrefix.Length].SequenceEqual(DataPrefix))
            value = TrimAscii(value[DataPrefix.Length..]);
        if (value.SequenceEqual(Done)) { done = true; readError = null; return null; }
        if (value.Length == 0) return null;
        return value.ToArray();
    }
    public async ValueTask<byte[]?> NextPayloadBytesAsync(CancellationToken cancellation)
    {
        if (done) { if (readError is not null) throw readError; return null; }
        while (true)
        {
            if (offset == available && !await FillAsync(cancellation))
            {
                done = true;
                byte[]? tail = line.WrittenCount > 0 ? ProcessLine(line.WrittenSpan, true) : null;
                line.Clear();
                if (dataLines != 0) throw new ConversionFailure("incomplete_event");
                if (tail is null) { if (readError is not null) throw readError; return null; }
                var final = FinishPayload(tail);
                if (final is not null) return final;
                if (done) { if (readError is not null) throw readError; return null; }
                continue;
            }
            byte value = buffer[offset++];
            if (skipLf)
            {
                skipLf = false;
                if (value == (byte)'\n')
                {
                    // The LF belongs to the preceding CR delimiter. Count it
                    // while the event is open, including metadata lines.
                    if (hadLine && ++frameBytes >= maxEventBytes) throw new ConversionFailure("event_limit");
                    continue;
                }
            }
            if (++frameBytes >= maxEventBytes) throw new ConversionFailure("event_limit");
            if (value is (byte)'\r' or (byte)'\n')
            {
                skipLf = value == (byte)'\r';
                byte[]? payload = ProcessLine(line.WrittenSpan, false);
                line.Clear();
                if (payload is not null)
                {
                    var final = FinishPayload(payload);
                    if (final is not null) return final;
                    if (done) { if (readError is not null) throw readError; return null; }
                }
            }
            else
            {
                line.GetSpan(1)[0] = value;
                line.Advance(1);
            }
        }
    }
    public async ValueTask<ChatChunk?> NextAsync(CancellationToken cancellation)
    {
        var payload = await NextPayloadBytesAsync(cancellation);
        return payload is null ? null : ChunkReader.Parse(payload);
    }
    public async ValueTask ThrowIfFailedFinishAsync(ChatChunk chunk, CancellationToken cancellation)
    {
        if (!chunk.Choices.Any(choice => choice.Finish == "error")) return;

        string? usage = chunk.Usage?.RawJson;
        if (usage is null)
        {
            // A usage-only event may follow the failed choice. Bound how long an
            // explicit failure can be delayed by an optional trailer.
            using var trailer = CancellationTokenSource.CreateLinkedTokenSource(cancellation);
            trailer.CancelAfter(TimeSpan.FromMilliseconds(500));
            try
            {
                if (await NextPayloadBytesAsync(trailer.Token) is { } payload)
                {
                    var next = ChunkReader.Parse(payload);
                    if (next.Choices.IsEmpty) usage = next.Usage?.RawJson;
                }
            }
            catch (OperationCanceledException) when (trailer.IsCancellationRequested && !cancellation.IsCancellationRequested) { }
            catch (Exception e) when (e is IOException or ConversionFailure) { }
        }
        throw new ConversionFailure("upstream_finish_error") { RawUsageJson = usage };
    }
    public async ValueTask<string?> NextPayloadAsync(CancellationToken cancellation)
    {
        var payload = await NextPayloadBytesAsync(cancellation);
        return payload is null ? null : Encoding.UTF8.GetString(payload);
    }
}
