using System.Text;
using System.Buffers;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Encodings.Web;
using System.Runtime.InteropServices;
using System.Text.Unicode;
namespace TypedMigration;
static class GoJson
{
    static readonly JsonSerializerOptions ScalarJson = new() { Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping };
    static readonly SearchValues<char> Escapes = SearchValues.Create(new string(Enumerable.Range(0,32).Select(i=>(char)i).ToArray()) + "\"\\<>&\u2028\u2029");
    static readonly SearchValues<byte> RawEscapes=SearchValues.Create(new byte[]{(byte)'<',(byte)'>',(byte)'&',0xe2});
    static readonly SearchValues<byte> HtmlEscapes=SearchValues.Create(new byte[]{(byte)'<',(byte)'>',(byte)'&'});
    const string Hex = "0123456789abcdef";
    // Common tool-result strings can remain UTF-8 throughout mapping. Unusual
    // escape spellings fall back to the existing decoder/canonicalizer.
    internal static bool TryRawString(JsonValue value,out ReadOnlySpan<byte> raw,out int size)
    {
        raw=default;size=0;
        return value.TryGetValue<JsonElement>(out var element)&&TryRawString(element,out raw,out size);
    }
    internal static bool TryRawString(JsonElement element,out ReadOnlySpan<byte> raw,out int size)
    {
        raw=default;size=0;
        if(element.ValueKind!=JsonValueKind.String)return false;
        var token=JsonMarshal.GetRawUtf8Value(element);
        if(token.Length<4096||!Utf8.IsValid(token))return false;
        var content=token[1..^1];var rest=content;
        while(true)
        {
            int i=rest.IndexOf((byte)'\\');if(i<0)break;
            if(i+1>=rest.Length||rest[i+1] is not ((byte)'"' or (byte)'\\' or (byte)'b' or (byte)'f' or (byte)'n' or (byte)'r' or (byte)'t'))return false;
            rest=rest[(i+2)..];
        }
        int extra=checked(5*content.CountAny(HtmlEscapes));rest=content;
        while(true)
        {
            int i=rest.IndexOf((byte)0xe2);if(i<0)break;
            if(i+2<rest.Length&&rest[i+1]==0x80&&rest[i+2] is 0xa8 or 0xa9)extra=checked(extra+3);
            rest=rest[(i+1)..];
        }
        raw=token;size=checked(token.Length+extra);return true;
    }
    static void WriteRawString(ReadOnlySpan<byte> raw,Span<byte> output)
    {
        int position=0,start=0,scan=1;
        while(scan<raw.Length-1)
        {
            int next=raw[scan..^1].IndexOfAny(RawEscapes);if(next<0)break;
            int i=scan+next;byte b=raw[i];
            if(b==(byte)'\\'){scan=i+2;continue;}
            bool separator=b==0xe2&&i+2<raw.Length-1&&raw[i+1]==0x80&&raw[i+2] is 0xa8 or 0xa9;
            if(b==0xe2&&!separator){scan=i+1;continue;}
            raw[start..i].CopyTo(output[position..]);position+=i-start;
            char c=separator?(char)(0x2028+raw[i+2]-0xa8):(char)b;
            output[position++]=(byte)'\\';output[position++]=(byte)'u';
            output[position++]=(byte)Hex[c>>12];output[position++]=(byte)Hex[(c>>8)&15];
            output[position++]=(byte)Hex[(c>>4)&15];output[position++]=(byte)Hex[c&15];
            scan=i+(separator?3:1);start=scan;
        }
        raw[start..].CopyTo(output[position..]);position+=raw.Length-start;
        if(position!=output.Length)throw new InvalidOperationException("JSON size mismatch");
    }
    internal static int QuotedUtf8Size(string value)
    {
        int size=checked(2+Encoding.UTF8.GetByteCount(value));ReadOnlySpan<char> rest=value;
        while(true)
        {
            int i=rest.IndexOfAny(Escapes);if(i<0)return size;
            char c=rest[i];size=checked(size+(c is '"' or '\\' or '\b' or '\f' or '\n' or '\r' or '\t'?1:c is '\u2028' or '\u2029'?3:5));rest=rest[(i+1)..];
        }
    }
    // Shared wire escaping for directly written SSE frames. The size preflight also
    // accounts for invalid UTF-16, which Encoding.UTF8 replaces with U+FFFD.
    internal static void WriteQuotedUtf8(IBufferWriter<byte> output, string value)
    {
        int size=QuotedUtf8Size(value);Span<byte> bytes=output.GetSpan(size);int position=0;
        bytes[position++]=(byte)'"';ReadOnlySpan<char> rest=value;
        while(true)
        {
            int i=rest.IndexOfAny(Escapes);
            if(i<0){position+=Encoding.UTF8.GetBytes(rest,bytes[position..]);break;}
            position+=Encoding.UTF8.GetBytes(rest[..i],bytes[position..]);char c=rest[i];
            bytes[position++]=(byte)'\\';char escaped=Escape(c);bytes[position++]=(byte)escaped;
            if(escaped=='u')
            {
                bytes[position++]=(byte)Hex[c>>12];bytes[position++]=(byte)Hex[(c>>8)&15];
                bytes[position++]=(byte)Hex[(c>>4)&15];bytes[position++]=(byte)Hex[c&15];
            }
            rest=rest[(i+1)..];
        }
        bytes[position++]=(byte)'"';
        if(position!=size)throw new InvalidOperationException("JSON size mismatch");
        output.Advance(position);
    }
    // The caller checks the exact escaped size before allocating the output.
    internal static byte[] FormatUtf8(JsonNode? node,int size,IReadOnlyDictionary<JsonValue,int>? rawSizes=null)
    {
        var output=new Utf8Output(size,rawSizes);output.Walk(node);return output.Finish();
    }
    sealed class Utf8Output(int size,IReadOnlyDictionary<JsonValue,int>? rawSizes)
    {
        readonly byte[] bytes=new byte[size];int position;
        void Text(ReadOnlySpan<char> text)=>position+=Encoding.UTF8.GetBytes(text,bytes.AsSpan(position));
        void One(byte b)=>bytes[position++]=b;
        void Quote(string value)
        {
            One((byte)'"');ReadOnlySpan<char> rest=value;
            while(true)
            {
                int i=rest.IndexOfAny(Escapes);if(i<0){Text(rest);break;}
                Text(rest[..i]);char c=rest[i];One((byte)'\\');
                char escaped=Escape(c);One((byte)escaped);
                if(escaped=='u'){One((byte)Hex[c>>12]);One((byte)Hex[(c>>8)&15]);One((byte)Hex[(c>>4)&15]);One((byte)Hex[c&15]);}
                rest=rest[(i+1)..];
            }
            One((byte)'"');
        }
        internal void Walk(JsonNode? node)
        {
            switch(node)
            {
                case null:Text("null");break;
                case JsonObject obj:
                    One((byte)'{');bool first=true;
                    foreach(var p in obj.OrderBy(p=>p.Key,Keys)){if(!first)One((byte)',');first=false;Quote(p.Key);One((byte)':');Walk(p.Value);}
                    One((byte)'}');break;
                case JsonArray array:
                    One((byte)'[');for(int i=0;i<array.Count;i++){if(i>0)One((byte)',');Walk(array[i]);}One((byte)']');break;
                case JsonValue value:
                    if(rawSizes is not null&&rawSizes.TryGetValue(value,out int cached))
                    {WriteRawString(JsonMarshal.GetRawUtf8Value(value.GetValue<JsonElement>()),bytes.AsSpan(position,cached));position+=cached;}
                    else if(TryRawString(value,out var raw,out int size)){WriteRawString(raw,bytes.AsSpan(position,size));position+=size;}
                    else if(value.TryGetValue<string>(out var text))Quote(text);else Text(value.ToJsonString(ScalarJson));break;
            }
        }
        internal byte[] Finish(){if(position!=bytes.Length)throw new InvalidOperationException("JSON size mismatch");return bytes;}
    }
    static char Escape(char c)=>c switch{'"'=>'"','\\'=>'\\','\b'=>'b','\f'=>'f','\n'=>'n','\r'=>'r','\t'=>'t',_=>'u'};
    public static string Format(JsonElement e) { var b = new StringBuilder(); Append(b, e); return b.ToString(); }
    public static string Format(JsonNode? node)
    {
        var b = new StringBuilder();
        void Walk(JsonNode? n)
        {
            switch (n)
            {
                case null: b.Append("null"); break;
                case JsonObject obj:
                    b.Append('{'); bool first = true;
                    foreach (var p in obj.OrderBy(p => p.Key, Keys)) { if (!first) b.Append(','); first = false; Quote(b,p.Key); b.Append(':'); Walk(p.Value); }
                    b.Append('}'); break;
                case JsonArray array:
                    b.Append('['); for (int i=0;i<array.Count;i++) { if(i>0)b.Append(','); Walk(array[i]); } b.Append(']'); break;
                case JsonValue value:
                    if (value.TryGetValue<string>(out var text)) Quote(b,text);
                    else b.Append(value.ToJsonString(ScalarJson));
                    break;
            }
        }
        Walk(node); return b.ToString();
    }
    static void Quote(StringBuilder b, string value)
    {
        b.Append('"');
        ReadOnlySpan<char> rest=value;
        while(true)
        {
            int i=rest.IndexOfAny(Escapes);if(i<0){b.Append(rest);break;}
            b.Append(rest[..i]);char c=rest[i];b.Append('\\');char escaped=Escape(c);b.Append(escaped);
            if(escaped=='u')b.Append(Hex[c>>12]).Append(Hex[(c>>8)&15]).Append(Hex[(c>>4)&15]).Append(Hex[c&15]);
            rest=rest[(i+1)..];
        }
        b.Append('"');
    }
    static int ScalarCompare(string? a, string? b)
    {
        var x = a.AsSpan().EnumerateRunes(); var y = b.AsSpan().EnumerateRunes();
        while (true) { bool p = x.MoveNext(), q = y.MoveNext(); if (!p || !q) return p ? 1 : q ? -1 : 0; int c = x.Current.Value.CompareTo(y.Current.Value); if (c != 0) return c; }
    }
    static readonly IComparer<string> Keys = Comparer<string>.Create(ScalarCompare);
    static void Append(StringBuilder b, JsonElement e)
    {
        switch (e.ValueKind)
        {
            case JsonValueKind.Object:
                b.Append('{'); bool first = true;
                // A DOM can contain duplicate keys; Go's decoder retains last.
                foreach (var p in e.EnumerateObject().GroupBy(p => p.Name, StringComparer.Ordinal).Select(g => g.Last()).OrderBy(p => p.Name, Keys))
                { if (!first) b.Append(','); first = false; Quote(b, p.Name); b.Append(':'); Append(b, p.Value); }
                b.Append('}'); break;
            case JsonValueKind.Array: b.Append('['); bool head = true; foreach (var v in e.EnumerateArray()) { if (!head) b.Append(','); head = false; Append(b, v); } b.Append(']'); break;
            case JsonValueKind.String: Quote(b, e.GetString()!); break;
            default: b.Append(e.GetRawText()); break;
        }
    }
}
