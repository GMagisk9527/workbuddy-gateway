using AccountScheduling;
namespace AccountLogin;

public static class LoginOrigins
{
    public static Uri Control(AccountSite site)=>site switch
    {AccountSite.Cn=>new("https://copilot.tencent.com/"),AccountSite.Intl=>new("https://www.workbuddy.ai/"),_=>throw new ArgumentException("Invalid login site")};
    public static bool AllowedChallenge(Uri uri,AccountSite site)=>uri.IsAbsoluteUri&&uri.Scheme=="https"&&uri.IsDefaultPort&&uri.UserInfo.Length==0&&
        (site==AccountSite.Cn?uri.Host is "copilot.tencent.com" or "www.codebuddy.cn" or "www.workbuddy.cn":uri.Host is "www.workbuddy.ai" or "workbuddy.ai" or "www.codebuddy.ai" or "codebuddy.ai");
}
