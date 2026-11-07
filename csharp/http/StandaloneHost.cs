using System.Text.Json;
using System.Text.Json.Serialization;
using KeySecurity;
using LedgerMigration;
using AccountMaintenance;
namespace HttpMigration;

internal static partial class Program
{
    /// <summary>Identity options from WORKBUDDY_IDENTITY_*; null keeps the Go-matching defaults.</summary>
    internal static IdentityOptions? IdentityFromEnvironment()
    {
        string? enabledRaw=Environment.GetEnvironmentVariable("WORKBUDDY_IDENTITY_ENABLED");
        string target=Environment.GetEnvironmentVariable("WORKBUDDY_IDENTITY_TARGET")??"";
        string? replacementsRaw=Environment.GetEnvironmentVariable("WORKBUDDY_IDENTITY_REPLACEMENTS");
        if(string.IsNullOrWhiteSpace(enabledRaw)&&target.Length==0&&string.IsNullOrWhiteSpace(replacementsRaw))return null;
        bool? enabled=enabledRaw?.Trim().ToLowerInvariant() switch
        {
            null or ""=>null,"1" or "true"=>true,"0" or "false"=>false,
            _=>throw new ArgumentException("WORKBUDDY_IDENTITY_ENABLED must be true or false"),
        };
        Dictionary<string,string>? replacements=null;
        if(!string.IsNullOrWhiteSpace(replacementsRaw))
        {
            using var doc=JsonDocument.Parse(replacementsRaw);
            if(doc.RootElement.ValueKind!=JsonValueKind.Object)throw new ArgumentException("WORKBUDDY_IDENTITY_REPLACEMENTS must be a JSON object");
            replacements=[];
            foreach(var p in doc.RootElement.EnumerateObject())replacements[p.Name]=p.Value.GetString()??"";
        }
        return new IdentityOptions(enabled,target,replacements);
    }

    internal static async Task RunStandalone(string[] args)
    {
            string standaloneCredential=args[1],standaloneKeys=args[2],standalonePort=args[3];
            var catalog=new ModelCatalog();
            if(args.Length==5)
            {
                try{catalog.LoadFile(args[4]);}
                catch(Exception e) when(e is IOException or UnauthorizedAccessException or JsonException or ArgumentException or KeyNotFoundException or InvalidOperationException or FormatException or OverflowException)
                {Console.Error.WriteLine("catalog_seed_unavailable");}
            }
            var admin=new Credential(Environment.GetEnvironmentVariable("WORKBUDDY_STANDALONE_ADMIN_KEY") ?? throw new ArgumentException("Set WORKBUDDY_STANDALONE_ADMIN_KEY"));
            using var runtime=args[0]=="--standalone"?StandaloneAccounts.OpenSingle(standaloneCredential):StandaloneAccounts.OpenManifest(standaloneCredential);
            var endpoint=runtime.Upstream;var pool=runtime.Pool;var credentials=runtime.Credentials;
            var tasks=StandaloneTaskOptions.FromEnvironment();
            using var keys=PreviewKeyDirectory.Open(standaloneKeys,DeploymentSupport.Legacy()); using var ledger=CreditLedger.Open(standaloneKeys,keys.List().Select(k=>k.Id));
            using var cache=OpenCatalogCache(Path.Combine(standaloneKeys,"model-catalog"));
            using var resourceCache=OpenResourceCache(Path.Combine(standaloneKeys,"account-resources"));
            using var runtimeCache=OpenRuntimeCache(Path.Combine(standaloneKeys,"model-runtime"));
            var prompts=new SystemPromptOptions(Environment.GetEnvironmentVariable("WORKBUDDY_SYSTEM_FALLBACK")??"You are a helpful assistant.",Environment.GetEnvironmentVariable("WORKBUDDY_SYSTEM_FORCE")??"",IdentityFromEnvironment());
            await using var app=LocalGateway.Build(new(endpoint,new byte[32],Port:int.Parse(standalonePort),MaxPreparing:0,MaxConcurrent:DeploymentSupport.Number("WORKBUDDY_MAX_ACTIVE",2,1,64),HeaderTimeout:TimeSpan.FromSeconds(DeploymentSupport.Number("WORKBUDDY_HEADER_SECONDS",300,1,3600)),IdleTimeout:TimeSpan.FromSeconds(DeploymentSupport.Number("WORKBUDDY_IDLE_SECONDS",120,1,3600)),Security:new(keys,admin,ledger),Accounts:pool,Credentials:credentials,DirectUpstream:true,Catalog:catalog,CatalogRefreshInterval:tasks.Catalog,CatalogCache:cache,SystemPrompts:prompts,Affinity:tasks.Affinity,Maintenance:tasks.Maintenance,Resources:tasks.Resources,ResourceCache:resourceCache,RuntimeCache:runtimeCache,ConsoleDirectory:Path.Combine(standaloneKeys,"console"),Calendar:CalendarMaintenanceOptions.FromEnvironment(Path.Combine(standaloneKeys,"scheduled-maintenance")),AccountOwner:runtime,CredentialReloadInterval:runtime.CanReload?tasks.Reload:null,PriceProbe:tasks.PriceProbe,NpmCatalog:true));
            await app.StartAsync(); Console.WriteLine(JsonSerializer.Serialize(new StandaloneInfo(LocalGateway.Address(app),"csharp-standalone-experimental",pool.Count),StandaloneJson.Default.StandaloneInfo)); await app.WaitForShutdownAsync(); return;
    }
    static CatalogCache? OpenCatalogCache(string directory)
    {
        try{return CatalogCache.Open(directory);}
        catch(Exception e) when(e is IOException or UnauthorizedAccessException or ArgumentException)
        {Console.Error.WriteLine("catalog_cache_unavailable");return null;}
    }
    static ResourceSnapshotCache? OpenResourceCache(string directory)
    {try{return ResourceSnapshotCache.Open(directory);}catch(Exception e) when(e is IOException or UnauthorizedAccessException or ArgumentException){Console.Error.WriteLine("resource_cache_unavailable");return null;}}
    static RuntimeSnapshotCache? OpenRuntimeCache(string directory)
    {try{return RuntimeSnapshotCache.Open(directory);}catch(Exception e) when(e is IOException or UnauthorizedAccessException or ArgumentException){Console.Error.WriteLine("runtime_cache_unavailable");return null;}}
}

internal sealed record StandaloneInfo(string Address,string Mode,int Accounts);
[JsonSourceGenerationOptions(PropertyNamingPolicy=JsonKnownNamingPolicy.CamelCase)]
[JsonSerializable(typeof(StandaloneInfo))]
internal partial class StandaloneJson:JsonSerializerContext {}
