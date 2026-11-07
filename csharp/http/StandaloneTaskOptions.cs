namespace HttpMigration;

// Standalone startup defaults match Go. Explicit deployment overrides survive.
internal sealed record StandaloneTaskOptions(TimeSpan? Catalog,CredentialMaintenanceOptions? Maintenance,
    AccountResourceOptions? Resources,TimeSpan? Reload,bool PriceProbe,AffinityOptions Affinity)
{
    internal static StandaloneTaskOptions FromEnvironment()
    {
        static TimeSpan? Interval(string name,int fallback,int minimum,int maximum)
        {
            int value=DeploymentSupport.Number(name,fallback,0,maximum);
            if(value!=0&&value<minimum)throw new ArgumentException("Invalid "+name);
            return value==0?null:TimeSpan.FromSeconds(value);
        }
        var catalog=Interval("WORKBUDDY_CATALOG_REFRESH_SECONDS",3600,10,86400);
        var maintenance=Interval("WORKBUDDY_MAINTENANCE_SECONDS",300,10,3600);
        var resources=Interval("WORKBUDDY_RESOURCES_SECONDS",300,30,86400);
        var reload=Interval("WORKBUDDY_RELOAD_SECONDS",5,1,3600);
        bool expiring=DeploymentSupport.Flag("WORKBUDDY_EXPIRING_FIRST",false);
        if(expiring&&resources is null)throw new ArgumentException("Expiring priority requires resource polling");
        var affinity=Environment.GetEnvironmentVariable("WORKBUDDY_AFFINITY") switch
        {"off"=>new AffinityOptions(),null or "" or "hybrid"=>new(true),"session"=>new(true,AffinityMode.Session),"prefix"=>new(true,AffinityMode.Prefix),_=>throw new ArgumentException("Invalid WORKBUDDY_AFFINITY")};
        return new(catalog,maintenance is{} m?new(m):null,resources is{} r?new(r,ExpiringFirst:expiring):null,reload,DeploymentSupport.Flag("WORKBUDDY_PRICE_PROBE",false),affinity);
    }
}
