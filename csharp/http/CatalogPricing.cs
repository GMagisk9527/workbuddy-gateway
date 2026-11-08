using System.Globalization;
using System.Text.Json;
using AccountScheduling;
using KeySecurity;

namespace HttpMigration;
public sealed partial class ModelCatalog
{
    sealed record Promotion(bool Enabled,double Factor,long From=0,long Until=0);
    sealed record RateObservation(AccountSite Site,string Model,ModelCost Cost,long At,bool Probe=false,
        long FreeUntil=0,double? Credit=null,long Tokens=0,bool Restored=false);
    sealed record Rate(string? Value,ModelCost Cost,string Source,long At,long Checked,long Changed,bool Conflict,
        bool HasSource,bool HasLog,bool Cached,string Original,string Explain);
    readonly Dictionary<(AccountSite Site,string Model),RateObservation> observations=[];
    AccountPool? schedulingPool;
    long revision,persistedRevision;
    string publishedRevision="";
    internal long Revision {get{lock(publish)return revision;}}
    internal bool NeedsSave {get{lock(publish)return revision!=persistedRevision;}}
    internal void MarkSaved(long version){lock(publish)persistedRevision=Math.Max(persistedRevision,version);}
    internal static string DisplayId(AccountSite site,string id)
    {id=KeyRules.Model(id);return site==AccountSite.Intl&&id.Length>3&&id.EndsWith("-sg",StringComparison.Ordinal)?id[..^3]:id;}
    static void ValidateObservations(RateObservation[] rows)
    {
        if(rows.Length>2000||rows.Any(e=>e is null||!Enum.IsDefined(e.Site)||e.Model is null||e.Model.Length is 0 or >256||e.Model!=KeyRules.Model(e.Model)||e.Model.Any(char.IsControl)||e.Cost is not(ModelCost.Free or ModelCost.Paid)||e.At<0||e.FreeUntil<0||e.Tokens<0||e.Credit is{} c&&(!double.IsFinite(c)||c<0||e.Cost==ModelCost.Free&&(c!=0||e.Tokens<100)||e.Cost==ModelCost.Paid&&c<=0))||rows.Select(e=>(e.Site,e.Model,e.Probe)).Distinct().Count()!=rows.Length)throw new ArgumentException("Invalid rate evidence");
    }
    // Normal and probe samples have separate keys so a failed probe cannot refresh a verdict.
    readonly Dictionary<(AccountSite Site,string Model),RateObservation> probeObservations=[];
    void StoreObservation(RateObservation sample)
    {
        var entries=sample.Probe?probeObservations:observations;var key=(sample.Site,DisplayId(sample.Site,sample.Model));
        if(entries.TryGetValue(key,out var previous)&&previous.At>sample.At||entries.Count>=1000&&!entries.ContainsKey(key))return;
        entries[key]=sample with{Model=key.Item2};revision++;
    }
    internal string RateRevision
    {
        get{lock(publish){long now=clock.GetUtcNow().ToUnixTimeMilliseconds();long boundary=state.Cn.Concat(state.Intl).SelectMany(r=>r.Promotions??[]).SelectMany(p=>new[]{p.From,p.Until}).Concat(observations.Values.Concat(probeObservations.Values).Select(e=>e.FreeUntil)).Where(x=>x>0&&x<=now).DefaultIfEmpty().Max();return revision.ToString(CultureInfo.InvariantCulture)+":"+boundary.ToString(CultureInfo.InvariantCulture);}}
    }
    Row? Find(AccountSite site,string id)=>(site==AccountSite.Cn?state.Cn:state.Intl).FirstOrDefault(r=>r.Id==DisplayId(site,id));
    (double? Value,long Changed) CatalogRate(Row? row,long now)
    {
        if(row is null)return(null,0);
        long changed=row.RateChangedAt;double? value=row.HasMultiplier?row.Multiplier:null;
        if(row.Promotions is{Length:>0} promotions)
        {
            foreach(var p in promotions)foreach(long boundary in new[]{p.From,p.Until})if(boundary>0&&boundary<=now)changed=Math.Max(changed,boundary);
            var active=promotions.Where(p=>p.Enabled&&(p.From==0||now>=p.From)&&(p.Until==0||now<p.Until)).ToArray();
            if(active.Length>0){double factor=active.Min(p=>p.Factor);value=row.BaseMultiplier is{} b?b*factor:factor==0?0:null;}
            else value=row.BaseMultiplier is >0?row.BaseMultiplier:null;
        }
        else if(row.PromoExpired)value=row.BaseMultiplier is >0?row.BaseMultiplier:null;
        return(value,changed);
    }
    Rate Resolve(AccountSite site,string id,long now)
    {
        id=DisplayId(site,id);var row=Find(site,id);var (value,changed)=CatalogRate(row,now);
        long boundary=row?.FromLive==true?changed:0;
        var samples=new[]{observations.GetValueOrDefault((site,id)),probeObservations.GetValueOrDefault((site,id))};
        var sample=samples.Where(e=>e is not null&&e.At>=boundary&&!(e.Cost==ModelCost.Free&&e.FreeUntil>0&&now>=e.FreeUntil)).OrderByDescending(e=>e!.At).FirstOrDefault();
        bool conflict=sample is not null&&value is{} v&&(sample.Cost==ModelCost.Free?v>0:v==0);
        string source=row is null?"":row.FromLive?"上游模型目录":"npm 静态目录";
        long at=row?.RateCheckedAt??0;string? label=value?.ToString("0.##",CultureInfo.InvariantCulture)+ (value.HasValue?"x":"");
        if(!value.HasValue)label=null;
        var cost=value.HasValue?(value==0?ModelCost.Free:ModelCost.Paid):ModelCost.Unknown;
        string explanation=value.HasValue?"采用目录倍率；请求积分只能确认免费或收费，不用于反推倍率。":"当前没有可信倍率；已到期促销及过早的实测只作历史参考。";
        if(sample is not null)
        {
            cost=sample.Cost;
            if(cost==ModelCost.Free||value is null or 0)
            {source=sample.Probe?"已保存的模型探测用量":sample.Restored?"历史实测账本（升级恢复）":"正常请求实测";at=sample.At;label=cost==ModelCost.Free?"免费（实测）":"收费(倍率未知)";}
            explanation=cost==ModelCost.Free?"最近有效实测确认免费；仅表示该站点的观测。":"最近有效实测确认扣费；数值倍率只来自可信目录。";
            if(conflict)explanation+="与目录存在冲突，目录原记录保留供核对。";
        }
        bool cached=row?.RateCached==true||row is not null&&!((site==AccountSite.Cn?state.CnLive:state.IntlLive));
        return new(label,cost,source,at/1000,(row?.RateCheckedAt??0)/1000,changed/1000,conflict,row is not null||samples.Any(e=>e is not null),samples.Any(e=>e is not null),cached,row?.OriginalId??row?.Id??"",explanation);
    }
    public string? PriceLabel(AccountSite site,string id){lock(publish)return Resolve(site,id,clock.GetUtcNow().ToUnixTimeMilliseconds()).Value;}
    public void AttachScheduling(AccountPool pool)
    {
        // Runtime snapshots retain the real original observation time, with no invented usage.
        var recovered=pool.Snapshots();
        lock(publish)
        {
            schedulingPool=pool;publishedRevision="";
            foreach(var a in recovered.Where(a=>!a.Disabled))foreach(var (model,s) in a.Models??new Dictionary<string,AccountModelState>())
                if(s.Cost!=ModelCost.Unknown&&s.ObservedAt>default(DateTimeOffset)&&!observations.ContainsKey((a.Site,DisplayId(a.Site,model))))
                    StoreObservation(new(a.Site,model,s.Cost,s.ObservedAt.ToUnixTimeMilliseconds(),Restored:true,FreeUntil:FreeUntil(a.Site,model,s.ObservedAt.ToUnixTimeMilliseconds())));
            PublishPricing();
        }
    }
    long FreeUntil(AccountSite site,string id,long at)=>Find(site,id)?.Promotions?.Where(p=>p.Enabled&&p.Until>at).Select(p=>p.Until).DefaultIfEmpty().Min()??0;
    public void ObserveProbeCost(AccountSite site,string model,ModelCost cost,long at=0,long freeUntil=0)
    {
        if(cost==ModelCost.Unknown)return;
        lock(publish){long stamp=checked(at*1000);StoreObservation(new(site,model,cost,stamp,true,freeUntil>0?freeUntil*1000:cost==ModelCost.Free?FreeUntil(site,model,stamp):0));PublishPricing();}
    }
    internal long ProbeFreeUntil(AccountSite site,string id,long at){lock(publish)return FreeUntil(site,id,at*1000)/1000;}
    internal void ObserveUsage(AccountSite site,string model,string? raw)
    {
        var cost=ModelObservations.Cost(raw);if(cost==ModelCost.Unknown)return;
        using var doc=JsonDocument.Parse(raw!);var root=doc.RootElement;
        double? credit=root.TryGetProperty("credit",out var c)&&Number(c,out var n)?n:null;
        long tokens=0;
        if(root.TryGetProperty("total_tokens",out var total)&&Number(total,out var t))tokens=t>=long.MaxValue?long.MaxValue:(long)t;
        else foreach(string key in new[]{"prompt_tokens","completion_tokens"})if(root.TryGetProperty(key,out var e)&&Number(e,out var v))tokens=v>=long.MaxValue-tokens?long.MaxValue:tokens+(long)v;
        lock(publish){long at=clock.GetUtcNow().ToUnixTimeMilliseconds();StoreObservation(new(site,model,cost,at,FreeUntil:cost==ModelCost.Free?FreeUntil(site,model,at):0,Credit:credit,Tokens:tokens));PublishPricing();}
    }
    IEnumerable<(AccountSite Site,string Id)> RateKeys()=>state.Cn.Select(r=>(AccountSite.Cn,r.Id)).Concat(state.Intl.Select(r=>(AccountSite.Intl,r.Id))).Concat(observations.Keys).Concat(probeObservations.Keys).Distinct();
    public object PriceDetails()
    {
        lock(publish)
        {
            var rates=new Dictionary<string,object>();long now=clock.GetUtcNow().ToUnixTimeMilliseconds();
            foreach(var (site,id) in RateKeys())
            {
                var r=Resolve(site,id,now);
                rates[(site==AccountSite.Cn?"cn":"intl")+"|"+id]=new Dictionary<string,object?>
                {
                    ["value"]=r.Value,["hasSource"]=r.HasSource,["hasLog"]=false,["source"]=r.Source,["updatedAt"]=r.At,["checkedAt"]=r.Checked,["changedAt"]=r.Changed,["conflict"]=r.Conflict,["cached"]=r.Cached,["originalModel"]=r.Original,
                    ["explain"]=r.Explain,["unknown"]=r.Cost==ModelCost.Unknown||r.Value=="收费(倍率未知)",
                    ["observation"]=Evidence(observations.GetValueOrDefault((site,id))),["probe"]=Evidence(probeObservations.GetValueOrDefault((site,id))),
                    ["catalog"]=Find(site,id) is{} row?new Dictionary<string,object?>{["value"]=CatalogRate(row,now).Value,["baseMultiplier"]=row.BaseMultiplier,["fromLive"]=row.FromLive}:null,
                    ["refreshHint"]="目录定时刷新；价格探测需显式开启且可能消耗积分。",["records"]=Array.Empty<object>(),["others"]=Array.Empty<object>()
                };
            }
            return new Dictionary<string,object?>{["rates"]=rates,["rateDisplayRevision"]=RateRevision,["noLogHint"]="尚无目录价格或有效实测来源，不推测倍率。"};
        }
    }
    static object? Evidence(RateObservation? e)=>e is null?null:new Dictionary<string,object?>{["updatedAt"]=e.At/1000,["class"]=e.Cost.ToString().ToLowerInvariant(),["credit"]=e.Credit,["tokens"]=e.Tokens,["freeUntil"]=e.FreeUntil/1000,["restored"]=e.Restored};
    public void RefreshPricing(){lock(publish)PublishPricing();}
    void PublishPricing()
    {
        if(schedulingPool is null||publishedRevision==RateRevision)return;
        var rules=new Dictionary<string,ModelPolicy>();long now=clock.GetUtcNow().ToUnixTimeMilliseconds();
        foreach(string id in RateKeys().Select(k=>k.Id).Distinct())
        {
            var free=new HashSet<AccountSite>();var paid=new HashSet<AccountSite>();
            foreach(var site in new[]{AccountSite.Cn,AccountSite.Intl}){var cost=Resolve(site,id,now).Cost;if(cost==ModelCost.Free)free.Add(site);if(cost==ModelCost.Paid)paid.Add(site);}
            rules[id]=new(FreeSites:free,PaidSites:paid);
        }
        schedulingPool.SetCatalogPolicies(rules);publishedRevision=RateRevision;
    }
    public string[] ModelIds()=>Volatile.Read(ref state).Cn.Concat(Volatile.Read(ref state).Intl).Select(r=>r.Id).Distinct().Where(Allows).ToArray();
    public bool NeedsProbe(AccountSite site,string id,bool requested)
    {lock(publish){var row=Find(site,id);return row is null||!row.FromLive?requested:row.Promotions is{Length:>0}&&CatalogRate(row,clock.GetUtcNow().ToUnixTimeMilliseconds()).Value is null;}}
    static bool Number(JsonElement value,out double number)
    {
        string text=value.ValueKind==JsonValueKind.String?value.GetString()!.Trim().ToLowerInvariant().TrimStart('x').Replace("credits","").Replace("credit","").Trim():value.GetRawText();
        return double.TryParse(text,NumberStyles.Float,CultureInfo.InvariantCulture,out number)&&double.IsFinite(number)&&number>=0;
    }
    static void ReadPrice(Row row,JsonElement model,JsonElement data)
    {
        if(model.TryGetProperty("credits",out var credits)&&Number(credits,out double value)){row.BaseMultiplier=value;row.HasMultiplier=true;row.Multiplier=value;}
        if(!data.TryGetProperty("modelPromotions",out var promotions)||promotions.ValueKind!=JsonValueKind.Array)return;
        if(promotions.GetArrayLength()>1024)throw new ArgumentException("Too many promotions");
        var entries=new List<Promotion>();
        foreach(var promo in promotions.EnumerateArray())
        {
            if(promo.ValueKind!=JsonValueKind.Object||!promo.TryGetProperty("modelIds",out var ids)||ids.ValueKind!=JsonValueKind.Array||!ids.EnumerateArray().Any(x=>x.ValueKind==JsonValueKind.String&&x.GetString()==row.Id))continue;
            if(!promo.TryGetProperty("discount",out var d)||d.ValueKind!=JsonValueKind.Object||!d.TryGetProperty("factor",out var f)||!Number(f,out var factor)||factor>1)continue;
            bool enabled=promo.TryGetProperty("enabled",out var e)&&e.ValueKind==JsonValueKind.True;
            long from=0,until=0;
            if(promo.TryGetProperty("schedule",out var s)&&s.ValueKind==JsonValueKind.Object)
            {
                long Stamp(string name)=>s.TryGetProperty(name,out var v)&&v.ValueKind==JsonValueKind.String&&DateTimeOffset.TryParse(v.GetString(),CultureInfo.InvariantCulture,DateTimeStyles.None,out var date)?Math.Max(0,date.ToUnixTimeMilliseconds()):0;
                from=Stamp("validFrom");until=Stamp("validUntil");
            }
            entries.Add(new(enabled,factor,from,until));
        }
        row.Promotions=entries.ToArray();
    }
    void Stamp(IEnumerable<Row> rows,Row[] previous)
    {
        long now=clock.GetUtcNow().ToUnixTimeMilliseconds();
        foreach(var row in rows)
        {
            var old=previous.FirstOrDefault(r=>r.Id==row.Id);row.RateCheckedAt=now;row.PriceUpdatedAt=now/1000;row.RateCached=false;
            bool same=old is not null&&old.FromLive==row.FromLive&&old.HasMultiplier==row.HasMultiplier&&old.Multiplier==row.Multiplier&&old.BaseMultiplier==row.BaseMultiplier&&old.PromoExpired==row.PromoExpired&&(old.Promotions??[]).SequenceEqual(row.Promotions??[]);
            row.RateChangedAt=same||CatalogRate(row,now).Value is null?old?.RateChangedAt??0:now;
        }
    }
}
