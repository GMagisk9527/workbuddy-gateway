using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Encodings.Web;
using TypedMigration;
using StreamingMigration;
using System.Runtime.InteropServices;
using System.Text.Unicode;

namespace OpenAiProtocols;

public enum ClientProtocol { Chat, Responses }
public enum ResponseToolKind { Function, Namespace, Custom, WebSearch }
public sealed record ResponseTool(string Name, string Namespace, ResponseToolKind Kind);
public sealed record OpenAiRequest(string Model, bool Stream, byte[] Body, ClientProtocol Protocol,
    IReadOnlyDictionary<string, ResponseTool> Tools, JsonObject Metadata, bool EncryptedReasoning = false)
{public bool OutboundCompatible{get;init;}}
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
    internal sealed class RequestDocument(JsonDocument document,JsonObject root,ReadOnlyMemory<byte> raw,bool hasDuplicates):IDisposable
    {
        internal JsonObject Root{get;}=root;
        internal ReadOnlyMemory<byte> Raw{get;}=raw;
        internal bool HasDuplicates{get;}=hasDuplicates;
        public void Dispose()=>document.Dispose();
    }
    // The request adapters consume this tree synchronously. Only encoded bytes
    // and explicitly cloned response metadata may leave this scope.
    internal static RequestDocument ParseRequest(ReadOnlyMemory<byte> input)
    {
        if(input.Length>RequestLimit)throw new OpenAiFailure("request_too_large","请求体超过大小限制",413);
        JsonDocument? document=null;bool duplicate=false;
        try
        {
            try { document=JsonDocument.Parse(input,new JsonDocumentOptions{MaxDepth=256,AllowDuplicateProperties=false}); }
            catch(JsonException) { duplicate=true;document=JsonDocument.Parse(input,new JsonDocumentOptions{MaxDepth=256}); }
            if(document.RootElement.ValueKind!=JsonValueKind.Object)throw new OpenAiFailure("invalid_request","请求体必须是 JSON 对象");
            return new(document,(JsonObject)Deduplicate(document.RootElement,duplicate)!,input,duplicate);
        }
        catch(JsonException){document?.Dispose();throw new OpenAiFailure("invalid_json","无效的 JSON 请求体");}
        catch{document?.Dispose();throw;}
    }
    internal static bool IsMinified(ReadOnlySpan<byte> bytes)
    {
        bool quoted=false,escaped=false;
        foreach(byte b in bytes)
        {
            if(quoted){if(escaped)escaped=false;else if(b==(byte)'\\')escaped=true;else if(b==(byte)'"')quoted=false;continue;}
            if(b==(byte)'"'){quoted=true;continue;}
            if(b is (byte)' ' or (byte)'\t' or (byte)'\r' or (byte)'\n')return false;
        }
        return !quoted&&!escaped;
    }
    internal static bool IsString(JsonValue value)
    {
        if(value.TryGetValue<JsonElement>(out var element)&&element.ValueKind==JsonValueKind.String)
        {
            var raw=JsonMarshal.GetRawUtf8Value(element);
            if(raw.Length>=256&&CanonicalString(raw[1..^1]))return true;
        }
        return value.TryGetValue<string>(out _);
    }
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
    static JsonNode? Deduplicate(JsonElement e,bool own=true) => e.ValueKind switch
    {
        JsonValueKind.Object => new JsonObject(e.EnumerateObject().GroupBy(p => p.Name).Select(g => g.Last()).Select(p => new KeyValuePair<string, JsonNode?>(p.Name, Deduplicate(p.Value,own)))),
        JsonValueKind.Array => new JsonArray(e.EnumerateArray().Select(v=>Deduplicate(v,own)).ToArray()),
        JsonValueKind.Null => null,
        _ => JsonValue.Create(own?e.Clone():e)
    };
    public static string Model(JsonObject root)
    {
        var n = root["model"]; string model = !root.ContainsKey("model") || Text(n) == "" && n is JsonValue v && v.TryGetValue<string>(out _) ? "hy4-preview" : Text(n);
        if (model.Length == 0 || Encoding.UTF8.GetByteCount(model) > 256 || model.EnumerateRunes().Any(r => Rune.IsWhiteSpace(r) || Rune.GetUnicodeCategory(r) is System.Globalization.UnicodeCategory.Control or System.Globalization.UnicodeCategory.Format))
            throw new OpenAiFailure("invalid_request", "model 必须是 1～256 字节的有效 UTF-8 字符串，不能包含空白或控制字符");
        return model;
    }
    public static byte[] Encode(JsonNode? node, long limit,IRetainedOutputBudget? budget=null,bool optimizeLarge=false)
    {
        int capacity=optimizeLarge&&budget is null&&TryEncodedSize(node,limit,out int size)?size:0;
        using var stream = new BudgetStream(limit,budget,capacity);
        using (var writer = new Utf8JsonWriter(stream, new() { Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping, MaxDepth = 256 }))
        { WriteNode(writer,node); }
        return stream.ToArray();
    }
    // Only take this path for large, already canonical UTF-8 request values.
    // It avoids MemoryStream growth and its final full-body copy. Streaming
    // output retains the existing incremental shared-budget behavior.
    static bool TryEncodedSize(JsonNode? node,long limit,out int size)
    {
        long total=0;bool large=false;
        bool Add(long bytes){total+=bytes;return total<=limit&&total<=int.MaxValue;}
        bool Text(string text)
        {
            if(text.Length>4096)return false;
            try{return Add(2+JsonEncodedText.Encode(text,JavaScriptEncoder.UnsafeRelaxedJsonEscaping).EncodedUtf8Bytes.Length);}
            catch(ArgumentException){return false;} // Let the original writer handle malformed UTF-16.
        }
        bool Walk(JsonNode? item,int depth)
        {
            if(depth>256)return false;
            switch(item)
            {
                case null:return Add(4);
                case JsonObject obj:
                    if(!Add(2))return false;int properties=0;
                    foreach(var p in obj)if(!Add(properties++==0?1:2)||!Text(p.Key)||!Walk(p.Value,depth+1))return false;
                    return true;
                case JsonArray array:
                    if(!Add(2))return false;
                    for(int i=0;i<array.Count;i++)if(i>0&&!Add(1)||!Walk(array[i],depth+1))return false;
                    return true;
                case JsonValue value when value.TryGetValue<JsonElement>(out var element):
                    var raw=JsonMarshal.GetRawUtf8Value(element);
                    if(element.ValueKind!=JsonValueKind.String)return Add(raw.Length);
                    if(CanonicalString(raw[1..^1])){large|=raw.Length>=4096;return Add(raw.Length);}
                    return raw.Length<=4096&&Text(element.GetString()!);
                case JsonValue value when value.TryGetValue<string>(out var text):return Text(text);
                default:return Add(item.ToJsonString(Json).Length);
            }
        }
        bool success=Walk(node,0)&&large;size=success?(int)total:0;return success;
    }
    static void WriteNode(Utf8JsonWriter writer,JsonNode? node)
    {
        switch(node)
        {
            case null:writer.WriteNullValue();return;
            case JsonObject obj:
                writer.WriteStartObject();foreach(var p in obj){writer.WritePropertyName(p.Key);WriteNode(writer,p.Value);}writer.WriteEndObject();return;
            case JsonArray array:
                writer.WriteStartArray();foreach(var item in array)WriteNode(writer,item);writer.WriteEndArray();return;
            case JsonValue value when value.TryGetValue<JsonElement>(out var element)&&element.ValueKind==JsonValueKind.String:
                var raw=JsonMarshal.GetRawUtf8Value(element);
                if(raw.Length>=256&&CanonicalString(raw[1..^1])){writer.WriteRawValue(raw,skipInputValidation:true);return;}
                break;
        }
        node.WriteTo(writer,Json);
    }
    // Reuse validated, already canonical UTF-8 string tokens. Unusual escapes or
    // characters still use the normal encoder, keeping the exact wire format.
    static bool CanonicalString(ReadOnlySpan<byte> raw)
    {
        if(!Utf8.IsValid(raw))return false;
        while(!raw.IsEmpty)
        {
            int i=raw.IndexOf((byte)'\\');
            var plain=i<0?raw:raw[..i];
            if(JavaScriptEncoder.UnsafeRelaxedJsonEscaping.FindFirstCharacterToEncodeUtf8(plain)>=0)return false;
            if(i<0)return true;
            if(i+1>=raw.Length||raw[i+1] is not ((byte)'"' or (byte)'\\' or (byte)'b' or (byte)'f' or (byte)'n' or (byte)'r' or (byte)'t'))return false;
            raw=raw[(i+2)..];
        }
        return true;
    }
    sealed class BudgetStream(long limit,IRetainedOutputBudget? budget,int capacity=0) : Stream
    {
        readonly MemoryStream inner = new(capacity);
        public byte[] ToArray() => inner.TryGetBuffer(out var buffer)&&buffer.Offset==0&&buffer.Array!.Length==inner.Length?buffer.Array:inner.ToArray();
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
