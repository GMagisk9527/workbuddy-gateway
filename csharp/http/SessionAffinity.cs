using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using AccountScheduling;
using KeySecurity;
using System.Runtime.InteropServices;
using System.Text.Json.Nodes;
using TypedMigration;
using OpenAiProtocols;
using System.Buffers;

namespace HttpMigration;

public enum AffinityMode { Hybrid, Session, Prefix }
public sealed record AffinityOptions(bool Enabled=false, AffinityMode Mode=AffinityMode.Hybrid,
    int Capacity=4096, int TtlSeconds=1800, int PrefixBytes=65536)
{
    internal void Validate()
    {
        if(!Enum.IsDefined(Mode)||Capacity is <1 or >65536||TtlSeconds is <1 or >86400||PrefixBytes is <2048 or >1048576)
            throw new ArgumentException("Invalid affinity settings");
    }
}

// Stores digests only. The preference is always revalidated by AccountPool;
// no credential, request body, raw session ID or account quota is retained here.
public sealed class SessionAffinity
{
    sealed record Entry(string Key,AccountId Account,long LastUsed,string? Identity=null,int CacheHits=0,long CacheAt=0);
    readonly object sync=new();
    readonly Dictionary<string,LinkedListNode<Entry>> entries=[];
    readonly LinkedList<Entry> lru=[];
    readonly TimeProvider time;
    volatile AffinityOptions options;
    public void Configure(AffinityOptions next)
    {next.Validate();lock(sync){if(next==options)return;options=next;entries.Clear();lru.Clear();}}
    public SessionAffinity(AffinityOptions options,TimeProvider? time=null)
    {options.Validate();this.options=options;this.time=time??TimeProvider.System;}
    public int Count{get{lock(sync)return entries.Count;}}
    public bool UsesSession=>options is{Enabled:true,Mode:not AffinityMode.Prefix};
    internal int PrefixBytes=>options.PrefixBytes;
    // Hash just the same bounded tools/first-two-message fragments while the
    // converted tree is alive. Canonical OpenAI fragments stop encoding at the
    // bound; other large fragments keep the original byte-scanner fallback.
    internal string? PrefixDigest(JsonObject root,bool canonical,string? session)
    {
        var current=options;
        if(!current.Enabled||current.Mode==AffinityMode.Session||current.Mode!=AffinityMode.Prefix&&session is not null)return null;
        if(root["messages"] is not JsonArray{Count:>0} messages)return null;
        using var hash=IncrementalHash.CreateHash(HashAlgorithmName.SHA256);hash.AppendData("affinity-prefix-v1\0"u8);
        bool Add(JsonNode? node,ReadOnlySpan<byte> label)
        {
            int remaining=current.PrefixBytes;
            bool Fits(JsonNode? value)
            {
                if(remaining<0)return false;
                switch(value)
                {
                    case null:remaining-=4;break;
                    case JsonObject obj:
                        remaining-=2;foreach(var p in obj){remaining-=p.Key.Length*6+4;if(!Fits(p.Value))return false;}break;
                    case JsonArray array:
                        remaining-=2;foreach(var item in array){remaining--;if(!Fits(item))return false;}break;
                    case JsonValue v when v.TryGetValue<JsonElement>(out var element):remaining-=checked(JsonMarshal.GetRawUtf8Value(element).Length*6);break;
                    case JsonValue v when v.TryGetValue<string>(out var text):remaining-=checked(text.Length*6+2);break;
                    default:remaining-=64;break;
                }
                return remaining>=0;
            }
            if(!Fits(node))
            {
                if(canonical)return false;
                byte[] rented=ArrayPool<byte>.Shared.Rent(current.PrefixBytes);
                try
                {
                    if(!OpenWire.TryEncodePrefix(node,rented.AsSpan(0,current.PrefixBytes),out int written))return false;
                    hash.AppendData(label);hash.AppendData(rented.AsSpan(0,written));return true;
                }
                finally{ArrayPool<byte>.Shared.Return(rented,clearArray:true);}
            }
            byte[] bytes=canonical?Encoding.UTF8.GetBytes(GoJson.Format(node)):OpenWire.Encode(node,current.PrefixBytes);
            hash.AppendData(label);hash.AppendData(bytes.AsSpan(0,Math.Min(bytes.Length,current.PrefixBytes)));return true;
        }
        if(root.TryGetPropertyValue("tools",out var tools)&&!Add(tools,"tools\0"u8))return null;
        for(int i=0;i<Math.Min(2,messages.Count);i++)if(!Add(messages[i],i==0?"msg0\0"u8:"msg1\0"u8))return null;
        return Convert.ToHexString(hash.GetHashAndReset());
    }
    internal string? KeyFromPrefix(string? caller,string model,string prefix,int prefixBytes)
    {
        if(!options.Enabled||options.Mode==AffinityMode.Session||string.IsNullOrEmpty(caller)||options.PrefixBytes!=prefixBytes)return null;
        return Digest("affinity-prefix-key-v1",caller,KeyRules.Model(model),prefix);
    }
    public AccountId? Lookup(string? key)
    {
        if(!options.Enabled||key is null)return null;
        lock(sync)
        {
            if(!entries.TryGetValue(key,out var node))return null;
            if(time.GetElapsedTime(node.Value.LastUsed)>TimeSpan.FromSeconds(options.TtlSeconds)){Remove(node);return null;}
            node.Value=node.Value with{LastUsed=time.GetTimestamp()};lru.Remove(node);lru.AddFirst(node);return node.Value.Account;
        }
    }
    public void Remember(string? key,AccountId account)
    {
        if(!options.Enabled||key is null)return;
        lock(sync)
        {
            Entry next=new(key,account,time.GetTimestamp());
            if(entries.TryGetValue(key,out var existing)){if(existing.Value.Account==account)next=existing.Value with{LastUsed=time.GetTimestamp()};Remove(existing);}
            entries[key]=lru.AddFirst(next);
            while(entries.Count>options.Capacity)Remove(lru.Last!);
        }
    }
    public void ObserveCache(string? key,AccountId account,string? identity,long? cachedTokens,bool complete)
    {
        if(!options.Enabled||key is null||identity is null||cachedTokens is null||!complete)return;
        lock(sync)
        {
            if(!entries.TryGetValue(key,out var node)||node.Value.Account!=account||time.GetElapsedTime(node.Value.LastUsed)>=TimeSpan.FromSeconds(options.TtlSeconds))return;
            var entry=node.Value;int hits=cachedTokens<32768?0:entry.Identity==identity&&time.GetElapsedTime(entry.CacheAt)<TimeSpan.FromMinutes(5)?Math.Min(2,entry.CacheHits+1):1;
            node.Value=entry with{Identity=identity,CacheHits=hits,CacheAt=time.GetTimestamp()};
        }
    }
    public TimeSpan? CacheGrace(string? key,AccountId? account,string? identity,int inputBytes,TimeSpan extra)
    {
        if(!options.Enabled||key is null||account is null||identity is null||inputBytes<256<<10)return null;
        lock(sync)return entries.TryGetValue(key,out var node)&&node.Value.Account==account&&node.Value.Identity==identity&&node.Value.CacheHits>=2&&time.GetElapsedTime(node.Value.CacheAt)<TimeSpan.FromMinutes(5)&&time.GetElapsedTime(node.Value.LastUsed)<TimeSpan.FromSeconds(options.TtlSeconds)?extra:null;
    }
    internal static long? CachedTokens(string? usage)
    {
        if(usage is null)return null;
        try{using var doc=JsonDocument.Parse(usage);var root=doc.RootElement;
            foreach(string key in new[]{"prompt_tokens_details","input_tokens_details"})if(root.TryGetProperty(key,out var detail)&&detail.ValueKind==JsonValueKind.Object&&detail.TryGetProperty("cached_tokens",out var value)&&value.TryGetInt64(out long n)&&n>=0)return n;
            return root.TryGetProperty("cache_read_input_tokens",out var cached)&&cached.TryGetInt64(out long count)&&count>=0?count:null;
        }catch(JsonException){return null;}catch(InvalidOperationException){return null;}
    }
    // Conditional invalidation prevents an older failed request from deleting a
    // binding that a concurrent successful request has already moved elsewhere.
    public void Forget(string? key,AccountId account)
    {
        if(key is null)return;
        lock(sync)if(entries.TryGetValue(key,out var node)&&node.Value.Account==account)Remove(node);
    }
    void Remove(LinkedListNode<Entry> node){entries.Remove(node.Value.Key);lru.Remove(node);}

    internal static string? SessionDigest(IHeaderDictionary headers,ReadOnlySpan<byte> original)
    {
        foreach(string name in new[]{"X-Conversation-ID","thread-id","X-Session-ID","session-id","session_id"})
            if(headers.TryGetValue(name,out var values)&&values.Count==1&&ValidSeed(values[0]) is {} seed)return Digest("session",seed);
        if(headers.TryGetValue("x-claude-code-session-id",out var session)&&session.Count==1&&ValidSeed(session[0]) is {} claude)
        {
            var agent=headers["x-claude-code-agent-id"];
            return agent.Count==1&&ValidSeed(agent[0]) is {} child?Digest("claude-agent",claude,child):Digest("session",claude);
        }
        // Scan the validated envelope without allocating a DOM or retaining the
        // body. Extract only bounded scalar identity fields before conversion.
        var top=new Dictionary<string,string>();var metadata=new Dictionary<string,string>();
        var reader=new Utf8JsonReader(original,new JsonReaderOptions{MaxDepth=256});
        if(!reader.Read()||reader.TokenType!=JsonTokenType.StartObject)return null;
        while(reader.Read()&&reader.TokenType!=JsonTokenType.EndObject)
        {
            string? name=reader.TokenType==JsonTokenType.PropertyName?reader.GetString():null;
            if(!reader.Read())break;
            if(name=="metadata"&&reader.TokenType==JsonTokenType.StartObject)
            {
                metadata.Clear();
                while(reader.Read()&&reader.TokenType!=JsonTokenType.EndObject)
                {
                    string field=reader.GetString()!;reader.Read();ReadSeed(ref reader,field,metadata);reader.Skip();
                }
            }
            else {if(name is not null)ReadSeed(ref reader,name,top);reader.Skip();}
        }
        foreach(var source in new[]{top,metadata})
            foreach(string name in new[]{"conversation_id","conversationId","session_id","sessionId"})
                if(source.TryGetValue(name,out var value))return Digest("session",value);
        return null;
    }
    internal static string? SessionDigest(IHeaderDictionary headers,JsonElement original)
    {
        string? fromHeaders=SessionDigest(headers,"{}"u8);if(fromHeaders is not null)return fromHeaders;
        var top=new Dictionary<string,string>();var metadata=new Dictionary<string,string>();
        foreach(var property in original.EnumerateObject())
        {
            if(property.Name=="metadata"&&property.Value.ValueKind==JsonValueKind.Object)
            {metadata.Clear();foreach(var field in property.Value.EnumerateObject())Seed(field.Name,field.Value,metadata);}
            else Seed(property.Name,property.Value,top);
        }
        foreach(var source in new[]{top,metadata})
            foreach(string name in new[]{"conversation_id","conversationId","session_id","sessionId"})
                if(source.TryGetValue(name,out var value))return Digest("session",value);
        return null;
        static void Seed(string name,JsonElement field,Dictionary<string,string> destination)
        {
            if(name is not ("conversation_id" or "conversationId" or "session_id" or "sessionId"))return;
            destination.Remove(name);
            if(field.ValueKind==JsonValueKind.String&&JsonMarshal.GetRawUtf8Value(field).Length<=3074&&ValidSeed(field.GetString()) is{} value)destination[name]=value;
        }
    }
    static void ReadSeed(ref Utf8JsonReader reader,string name,Dictionary<string,string> result)
    {
        if(name is not ("conversation_id" or "conversationId" or "session_id" or "sessionId"))return;
        result.Remove(name);
        if(reader.TokenType==JsonTokenType.String&&reader.ValueSpan.Length<=3072&&ValidSeed(reader.GetString()) is {} value)result[name]=value;
    }
    static string? ValidSeed(string? value)
    {
        value=value?.Trim();
        if(string.IsNullOrEmpty(value)||Encoding.UTF8.GetByteCount(value)>512||value.Any(c=>c<32||c==127))return null;
        return value;
    }
    internal string? Key(string? caller,string model,string? sessionDigest,ReadOnlySpan<byte> prepared)
    {
        if(!options.Enabled||string.IsNullOrEmpty(caller))return null;
        model=KeyRules.Model(model);
        if(options.Mode!=AffinityMode.Prefix&&sessionDigest is not null)return Digest("affinity-session-v1",caller,model,sessionDigest);
        if(options.Mode==AffinityMode.Session)return null;
        using var hash=IncrementalHash.CreateHash(HashAlgorithmName.SHA256);
        hash.AppendData("affinity-prefix-v1\0"u8);
        // Selection uses final prepared bytes. Only the first two messages and
        // tools affect the digest; appended history and generation params do not.
        // Raw slices avoid copying/serializing multi-megabyte user content.
        var reader=new Utf8JsonReader(prepared,new JsonReaderOptions{MaxDepth=256});
        (int Start,int Length)? tools=null;
        var messages=new List<(int Start,int Length)>(2);
        reader.Read();
        while(reader.Read()&&reader.TokenType!=JsonTokenType.EndObject)
        {
            bool isTools=reader.ValueTextEquals("tools"u8),isMessages=reader.ValueTextEquals("messages"u8);reader.Read();
            int start=checked((int)reader.TokenStartIndex);
            if(isMessages&&reader.TokenType==JsonTokenType.StartArray)
            {
                messages.Clear();
                while(reader.Read()&&reader.TokenType!=JsonTokenType.EndArray)
                {
                    int first=checked((int)reader.TokenStartIndex);reader.Skip();
                    if(messages.Count<2)messages.Add((first,Math.Min(options.PrefixBytes,checked((int)reader.BytesConsumed)-first)));
                }
            }
            else {reader.Skip();if(isTools)tools=(start,Math.Min(options.PrefixBytes,checked((int)reader.BytesConsumed)-start));}
        }
        if(messages.Count==0)return null;
        if(tools is {} t){hash.AppendData("tools\0"u8);hash.AppendData(prepared.Slice(t.Start,t.Length));}
        for(int i=0;i<messages.Count;i++){hash.AppendData(i==0?"msg0\0"u8:"msg1\0"u8);hash.AppendData(prepared.Slice(messages[i].Start,messages[i].Length));}
        return Digest("affinity-prefix-key-v1",caller,model,Convert.ToHexString(hash.GetHashAndReset()));
    }
    static string Digest(params string[] values)
    {
        using var hash=IncrementalHash.CreateHash(HashAlgorithmName.SHA256);
        Span<byte> length=stackalloc byte[4];
        foreach(var value in values){var bytes=Encoding.UTF8.GetBytes(value);System.Buffers.Binary.BinaryPrimitives.WriteInt32BigEndian(length,bytes.Length);hash.AppendData(length);hash.AppendData(bytes);}
        return Convert.ToHexString(hash.GetHashAndReset());
    }
}
