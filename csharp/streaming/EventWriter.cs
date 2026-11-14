using System.Buffers.Text;
using System.Text;
using TypedMigration;

namespace StreamingMigration;

// Canonical key order and Go-compatible escaping are part of the existing wire
// contract. Write bytes directly: no anonymous objects, reflection, DOM or UTF-16 frame.
// A frame is borrowed until the next Write. The pump awaits transport completion
// before reusing the buffer; EventWriter below preserves the owned-array API.
internal sealed class EventBufferWriter(Action<long>? reserve = null) : IEventWriter<ReadOnlyMemory<byte>>
{
    SseFrameBuffer bytes = new(reserve);
    readonly SseFrameBuffer errorBytes = new();
    void Reset() => bytes.Reset();
    EventBufferWriter Raw(string text)
    {
        int size = Encoding.UTF8.GetByteCount(text);
        bytes.Advance(Encoding.UTF8.GetBytes(text, bytes.GetSpan(size)));
        return this;
    }
    EventBufferWriter Quote(string text) { GoJson.WriteQuotedUtf8(bytes, text); return this; }
    EventBufferWriter Number(long value)
    {
        if (!Utf8Formatter.TryFormat(value, bytes.GetSpan(20), out int written)) throw new InvalidOperationException();
        bytes.Advance(written);
        return this;
    }
    EventBufferWriter Prefix(string name) { Raw("event: "); Raw(name); Raw("\ndata: "); return this; }
    ReadOnlyMemory<byte> Finish() { Raw("\n\n"); return bytes.WrittenMemory; }
    ReadOnlyMemory<byte> Start(int index, string kind, string field, string value)
    {
        Reset(); Prefix("content_block_start");
        Raw("{\"content_block\":{"); Quote(field); Raw(":"); Quote(value);
        Raw(",\"type\":"); Quote(kind); Raw("},\"index\":"); Number(index); Raw(",\"type\":\"content_block_start\"}");
        return Finish();
    }
    ReadOnlyMemory<byte> Delta(int index, string kind, string field, string value)
    {
        Reset(); Prefix("content_block_delta");
        Raw("{\"delta\":{"); Quote(field); Raw(":"); Quote(value);
        Raw(",\"type\":"); Quote(kind); Raw("},\"index\":"); Number(index); Raw(",\"type\":\"content_block_delta\"}");
        return Finish();
    }
    public ReadOnlyMemory<byte> Write(MessageStart e)
    {
        Reset(); Prefix("message_start");
        Raw("{\"message\":{\"content\":[],\"id\":"); Quote(e.Id); Raw(",\"model\":"); Quote(e.Model)
        .Raw(",\"role\":\"assistant\",\"type\":\"message\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}},\"type\":\"message_start\"}");
        return Finish();
    }
    public ReadOnlyMemory<byte> Write(TextStart e) => Start(e.Index, "text", "text", "");
    public ReadOnlyMemory<byte> Write(ThinkingStart e)
    {
        Reset(); Prefix("content_block_start");
        Raw("{\"content_block\":{\"signature\":\"\",\"thinking\":\"\",\"type\":\"thinking\"},\"index\":"); Number(e.Index)
        .Raw(",\"type\":\"content_block_start\"}");
        return Finish();
    }
    public ReadOnlyMemory<byte> Write(ToolStart e)
    {
        Reset(); Prefix("content_block_start");
        Raw("{\"content_block\":{\"id\":"); Quote(e.Id.Value); Raw(",\"input\":{},\"name\":"); Quote(e.Name.Value)
        .Raw(",\"type\":\"tool_use\"},\"index\":"); Number(e.Index); Raw(",\"type\":\"content_block_start\"}");
        return Finish();
    }
    public ReadOnlyMemory<byte> Write(TextDelta e) => Delta(e.Index, "text_delta", "text", e.Text);
    public ReadOnlyMemory<byte> Write(ThinkingDelta e) => Delta(e.Index, "thinking_delta", "thinking", e.Text);
    public ReadOnlyMemory<byte> Write(SignatureDelta e) => Delta(e.Index, "signature_delta", "signature", e.Signature);
    public ReadOnlyMemory<byte> Write(ToolDelta e) => Delta(e.Index, "input_json_delta", "partial_json", e.Arguments);
    public ReadOnlyMemory<byte> Write(BlockStop e)
    {
        Reset(); Prefix("content_block_stop");
        Raw("{\"index\":"); Number(e.Index); Raw(",\"type\":\"content_block_stop\"}");
        return Finish();
    }
    public ReadOnlyMemory<byte> Write(MessageDelta e)
    {
        Reset(); Prefix("message_delta");
        Raw("{\"delta\":{\"stop_reason\":"); Quote(e.StopReason); Raw(",\"stop_sequence\":null},\"type\":\"message_delta\",\"usage\":{\"cache_creation_input_tokens\":");
        Number(e.Usage.Created); Raw(",\"cache_read_input_tokens\":"); Number(e.Usage.Cached); Raw(",\"input_tokens\":"); Number(e.Usage.Input)
        .Raw(",\"output_tokens\":"); Number(e.Usage.Output); Raw("}}");
        return Finish();
    }
    public ReadOnlyMemory<byte> Write(MessageStop e)
    {
        Reset(); Prefix("message_stop"); Raw("{\"type\":\"message_stop\"}"); return Finish();
    }
    public ReadOnlyMemory<byte> Write(StreamError e)
    {
        var ordinary = bytes; bytes = errorBytes;
        try { return Error(e); } finally { bytes = ordinary; }
    }
    ReadOnlyMemory<byte> Error(StreamError e)
    {
        Reset(); Prefix("error");
        Raw("{\"error\":{\"code\":"); Quote(e.Code); Raw(",\"message\":\"Response incomplete\",\"type\":\"api_error\"},\"type\":\"error\"}");
        return Finish();
    }
}

// Compatibility facade for consumers that retain frames beyond the next Write.
public sealed class EventWriter : IEventWriter<byte[]>
{
    readonly EventBufferWriter writer = new();
    public byte[] Write(MessageStart e) => writer.Write(e).ToArray();
    public byte[] Write(TextStart e) => writer.Write(e).ToArray();
    public byte[] Write(ThinkingStart e) => writer.Write(e).ToArray();
    public byte[] Write(ToolStart e) => writer.Write(e).ToArray();
    public byte[] Write(TextDelta e) => writer.Write(e).ToArray();
    public byte[] Write(ThinkingDelta e) => writer.Write(e).ToArray();
    public byte[] Write(SignatureDelta e) => writer.Write(e).ToArray();
    public byte[] Write(ToolDelta e) => writer.Write(e).ToArray();
    public byte[] Write(BlockStop e) => writer.Write(e).ToArray();
    public byte[] Write(MessageDelta e) => writer.Write(e).ToArray();
    public byte[] Write(MessageStop e) => writer.Write(e).ToArray();
    public byte[] Write(StreamError e) => writer.Write(e).ToArray();
}
