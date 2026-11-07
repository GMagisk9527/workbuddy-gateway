using System.Security.Cryptography;
using System.Text.Json;
using KeySecurity;
using LedgerMigration;

namespace HttpMigration;

internal static class DeploymentSupport
{
    internal static bool Flag(string name,bool fallback)=>Environment.GetEnvironmentVariable(name) switch
    {null or ""=>fallback,"true" or "1"=>true,"false" or "0"=>false,_=>throw new ArgumentException("Invalid "+name)};
    internal static Credential? Legacy()=>Environment.GetEnvironmentVariable("WORKBUDDY_STANDALONE_API_KEY") is{Length:>0} token?new(token):null;
    internal static int Number(string name,int fallback,int min,int max)
    {
        string? raw=Environment.GetEnvironmentVariable(name);if(string.IsNullOrEmpty(raw))return fallback;
        if(!int.TryParse(raw,System.Globalization.NumberStyles.None,System.Globalization.CultureInfo.InvariantCulture,out int n)||n<min||n>max)throw new ArgumentException("Invalid "+name);return n;
    }
    internal static void Check(string accounts,string keyDirectory)
    {
        using var runtime=StandaloneAccounts.OpenManifest(accounts);using var keys=PreviewKeyDirectory.Open(keyDirectory,Legacy());using var ledger=CreditLedger.Open(keyDirectory,keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);
        if(!ledger.Health.Healthy||ledger.Health.PendingAttempts!=0||keys.Degraded||!keys.RequireAuth)throw new InvalidDataException("Imported stores are not ready");
        using var resources=ResourceSnapshotCache.Open(Path.Combine(keyDirectory,"account-resources"));resources.TryLoad(runtime.Credentials);
        using var cache=RuntimeSnapshotCache.Open(Path.Combine(keyDirectory,"model-runtime"));cache.TryLoad(runtime.Pool,runtime.Credentials);
        var usage=keys.List().Select(k=>k.Id).Append("legacy").Order(StringComparer.Ordinal).ToDictionary(id=>id,id=>ledger.GetUsage(id));
        Console.WriteLine(JsonSerializer.Serialize(new{ok=true,accounts=runtime.Pool.Count,disabled=runtime.Credentials.Status.Count(s=>s.Disabled),keys=keys.List().Length,ledger=ledger.Health,usageDigest=Convert.ToHexStringLower(SHA256.HashData(JsonSerializer.SerializeToUtf8Bytes(usage))),runtime=cache.Status,resources=resources.Status}));
    }
}
