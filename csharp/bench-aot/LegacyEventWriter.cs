using System.Text;
using System.Text.Json;
using TypedMigration;
using StreamingMigration;
// JSON names are confined to the boundary. The state machine only emits typed events.
internal sealed class LegacyEventWriter : IEventWriter<byte[]>
{
    static byte[] Frame(string name, object value) => Encoding.UTF8.GetBytes(
        $"event: {name}\ndata: {GoJson.Format(JsonSerializer.SerializeToElement(value))}\n\n");
    public byte[] Write(MessageStart e) => Frame("message_start", new
    {
        type = "message_start",
        message = new
        {
            id = e.Id,
            type = "message",
            role = "assistant",
            content = Array.Empty<object>(),
            model = e.Model,
            usage = new { input_tokens = 0, output_tokens = 0 }
        }
    });
    public byte[] Write(TextStart e) => Frame("content_block_start", new { type = "content_block_start", index = e.Index, content_block = new { type = "text", text = "" } });
    public byte[] Write(ThinkingStart e) => Frame("content_block_start", new { type = "content_block_start", index = e.Index, content_block = new { type = "thinking", thinking = "", signature = "" } });
    public byte[] Write(ToolStart e) => Frame("content_block_start", new { type = "content_block_start", index = e.Index, content_block = new { type = "tool_use", id = e.Id.Value, name = e.Name.Value, input = new { } } });
    public byte[] Write(TextDelta e) => Frame("content_block_delta", new { type = "content_block_delta", index = e.Index, delta = new { type = "text_delta", text = e.Text } });
    public byte[] Write(ThinkingDelta e) => Frame("content_block_delta", new { type = "content_block_delta", index = e.Index, delta = new { type = "thinking_delta", thinking = e.Text } });
    public byte[] Write(SignatureDelta e) => Frame("content_block_delta", new { type = "content_block_delta", index = e.Index, delta = new { type = "signature_delta", signature = e.Signature } });
    public byte[] Write(ToolDelta e) => Frame("content_block_delta", new { type = "content_block_delta", index = e.Index, delta = new { type = "input_json_delta", partial_json = e.Arguments } });
    public byte[] Write(BlockStop e) => Frame("content_block_stop", new { type = "content_block_stop", index = e.Index });
    public byte[] Write(MessageDelta e) => Frame("message_delta", new
    {
        type = "message_delta",
        delta = new { stop_reason = e.StopReason, stop_sequence = (string?)null },
        usage = new
        {
            input_tokens = e.Usage.Input,
            output_tokens = e.Usage.Output,
            cache_read_input_tokens = e.Usage.Cached,
            cache_creation_input_tokens = e.Usage.Created
        }
    });
    public byte[] Write(MessageStop e) => Frame("message_stop", new { type = "message_stop" });
    public byte[] Write(StreamError e) => Frame("error", new { type = "error", error = new { type = "api_error", code = e.Code, message = "Response incomplete" } });
}
