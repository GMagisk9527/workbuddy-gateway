using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;
using System.Text.Json.Nodes;

namespace HttpMigration;

public sealed record SystemPromptOptions(string Fallback="You are a helpful assistant.",string Force="",IdentityOptions? Identity=null)
{
    internal void Validate()
    {
        var utf8=new UTF8Encoding(false,true);
        if(Fallback is null||Force is null||utf8.GetByteCount(Fallback)>65536||utf8.GetByteCount(Force)>65536)throw new ArgumentException("System prompt settings exceed bounds");
    }
    public override string ToString()=>"SystemPromptOptions (redacted)";
}

// Normalises message structure and configured prompt text. User text and function
// argument strings are never rewritten.
public static class OutboundPreparation
{
    // Prove the common case is unchanged while its parsed request is still alive.
    // Any uncertain role, prompt rule or tool ordering uses the full normalizer.
    internal static bool IsCompatible(JsonObject root,SystemPromptOptions settings)
    {
        settings.Validate();
        static string Text(JsonNode? n)=>n is JsonValue v&&v.TryGetValue<string>(out var s)?s:"";
        if(!string.IsNullOrWhiteSpace(settings.Force)||root.ContainsKey("reasoning_effort")||root.ContainsKey("reasoning_summary")||Text(root["model"]).Trim().StartsWith("deepseek",StringComparison.OrdinalIgnoreCase))return false;
        if(root["messages"] is not JsonArray{Count:>0} messages||messages[0] is not JsonObject first||Text(first["role"])!="system")return false;
        var identity=IdentitySettings.Build(settings.Identity);var ids=new HashSet<string>(StringComparer.Ordinal);var pending=new Queue<string>();
        foreach(var item in messages)
        {
            if(item is not JsonObject message)return false;
            string role=Text(message["role"]);
            if(role is not ("system" or "assistant" or "user" or "tool" or "function"))return false;
            if(identity.Enabled&&role is "system" or "assistant")
            {
                if(message["content"] is JsonValue text&&identity.MightMatch(Text(text),true))return false;
                if(message["content"] is JsonArray parts)
                    foreach(var part in parts.OfType<JsonObject>())
                        if(Text(part["type"])=="text"&&identity.MightMatch(Text(part["text"]),true))return false;
            }
            if(role=="tool")
            {
                if(!pending.TryDequeue(out string? id)||Text(message["tool_call_id"])!=id)return false;
                continue;
            }
            if(pending.Count>0)return false;
            if(role=="assistant"&&message["tool_calls"] is JsonArray{Count:>0} calls)
            {
                foreach(var call in calls)
                {
                    string id=call is JsonObject c?Text(c["id"]):"";
                    if(id.Length==0||id!=id.Trim()||!ids.Add(id))return false;
                    pending.Enqueue(id);
                }
            }
        }
        return pending.Count==0;
    }
    // Lazily materialize just the objects/arrays being edited. Large content and
    // tool schemas remain borrowed JsonElements for the lifetime of this call.
    internal sealed class V
    {
        readonly JsonElement raw;
        readonly JsonValueKind kind;
        Dictionary<string,V>? obj;List<V>? array;string? text;
        V(JsonElement e){raw=e;kind=e.ValueKind;}
        V(JsonValueKind k){kind=k;}
        internal static V From(JsonElement e)=>new(e);
        internal static V Text(string s)=>new(JsonValueKind.String){text=s};
        internal static V Object(params (string Key,V Value)[] pairs)=>new(JsonValueKind.Object){obj=pairs.ToDictionary(p=>p.Key,p=>p.Value,StringComparer.Ordinal)};
        internal static V Array(IEnumerable<V> values)=>new(JsonValueKind.Array){array=values.ToList()};
        internal bool Is(JsonValueKind k)=>kind==k;
        internal string Str()=>kind==JsonValueKind.String?text??raw.GetString()!:"";
        internal Dictionary<string,V> Obj=>obj??=raw.EnumerateObject().GroupBy(p=>p.Name).ToDictionary(g=>g.Key,g=>From(g.Last().Value),StringComparer.Ordinal);
        internal List<V> Arr=>array??=raw.EnumerateArray().Select(From).ToList();
        internal V? Get(string key)=>kind==JsonValueKind.Object?Obj.GetValueOrDefault(key):null;
        internal void Set(string key,V value)=>Obj[key]=value;
        internal bool Remove(string key)=>Obj.Remove(key);
        internal string S(string key)=>Get(key)?.Str()??"";
        internal string Role=>S("role").Trim().ToLowerInvariant();
        internal void Write(Utf8JsonWriter w)
        {
            if(obj is not null){w.WriteStartObject();foreach(var p in obj){w.WritePropertyName(p.Key);p.Value.Write(w);}w.WriteEndObject();}
            else if(array is not null){w.WriteStartArray();foreach(var v in array)v.Write(w);w.WriteEndArray();}
            else if(text is not null)w.WriteStringValue(text);
            else raw.WriteTo(w);
        }
    }
    public static byte[] Prepare(byte[] input,SystemPromptOptions settings,Action<int> reserve,int limit)
    {
        settings.Validate();if(input.Length>limit)throw new RequestFailure("出站请求超过大小限制",413,"request_too_large");
        using var document=JsonDocument.Parse(input,new JsonDocumentOptions{MaxDepth=256});
        if(document.RootElement.ValueKind!=JsonValueKind.Object)throw new RequestFailure("请求体必须是 JSON 对象");
        var root=V.From(document.RootElement);bool changed=false;
        string effort=root.S("reasoning_effort");
        if(effort is "" or "off" or "none") {changed|=root.Remove("reasoning_effort");changed|=root.Remove("reasoning_summary");}
        else if(string.IsNullOrWhiteSpace(root.S("reasoning_summary"))){root.Set("reasoning_summary",V.Text("auto"));changed=true;}
        var messages=root.Get("messages") is { } m&&m.Is(JsonValueKind.Array)?m.Arr:[];
        foreach(var msg in messages.Where(v=>v.Role=="developer")){msg.Set("role",V.Text("system"));changed=true;}
        var identity=IdentitySettings.Build(settings.Identity);
        // Pass A: identity replacement on the client blocks before any prompt is injected.
        changed|=IdentityRewrite.Apply(messages,identity);
        bool injected=false;
        if(messages.Count==0||messages[0].Role!="system")
        {
            int index=messages.FindIndex(v=>v.Role=="system");
            if(index>=0){var system=messages[index];messages.RemoveAt(index);messages.Insert(0,system);}
            else{messages.Insert(0,V.Object(("role",V.Text("system")),("content",V.Text(string.IsNullOrWhiteSpace(settings.Fallback)?"You are a helpful assistant.":settings.Fallback))));injected=true;}
            changed=true;
        }
        // Go applies its injected-prompt pass with wildcards still enabled.
        if(injected&&messages.Count>0)changed|=IdentityRewrite.Apply([messages[0]],identity);
        var first=messages[0];string force=settings.Force;
        if(!string.IsNullOrWhiteSpace(force))
        {
            var content=first.Get("content");
            if(content is null||content.Is(JsonValueKind.Null)){first.Set("content",V.Text(force));changed=true;injected=true;}
            else if(content.Is(JsonValueKind.String))
            {
                string old=content.Str();
                // Compare both raw and identity-rewritten force so a rewritten client prompt is not duplicated.
                string forceWire=IdentityRewrite.Transform(force,identity);
                bool has=old==force||old.EndsWith("\n\n"+force,StringComparison.Ordinal)
                    ||(forceWire.Length>0&&(old==forceWire||old.EndsWith("\n\n"+forceWire,StringComparison.Ordinal)));
                if(!has){first.Set("content",V.Text(old.Length==0?force:old+"\n\n"+force));changed=true;injected=true;}
            }
            else if(content.Is(JsonValueKind.Array))
            {
                var parts=content.Arr;var last=parts.LastOrDefault();
                string forceWire=IdentityRewrite.Transform(force,identity);
                if(last is null||last.S("type")!="text"||last.Get("text") is not { } lastText||!lastText.Is(JsonValueKind.String)||(lastText.Str()!=force&&lastText.Str()!=forceWire))
                {parts.Add(V.Object(("type",V.Text("text")),("text",V.Text(force))));changed=true;injected=true;}
            }
        }
        // Pass B: after injection, Go normalises every message again with wildcards disabled.
        changed|=IdentityRewrite.Apply(messages,identity,false);
        messages=RepairTools(messages,ref changed);
        root.Set("messages",V.Array(messages));
        if(root.S("model").Trim().StartsWith("deepseek",StringComparison.OrdinalIgnoreCase))
        {
            bool Enabled(string s)=>s.Trim().ToLowerInvariant() is not ("" or "none" or "off" or "disabled");
            string thinking=root.Get("thinking")?.S("type").Trim().ToLowerInvariant()??"";
            bool enabled=thinking=="enabled"||thinking!="disabled"&&(Enabled(root.S("reasoning_effort"))||Enabled(root.S("reasoningEffort"))||Enabled(root.S("reasoning_summary"))||Enabled(root.Get("reasoning")?.S("effort")??""));
            var assistants=messages.Where(v=>v.S("role")=="assistant").ToArray();
            if(enabled||assistants.Any(v=>v.Get("reasoning_content") is not null||v.S("reasoning")!=""))
                foreach(var msg in assistants)
                {
                    var rc=msg.Get("reasoning_content");string value;
                    if(rc is not null&&rc.Is(JsonValueKind.String))value=rc.Str();
                    else {value=msg.S("reasoning");msg.Set("reasoning_content",V.Text(value));changed=true;}
                    if(msg.S("reasoning")==""){msg.Set("reasoning",V.Text(value.Length==0?" ":value));changed=true;}
                }
        }
        if(!changed)return input;
        using var count=new CountStream(limit);using var writer=new Utf8JsonWriter(count,new(){Encoder=JavaScriptEncoder.UnsafeRelaxedJsonEscaping,MaxDepth=256});
        root.Write(writer);writer.Flush();int size=checked((int)count.Length);
        reserve(size);byte[] result=new byte[size];using var output=new MemoryStream(result,true);
        writer.Reset(output);root.Write(writer);writer.Flush();writer.Reset(Stream.Null);return result;
    }
    static List<V> RepairTools(List<V> messages,ref bool changed)
    {
        List<V>? Calls(V v)=>v.Role=="assistant"&&v.Get("tool_calls") is { } c&&c.Is(JsonValueKind.Array)&&c.Arr.Count>0?c.Arr:null;
        string Id(V call)=>call.S("id").Trim();
        string Output(V v)=>v.S("tool_call_id").Trim();
        bool Payload(V v)
        {var c=v.Get("content");return c is not null&&!c.Is(JsonValueKind.Null)&&(c.Is(JsonValueKind.String)?!string.IsNullOrWhiteSpace(c.Str()):!c.Is(JsonValueKind.Array)||c.Arr.Count>0);}
        var calls=new Dictionary<string,(int Message,int Entry)>(StringComparer.Ordinal);var results=new Dictionary<string,int>(StringComparer.Ordinal);
        for(int i=0;i<messages.Count;i++)if(Calls(messages[i]) is { } list)for(int j=0;j<list.Count;j++)if(Id(list[j]) is {Length:>0} id)calls.TryAdd(id,(i,j));
        for(int i=0;i<messages.Count;i++)if(messages[i].Role=="tool"&&calls.TryGetValue(Output(messages[i]),out var call)&&i>call.Message)results.TryAdd(Output(messages[i]),i);
        var filtered=new List<V>();
        for(int i=0;i<messages.Count;i++)
        {
            var msg=messages[i];
            if(Calls(msg) is { } list)
            {
                var kept=list.Where((v,j)=>Id(v) is {Length:>0} id&&results.ContainsKey(id)&&calls[id]==(i,j)).ToArray();
                if(kept.Length!=list.Count)changed=true;
                if(kept.Length>0){msg.Set("tool_calls",V.Array(kept));filtered.Add(msg);}
                else {msg.Remove("tool_calls");if(Payload(msg))filtered.Add(msg);}
            }
            else if(msg.Role=="tool") {if(results.TryGetValue(Output(msg),out int index)&&index==i)filtered.Add(msg);else changed=true;}
            else filtered.Add(msg);
        }
        var repaired=new List<V>();
        for(int i=0;i<filtered.Count;)
        {
            var first=filtered[i];var initial=Calls(first);if(initial is null){repaired.Add(first);i++;continue;}
            var batch=initial.ToList();var deferred=new List<V>();int merged=0,j=i+1;
            for(;j<filtered.Count;j++)
            {
                var next=filtered[j];var more=Calls(next);
                if(more is not null)
                {if(Payload(next)||next.Obj.Keys.Any(k=>k is not ("role" or "content" or "tool_calls")))break;batch.AddRange(more);merged++;continue;}
                if(next.Role=="tool")break;deferred.Add(next);
            }
            if(batch.Count<2){repaired.Add(first);i++;continue;}
            var ids=batch.Select(Id).Where(s=>s!="").ToHashSet(StringComparer.Ordinal);var found=new HashSet<string>(StringComparer.Ordinal);var outputs=new List<V>();int k=j;
            for(;k<filtered.Count&&found.Count<ids.Count;k++)
            {
                var next=filtered[k];if(Calls(next) is not null)break;
                if(next.Role=="tool"&&ids.Contains(Output(next))&&found.Add(Output(next)))outputs.Add(next);else deferred.Add(next);
            }
            if(found.Count!=ids.Count){repaired.Add(first);i++;continue;}
            first.Set("tool_calls",V.Array(batch));repaired.Add(first);repaired.AddRange(outputs);repaired.AddRange(deferred);
            changed|=merged>0||deferred.Count>0;i=k;
        }
        return repaired;
    }
    sealed class CountStream(int limit):Stream
    {
        long count;
        public override void Write(ReadOnlySpan<byte> b){if(b.Length>limit-count)throw new RequestFailure("转换后的请求超过大小限制",413,"request_too_large");count+=b.Length;}
        public override void Write(byte[] b,int o,int n)=>Write(b.AsSpan(o,n));
        public override long Length=>count;public override bool CanWrite=>true;public override bool CanRead=>false;public override bool CanSeek=>false;
        public override long Position{get=>count;set=>throw new NotSupportedException();}
        public override void Flush(){ }public override int Read(byte[] b,int o,int n)=>throw new NotSupportedException();public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long n)=>throw new NotSupportedException();
    }
}
