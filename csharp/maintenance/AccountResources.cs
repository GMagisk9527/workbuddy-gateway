using System.Globalization;
using System.Text.Json;
namespace AccountMaintenance;
public sealed record QuotaObservation(double Total,double Used,double Remaining,bool Unlimited=false,bool Paid=false);
public sealed record CreditPackage(string Name,double Remain,long ExpiresAt,bool ExpiryKnown);
public sealed record AccountResourceObservation(QuotaObservation Quota,IReadOnlyList<CreditPackage> Packages,bool PackagesKnown);
public interface IAccountResourcesControlPlane
{Task<AccountResourceObservation> FetchResourcesAsync(AccountCredential credential,CancellationToken cancellation,bool fetchPackages=true);}
public static class ResourceParser
{
    static void Fail()=>throw new CredentialFailure("invalid_resource_response");
    static double Number(JsonElement e,bool stringsOnly=false)
    {
        double n=0;bool valid=e.ValueKind==JsonValueKind.String?double.TryParse(e.GetString()!.Trim(),NumberStyles.Float,CultureInfo.InvariantCulture,out n):!stringsOnly&&e.ValueKind==JsonValueKind.Number&&e.TryGetDouble(out n);
        if(!valid||!double.IsFinite(n))Fail();return n;
    }
    static double Capacity(JsonElement row,string key)
    {
        if(!row.TryGetProperty(key,out var v)||v.ValueKind==JsonValueKind.Null||v.ValueKind==JsonValueKind.String&&string.IsNullOrWhiteSpace(v.GetString()))return 0;
        return Number(v,true);
    }
    public static QuotaObservation Quota(JsonElement data,bool enterprise)
    {
        if(data.ValueKind!=JsonValueKind.Object)Fail();
        if(enterprise)
        {
            double Field(params string[] names)
            {
                double? found=null;foreach(string name in names)if(data.TryGetProperty(name,out var value)&&value.ValueKind!=JsonValueKind.Null)
                {double n=Number(value);if(found is not null&&found.Value!=n)Fail();found=n;}
                if(found is null)Fail();return found!.Value;
            }
            double limit=Field("limitNum","limit_num"),used=Field("credit","used_num","usedNum");
            if(used<0||limit<0&&limit!=-1)Fail();
            return limit==-1?new(-1,used,-1,true):new(limit,used,Math.Max(0,limit-used));
        }
        if(!data.TryGetProperty("Packages",out var packages)||packages.ValueKind!=JsonValueKind.Array||packages.GetArrayLength() is <1 or >64)Fail();
        double total=0,consumed=0,remaining=0;
        foreach(var row in packages.EnumerateArray())
        {
            if(row.ValueKind!=JsonValueKind.Object)Fail();
            if(!row.TryGetProperty("CycleRemainCapacity",out var remain)||remain.ValueKind!=JsonValueKind.String||string.IsNullOrWhiteSpace(remain.GetString()))Fail();
            double a=Capacity(row,"CycleTotalCapacity"),b=Capacity(row,"CycleUsedCapacity"),c=Capacity(row,"CycleRemainCapacity");
            if(a<0||b<0)Fail();total+=a;consumed+=b;remaining+=c;
        }
        if(!double.IsFinite(total)||!double.IsFinite(consumed)||!double.IsFinite(remaining))Fail();
        bool paid=false;if(data.TryGetProperty("IsPaidUser",out var p)){if(p.ValueKind is not(JsonValueKind.True or JsonValueKind.False))Fail();paid=p.GetBoolean();}
        return new(total,consumed,Math.Max(0,remaining),Paid:paid);
    }
    public static string[] PackageCodes(JsonElement summary)
    {
        var result=new List<string>();foreach(var row in summary.GetProperty("Packages").EnumerateArray())
        {
            if(row.TryGetProperty("PackageCode",out var c)&&c.ValueKind!=JsonValueKind.Null)
            {if(c.ValueKind!=JsonValueKind.String)Fail();string code=c.GetString()!.Trim();if(code.Length>128||code.Any(char.IsControl))Fail();if(code.Length>0&&!result.Contains(code,StringComparer.Ordinal))result.Add(code);}
        }
        return result.ToArray();
    }
    public static CreditPackage Package(JsonElement row)
    {
        if(row.ValueKind!=JsonValueKind.Object)Fail();
        string name="未命名积分包";if(row.TryGetProperty("PackageName",out var text)&&text.ValueKind==JsonValueKind.String&&!string.IsNullOrWhiteSpace(text.GetString()))name=text.GetString()!.Trim();
        if(name.Length>256||name.Any(char.IsControl))Fail();
        double remain=0;foreach(string field in new[]{"CycleCapacityRemainPrecise","CycleCapacityRemain","CapacityRemainPrecise","CapacityRemain"})
        {
            if(!row.TryGetProperty(field,out var value))continue;
            try{remain=Number(value);break;}catch(CredentialFailure){ }
        }
        long expiry=0;
        if(row.TryGetProperty("DeductionEndTime",out var exp))
        {
            try{double n=Number(exp);if(n>0&&n<long.MaxValue){long seconds=n>1e11?(long)n/1000:(long)n;if(seconds<=253402300799)expiry=seconds;}}
            catch(CredentialFailure){ }
        }
        return new(name,remain,expiry,expiry>0);
    }
    internal static void AddPackages(List<CreditPackage> output,HashSet<string> ids,JsonElement data)
    {
        if(!data.TryGetProperty("Accounts",out var rows)||rows.ValueKind!=JsonValueKind.Array||rows.GetArrayLength()>200)Fail();
        foreach(var row in rows.EnumerateArray())
        {
            if(output.Count>=64)break;
            if(row.TryGetProperty("AccountId",out var id)&&id.ValueKind==JsonValueKind.String)
            {string key=id.GetString()!;if(key.Length>256)Fail();if(key.Length>0&&!ids.Add(key))continue;}
            output.Add(Package(row));
        }
    }
}
