using System.Globalization;
using System.Text.Json;
using AccountScheduling;

namespace HttpMigration;
public sealed partial class ModelCatalog
{
    public string? PriceLabel(AccountSite site,string id)
    {
        lock(publish)
        {
            var cost=probeCosts.GetValueOrDefault((site,id));if(cost==ModelCost.Free)return "免费（实测）";
            var row=(site==AccountSite.Cn?state.Cn:state.Intl).FirstOrDefault(r=>r.Id==id);
            if(row is{HasMultiplier:true}&&(row.Multiplier>0||!row.PromoExpired))return row.Multiplier.ToString("0.##",CultureInfo.InvariantCulture)+"x";
            return cost==ModelCost.Paid?"收费（实测）":null;
        }
    }
    AccountPool? schedulingPool;
    readonly Dictionary<(AccountSite Site,string Model),ModelCost> probeCosts=[];
    readonly Dictionary<(AccountSite Site,string Model),long> probeTimes=[];
    public void AttachScheduling(AccountPool pool){lock(publish){schedulingPool=pool;PublishPricing();}}
    public void ObserveProbeCost(AccountSite site,string model,ModelCost cost,long at=0)
    {if(cost==ModelCost.Unknown)return;lock(publish){if(probeCosts.Count>=1000&&!probeCosts.ContainsKey((site,model)))return;probeCosts[(site,model)]=cost;probeTimes[(site,model)]=at;PublishPricing();}}
    public object PriceDetails()
    {
        lock(publish)
        {
            var rates=new Dictionary<string,object>();
            foreach(var (site,rows) in new[]{(AccountSite.Cn,state.Cn),(AccountSite.Intl,state.Intl)})
            foreach(var row in rows)
            {
                var cost=probeCosts.GetValueOrDefault((site,row.Id));bool probed=cost!=ModelCost.Unknown;
                bool known=probed||row.HasMultiplier;
                string label=PriceLabel(site,row.Id)??"未知";
                rates[(site==AccountSite.Cn?"cn":"intl")+"|"+row.Id]=new Dictionary<string, object?>
{
    ["hasLog"] = known,
    ["source"] = probed ? "已保存的模型探测用量" : "上游模型目录",
    ["updatedAt"] = probed ? probeTimes.GetValueOrDefault((site, row.Id)) : row.PriceUpdatedAt,
    ["explain"] = probed ? $"探测用量结论：{(cost == ModelCost.Free ? "免费" : "收费")}；显示值：{label}。不通过余额变化推算倍率。" : row.PromoExpired ? $"目录促销已失效；当前显示值：{label}。" : $"目录声明倍率：{label}。",
    ["unknown"] = !known,
    ["refreshHint"] = "目录定时刷新；价格探测需显式开启且可能消耗积分。",
    ["records"] = Array.Empty<object>(),
    ["others"] = Array.Empty<object>()
};
            }
            return new Dictionary<string, object?> { ["rates"] = rates, ["noLogHint"] = "尚无目录价格或保存的探测用量记录，不推测倍率。" };
        }
    }
    void PublishPricing()
    {
        if(schedulingPool is null)return;var rules=new Dictionary<string,ModelPolicy>();
        foreach(string id in state.Cn.Concat(state.Intl).Select(r=>r.Id).Distinct())
        {
            var free=new HashSet<AccountSite>();var paid=new HashSet<AccountSite>();
            foreach(var (site,rows) in new[]{(AccountSite.Cn,state.Cn),(AccountSite.Intl,state.Intl)})
            {
                var row=rows.FirstOrDefault(r=>r.Id==id);var probe=probeCosts.GetValueOrDefault((site,id));
                if(probe==ModelCost.Free||row is{HasMultiplier:true,Multiplier:0,PromoExpired:false})free.Add(site);
                if(probe==ModelCost.Paid||row is{HasMultiplier:true,Multiplier:>0})paid.Add(site);
            }
            rules[id]=new(FreeSites:free,PaidSites:paid);
        }
        schedulingPool.SetCatalogPolicies(rules);
    }
    public string[] ModelIds()=>Volatile.Read(ref state).Cn.Concat(Volatile.Read(ref state).Intl).Select(r=>r.Id).Distinct().Where(Allows).ToArray();
    public bool NeedsProbe(AccountSite site,string id,bool requested)
    {
        var s=Volatile.Read(ref state);var row=(site==AccountSite.Cn?s.Cn:s.Intl).FirstOrDefault(r=>r.Id==id);
        return row is null||!row.FromLive?requested:row.HasMultiplier&&row.Multiplier==0&&row.PromoExpired;
    }
    static bool Number(JsonElement value,out double number)
    {
        string text=value.ValueKind==JsonValueKind.String?value.GetString()!.Trim().ToLowerInvariant().TrimStart('x').Replace("credits","").Replace("credit","").Trim():value.GetRawText();
        return double.TryParse(text,NumberStyles.Float,CultureInfo.InvariantCulture,out number)&&double.IsFinite(number)&&number>=0;
    }
    static void ReadPrice(Row row,JsonElement model,JsonElement data,DateTimeOffset now)
    {
        if(model.TryGetProperty("credits",out var credits)&&Number(credits,out double value)){row.HasMultiplier=true;row.Multiplier=value;}
        if(!data.TryGetProperty("modelPromotions",out var promotions)||promotions.ValueKind!=JsonValueKind.Array)return;
        if(promotions.GetArrayLength()>1024)throw new ArgumentException("Too many promotions");
        double? discount=null;bool inactive=false;
        foreach(var promo in promotions.EnumerateArray())
        {
            if(!promo.TryGetProperty("modelIds",out var ids)||ids.ValueKind!=JsonValueKind.Array||!ids.EnumerateArray().Any(x=>x.ValueKind==JsonValueKind.String&&x.GetString()==row.Id))continue;
            bool on=promo.TryGetProperty("enabled",out var enabled)&&enabled.ValueKind==JsonValueKind.True;
            if(promo.TryGetProperty("schedule",out var schedule)&&schedule.ValueKind==JsonValueKind.Object)
            {
                if(schedule.TryGetProperty("validFrom",out var from)&&from.ValueKind==JsonValueKind.String&&DateTimeOffset.TryParse(from.GetString(),CultureInfo.InvariantCulture,DateTimeStyles.None,out var start)&&now<start)on=false;
                if(schedule.TryGetProperty("validUntil",out var until)&&until.ValueKind==JsonValueKind.String&&DateTimeOffset.TryParse(until.GetString(),CultureInfo.InvariantCulture,DateTimeStyles.None,out var end)&&now>=end)on=false;
            }
            if(!on){inactive=true;continue;}
            if(promo.TryGetProperty("discount",out var d)&&d.TryGetProperty("factor",out var f)&&Number(f,out var factor)&&factor<=1)discount=discount is{} old?Math.Min(old,factor):factor;
        }
        if(discount is{} multiplier){row.Multiplier=row.HasMultiplier?row.Multiplier*multiplier:multiplier;row.HasMultiplier=true;}
        else row.PromoExpired=inactive;
    }
}
