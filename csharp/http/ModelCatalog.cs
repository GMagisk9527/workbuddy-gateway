using System.Collections.Frozen;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using AccountScheduling;
using KeySecurity;

namespace HttpMigration;

public sealed partial class ModelCatalog
{
    sealed class Row
    {
        public string Id { get; set; }="";
        public int MaxInputTokens { get; set; }
        public int MaxOutputTokens { get; set; }
        public int MaxAllowedSize { get; set; }
        public int[]? ContextLengths { get; set; }
        public int ContextDefaultLength { get; set; }
        public bool FromLive { get; set; }
        public bool HasMultiplier { get; set; }
        public double Multiplier { get; set; }
        public bool PromoExpired { get; set; }
        public long PriceUpdatedAt { get; set; }
    }
    sealed class LiveRow
    {
        public string? Id { get; set; }
        public int MaxInputTokens { get; set; }
        public int MaxOutputTokens { get; set; }
        public int MaxAllowedSize { get; set; }
        public LiveWindow? ContextWindow { get; set; }
    }
    sealed class LiveWindow
    {
        public int[]? SupportedLengths { get; set; }
        public int DefaultLength { get; set; }
    }
    sealed record Capability(int Ceiling,int Output,int Default,int[] Options)
    {
        internal JsonObject Json()
        {
            var o=new JsonObject();
            if(Ceiling>0)o["context_length"]=Ceiling;
            if(Output>0)o["max_output_tokens"]=Output;
            if(Default>0)o["default_context_length"]=Default;
            if(Options.Length>=2)o["context_length_options"]=new JsonArray(Options.Select(x=>(JsonNode)JsonValue.Create(x)!).ToArray());
            return o;
        }
        internal bool Known=>Ceiling>0||Output>0||Default>0||Options.Length>0;
    }
    sealed record State(Row[] Cn,Row[] Intl,bool CnLive=false,bool IntlLive=false);
    State state=new([],[]);
    readonly object publish=new();
    public bool IsEmpty { get {var s=Volatile.Read(ref state);return s.Cn.Length==0&&s.Intl.Length==0;} }
    public byte[] Export()
    {
        var s=Volatile.Read(ref state);var sites=new Dictionary<string,Row[]>();
        if(s.Cn.Length>0)sites["cn"]=s.Cn;
        if(s.Intl.Length>0)sites["intl"]=s.Intl;
        return JsonSerializer.SerializeToUtf8Bytes(new CatalogDocument(5, sites), CatalogJson.Default.CatalogDocument);
    }
    public string Source
    {
        get { var s=Volatile.Read(ref state);return !s.CnLive&&!s.IntlLive?"cache":s.Cn.Length>0&&!s.CnLive||s.Intl.Length>0&&!s.IntlLive?"mixed":"live"; }
    }
    readonly FrozenSet<string>? allow;
    readonly FrozenSet<string> block;
    sealed record ConsolePolicy(FrozenSet<string> Block,FrozenSet<string>? Allow);
    ConsolePolicy consolePolicy=new(FrozenSet<string>.Empty,null);
    internal void SetConsolePolicy(FrozenSet<string> blocked,FrozenSet<string>? allowed)=>Volatile.Write(ref consolePolicy,new(blocked,allowed));
    sealed record CatalogDocument(int Schema, Dictionary<string, Row[]> Catalogs);

    [System.Text.Json.Serialization.JsonSourceGenerationOptions(JsonSerializerDefaults.Web)]
    [System.Text.Json.Serialization.JsonSerializable(typeof(CatalogDocument))]
    [System.Text.Json.Serialization.JsonSerializable(typeof(Row[]))]
    [System.Text.Json.Serialization.JsonSerializable(typeof(LiveRow))]
    partial class CatalogJson : System.Text.Json.Serialization.JsonSerializerContext { }
    public ModelCatalog(IEnumerable<string>? allow=null,IEnumerable<string>? block=null)
    {
        var allowed=(allow??[]).Select(KeyRules.Model).Where(m=>m.Length>0).ToFrozenSet();
        this.allow=allowed.Count>0?allowed:null;
        this.block=(block??[]).Select(KeyRules.Model).Where(m=>m.Length>0).ToFrozenSet();
    }
    public bool Allows(string model)
    {model=KeyRules.Model(model);var policy=Volatile.Read(ref consolePolicy);return (policy.Allow is{Count:>0}?policy.Allow.Contains(model):!policy.Block.Contains(model))&&!block.Contains(model)&&(allow is null||allow.Contains(model));}
    public void LoadFile(string path)
    {
        using var file=new FileStream(path,FileMode.Open,FileAccess.Read,FileShare.Read);
        if(file.Length is <1 or >8388608)throw new ArgumentException("Catalog file exceeds bounds");
        byte[] bytes=new byte[(int)file.Length];file.ReadExactly(bytes);Load(bytes);
    }
    public void Load(ReadOnlyMemory<byte> bytes)
    {
        if(bytes.Length>8<<20)throw new ArgumentException("Catalog document exceeds bounds");
        using var doc=JsonDocument.Parse(bytes,new JsonDocumentOptions{MaxDepth=32});
        var root=doc.RootElement;
        if(root.GetProperty("schema").GetInt32()!=5 || root.GetProperty("catalogs").ValueKind!=JsonValueKind.Object)throw new ArgumentException("Unsupported catalog snapshot");
        Row[] Site(string site)
        {
            if(!root.GetProperty("catalogs").TryGetProperty(site,out var list))return [];
            if(list.ValueKind!=JsonValueKind.Array || list.GetArrayLength() is <1 or >500)throw new ArgumentException("Catalog count exceeds bounds");
            var rows=JsonSerializer.Deserialize(list, CatalogJson.Default.RowArray)!;
            foreach(var r in rows)Normalize(r);
            return rows;
        }
        var next=new State(Site("cn"),Site("intl"));
        lock(publish){Volatile.Write(ref state,next);PublishPricing();} // Invalid reloads never publish a half catalog.
    }
    static void Normalize(Row r)
    {
        if(r is not null&&(!double.IsFinite(r.Multiplier)||r.Multiplier<0))throw new ArgumentException("Invalid model multiplier");
        if(r is null || r.Id is null || r.Id!=KeyRules.Model(r.Id) || r.Id.Length==0 || Encoding.UTF8.GetByteCount(r.Id)>256 || r.Id.EnumerateRunes().Any(x=>Rune.IsWhiteSpace(x)||Rune.GetUnicodeCategory(x) is System.Globalization.UnicodeCategory.Control or System.Globalization.UnicodeCategory.Format) || r.MaxInputTokens<0 || r.MaxAllowedSize<0 || r.MaxOutputTokens<0 || r.ContextDefaultLength<0 || r.ContextLengths?.Length>128)throw new ArgumentException("Invalid catalog entry");
        int ceiling=r.MaxInputTokens>0?r.MaxInputTokens:r.MaxAllowedSize;
        int[] options=(r.ContextLengths??[]).Where(n=>n>0&&(ceiling==0||n<=ceiling)).Distinct().Order().ToArray();
        r.ContextLengths=options.Length>=2?options:[];
        if(ceiling>0)r.ContextDefaultLength=Math.Min(r.ContextDefaultLength,ceiling);
    }
    public void LoadLive(AccountSite site,ReadOnlyMemory<byte> bytes)
    {
        if(!Enum.IsDefined(site)||bytes.Length>8<<20)throw new ArgumentException("Invalid live catalog bounds");
        using var doc=JsonDocument.Parse(bytes,new JsonDocumentOptions{MaxDepth=32});var root=doc.RootElement;
        if(root.ValueKind!=JsonValueKind.Object || !root.TryGetProperty("code",out var code) || !code.TryGetInt32(out int n) || n!=0 || !root.TryGetProperty("data",out var data) || data.ValueKind!=JsonValueKind.Object || !data.TryGetProperty("models",out var models) || models.ValueKind!=JsonValueKind.Array || models.GetArrayLength() is <1 or >500)throw new ArgumentException("Invalid live catalog envelope");
        var rows=new List<Row>();var seen=new HashSet<string>(StringComparer.Ordinal);
        foreach(var model in models.EnumerateArray())
        {
            if(model.ValueKind!=JsonValueKind.Object)throw new ArgumentException("Invalid live catalog entry");
            if(model.TryGetProperty("contextWindow",out var window)&&window.ValueKind==JsonValueKind.Object && window.TryGetProperty("supportedLengths",out var lengths)&&lengths.ValueKind==JsonValueKind.Array && lengths.GetArrayLength()>128)throw new ArgumentException("Too many context options");
            var live=JsonSerializer.Deserialize(model, CatalogJson.Default.LiveRow)??throw new ArgumentException("Invalid live catalog entry");
            // Live contextWindow is authoritative; cache-shaped extras are ignored.
            var row=new Row{Id=KeyRules.Model(live.Id??""),MaxInputTokens=live.MaxInputTokens,MaxOutputTokens=live.MaxOutputTokens,MaxAllowedSize=live.MaxAllowedSize,ContextLengths=live.ContextWindow?.SupportedLengths,ContextDefaultLength=live.ContextWindow?.DefaultLength??0};
            row.FromLive=true;row.PriceUpdatedAt=DateTimeOffset.UtcNow.ToUnixTimeSeconds();ReadPrice(row,model,data,DateTimeOffset.UtcNow);
            Normalize(row);if(seen.Add(row.Id))rows.Add(row);
        }
        lock(publish)
        {
            var s=Volatile.Read(ref state);
            Volatile.Write(ref state,site==AccountSite.Cn?s with{Cn=rows.ToArray(),CnLive=true}:s with{Intl=rows.ToArray(),IntlLive=true});
            PublishPricing();
        }
    }
    static Capability Cap(Row r)=>new(r.MaxInputTokens>0?r.MaxInputTokens:r.MaxAllowedSize,r.MaxOutputTokens,r.ContextDefaultLength,r.ContextLengths??[]);
    public JsonObject List(Func<string,bool>? allowed=null,bool includeDisabled=false)
    {
        var s=Volatile.Read(ref state);var items=new JsonArray();
        foreach(string id in s.Cn.Concat(s.Intl).Select(r=>r.Id).Distinct())
        {
            if(!includeDisabled&&!Allows(id)||allowed is not null&&!allowed(id))continue;
            var item=new JsonObject{["id"]=id,["object"]="model",["owned_by"]="workbuddy",["permission"]=new JsonArray()};
            var sites=new Dictionary<string,Capability>();
            foreach(var (name,rows) in new[]{("cn",s.Cn),("intl",s.Intl)})
            { var row=rows.FirstOrDefault(r=>r.Id==id);if(row is not null && Cap(row).Known)sites[name]=Cap(row); }
            if(sites.Count>0)
            {
                int Scalar(Func<Capability,int> pick) {var values=sites.Values.Select(pick).Distinct().ToArray();return values.Length==1&&values[0]>0?values[0]:0;}
                var first=sites.Values.First();int[] options=first.Options.Length>=2&&sites.Values.All(c=>c.Options.SequenceEqual(first.Options))?first.Options:[];
                var cap=new Capability(Scalar(c=>c.Ceiling),Scalar(c=>c.Output),Scalar(c=>c.Default),options);
                foreach(var p in cap.Json())item[p.Key]=p.Value?.DeepClone();
                if(sites.Count>1&&sites.Values.Any(c=>c.Ceiling!=cap.Ceiling||c.Output!=cap.Output||c.Default!=cap.Default||!c.Options.SequenceEqual(cap.Options)))
                {var bySite=new JsonObject();foreach(var p in sites)bySite[p.Key]=p.Value.Json();item["capabilities_by_site"]=bySite;}
            }
            items.Add((JsonNode)item);
        }
        return new(){["object"]="list",["data"]=items};
    }
    public int DefaultOutput(string model,AccountSite? site=null)
    {
        var s=Volatile.Read(ref state); model=KeyRules.Model(model);
        var rows=site is null?s.Cn.Concat(s.Intl):site==AccountSite.Cn?s.Cn:s.Intl;
        return rows.Where(r=>r.Id==model).Select(r=>r.MaxOutputTokens).DefaultIfEmpty().Max();
    }
    // Scan structural tokens only; never decode/copy large content strings.
    internal byte[] PrepareOutput(byte[] original,string model,AccountSite? site,Action<int> reserve,int limit)
    {
        int budget=DefaultOutput(model,site);if(budget<=0)return original;
        var reader=new Utf8JsonReader(original,new JsonReaderOptions{MaxDepth=512});int nullStart=-1,nullEnd=0,end=-1,properties=0;
        while(reader.Read())
        {
            if(reader.TokenType==JsonTokenType.PropertyName && reader.CurrentDepth==1)
            {
                properties++;bool token=reader.ValueTextEquals("max_tokens"),completion=reader.ValueTextEquals("max_completion_tokens");
                reader.Read();
                if((token||completion)&&reader.TokenType!=JsonTokenType.Null)return original;
                if(token){nullStart=(int)reader.TokenStartIndex;nullEnd=(int)reader.BytesConsumed;}
                reader.Skip();
            }
            if(reader.TokenType==JsonTokenType.EndObject&&reader.CurrentDepth==0)end=(int)reader.TokenStartIndex;
        }
        if(end<0)throw new JsonException();
        byte[] patch=Encoding.ASCII.GetBytes((nullStart<0?(properties>0?",":"")+"\"max_tokens\":" : "")+budget.ToString(System.Globalization.CultureInfo.InvariantCulture));
        int start=nullStart<0?end:nullStart,remove=nullStart<0?0:nullEnd-nullStart;
        int size=checked(original.Length-remove+patch.Length);if(size>limit)throw new RequestFailure("转换后的请求超过大小限制",413,"request_too_large");
        reserve(size);byte[] output=new byte[size];original.AsSpan(0,start).CopyTo(output);patch.CopyTo(output.AsSpan(start));original.AsSpan(start+remove).CopyTo(output.AsSpan(start+patch.Length));return output;
    }
}
