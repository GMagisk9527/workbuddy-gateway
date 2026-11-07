using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Encodings.Web;
using TypedMigration;
using StreamingMigration;

namespace OpenAiProtocols;

public enum ClientProtocol { Chat, Responses }
public enum ResponseToolKind { Function, Namespace, Custom, WebSearch }
public sealed record ResponseTool(string Name, string Namespace, ResponseToolKind Kind);
public sealed record OpenAiRequest(string Model, bool Stream, byte[] Body, ClientProtocol Protocol,
    IReadOnlyDictionary<string, ResponseTool> Tools, JsonObject Metadata, bool EncryptedReasoning = false);
public sealed class OpenAiFailure(string code, string message, int status = 400) : Exception(message)
{ public string Code { get; } = code; public int Status { get; } = status; }

public static class OpenWire
{
    public const int RequestLimit = 32 << 20;
    static readonly JsonSerializerOptions Json = new() { Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping, MaxDepth = 256 };
    public static JsonNode? Copy(JsonNode? node) => node?.DeepClone();
    public static string Text(JsonNode? node) => node is JsonValue v && v.TryGetValue<string>(out var s) ? s : "";
    public static bool Bool(JsonNode? node) => node is JsonValue v && v.TryGetValue<bool>(out var b) && b;
    public static long Int(JsonNode? node) => node is JsonValue v && v.TryGetValue<long>(out var n) ? n : 0;
    public static IEnumerable<JsonNode?> Items(JsonNode? node) => node is JsonArray a ? a : [];
    public static string Canonical(JsonNode? node) => GoJson.Format(node);
    public static JsonObject Parse(ReadOnlyMemory<byte> input)
    {
        if (input.Length > RequestLimit) throw new OpenAiFailure("request_too_large", "请求体超过大小限制", 413);
        try
        {
            using var doc = JsonDocument.Parse(input, new JsonDocumentOptions { MaxDepth = 256 });
            if (doc.RootElement.ValueKind != JsonValueKind.Object) throw new OpenAiFailure("invalid_request", "请求体必须是 JSON 对象");
            return (JsonObject)Deduplicate(doc.RootElement)!;
        }
        catch (JsonException) { throw new OpenAiFailure("invalid_json", "无效的 JSON 请求体"); }
    }
    static JsonNode? Deduplicate(JsonElement e) => e.ValueKind switch
    {
        JsonValueKind.Object => new JsonObject(e.EnumerateObject().GroupBy(p => p.Name).Select(g => g.Last()).Select(p => new KeyValuePair<string, JsonNode?>(p.Name, Deduplicate(p.Value)))),
        JsonValueKind.Array => new JsonArray(e.EnumerateArray().Select(Deduplicate).ToArray()),
        JsonValueKind.Null => null,
        _ => JsonValue.Create(e.Clone())
    };
    public static string Model(JsonObject root)
    {
        var n = root["model"]; string model = !root.ContainsKey("model") || Text(n) == "" && n is JsonValue v && v.TryGetValue<string>(out _) ? "hy4-preview" : Text(n);
        if (model.Length == 0 || Encoding.UTF8.GetByteCount(model) > 256 || model.EnumerateRunes().Any(r => Rune.IsWhiteSpace(r) || Rune.GetUnicodeCategory(r) is System.Globalization.UnicodeCategory.Control or System.Globalization.UnicodeCategory.Format))
            throw new OpenAiFailure("invalid_request", "model 必须是 1～256 字节的有效 UTF-8 字符串，不能包含空白或控制字符");
        return model;
    }
    public static byte[] Encode(JsonNode? node, long limit,IRetainedOutputBudget? budget=null)
    {
        using var stream = new BudgetStream(limit,budget);
        using (var writer = new Utf8JsonWriter(stream, new() { Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping, MaxDepth = 256 }))
        { if (node is null) writer.WriteNullValue(); else node.WriteTo(writer, Json); }
        return stream.ToArray();
    }
    sealed class BudgetStream(long limit,IRetainedOutputBudget? budget) : Stream
    {
        readonly MemoryStream inner = new();
        public byte[] ToArray() => inner.ToArray();
        public override void Write(ReadOnlySpan<byte> bytes) { if (bytes.Length > limit - inner.Length) throw new OpenAiFailure("output_budget_exceeded", "输出超过大小限制", 502);budget?.Scratch(3*(inner.Length+bytes.Length)); inner.Write(bytes); }
        public override void Write(byte[] bytes, int offset, int count) => Write(bytes.AsSpan(offset, count));
        public override bool CanRead => false; public override bool CanSeek => false; public override bool CanWrite => true;
        public override long Length => inner.Length; public override long Position { get => inner.Position; set => throw new NotSupportedException(); }
        public override void Flush() => inner.Flush(); public override int Read(byte[] b, int o, int c) => throw new NotSupportedException();
        public override long Seek(long o, SeekOrigin s) => throw new NotSupportedException(); public override void SetLength(long v) => throw new NotSupportedException();
        protected override void Dispose(bool disposing) { if (disposing) inner.Dispose(); base.Dispose(disposing); }
    }
    public static void Require(bool condition, string message) { if (!condition) throw new OpenAiFailure("invalid_request", message); }
}
