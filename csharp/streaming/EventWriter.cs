using System.Buffers;
using System.Buffers.Text;
using System.Text;
using TypedMigration;

namespace StreamingMigration;

// Canonical key order and Go-compatible escaping are part of the existing wire
// contract. Write bytes directly: no anonymous objects, reflection, DOM or UTF-16 frame.
public sealed class EventWriter : IEventWriter<byte[]>
{
    sealed class Frame
    {
        readonly ArrayBufferWriter<byte> bytes = new(256);
        internal Frame(string name) { Raw("event: "); Raw(name); Raw("\ndata: "); }
        internal Frame Raw(string text)
        {
            int size=Encoding.UTF8.GetByteCount(text);
            bytes.Advance(Encoding.UTF8.GetBytes(text,bytes.GetSpan(size)));return this;
        }
        internal Frame Quote(string text) { GoJson.WriteQuotedUtf8(bytes,text);return this; }
        internal Frame Number(long value)
        {
            if(!Utf8Formatter.TryFormat(value,bytes.GetSpan(20),out int written))throw new InvalidOperationException();
            bytes.Advance(written);return this;
        }
        internal byte[] Finish() { Raw("\n\n");return bytes.WrittenSpan.ToArray(); }
    }
    static byte[] Start(int index,string kind,string field,string value) =>
        new Frame("content_block_start").Raw("{\"content_block\":{").Quote(field).Raw(":").Quote(value)
        .Raw(",\"type\":").Quote(kind).Raw("},\"index\":").Number(index).Raw(",\"type\":\"content_block_start\"}").Finish();
    static byte[] Delta(int index,string kind,string field,string value) =>
        new Frame("content_block_delta").Raw("{\"delta\":{").Quote(field).Raw(":").Quote(value)
        .Raw(",\"type\":").Quote(kind).Raw("},\"index\":").Number(index).Raw(",\"type\":\"content_block_delta\"}").Finish();
    public byte[] Write(MessageStart e) => new Frame("message_start")
        .Raw("{\"message\":{\"content\":[],\"id\":").Quote(e.Id).Raw(",\"model\":").Quote(e.Model)
        .Raw(",\"role\":\"assistant\",\"type\":\"message\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}},\"type\":\"message_start\"}").Finish();
    public byte[] Write(TextStart e) => Start(e.Index,"text","text","");
    public byte[] Write(ThinkingStart e) => new Frame("content_block_start")
        .Raw("{\"content_block\":{\"signature\":\"\",\"thinking\":\"\",\"type\":\"thinking\"},\"index\":").Number(e.Index)
        .Raw(",\"type\":\"content_block_start\"}").Finish();
    public byte[] Write(ToolStart e) => new Frame("content_block_start")
        .Raw("{\"content_block\":{\"id\":").Quote(e.Id.Value).Raw(",\"input\":{},\"name\":").Quote(e.Name.Value)
        .Raw(",\"type\":\"tool_use\"},\"index\":").Number(e.Index).Raw(",\"type\":\"content_block_start\"}").Finish();
    public byte[] Write(TextDelta e) => Delta(e.Index,"text_delta","text",e.Text);
    public byte[] Write(ThinkingDelta e) => Delta(e.Index,"thinking_delta","thinking",e.Text);
    public byte[] Write(SignatureDelta e) => Delta(e.Index,"signature_delta","signature",e.Signature);
    public byte[] Write(ToolDelta e) => Delta(e.Index,"input_json_delta","partial_json",e.Arguments);
    public byte[] Write(BlockStop e) => new Frame("content_block_stop").Raw("{\"index\":").Number(e.Index).Raw(",\"type\":\"content_block_stop\"}").Finish();
    public byte[] Write(MessageDelta e) => new Frame("message_delta")
        .Raw("{\"delta\":{\"stop_reason\":").Quote(e.StopReason).Raw(",\"stop_sequence\":null},\"type\":\"message_delta\",\"usage\":{\"cache_creation_input_tokens\":")
        .Number(e.Usage.Created).Raw(",\"cache_read_input_tokens\":").Number(e.Usage.Cached).Raw(",\"input_tokens\":").Number(e.Usage.Input)
        .Raw(",\"output_tokens\":").Number(e.Usage.Output).Raw("}}").Finish();
    public byte[] Write(MessageStop e) => new Frame("message_stop").Raw("{\"type\":\"message_stop\"}").Finish();
    public byte[] Write(StreamError e) => new Frame("error").Raw("{\"error\":{\"code\":").Quote(e.Code)
        .Raw(",\"message\":\"Response incomplete\",\"type\":\"api_error\"},\"type\":\"error\"}").Finish();
}
