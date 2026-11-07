using System.Text.Json;
using System.Text.Json.Nodes;
using AccountScheduling;

namespace HttpMigration;
public sealed partial class ConsoleSettings
{
    AccountPool? pool;SchedulingOptions? baseline;IReadOnlyDictionary<string,string> labels=new Dictionary<string,string>();
    static void ValidatePolicies(JsonElement root)
    {
        if(root.TryGetProperty("concurrent",out var concurrent))
        {Fields(concurrent,"MaxAcc","MaxGlobal");foreach(var p in concurrent.EnumerateObject()){int n=p.Value.GetInt32();if(n<0||n>65536)throw new ArgumentException("Invalid concurrency");}}
        if(root.TryGetProperty("scheduling",out var scheduling))
        {Fields(scheduling,"loadAware","affinityWaitMs","cacheAffinityWaitMs","waitTimeoutMs");foreach(var p in scheduling.EnumerateObject()){if(p.Name=="loadAware"){_=p.Value.GetBoolean();continue;}int n=p.Value.GetInt32();if(n<0||n>900000)throw new ArgumentException("Invalid scheduling duration");}}
        if(root.TryGetProperty("models",out var models)&&models.TryGetProperty("accounts",out var accounts))
        {
            if(accounts.ValueKind!=JsonValueKind.Object||accounts.EnumerateObject().Count()>1024)throw new ArgumentException("Invalid account policies");var seen=new HashSet<string>();
            foreach(var p in accounts.EnumerateObject())
            {
                string id=KeySecurity.KeyRules.Model(p.Name);if(id.Length is 0 or >256||id.Any(char.IsWhiteSpace)||id.Any(char.IsControl)||!seen.Add(id))throw new ArgumentException("Invalid policy model");
                Fields(p.Value,"allowlist","blocklist");foreach(var list in p.Value.EnumerateObject())
                {if(list.Value.ValueKind!=JsonValueKind.Array||list.Value.GetArrayLength()>1024)throw new ArgumentException("Invalid account list");foreach(var value in list.Value.EnumerateArray()){string name=value.GetString()??"";if(name.Length is 0 or >256||name.Any(char.IsControl)||name.IndexOfAny(['/', '\\','*','?'])>=0)throw new ArgumentException("Account policy requires exact identifier or basename");}}
            }
        }
    }
    public void AttachAccountPolicies(AccountPool accounts,IReadOnlyDictionary<string,string> display)
    {lock(sync){baseline??=accounts.Options;pool=accounts;labels=display;ApplyPolicies();}}
    public void RefreshAccountPolicies(){lock(sync)ApplyPolicies();}
    void ApplyPolicies()
    {
        if(pool is null)return;using var doc=JsonDocument.Parse(document);var root=doc.RootElement;
        var rules=new Dictionary<string,ModelPolicy>();ModelPolicy global=new();
        HashSet<AccountId> Resolve(JsonElement rule,string name)
        {
            var result=new HashSet<AccountId>();if(!rule.TryGetProperty(name,out var list))return result;
            foreach(var value in list.EnumerateArray())
            {
                string target=value.GetString()!;var matches=pool.Snapshots().Where(a=>a.Id.Value==target||labels.GetValueOrDefault(a.Id.Value)==target).Select(a=>a.Id).ToArray();
                if(name=="blocklist"&&matches.Length>0)result.UnionWith(matches);
                else if(matches.Length==1)result.Add(matches[0]);else result.Add(new AccountId("unresolved:"+Convert.ToHexStringLower(System.Security.Cryptography.SHA256.HashData(System.Text.Encoding.UTF8.GetBytes(target)))));
            }
            return result;
        }
        if(root.GetProperty("models").TryGetProperty("accounts",out var configured))foreach(var p in configured.EnumerateObject())
        {var rule=new ModelPolicy(Allow:Resolve(p.Value,"allowlist"),Block:Resolve(p.Value,"blocklist"));string key=KeySecurity.KeyRules.Model(p.Name);if(key=="all")global=rule;else rules[key]=rule;}
        SchedulingOptions? limits=baseline;
        if(root.TryGetProperty("concurrent",out _)||root.TryGetProperty("scheduling",out _))
        {
            root.TryGetProperty("concurrent",out var c);root.TryGetProperty("scheduling",out var s);
            int Number(JsonElement o,string n,int fallback)=>o.ValueKind==JsonValueKind.Object&&o.TryGetProperty(n,out var v)&&v.GetInt32()>0?v.GetInt32():fallback;
            bool load=s.ValueKind!=JsonValueKind.Object||!s.TryGetProperty("loadAware",out var enabled)||enabled.GetBoolean();
            limits=baseline! with{MaxAccount=Number(c,"MaxAcc",4),MaxGlobal=Number(c,"MaxGlobal",0),LoadAware=load,WaitTimeout=TimeSpan.FromMilliseconds(Number(s,"waitTimeoutMs",15000)),AffinityWait=TimeSpan.FromMilliseconds(Number(s,"affinityWaitMs",250)),CacheAffinityWait=s.ValueKind==JsonValueKind.Object&&s.TryGetProperty("cacheAffinityWaitMs",out var grace)?TimeSpan.FromMilliseconds(grace.GetInt32()):TimeSpan.FromMilliseconds(1500)};
        }
        if(root.TryGetProperty("credits",out var credits))limits=limits! with{ExpiringFirst=credits.TryGetProperty("expiringFirst",out var expiring)?expiring.GetBoolean():baseline!.ExpiringFirst,CreditWindow=credits.TryGetProperty("windowDays",out var days)?TimeSpan.FromDays(days.GetInt32()):baseline!.CreditWindow};
        pool.SetAccountPolicies(rules,global,limits);
    }
    public void ToggleAccount(string id,bool enabled)
    {
        lock(sync)
        {
            if(pool?.Snapshot(new(id)) is null)throw new KeyNotFoundException();
            var root=JsonNode.Parse(document)!.AsObject();var models=root["models"]!.AsObject();
            var accounts=models["accounts"] as JsonObject??new();if(models["accounts"] is null)models["accounts"]=accounts;
            var globalKey=accounts.Select(p=>p.Key).FirstOrDefault(k=>KeySecurity.KeyRules.Model(k)=="all");
            if(globalKey is not null&&globalKey!="all")
            {var original=accounts[globalKey];accounts.Remove(globalKey);accounts["all"]=original;}
            var all=accounts["all"] as JsonObject??new();if(accounts["all"] is null)accounts["all"]=all;
            var list=(all["blocklist"] as JsonArray)?.Select(n=>n!.GetValue<string>()).ToHashSet()??[];
            // Go converts the current whitelist to explicit blocks before a UI
            // toggle, preserving every other account's current eligibility.
            if(all["allowlist"] is JsonArray allow&&allow.Count>0)
                foreach(var account in pool.Snapshots())
                    if(!pool.SchedulingEnabled(account.Id))list.Add(account.Id.Value);
            all.Remove("allowlist");
            if(enabled){list.Remove(id);if(labels.TryGetValue(id,out var label))list.Remove(label);}else list.Add(id);
            all["blocklist"]=new JsonArray(list.Order().Select(v=>(JsonNode)JsonValue.Create(v)!).ToArray());
            string content=root.ToJsonString();Update(content,revision);
        }
    }
}
