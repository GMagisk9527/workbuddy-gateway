using System.Globalization;
using System.Text;
using System.Text.Json;

namespace HttpMigration;

// Local heuristic matching the Go gateway, not a model tokenizer. Never fetches images.
internal sealed class TokenEstimation
{
    long cjk, other, images;
    internal static string Validate(JsonElement root)
    {
        if(root.ValueKind==JsonValueKind.Null)throw new RequestFailure("请求体必须是 JSON 对象");
        if(root.ValueKind!=JsonValueKind.Object)throw new JsonException();
        ValidateNumbers(root);
        var v=JsonFields.Get(root,"model");
        string model=v.ValueKind==JsonValueKind.Undefined||v.ValueKind==JsonValueKind.String&&v.GetString()==""?"hy4-preview":JsonFields.Text(v);
        if(model.Length==0||Encoding.UTF8.GetByteCount(model)>256||model.EnumerateRunes().Any(r=>Rune.IsWhiteSpace(r)||Rune.GetUnicodeCategory(r) is UnicodeCategory.Control or UnicodeCategory.Format))
            throw new RequestFailure("model 必须是 1～256 字节的有效 UTF-8 字符串，不能包含空白或控制字符");
        var messages=JsonFields.Get(root,"messages");
        if(messages.ValueKind!=JsonValueKind.Array)throw new RequestFailure("messages 字段必须是数组");
        if(messages.GetArrayLength()>0)RequestValidation.Input(root);
        return model;
    }
    static void ValidateNumbers(JsonElement e)
    {
        if(e.ValueKind==JsonValueKind.Number&&(!e.TryGetDouble(out var n)||!double.IsFinite(n)))throw new JsonException();
        // Reject malformed Unicode at the boundary, including unknown fields,
        // instead of letting lazy DOM string decoding become an HTTP 500.
        if(e.ValueKind==JsonValueKind.String)try{_ = e.GetString();}catch(InvalidOperationException ex){throw new JsonException("Invalid Unicode",ex);}
        if(e.ValueKind==JsonValueKind.Array)foreach(var v in e.EnumerateArray())ValidateNumbers(v);
        if(e.ValueKind==JsonValueKind.Object)foreach(var p in e.EnumerateObject())
        {try{_ = p.Name;}catch(InvalidOperationException ex){throw new JsonException("Invalid Unicode",ex);}ValidateNumbers(p.Value);}
    }
    internal static int Estimate(JsonElement root)
    {
        var c=new TokenEstimation();var system=JsonFields.Get(root,"system");
        if(system.ValueKind==JsonValueKind.String&&system.GetString() is {Length:>0} s){c.Text(s);c.other++;}
        else
        {
            bool has=false;
            foreach(var b in JsonFields.Items(system))if(JsonFields.Text(b,"type")=="text"&&JsonFields.Text(b,"text") is {Length:>0} t)
            {if(has)c.other++;c.Text(t);has=true;}
            if(has)c.other++;
        }
        foreach(var m in JsonFields.Items(JsonFields.Get(root,"messages")))if(m.ValueKind==JsonValueKind.Object){c.Content(JsonFields.Get(m,"content"));c.other++;}
        var tools=JsonFields.Get(root,"tools");if(tools.ValueKind==JsonValueKind.Array&&tools.GetArrayLength()>0)c.Json(tools);
        return (int)Math.Min(int.MaxValue,c.cjk+(c.other+3)/4+c.images);
    }
    void Text(string s){foreach(var r in s.EnumerateRunes())if(r.Value>=0x2e80)cjk++;else other++;}
    void Quoted(string s)
    {
        other+=2;
        foreach(var r in s.EnumerateRunes())
            if(r.Value is '"' or '\\' or '\b' or '\f' or '\n' or '\r' or '\t')other+=2;
            else if(r.Value<32||r.Value is '<' or '>' or '&' or 0x2028 or 0x2029)other+=6;
            else if(r.Value>=0x2e80)cjk++;else other++;
    }
    void Json(JsonElement e)
    {
        switch(e.ValueKind)
        {
            case JsonValueKind.String:Quoted(e.GetString()!);break;
            case JsonValueKind.True:other+=4;break;
            case JsonValueKind.False:other+=5;break;
            case JsonValueKind.Number:other+=NumberLength(e.GetDouble());break;
            case JsonValueKind.Array:
                other+=2+Math.Max(0,e.GetArrayLength()-1);foreach(var v in e.EnumerateArray())Json(v);break;
            case JsonValueKind.Object:
                // Go map decoding retains only the final occurrence of a property.
                var fields=new Dictionary<string,JsonElement>(StringComparer.Ordinal);foreach(var p in e.EnumerateObject())fields[p.Name]=p.Value;
                other+=2+Math.Max(0,fields.Count-1);foreach(var p in fields){Quoted(p.Key);other++;Json(p.Value);}break;
            default:other+=4;break;
        }
    }
    static int NumberLength(double n)
    {
        string s=n.ToString("R",CultureInfo.InvariantCulture);int at=s.IndexOf('E');
        if(at<0)return s.Length;
        int exponent=int.Parse(s.AsSpan(at+1),CultureInfo.InvariantCulture);
        if(Math.Abs(n)<1e-6||Math.Abs(n)>=1e21)return at+2+Math.Abs(exponent).ToString(CultureInfo.InvariantCulture).Length;
        bool negative=s[0]=='-';string digits=s[..at].TrimStart('-').Replace(".","");int point=1+exponent;
        return (negative?1:0)+(point<=0?2-point+digits.Length:point>=digits.Length?point:digits.Length+1);
    }
    void Content(JsonElement e)
    {
        if(e.ValueKind==JsonValueKind.String){Text(e.GetString()!);return;}
        foreach(var b in JsonFields.Items(e))switch(JsonFields.Text(b,"type"))
        {
            case "text":Text(JsonFields.Text(b,"text"));break;
            case "thinking":Text(JsonFields.Text(b,"thinking"));break;
            case "tool_result":Content(JsonFields.Get(b,"content"));break;
            case "tool_use":Text(JsonFields.Text(b,"name"));Json(JsonFields.Get(b,"input"));break;
            case "image":images+=ImageTokenEstimate.Count(b);break;
        }
    }
}
