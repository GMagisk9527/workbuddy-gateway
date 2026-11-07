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
    async ValueTask<int> ByteAsync(CancellationToken cancellation)
    {
        cancellation.ThrowIfCancellationRequested();
        if (offset == available)
        {
            try { available = await source.ReadAsync(buffer, cancellation); }
            catch (IOException e) { readError = e; return -1; }
            offset = 0; if (available == 0) return -1;
        }
        return buffer[offset++];
    }
    string TakeLine()
    {
        string value = Encoding.UTF8.GetString(line.GetBuffer(), 0, (int)line.Length);
        line.SetLength(0);
        if (first) { first = false; if (value.StartsWith('\ufeff')) value = value[1..]; }
        return value;
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
    public async ValueTask<string?> NextPayloadAsync(CancellationToken cancellation)
    {
        if (done) { if (readError is not null) throw readError; return null; }
        while (true)
        {
            int b = await ByteAsync(cancellation);
            string? payload = null;
            if (b < 0)
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
                    if (b == '\n')
                    {
                        // The LF belongs to the preceding CR delimiter. Count
                        // it while the event is open, including metadata lines.
                        if (hadLine && ++frameBytes >= maxEventBytes) throw new ConversionFailure("event_limit");
                        continue;
                    }
                }
                if (++frameBytes >= maxEventBytes) throw new ConversionFailure("event_limit");
                if (b is '\r' or '\n') { skipLf = b == '\r'; payload = ProcessLine(TakeLine(), false); }
                else line.WriteByte((byte)b);
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
