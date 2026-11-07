using System.Collections.Frozen;
using System.Text.Json;

namespace AccountScheduling;

public sealed record RuntimeIdentity(AccountId Id,AccountSite Site,string Digest);
public sealed record RuntimeExport(long Version,byte[] Snapshot);

public sealed partial class AccountPool
{
    public const int RuntimeSnapshotLimit=8<<20,RuntimeModelLimit=16384;
    long runtimeVersion;
    bool runtimeInitialized;
    readonly HashSet<AccountId> runtimeTouched=[];
    readonly HashSet<AccountId> runtimeRestored=[];
    public long RuntimeVersion{get{lock(sync)return runtimeVersion;}}
    void MarkRuntime(AccountId id){runtimeTouched.Add(id);runtimeVersion++;}
    static long Unix(DateTimeOffset at)=>at==default?0:at.ToUnixTimeSeconds();
    static FrozenDictionary<AccountId,RuntimeIdentity> Bindings(IEnumerable<RuntimeIdentity> identities)
    {
        var rows=identities.Take(1025).ToArray();
        if(rows.Length>1024||rows.Any(r=>r is null||r.Id is null||!Enum.IsDefined(r.Site)||r.Digest is not{Length:64}||r.Digest.Any(c=>!char.IsAsciiHexDigit(c)))||rows.Select(r=>r.Id).Distinct().Count()!=rows.Length)throw new ArgumentException("Invalid runtime identities");
        return rows.ToFrozenDictionary(r=>r.Id);
    }
    sealed record ExportRow(AccountSnapshot Account,RuntimeIdentity Identity,DateTimeOffset Rate);
    public RuntimeExport ExportRuntime(IEnumerable<RuntimeIdentity> identities)
    {
        var bindings=Bindings(identities);ExportRow[] rows;long version,at;
        lock(sync)
        {
            version=runtimeVersion;at=time.GetUtcNow().ToUnixTimeSeconds();
            rows=accounts.Where(a=>bindings.TryGetValue(a.Id,out var b)&&b.Site==a.Site).OrderBy(a=>a.Id.Value,StringComparer.Ordinal).Select(a=>new ExportRow(a,bindings[a.Id],rateCooldowns.GetValueOrDefault(a.Id))).ToArray();
        }
        if(rows.Sum(r=>r.Account.Models!.Count)>RuntimeModelLimit)throw new ArgumentException("Runtime state exceeds bounds");
        using var buffer=new BoundedSnapshot();
        using(var w=new Utf8JsonWriter(buffer))
        {
            w.WriteStartObject();w.WriteNumber("version",1);w.WriteNumber("at",at);w.WriteStartArray("accounts");
            foreach(var row in rows)
            {
                var a=row.Account;w.WriteStartObject();w.WriteString("id",a.Id.Value);w.WriteNumber("site",(int)a.Site);w.WriteString("identity",row.Identity.Digest);
                w.WriteBoolean("exhausted",a.QuotaExhausted);w.WriteNumber("rateUntil",Unix(row.Rate));w.WriteStartObject("models");
                foreach(var (name,s) in a.Models!.OrderBy(p=>p.Key,StringComparer.Ordinal))
                {
                    w.WriteStartObject(name);w.WriteNumber("cost",(int)s.Cost);w.WriteNumber("cooldown",Unix(s.CooldownUntil));w.WriteBoolean("blocked",s.QuotaBlocked);w.WriteNumber("probe",Unix(s.NextProbeAt));w.WriteNumber("observed",Unix(s.ObservedAt));w.WriteEndObject();
                }
                w.WriteEndObject();w.WriteEndObject();
            }
            w.WriteEndArray();w.WriteEndObject();
        }
        return new(version,buffer.ToArray());
    }
    sealed class BoundedSnapshot:MemoryStream
    {
        void Check(int count){if(count>RuntimeSnapshotLimit-Length)throw new ArgumentException("Runtime snapshot exceeds bounds");}
        public override void Write(byte[] b,int offset,int count){Check(count);base.Write(b,offset,count);}
        public override void Write(ReadOnlySpan<byte> b){Check(b.Length);base.Write(b);}
        public override void WriteByte(byte b){Check(1);base.WriteByte(b);}
    }
    sealed record RestoredRow(AccountId Id,AccountSite Site,string Identity,bool Exhausted,long Rate,FrozenDictionary<string,AccountModelState> Models);
    static void Fields(JsonElement value,params string[] names)
    {
        if(value.ValueKind!=JsonValueKind.Object)throw new ArgumentException("Invalid runtime object");
        var seen=new HashSet<string>(StringComparer.Ordinal);
        foreach(var p in value.EnumerateObject())if(!names.Contains(p.Name,StringComparer.Ordinal)||!seen.Add(p.Name))throw new ArgumentException("Invalid runtime fields");
        if(seen.Count!=names.Length)throw new ArgumentException("Missing runtime fields");
    }
    static long Stamp(JsonElement value,long maximum)
    {long n=value.GetInt64();return n is >=0 and <=253402300799&&n<=maximum?n:throw new ArgumentException("Invalid runtime timestamp");}
    static DateTimeOffset Date(long stamp)=>stamp==0?default:DateTimeOffset.FromUnixTimeSeconds(stamp);
    RestoredRow[] ParseRuntime(ReadOnlyMemory<byte> bytes)
    {
        if(bytes.Length>RuntimeSnapshotLimit)throw new ArgumentException("Runtime snapshot exceeds bounds");
        using var doc=JsonDocument.Parse(bytes,new JsonDocumentOptions{MaxDepth=12});var root=doc.RootElement;Fields(root,"version","at","accounts");
        if(root.GetProperty("version").GetInt32()!=1)throw new ArgumentException("Invalid runtime version");
        long at=Stamp(root.GetProperty("at"),time.GetUtcNow().ToUnixTimeSeconds()+300);if(at==0)throw new ArgumentException("Invalid runtime capture time");
        var all=root.GetProperty("accounts");if(all.ValueKind!=JsonValueKind.Array||all.GetArrayLength()>1024)throw new ArgumentException("Invalid runtime accounts");
        var rows=new List<RestoredRow>();var seen=new HashSet<AccountId>();int total=0;
        foreach(var r in all.EnumerateArray())
        {
            Fields(r,"id","site","identity","exhausted","rateUntil","models");var id=new AccountId(r.GetProperty("id").GetString()!);var site=(AccountSite)r.GetProperty("site").GetInt32();string identity=r.GetProperty("identity").GetString()!;
            if(!seen.Add(id)||!Enum.IsDefined(site)||identity is not{Length:64}||identity.Any(c=>!char.IsAsciiHexDigit(c)))throw new ArgumentException("Invalid runtime identity");
            long rate=Stamp(r.GetProperty("rateUntil"),at+86400);bool exhausted=r.GetProperty("exhausted").GetBoolean();var models=r.GetProperty("models");if(models.ValueKind!=JsonValueKind.Object)throw new ArgumentException("Invalid runtime models");
            var states=new Dictionary<string,AccountModelState>(StringComparer.Ordinal);
            foreach(var p in models.EnumerateObject())
            {
                string model=SchedulingCopy.Model(p.Name);if(model!=p.Name||states.Count>=1024||++total>RuntimeModelLimit)throw new ArgumentException("Runtime model bounds");
                var s=p.Value;Fields(s,"cost","cooldown","blocked","probe","observed");var cost=(ModelCost)s.GetProperty("cost").GetInt32();if(!Enum.IsDefined(cost))throw new ArgumentException("Invalid runtime cost");
                var state=new AccountModelState(cost,Date(Stamp(s.GetProperty("cooldown"),at+86400)),s.GetProperty("blocked").GetBoolean(),Date(Stamp(s.GetProperty("probe"),at+300)),Date(Stamp(s.GetProperty("observed"),at+300)));
                if(!states.TryAdd(model,state))throw new ArgumentException("Duplicate runtime model");
            }
            rows.Add(new(id,site,identity,exhausted,rate,states.ToFrozenDictionary()));
        }
        return rows.ToArray();
    }
    public void ValidateRuntimeSnapshot(ReadOnlyMemory<byte> bytes)=>ParseRuntime(bytes);
    public int ImportRuntime(ReadOnlyMemory<byte> bytes,IEnumerable<RuntimeIdentity> identities)
    {
        var rows=ParseRuntime(bytes);var bindings=Bindings(identities);int restored=0;
        lock(sync)
        {
            var now=time.GetUtcNow();
            foreach(var row in rows)
            {
                int i=Array.FindIndex(accounts,a=>a.Id==row.Id);if(i<0||!bindings.TryGetValue(row.Id,out var binding)||binding.Site!=row.Site||binding.Digest!=row.Identity||accounts[i].Site!=row.Site||runtimeTouched.Contains(row.Id)||runtimeRestored.Contains(row.Id)||accounts[i].Models!.Count>0)continue;
                var old=accounts[i];var models=row.Models.ToFrozenDictionary(p=>p.Key,p=>p.Value with{CooldownUntil=p.Value.CooldownUntil>now?p.Value.CooldownUntil:default,NextProbeAt=p.Value.NextProbeAt>now?p.Value.NextProbeAt:default});
                var rate=Date(row.Rate);if(rate<=now)rate=default;else if(rate>now.AddDays(1))rate=now.AddDays(1);
                if(rate>rateCooldowns.GetValueOrDefault(row.Id))rateCooldowns[row.Id]=rate;
                accounts[i]=old with{Models=models,QuotaExhausted=old.QuotaExhausted||row.Exhausted,CooldownUntil=rate>old.CooldownUntil?rate:old.CooldownUntil};runtimeRestored.Add(row.Id);restored++;
            }
            if(restored>0){runtimeVersion++;Notify();}
        }
        return restored;
    }
}
