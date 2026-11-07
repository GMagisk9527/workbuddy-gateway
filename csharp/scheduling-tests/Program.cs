using System.Text.Json;
using System.Text.Json.Serialization;
using AccountScheduling;

namespace SchedulingChecks;

internal static class Program
{
    internal static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web) { Converters = { new JsonStringEnumConverter(JsonNamingPolicy.SnakeCaseLower) } };
    internal sealed record AccountSpec(string Id, AccountSite Site = AccountSite.Cn, bool CredentialsPresent = true, bool HasAccessToken = true,
        bool Disabled = false, long Cooldown = 0, bool Exhausted = false, ModelCost? Cost = null, long ModelCooldown = 0, bool Blocked = false,
        long NextProbe = 0, long CreditExpiry = 0, double Credit = 0, int Load = 0);
    internal sealed record Spec(string Name, AccountSpec[] Accounts, string Model = "m", int RoundRobin = 0, string? Preferred = null,
        string[]? Attempted = null, AccountSite[]? Free = null, AccountSite[]? Paid = null, string[]? Allow = null, string[]? Block = null,
        string[]? GlobalAllow = null, string[]? GlobalBlock = null, bool LoadAware = true, bool ExpiringFirst = false, string Op = "pick", bool ReservedProbe = false);
    internal static readonly DateTimeOffset Now = DateTimeOffset.FromUnixTimeSeconds(1800000000);
    internal static DateTimeOffset At(long seconds) => seconds == 0 ? default : Now.AddSeconds(seconds);
    internal static AccountSnapshot Snapshot(AccountSpec a) => new(new(a.Id), new("http://127.0.0.1:1/"), a.Site, a.CredentialsPresent, a.HasAccessToken, a.Disabled,
        At(a.Cooldown), a.Exhausted, a.Cost is null ? null : new Dictionary<string, AccountModelState> { ["m"] = new(a.Cost.Value, At(a.ModelCooldown), a.Blocked, At(a.NextProbe)) }, At(a.CreditExpiry), a.Credit);
    internal static HashSet<AccountId>? Ids(string[]? ids) => ids?.Select(x => new AccountId(x)).ToHashSet();
    static async Task Main(string[] args)
    {
        if (args is ["--self-test"])
        {
            var scheduling = await PoolTests.Run(); var http = await HttpPoolTests.Run();
            Console.WriteLine(JsonSerializer.Serialize(new { schedulingTests = scheduling.Count, httpTests = http.Count, scheduling, http }, Json)); return;
        }
        if (args is not ["--probe"]) throw new ArgumentException("Use --probe or --self-test");
        while (await Console.In.ReadLineAsync() is { } line)
        {
            var s = JsonSerializer.Deserialize<Spec>(line, Json) ?? throw new JsonException(); object result;
            var a = s.Accounts.Select(Snapshot).ToArray();
            if (s.Op == "eligibility") result = a.Select(x => SchedulingRules.Eligibility(x, s.Model, Now, s.ReservedProbe)).ToArray();
            else
            {
                var selected = SchedulingRules.Choose(a, s.Model, new(s.Free?.ToHashSet(), s.Paid?.ToHashSet(), Ids(s.Allow), Ids(s.Block)),
                    new(Allow: Ids(s.GlobalAllow), Block: Ids(s.GlobalBlock)), s.Accounts.ToDictionary(x => new AccountId(x.Id), x => x.Load), s.RoundRobin,
                    s.Preferred is null ? null : new(s.Preferred), Ids(s.Attempted), Now, new(LoadAware: s.LoadAware, ExpiringFirst: s.ExpiringFirst));
                result = new { id = selected?.Account.Id.Value, kind = selected?.Kind, expiring = selected?.Expiring ?? false };
            }
            Console.WriteLine(JsonSerializer.Serialize(new { s.Name, result }, Json));
        }
    }
}
