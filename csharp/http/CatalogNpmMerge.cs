using System.Text.Json;
using AccountScheduling;
namespace HttpMigration;

public sealed partial class ModelCatalog
{
    public void LoadNpm(AccountSite site,ReadOnlyMemory<byte> bytes,bool liveSucceeded)
    {
        if(bytes.Length>4<<20)throw new ArgumentException("Npm catalog budget");
        using var doc=JsonDocument.Parse(bytes,new JsonDocumentOptions{MaxDepth=32});
        if(doc.RootElement.ValueKind!=JsonValueKind.Object||!doc.RootElement.TryGetProperty("models",out var models)||models.ValueKind!=JsonValueKind.Array||models.GetArrayLength() is <1 or >500)throw new ArgumentException("Invalid npm catalog");
        var staged=new ModelCatalog();staged.LoadLive(site,GatewayJson.Encode(new Dictionary<string, object?> { ["code"] = 0, ["data"] = doc.RootElement }));
        var fallback=(site==AccountSite.Cn?staged.state.Cn:staged.state.Intl);foreach(var row in fallback)row.FromLive=false;
        lock(publish)
        {
            var current=site==AccountSite.Cn?state.Cn:state.Intl;
            // Live values win; static entries supply absent models and capabilities.
            var rows=liveSucceeded?current.Select(row=>
            {
                var other=fallback.FirstOrDefault(r=>r.Id==row.Id);if(other is null)return row;
                return new Row{Id=row.Id,MaxInputTokens=row.MaxInputTokens>0?row.MaxInputTokens:other.MaxInputTokens,MaxAllowedSize=row.MaxAllowedSize>0?row.MaxAllowedSize:other.MaxAllowedSize,MaxOutputTokens=row.MaxOutputTokens>0?row.MaxOutputTokens:other.MaxOutputTokens,ContextLengths=row.ContextLengths is{Length:>0}?row.ContextLengths:other.ContextLengths,ContextDefaultLength=row.ContextDefaultLength>0?row.ContextDefaultLength:other.ContextDefaultLength,FromLive=true,HasMultiplier=row.HasMultiplier||other.HasMultiplier,Multiplier=row.HasMultiplier?row.Multiplier:other.Multiplier,PromoExpired=row.PromoExpired,PriceUpdatedAt=row.PriceUpdatedAt};
            }).Concat(fallback.Where(r=>!current.Any(c=>c.Id==r.Id))).ToArray():fallback;
            if(rows.Length>500)throw new ArgumentException("Merged catalog budget");
            foreach(var row in rows)Normalize(row);
            Volatile.Write(ref state,site==AccountSite.Cn?state with{Cn=rows,CnLive=liveSucceeded}:state with{Intl=rows,IntlLive=liveSucceeded});PublishPricing();
        }
    }
}
