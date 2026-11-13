using System.Text;
using System.Buffers;
using System.Text.Unicode;

namespace StreamingMigration;

// Byte framing avoids decoding split UTF-8 sequences. CR, LF and split CRLF are
// equivalent delimiters; the entire event, including metadata, has one bound.
public sealed class SseReader(Stream source, int maxEventBytes = 4 << 20)
{
    readonly byte[] buffer = new byte[8192];
    readonly MemoryStream line = new();
    readonly MemoryStream data = new();
    int offset, available, frameBytes;
    bool skipLf, first = true, done, hadLine, hasData;
    IOException? readError;
    ReadOnlyMemory<byte> FirstLine(ReadOnlyMemory<byte> bytes)
    {
        if (first) { first = false; if (bytes.Span.StartsWith("\ufeff"u8)) bytes = bytes[3..]; }
        return bytes;
    }
    ReadOnlyMemory<byte> TakeLine()
    {
        var value = FirstLine(line.GetBuffer().AsMemory(0, (int)line.Length));
        line.SetLength(0);
        return value;
    }
    ReadOnlyMemory<byte>? ScanLine()
    {
        int start = offset;
        var bytes = buffer.AsSpan(offset, available - offset);
        int delimiter = bytes.IndexOfAny((byte)'\r', (byte)'\n');
        int consumed = delimiter < 0 ? bytes.Length : delimiter + 1;
        if (consumed >= maxEventBytes - frameBytes) throw new ConversionFailure("event_limit");
        frameBytes += consumed;
        offset += consumed;

        if (delimiter < 0)
        {
            line.Write(bytes);
            return null;
        }
        skipLf = bytes[delimiter] == '\r';
        var content = bytes[..delimiter];
        if (line.Length == 0) return FirstLine(buffer.AsMemory(start, delimiter));
        line.Write(content);
        return TakeLine();
    }
    void Reset() { frameBytes = 0; hadLine = hasData = false; data.SetLength(0); }
    // Match string.Trim, including Unicode whitespace accepted by the legacy
    // reader. Invalid UTF-8 is repaired only when handing over a payload.
    static ReadOnlyMemory<byte> Trim(ReadOnlyMemory<byte> value)
    {
        int start = 0, end = value.Length;
        while (start < end)
        {
            byte b = value.Span[start];
            if (b < 128) { if (b is not (32 or >= 9 and <= 13)) break; start++; }
            else if (Rune.DecodeFromUtf8(value.Span[start..end], out var rune, out int size) == OperationStatus.Done && Rune.IsWhiteSpace(rune)) start += size;
            else break;
        }
        while (start < end)
        {
            byte b = value.Span[end - 1];
            if (b < 128) { if (b is not (32 or >= 9 and <= 13)) break; end--; }
            else if (Rune.DecodeLastFromUtf8(value.Span[start..end], out var rune, out int size) == OperationStatus.Done && Rune.IsWhiteSpace(rune)) end -= size;
            else break;
        }
        return value[start..end];
    }
    void AddData(ReadOnlyMemory<byte> value)
    {
        if (hasData) data.WriteByte((byte)'\n');
        data.Write(value.Span); hasData = true;
    }
    // Returns a data payload, null for metadata/comment-only events.
    ReadOnlyMemory<byte>? ProcessLine(ReadOnlyMemory<byte> value, bool eof)
    {
        var trimmed = Trim(value);
        if (!hadLine && (trimmed.Span.StartsWith("{"u8) || trimmed.Span.SequenceEqual("[DONE]"u8))) { Reset(); return trimmed; }
        if (!hadLine && trimmed.Span.StartsWith(":"u8)) { Reset(); return null; }
        if (value.Length == 0 && !eof) { ReadOnlyMemory<byte>? payload = hasData ? data.GetBuffer().AsMemory(0, (int)data.Length) : null; Reset(); return payload; }
        hadLine = true;
        if (trimmed.Span.StartsWith("data:"u8))
        {
            var v = trimmed[5..]; AddData(v.Span.StartsWith(" "u8) ? v[1..] : v);
        }
        else if (trimmed.Span.SequenceEqual("data"u8)) AddData(ReadOnlyMemory<byte>.Empty);
        if (eof && hasData) throw new ConversionFailure("incomplete_event");
        return null;
    }
    public async ValueTask<ChatChunk?> NextAsync(CancellationToken cancellation)
    {
        var payload = await NextPayloadUtf8Async(cancellation);
        return payload is null ? null : ChunkReader.Parse(payload.Value);
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
                if (await NextPayloadUtf8Async(trailer.Token) is { } payload)
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
        var payload = await NextPayloadUtf8Async(cancellation);
        return payload is null ? null : Encoding.UTF8.GetString(payload.Value.Span);
    }
    // Borrowed memory: valid until the next reader call. Consumers must parse
    // synchronously or copy it before advancing the reader; never retain it.
    public async ValueTask<ReadOnlyMemory<byte>?> NextPayloadUtf8Async(CancellationToken cancellation)
    {
        if (done) { if (readError is not null) throw readError; return null; }
        while (true)
        {
            cancellation.ThrowIfCancellationRequested();
            if (offset == available)
            {
                offset = 0;
                try { available = await source.ReadAsync(buffer, cancellation); }
                catch (IOException e) { readError = e; available = 0; }
            }
            ReadOnlyMemory<byte>? payload = null;
            if (available == 0)
            {
                done = true;
                if (line.Length > 0) payload = ProcessLine(TakeLine(), true);
                if (hasData) throw new ConversionFailure("incomplete_event");
                if (payload is null) { if (readError is not null) throw readError; return null; }
            }
            else
            {
                if (skipLf)
                {
                    skipLf = false;
                    if (buffer[offset] == '\n')
                    {
                        offset++;
                        // The LF belongs to the preceding CR delimiter. Count
                        // it while the event is open, including metadata lines.
                        if (hadLine && ++frameBytes >= maxEventBytes) throw new ConversionFailure("event_limit");
                        continue;
                    }
                }
                if (ScanLine() is { } value) payload = ProcessLine(value, false);
            }
            if (payload is null) continue;
            var result = Trim(payload.Value);
            if (result.Length == 0) continue;
            // Match stripDataPrefixBytes for repeated prefixes in existing upstreams.
            while (result.Span.StartsWith("data:"u8)) result = Trim(result[5..]);
            if (result.Span.SequenceEqual("[DONE]"u8)) { done = true; readError = null; return null; }
            // Encoding.UTF8 previously replaced malformed input. Keep that
            // behavior on the rare invalid path without transcoding normal SSE.
            return Utf8.IsValid(result.Span) ? result : Encoding.UTF8.GetBytes(Encoding.UTF8.GetString(result.Span));
        }
    }
}
