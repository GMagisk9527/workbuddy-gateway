using System.Collections.Frozen;
using System.Globalization;
using System.Text;
using KeySecurity;

namespace AccountScheduling;

public sealed record AccountId
{
    public string Value { get; }
    public AccountId(string value)
    {
        if (string.IsNullOrWhiteSpace(value) || value.Length > 256 || value.Any(char.IsControl)) throw new ArgumentException("Invalid account identity");
        Value = value;
    }
    public override string ToString() => Value;
}
public enum AccountSite { Cn, Intl }
public enum ModelCost { Unknown, Free, Paid }
public enum SelectionKind { Normal, FreeExhausted, ProbeExhausted }
public sealed record AccountModelState(ModelCost Cost = ModelCost.Unknown, DateTimeOffset CooldownUntil = default,
    bool QuotaBlocked = false, DateTimeOffset NextProbeAt = default, DateTimeOffset ObservedAt = default);
// No credentials are stored here. Targets are loopback fixtures or site-pinned HTTPS.
public sealed record AccountSnapshot(AccountId Id, Uri Upstream, AccountSite Site = AccountSite.Cn,
    bool CredentialsPresent = true, bool HasAccessToken = true, bool Disabled = false, DateTimeOffset CooldownUntil = default,
    bool QuotaExhausted = false, IReadOnlyDictionary<string, AccountModelState>? Models = null,
    DateTimeOffset CreditExpiresAt = default, double ExpiringCredit = 0);
// Catalog observations and resolved account allow/block rules, not catalog acquisition.
public sealed record ModelPolicy(IReadOnlySet<AccountSite>? FreeSites = null, IReadOnlySet<AccountSite>? PaidSites = null,
    IReadOnlySet<AccountId>? Allow = null, IReadOnlySet<AccountId>? Block = null);
public sealed record SchedulingOptions(int MaxAccount = 4, int MaxGlobal = 0, int MaxQueued = 128,
    TimeSpan? WaitTimeout = null, TimeSpan? AffinityWait = null, bool LoadAware = true,
    bool ExpiringFirst = false, TimeSpan? CreditWindow = null,TimeSpan? CacheAffinityWait=null)
{
    internal TimeSpan Wait => WaitTimeout ?? TimeSpan.FromSeconds(15);
    internal TimeSpan Grace => AffinityWait ?? TimeSpan.FromMilliseconds(250);
    internal TimeSpan Window => CreditWindow ?? TimeSpan.FromDays(7);
    public TimeSpan CacheGrace=>CacheAffinityWait??TimeSpan.FromMilliseconds(1500);
    internal void Validate()
    {
        if (MaxAccount is < 1 or > 65536 || MaxGlobal is < 0 or > 1048576 || MaxQueued is < 0 or > 65536 ||
            Wait <= TimeSpan.Zero || Wait > TimeSpan.FromMinutes(15) || Grace < TimeSpan.Zero || Grace > TimeSpan.FromMinutes(15) ||
            Window <= TimeSpan.Zero || Window > TimeSpan.FromDays(3650)||CacheGrace<TimeSpan.Zero||CacheGrace>TimeSpan.FromMinutes(15)) throw new ArgumentException("Invalid scheduling limits");
    }
}
public sealed record AccountChoice(AccountSnapshot Account, SelectionKind Kind, bool Expiring);
public sealed record SchedulingStatus(int Active, int Waiting, IReadOnlyDictionary<AccountId, int> Loads);
public sealed record ModelRuntimeStatus(int TrackedModels,int FreeModels,int PaidModels,int CoolingModels,int QuotaBlockedModels,DateTimeOffset AccountCooldownUntil);
public sealed class SchedulingFailure(string code) : Exception(code) { public string Code { get; } = code; }

internal static class SchedulingCopy
{
    public static string Model(string value)
    {
        value = KeyRules.Model(value);
        if (value.Length == 0 || Encoding.UTF8.GetByteCount(value) > 256 || value.EnumerateRunes().Any(r =>
            Rune.IsWhiteSpace(r) || Rune.GetUnicodeCategory(r) is UnicodeCategory.Control or UnicodeCategory.Format or UnicodeCategory.Surrogate))
            throw new ArgumentException("Invalid scheduling model");
        return value;
    }
    public static FrozenDictionary<string, AccountModelState> States(IReadOnlyDictionary<string, AccountModelState>? states)
    {
        if (states is { Count: > 1024 }) throw new ArgumentException("Too many model states");
        return (states ?? new Dictionary<string, AccountModelState>()).ToFrozenDictionary(p => Model(p.Key), p =>
            p.Value is not null && Enum.IsDefined(p.Value.Cost) ? p.Value : throw new ArgumentException("Invalid model state"), StringComparer.Ordinal);
    }
    public static AccountSnapshot Account(AccountSnapshot a)
    {
        ArgumentNullException.ThrowIfNull(a.Id);
        if (!Enum.IsDefined(a.Site) || !SiteEndpoints.Allowed(a.Upstream, a.Site) ||
            !Enum.IsDefined(a.Site) || !double.IsFinite(a.ExpiringCredit) || a.ExpiringCredit < 0) throw new ArgumentException("Invalid synthetic account");
        return a with { Models = States(a.Models) };
    }
    public static ModelPolicy Policy(ModelPolicy p) => p with
    {
        FreeSites = CopySites(p.FreeSites),
        PaidSites = CopySites(p.PaidSites),
        Allow = p.Allow?.ToFrozenSet(),
        Block = p.Block?.ToFrozenSet()
    };
    static FrozenSet<AccountSite>? CopySites(IReadOnlySet<AccountSite>? sites)
    {
        if (sites?.Any(s => !Enum.IsDefined(s)) == true) throw new ArgumentException("Invalid site");
        return sites?.ToFrozenSet();
    }
}
