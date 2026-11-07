using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Encodings.Web;
using TypedMigration;
using StreamingMigration;
using System.Runtime.InteropServices;
using System.Text.Unicode;
using System.Buffers;

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
        internal JsonElement Original=>document.RootElement;
        internal ReadOnlyMemory<byte> Raw{get;}=raw;
        internal bool HasDuplicates{get;}=hasDuplicates;
        internal bool HasCanonicalStrings()=>CanonicalElements(document.RootElement);
        public void Dispose()=>document.Dispose();
    }
    static bool CanonicalElements(JsonElement element)
    {
        switch(element.ValueKind)
        {
            case JsonValueKind.Object:
                foreach(var p in element.EnumerateObject())
                    if(!CanonicalString(JsonMarshal.GetRawUtf8PropertyName(p))||!CanonicalElements(p.Value))return false;
                return true;
            case JsonValueKind.Array:
                foreach(var item in element.EnumerateArray())if(!CanonicalElements(item))return false;
                return true;
            case JsonValueKind.String:return CanonicalString(JsonMarshal.GetRawUtf8Value(element)[1..^1]);
            default:return true;
        }
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
        if(bytes.IsEmpty||bytes[0]!=(byte)'{')return false;
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
            // Type validation needs valid Unicode, not canonical wire escaping.
            // JSON parsing has checked ordinary escapes; Unicode escapes retain
            // the decoder path so malformed surrogate pairs still fail there.
            if(raw.Length>=256&&Utf8.IsValid(raw)&&raw.IndexOf("\\u"u8)<0)return true;
        }
        return value.TryGetValue<string>(out _);
    }
    internal static JsonNode BorrowScalar(JsonValue value)=>value.TryGetValue<JsonElement>(out var element)?JsonValue.Create(element)!:value.DeepClone();
    internal static bool HasText(JsonValue value)
    {
        if(value.TryGetValue<JsonElement>(out var element)&&element.ValueKind==JsonValueKind.String)
        {
            var raw=JsonMarshal.GetRawUtf8Value(element);
            for(int i=1;i<Math.Min(raw.Length-1,129);i++)
            {
                byte b=raw[i];if(b>=128)break;
                if(b==(byte)'\\'&&i+1<raw.Length&&raw[i+1]==(byte)'u')break;
                if(b==(byte)'\\'&&i+1<raw.Length&&raw[i+1] is (byte)'n' or (byte)'r' or (byte)'t' or (byte)'f'){i++;continue;}
                if(b>32)return true;
            }
        }
        return value.TryGetValue<string>(out var text)&&!string.IsNullOrWhiteSpace(text);
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
    public static byte[] Encode(JsonNode? node, long limit,IRetainedOutputBudget? budget=null,bool optimizeLarge=false,Action<int>? reserve=null)
    {
        Dictionary<JsonValue,JsonElement>? rawStrings=null;
        int capacity=optimizeLarge&&budget is null&&TryEncodedSize(node,limit,out int size,out rawStrings)?size:0;
        if(reserve is not null)
        {
            if(capacity==0)
            {
                using var count=new CountingBufferWriter(limit);
                using var counter=new Utf8JsonWriter(count,new(){Encoder=JavaScriptEncoder.UnsafeRelaxedJsonEscaping,MaxDepth=256});
                WriteNode(counter,node);counter.Flush();capacity=checked((int)count.Written);
            }
            reserve(capacity);
        }
        if(capacity>0&&budget is null)
        {
            using var output=new ExactBufferWriter(capacity);
            using(var direct=new Utf8JsonWriter(output,new(){Encoder=JavaScriptEncoder.UnsafeRelaxedJsonEscaping,MaxDepth=256}))
            {WriteNode(direct,node,rawStrings);}
            return output.Finish();
        }
        using var stream = new BudgetStream(limit,budget,capacity);
        using (var writer = new Utf8JsonWriter(stream, new() { Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping, MaxDepth = 256 }))
        { WriteNode(writer,node,capacity>0?rawStrings:null); }
        return stream.ToArray();
    }
    public static byte[] EncodeDocument(Action<Utf8JsonWriter> write,int limit,Action<int> reserve)
    {
        int size;
        using(var count=new CountingBufferWriter(limit))
        {
            using(var counter=new Utf8JsonWriter(count,new(){Encoder=JavaScriptEncoder.UnsafeRelaxedJsonEscaping,MaxDepth=256}))write(counter);
            size=checked((int)count.Written);
        }
        reserve(size);
        using var output=new ExactBufferWriter(size);
        using(var writer=new Utf8JsonWriter(output,new(){Encoder=JavaScriptEncoder.UnsafeRelaxedJsonEscaping,MaxDepth=256}))write(writer);
        return output.Finish();
    }
    // Only take this path for large, already canonical UTF-8 request values.
    // It avoids MemoryStream growth and its final full-body copy. Streaming
    // output retains the existing incremental shared-budget behavior.
    static bool TryEncodedSize(JsonNode? node,long limit,out int size,out Dictionary<JsonValue,JsonElement>? strings)
    {
        long total=0;bool large=false;
        Dictionary<JsonValue,JsonElement>? validated=null;
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
                    if(CanonicalString(raw[1..^1]))
                    {
                        large|=raw.Length>=4096;
                        if(raw.Length>=256)(validated??=[])[value]=element;
                        return Add(raw.Length);
                    }
                    return raw.Length<=4096&&Text(element.GetString()!);
                case JsonValue value when value.TryGetValue<string>(out var text):return Text(text);
                default:return Add(item.ToJsonString(Json).Length);
            }
        }
        bool success=Walk(node,0)&&large;size=success?(int)total:0;strings=success?validated:null;return success;
    }
    static void WriteNode(Utf8JsonWriter writer,JsonNode? node,Dictionary<JsonValue,JsonElement>? rawStrings=null)
    {
        switch(node)
        {
            case null:writer.WriteNullValue();return;
            case JsonObject obj:
                writer.WriteStartObject();foreach(var p in obj){writer.WritePropertyName(p.Key);WriteNode(writer,p.Value,rawStrings);}writer.WriteEndObject();return;
            case JsonArray array:
                writer.WriteStartArray();foreach(var item in array)WriteNode(writer,item,rawStrings);writer.WriteEndArray();return;
            case JsonValue value when value.TryGetValue<JsonElement>(out var element)&&element.ValueKind==JsonValueKind.String:
                var raw=JsonMarshal.GetRawUtf8Value(element);
                if(rawStrings?.ContainsKey(value)==true){writer.WriteRawValue(raw,skipInputValidation:true);return;}
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
            int i=JavaScriptEncoder.UnsafeRelaxedJsonEscaping.FindFirstCharacterToEncodeUtf8(raw);
            if(i<0)return true;
            if(raw[i]!=(byte)'\\'||i+1>=raw.Length||raw[i+1] is not ((byte)'"' or (byte)'\\' or (byte)'b' or (byte)'f' or (byte)'n' or (byte)'r' or (byte)'t'))return false;
            raw=raw[(i+2)..];
        }
        return true;
    }
    // Encode only a bounded prefix of an already validated request tree. A
    // noncanonical string falls back to the complete encoder; no partial result
    // may be used when false is returned. Large canonical strings stay UTF-8.
    public static bool TryEncodePrefix(JsonNode? node,Span<byte> destination,out int written)
    {
        var writer=new PrefixWriter(destination);
        bool result=writer.Walk(node,0);written=writer.Written;return result;
    }
    ref struct PrefixWriter(Span<byte> destination)
    {
        readonly Span<byte> bytes=destination;
        public int Written{get;private set;}
        bool Full=>Written==bytes.Length;
        void Put(ReadOnlySpan<byte> value)
        {int take=Math.Min(value.Length,bytes.Length-Written);value[..take].CopyTo(bytes[Written..]);Written+=take;}
        void One(byte value){if(!Full)bytes[Written++]=value;}
        bool Name(string name)
        {
            if(name.Length>4096)return false;
            One((byte)'"');Put(JsonEncodedText.Encode(name,JavaScriptEncoder.UnsafeRelaxedJsonEscaping).EncodedUtf8Bytes);One((byte)'"');return true;
        }
        public bool Walk(JsonNode? node,int depth)
        {
            if(Full)return true;
            if(depth>256)return false;
            switch(node)
            {
                case null:Put("null"u8);return true;
                case JsonObject obj:
                    One((byte)'{');bool first=true;
                    foreach(var p in obj)
                    {
                        if(Full)return true;
                        if(!first)One((byte)',');first=false;if(Full)return true;
                        if(!Name(p.Key))return false;One((byte)':');if(!Walk(p.Value,depth+1))return false;
                    }
                    One((byte)'}');return true;
                case JsonArray array:
                    One((byte)'[');for(int i=0;i<array.Count&&!Full;i++){if(i>0)One((byte)',');if(!Walk(array[i],depth+1))return false;}One((byte)']');return true;
                case JsonValue value when value.TryGetValue<JsonElement>(out var element):
                    var raw=JsonMarshal.GetRawUtf8Value(element);
                    if(element.ValueKind==JsonValueKind.String)
                    {
                        // Inspect beyond the cut so a split escape cannot look
                        // canonical, and finish at most one partial UTF-8 rune.
                        int count=Math.Min(raw.Length-2,bytes.Length-Written+8);
                        for(int extra=0;extra<3&&count<raw.Length-2&&(raw[count+1]&0xc0)==0x80;extra++)count++;
                        if(!CanonicalString(raw.Slice(1,count)))return false;
                    }
                    Put(raw);return true;
                case JsonValue value:
                    if(value.TryGetValue<string>(out var text)&&text.Length>4096)return false;
                    Put(Encode(value,RequestLimit));return true;
                default:return false;
            }
        }
    }
    sealed class CountingBufferWriter(long limit):IBufferWriter<byte>,IDisposable
    {
        byte[]? scratch;internal long Written{get;private set;}
        public Memory<byte> GetMemory(int sizeHint=0)
        {
            sizeHint=Math.Max(sizeHint,256);
            if(scratch is null||scratch.Length<sizeHint)
            {if(scratch is not null)ArrayPool<byte>.Shared.Return(scratch,clearArray:true);scratch=ArrayPool<byte>.Shared.Rent(sizeHint);}
            return scratch;
        }
        public Span<byte> GetSpan(int sizeHint=0)=>GetMemory(sizeHint).Span;
        public void Advance(int count)
        {
            if(count<0||scratch is null||count>scratch.Length)throw new ArgumentOutOfRangeException(nameof(count));
            if(count>limit-Written)throw new OpenAiFailure("output_budget_exceeded","输出超过大小限制",502);Written+=count;
        }
        public void Dispose(){if(scratch is not null){ArrayPool<byte>.Shared.Return(scratch,clearArray:true);scratch=null;}}
    }
    // Utf8JsonWriter's Stream constructor buffers the complete document itself.
    // An IBufferWriter lets it write directly into the reserved result array.
    // Pessimistic size hints near the end use a small pooled overflow buffer;
    // only the bytes actually written are copied back into the result.
    sealed class ExactBufferWriter(int size):IBufferWriter<byte>,IDisposable
    {
        readonly byte[] bytes=new byte[size];int written;byte[]? scratch;bool usingScratch;
        public Memory<byte> GetMemory(int sizeHint=0)
        {
            sizeHint=Math.Max(1,sizeHint);
            if(sizeHint<=bytes.Length-written){usingScratch=false;return bytes.AsMemory(written);}
            if(scratch is null||scratch.Length<sizeHint)
            {if(scratch is not null)ArrayPool<byte>.Shared.Return(scratch,clearArray:true);scratch=ArrayPool<byte>.Shared.Rent(sizeHint);}
            usingScratch=true;return scratch;
        }
        public Span<byte> GetSpan(int sizeHint=0)=>GetMemory(sizeHint).Span;
        public void Advance(int count)
        {
            if(count<0||count>bytes.Length-written)throw new InvalidOperationException("JSON size mismatch");
            if(usingScratch)scratch!.AsSpan(0,count).CopyTo(bytes.AsSpan(written));written+=count;
        }
        internal byte[] Finish(){if(written!=bytes.Length)throw new InvalidOperationException("JSON size mismatch");return bytes;}
        public void Dispose(){if(scratch is not null){ArrayPool<byte>.Shared.Return(scratch,clearArray:true);scratch=null;}}
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
