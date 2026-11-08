using System.Globalization;
using System.Text.Json;

namespace HttpMigration;

internal static class AgentToolProtocol
{
    internal const int Limit=1<<20;
    internal static byte[] Prepare(byte[] input,bool fetch)
    {
        try
        {
            using var doc=JsonDocument.Parse(input.Length==0?"null"u8.ToArray():input,new JsonDocumentOptions{MaxDepth=256});
            var root=doc.RootElement;if(root.ValueKind is not (JsonValueKind.Object or JsonValueKind.Null))throw new JsonException();
            string Field(string name)
            {
                var v=Get(root,name);if(v.ValueKind is JsonValueKind.Undefined or JsonValueKind.Null)return "";
                if(v.ValueKind!=JsonValueKind.String)throw new JsonException();return v.GetString()!;
            }
            if(fetch)
            {
                string url=Field("url").Trim();if(url.Length==0)throw new RequestFailure("url 不能为空");
                if(!Uri.TryCreate(url,UriKind.Absolute,out var parsed)||parsed.Scheme is not ("http" or "https")||parsed.Host.Length==0)
                    throw new RequestFailure("url 必须是 http/https 绝对地址");
                return GatewayJson.Encode(new Dictionary<string, object?> { ["url"] = url, ["format"] = "markdown", ["prompt"] = "Extract the main content of this page as clean Markdown. Keep headings, lists, code blocks and key facts. Remove navigation, ads and boilerplate.", ["timeout"] = 30, ["max_length"] = 100000 });
            }
            string query=Field("query").Trim();_ = Field("ori_question");
            var need=Get(root,"need_content");if(need.ValueKind is not (JsonValueKind.Undefined or JsonValueKind.Null or JsonValueKind.True or JsonValueKind.False))throw new JsonException();
            int count=5;var raw=Get(root,"count");
            if(raw.ValueKind is not (JsonValueKind.Undefined or JsonValueKind.Null))
            {if(raw.ValueKind!=JsonValueKind.Number||!raw.TryGetInt64(out long n))throw new JsonException();count=n<=0?5:(int)Math.Min(n,20);}
            int length=query.EnumerateRunes().Count();
            if(length==0)throw new RequestFailure("query 不能为空");
            if(length<2)throw new RequestFailure("query 至少需要 2 个字符");
            if(length>2000)throw new RequestFailure("query 过长（最多 2000 个字符）");
            return GatewayJson.Encode(new Dictionary<string, object?> { ["query"] = query, ["type"] = "text2text", ["max_results"] = count });
        }
        catch(Exception e) when(e is JsonException or InvalidOperationException){throw new RequestFailure("无效的 JSON 请求体",400,"invalid_json");}
    }
    // Go struct decoding matches field names case-insensitively, with last value winning.
    static JsonElement Get(JsonElement e,string name)
    {JsonElement result=default;if(e.ValueKind==JsonValueKind.Object)foreach(var p in e.EnumerateObject())if(p.Name.Equals(name,StringComparison.OrdinalIgnoreCase))result=p.Value;return result;}
    internal static object Project(JsonElement root,bool fetch)
    {
        if(fetch)
        {
            string content=JsonFields.Text(root,"content");
            if(content.Length==0)throw new RequestFailure("上游未返回可用的页面正文",502,"upstream_error");
            return new Dictionary<string, object?> { ["content"] = content };
        }
        var list=JsonFields.Get(root,"results");if(list.ValueKind!=JsonValueKind.Array)throw new RequestFailure("上游搜索响应缺少有效 results 数组",502,"upstream_error");
        var results=new List<object>();
        foreach(var item in list.EnumerateArray())
        {
            if(item.ValueKind!=JsonValueKind.Object)continue;
            string url=JsonFields.Text(item,"url").Trim();
            if(url.Length==0)
            {
                string site=JsonFields.Text(item,"site").Trim();
                if(site.Length>0&&!site.Contains(' ')&&!site.Contains('\t')&&site.Contains('.'))url="https://"+site.Trim('.');
            }
            if(url.Length>0)results.Add(new Dictionary<string, object?> { ["title"] = JsonFields.Text(item,"title"), ["url"] = url, ["snippet"] = JsonFields.Text(item,"snippet"), ["date"] = (string?)null });
        }
        return new Dictionary<string, object?> { ["results"] = results };
    }
    internal static string? Code(JsonElement root)
    {
        string? Read(JsonElement v)=>v.ValueKind switch
        {
            JsonValueKind.Undefined or JsonValueKind.Null=>null,
            JsonValueKind.String=>string.IsNullOrWhiteSpace(v.GetString())?null:long.TryParse(v.GetString()!.Trim(),NumberStyles.AllowLeadingSign,CultureInfo.InvariantCulture,out long n)?n.ToString(CultureInfo.InvariantCulture):"upstream_business_error",
            JsonValueKind.Number=>v.TryGetDouble(out double n)&&n>=0&&n<=999999999&&n==Math.Truncate(n)?((long)n).ToString(CultureInfo.InvariantCulture):"upstream_business_error",
            _=>"upstream_business_error"
        };
        string? outer=Read(JsonFields.Get(root,"code")),inner=Read(JsonFields.Get(JsonFields.Get(root,"error"),"code"));
        if(inner is "11128" or "11140")return inner;
        return outer??inner;
    }
}
