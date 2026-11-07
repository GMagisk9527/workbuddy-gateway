using KeySecurity;
using LedgerMigration;

namespace HttpMigration;

internal static class LedgerPresentation
{
    internal static string Status(KeyView p,CreditLedger? ledger)
    {
        if(!p.Enabled)return "disabled";if(p.ExpiresAt>0&&DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()>=p.ExpiresAt)return "expired";
        if(ledger is null||!ledger.Health.Healthy)return "unavailable";var u=ledger.GetUsage(p.Id);
        return p.QuotaTotal>0&&u.Total>=p.QuotaTotal||p.QuotaDaily>0&&u.Daily>=p.QuotaDaily||p.QuotaMonthly>0&&u.Monthly>=p.QuotaMonthly||p.Limited&&u.Unknown>=10?"exhausted":"active";
    }
    internal static object Usage(KeyView policy, CreditLedger ledger, bool legacy = false)
    {
        var u = ledger.GetUsage(policy.Id); double? limit = policy.QuotaTotal > 0 ? policy.QuotaTotal : null;
        double? remaining = limit is double n ? Math.Max(0, n - u.Total) : null;
        double? daily = policy.QuotaDaily > 0 ? Math.Max(0, policy.QuotaDaily - u.Daily) : null;
        double? monthly = policy.QuotaMonthly > 0 ? Math.Max(0, policy.QuotaMonthly - u.Monthly) : null;
        string status = !policy.Enabled ? "disabled" : policy.ExpiresAt > 0 && DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() >= policy.ExpiresAt ? "expired" :
            !ledger.Health.Healthy ? "unavailable" : remaining <= 0 || daily <= 0 || monthly <= 0 || policy.Limited && u.Unknown >= 10 ? "exhausted" : "active";
        return new
        {
            policy.Name,
            status,
            legacy,
            used = u.Total,
            u.Lifetime,
            limit,
            remaining,
            dailyUsed = u.Daily,
            dailyRemaining = daily,
            monthlyUsed = u.Monthly,
            monthlyRemaining = monthly,
            u.Requests,
            inputTokens = u.Input,
            outputTokens = u.Output,
            cachedTokens = u.Cached,
            unknownSettlements = u.Unknown,
            zeroCostSettlements = u.ZeroCost,
            u.LastUsedAt,
            policy.ExpiresAt,
            u.DayKey,
            u.MonthKey,
            ledger = ledger.Health
        };
    }
    internal static object Summary(KeyStore store, CreditLedger ledger)
    {
        var keys=store.List();var active = keys.Select(k => k.Id).ToHashSet(StringComparer.Ordinal);if(store.HasLegacy)active.Add("legacy");var all = ledger.AllUsage();
        return new
        {
            total=keys.Length+(store.HasLegacy?1:0),enabled=keys.Count(k=>k.Enabled)+(store.HasLegacy?1:0),
            allTimeCredits = all.Values.Sum(u => u.Lifetime),
            todayCredits = all.Values.Sum(u => u.SpentToday),
            archivedCredits = all.Where(p => !active.Contains(p.Key)).Sum(p => p.Value.Lifetime),
            ledger = ledger.Health
        };
    }
    internal static async Task HandleUsage(HttpContext c, LocalKeySecurity security)
    {
        c.Response.Headers.CacheControl = "no-store";
        try
        {
            var id = security.Store.Authenticate(KeyRules.Token(c.Request.Headers.Authorization.ToString(), c.Request.Headers["x-api-key"].ToString()));
            if (id is null) throw new KeyFailure(401, "invalid_api_key", "未提供有效 API 密钥");
            var policy = id.Legacy ? new KeyView("legacy", id.Name, id.Prefix, true, 0, 0, [], 0, 0, 0, 0, "") : security.Store.GetView(id.Id);
            await c.Response.WriteAsJsonAsync(Usage(policy, security.Ledger!, id.Legacy), c.RequestAborted);
        }
        catch (KeyFailure e)
        { c.Response.StatusCode = e.Status; await c.Response.WriteAsJsonAsync(new { error = new { type = e.Code, message = e.Message } }, c.RequestAborted); }
    }
}
