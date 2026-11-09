using System.Text;

namespace StreamingMigration;

// Byte framing avoids decoding split UTF-8 sequences. CR, LF and split CRLF are
// equivalent delimiters; the entire event, including metadata, has one bound.
public sealed class SseReader(Stream source, int maxEventBytes = 4 << 20)
{
    readonly byte[] buffer = new byte[8192];
    readonly MemoryStream line = new();
    readonly List<string> data = [];
    int offset, available, frameBytes;
    bool skipLf, first = true, done, hadLine;
    IOException? readError;
    string DecodeLine(ReadOnlySpan<byte> bytes)
    {
        string value = Encoding.UTF8.GetString(bytes);
        if (first) { first = false; if (value.StartsWith('\ufeff')) value = value[1..]; }
        return value;
    }
    string TakeLine()
    {
        string value = DecodeLine(line.GetBuffer().AsSpan(0, (int)line.Length));
        line.SetLength(0);
        return value;
    }
    string? ScanLine()
    {
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
        if (line.Length == 0) return DecodeLine(content);
        line.Write(content);
        return TakeLine();
    }
    void Reset() { frameBytes = 0; hadLine = false; data.Clear(); }
    // Returns a data payload, null for metadata/comment-only events.
    string? ProcessLine(string value, bool eof)
    {
        string trimmed = value.Trim();
        if (!hadLine && (trimmed.StartsWith('{') || trimmed == "[DONE]")) { Reset(); return trimmed; }
        if (!hadLine && trimmed.StartsWith(':')) { Reset(); return null; }
        if (value.Length == 0 && !eof) { string? payload = data.Count > 0 ? string.Join('\n', data) : null; Reset(); return payload; }
        hadLine = true;
        if (trimmed.StartsWith("data:", StringComparison.Ordinal))
        {
            string v = trimmed[5..]; data.Add(v.StartsWith(' ') ? v[1..] : v);
        }
        else if (trimmed == "data") data.Add("");
        if (eof && data.Count != 0) throw new ConversionFailure("incomplete_event");
        return null;
    }
    public async ValueTask<ChatChunk?> NextAsync(CancellationToken cancellation)
    {
        var payload = await NextPayloadAsync(cancellation);
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
                if (await NextPayloadAsync(trailer.Token) is { } payload)
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
            string? payload = null;
            if (available == 0)
            {
                done = true;
                if (line.Length > 0) payload = ProcessLine(TakeLine(), true);
                if (data.Count > 0) throw new ConversionFailure("incomplete_event");
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
            if (payload is null || payload.Trim().Length == 0) continue;
            payload = payload.Trim();
            // Match stripDataPrefixBytes for repeated prefixes in existing upstreams.
            while (payload.StartsWith("data:", StringComparison.Ordinal)) payload = payload[5..].Trim();
            if (payload == "[DONE]") { done = true; readError = null; return null; }
            return payload;
        }
    }
}
