namespace AccountScheduling;

public static class SiteEndpoints
{
    public static Uri EnterpriseUsage(AccountSite site)=>new(Verify(site),"/v2/billing/meter/get-enterprise-user-usage");
    public static Uri CreditPackages(AccountSite site,bool paid)=>new(Catalog(site),paid?"/billing/meter/get-user-resource-paid-packages":"/billing/meter/get-user-resource-free-packages");
    public static Uri AgentTool(AccountSite site,bool fetch) => new(Model(site),fetch?"/agenttool/v1/webfetch":"/agenttool/v1/search");
    public static Uri Catalog(AccountSite site) => new(site switch
    {
        AccountSite.Cn => "https://copilot.tencent.com/v2/enterprises/personal/models",
        AccountSite.Intl => "https://www.workbuddy.ai/v2/enterprises/personal/models",
        _ => throw new ArgumentException("Invalid site")
    });
    public static Uri Model(AccountSite site) => new(site switch
    {
        AccountSite.Cn => "https://www.workbuddy.cn/v2/chat/completions",
        AccountSite.Intl => "https://www.workbuddy.ai/v2/chat/completions",
        _ => throw new ArgumentException("Invalid site")
    });
    public static Uri Refresh(AccountSite site) => new(site switch
    {
        AccountSite.Cn => "https://copilot.tencent.com/v2/plugin/auth/token/refresh",
        AccountSite.Intl => "https://www.workbuddy.ai/v2/plugin/auth/token/refresh",
        _ => throw new ArgumentException("Invalid site")
    });
    public static Uri Verify(AccountSite site) => new(site switch
    {
        AccountSite.Cn => "https://www.codebuddy.cn/billing/meter/get-user-resource-summary",
        AccountSite.Intl => "https://www.workbuddy.ai/billing/meter/get-user-resource-summary",
        _ => throw new ArgumentException("Invalid site")
    });
    public static bool Allowed(Uri endpoint, AccountSite site) => Enum.IsDefined(site) && endpoint.IsAbsoluteUri && endpoint.UserInfo.Length == 0 && endpoint.Fragment.Length == 0 &&
        (endpoint.Scheme == "http" && endpoint.IsLoopback || endpoint == Model(site));
}
