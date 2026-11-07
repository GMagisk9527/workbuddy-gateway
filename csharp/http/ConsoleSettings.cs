using System.Collections.Frozen;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;

namespace HttpMigration;

// Console rules never contain authentication secrets or own the credit ledger.
public sealed partial class ConsoleSettings:IDisposable
{
    readonly object sync=new();readonly string? directory;readonly FileStream? owner;readonly ModelCatalog catalog;
    byte[] document;string revision;int retention=7;FrozenSet<string> blocked=FrozenSet<string>.Empty;
    public int RetentionDays{get{lock(sync)return retention;}}
    public ConsoleSettings(ModelCatalog catalog,string? directory=null)
    {
        this.catalog=catalog;this.directory=directory is null?null:Path.GetFullPath(directory);
        document=Encode([],7);revision=Hash(document);
        if(this.directory is null)return;
        Directory.CreateDirectory(this.directory);CheckLink(this.directory);string marker=Path.Combine(this.directory,".csharp-console");CheckLink(marker);
        if(!File.Exists(marker)){if(Directory.EnumerateFileSystemEntries(this.directory).Any())throw new InvalidDataException("Console directory is not owned");File.WriteAllText(marker,"console-v1");}
        if(File.ReadAllText(marker)!="console-v1")throw new InvalidDataException("Invalid console ownership");
        CheckLink(Path.Combine(this.directory,".console.lock"));owner=new(Path.Combine(this.directory,".console.lock"),FileMode.OpenOrCreate,FileAccess.ReadWrite,FileShare.None);
        try{string path=Path.Combine(this.directory,"config.json");CheckLink(path);if(File.Exists(path)){byte[] raw=Read(path,65536);var parsed=Parse(raw);Publish(Encode(parsed.Block,parsed.Days,raw),parsed.Block,parsed.Days);}}
        catch{owner.Dispose();throw;}
    }
    internal string? DirectoryPath=>directory;
    internal static void CheckLink(string path){try{if((File.GetAttributes(path)&FileAttributes.ReparsePoint)!=0)throw new IOException("Console links are not supported");}catch(FileNotFoundException){}catch(DirectoryNotFoundException){}}
    internal static byte[] Read(string path,int limit)
    {CheckLink(path);using var f=new FileStream(path,FileMode.Open,FileAccess.Read,FileShare.Read);if(f.Length>limit)throw new InvalidDataException("Console file budget");byte[] b=new byte[(int)f.Length];f.ReadExactly(b);return b;}
    internal static void Atomic(string path,byte[] data)
    {
        CheckLink(path);string temp=path+".tmp";CheckLink(temp);
        try{using(var f=new FileStream(temp,FileMode.Create,FileAccess.Write,FileShare.None,4096,FileOptions.WriteThrough)){f.Write(data);f.Flush(true);}File.Move(temp,path,true);}
        finally{if(File.Exists(temp))File.Delete(temp);}
    }
    static string Hash(byte[] b)=>Convert.ToHexStringLower(SHA256.HashData(b));
    static byte[] Encode(IEnumerable<string> block,int days,byte[]? source=null)
    {
        var root=source is null?new JsonObject():JsonNode.Parse(source)!.AsObject();
        var models=root["models"] as JsonObject??new();if(root["models"] is null)root["models"]=models;
        models["blocklist"]=new JsonArray(block.Order(StringComparer.Ordinal).Select(s=>(JsonNode)JsonValue.Create(s)!).ToArray());
        var logs=root["logs"] as JsonObject??new();if(root["logs"] is null)root["logs"]=logs;logs["retentionDays"]=days;return JsonSerializer.SerializeToUtf8Bytes(root,new JsonSerializerOptions{WriteIndented=true});
    }
    static void Fields(JsonElement r,params string[] allowed)
    {if(r.ValueKind!=JsonValueKind.Object||r.EnumerateObject().Any(p=>!allowed.Contains(p.Name,StringComparer.Ordinal))||r.EnumerateObject().Select(p=>p.Name).Distinct(StringComparer.Ordinal).Count()!=r.EnumerateObject().Count())throw new ArgumentException("配置包含未知或重复字段");}
    static (FrozenSet<string> Block,int Days) Parse(byte[] raw)
    {
        if(raw.Length>65536)throw new ArgumentException("配置超过64KiB");using var doc=JsonDocument.Parse(raw);var r=doc.RootElement;Fields(r,"models","logs","concurrent","scheduling","systemPrompt","identity","affinity","tokenEstimation","credits","admission","upstream","server","downstream","output","retry");
        FrozenSet<string> ids=FrozenSet<string>.Empty;int days=7;
        if(r.TryGetProperty("models",out var m))
        {Fields(m,"blocklist","allowlist","accounts");if(m.TryGetProperty("blocklist",out var list))ids=ValidateModelList(list);if(m.TryGetProperty("allowlist",out var allowed))ValidateModelList(allowed);}
        if(r.TryGetProperty("logs",out var l))
        {Fields(l,"retentionDays","maxFileBytes","maxTotalBytes","maxFiles","sampleFirst","sampleEvery");if(l.TryGetProperty("retentionDays",out var retention))days=retention.GetInt32();}
        if(days is <0 or >36500)throw new ArgumentException("配置范围错误");
        ValidatePolicies(r);ValidateRuntime(r);return(ids,days);
    }
    static FrozenSet<string> ValidateModelList(JsonElement list)
    {if(list.ValueKind!=JsonValueKind.Array||list.GetArrayLength()>1024)throw new ArgumentException("Invalid model list");var ids=new HashSet<string>();foreach(var value in list.EnumerateArray()){string? id=value.GetString();if(string.IsNullOrWhiteSpace(id)||id!=KeySecurity.KeyRules.Model(id)||Encoding.UTF8.GetByteCount(id)>256||id.Any(c=>char.IsControl(c)||char.IsWhiteSpace(c)))throw new ArgumentException("Invalid model id");ids.Add(id);}return ids.ToFrozenSet();}
    void Publish(byte[] next,FrozenSet<string> ids,int days){document=next;revision=Hash(next);blocked=ids;retention=days;using var doc=JsonDocument.Parse(next);catalog.SetConsolePolicy(ids,doc.RootElement.GetProperty("models").TryGetProperty("allowlist",out var allowed)?ValidateModelList(allowed):null);ApplyPolicies();ApplyRuntime();}
    public (string Content,string Revision) View(){lock(sync)return(Encoding.UTF8.GetString(document),revision);}
    // Serialize disk reads with UI writes. Invalid external edits keep the last
    // valid document and never get silently overwritten by the watcher.
    public bool Reload()
    {
        lock(sync)
        {
            if(directory is null)return false;
            string path=Path.Combine(directory,"config.json");if(!File.Exists(path))return false;
            var raw=Read(path,65536);var parsed=Parse(raw);var next=Encode(parsed.Block,parsed.Days,raw);
            if(Hash(next)==revision)return false;
            Publish(next,parsed.Block,parsed.Days);return true;
        }
    }
    public void Update(string content,string expected)
    {
        var raw=Encoding.UTF8.GetBytes(content);var parsed=Parse(raw);var next=Encode(parsed.Block,parsed.Days,raw);
        lock(sync){if(expected!=revision)throw new ConsoleConflict();if(directory is not null)Atomic(Path.Combine(directory,"config.json"),next);Publish(next,parsed.Block,parsed.Days);}
    }
    public void Toggle(string model,bool enabled)
    {lock(sync){var ids=blocked.ToHashSet(StringComparer.Ordinal);var root=JsonNode.Parse(document)!.AsObject();var models=root["models"]!.AsObject();if(models["allowlist"] is JsonArray allow&&allow.Count>0)foreach(var item in catalog.List(includeDisabled:true)["data"]!.AsArray()){string id=item!["id"]!.GetValue<string>();if(!catalog.Allows(id))ids.Add(id);}models.Remove("allowlist");if(enabled)ids.Remove(model);else ids.Add(model);var raw=Encode(ids,retention,JsonSerializer.SerializeToUtf8Bytes(root));var parsed=Parse(raw);if(directory is not null)Atomic(Path.Combine(directory,"config.json"),raw);Publish(raw,parsed.Block,parsed.Days);}}
    public void SetRetention(int days){lock(sync){var raw=Encode(blocked,days,document);var parsed=Parse(raw);if(directory is not null)Atomic(Path.Combine(directory,"config.json"),raw);Publish(raw,parsed.Block,parsed.Days);}}
    public void Dispose()=>owner?.Dispose();
}
internal sealed class ConsoleConflict:Exception;
