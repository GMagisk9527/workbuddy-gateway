using System.Globalization;
using System.Text.Json;
using System.Text.Json.Serialization;

namespace LedgerMigration;

public enum SettlementKind { Charged, Zero, Unknown, Duplicate, Cancelled }
public sealed record CreditObservation(SettlementKind Kind, double Credit, long Input, long Output, long Cached);
public sealed record CreditUsage(double Total = 0, double Daily = 0, double Monthly = 0, string DayKey = "", string MonthKey = "",
    long Requests = 0, [property: JsonPropertyName("inputTokens")] long Input = 0, [property: JsonPropertyName("outputTokens")] long Output = 0,
    [property: JsonPropertyName("cachedTokens")] long Cached = 0, [property: JsonPropertyName("unknownSettlements")] long Unknown = 0,
    [property: JsonPropertyName("zeroCostSettlements")] long ZeroCost = 0, double Lifetime = 0, double SpentToday = 0, string SpentDayKey = "", long LastUsedAt = 0);

public sealed record LedgerEntry(long Seq, string Key, string? Attempt = null, string Kind = "settle", double Credit = 0, bool Known = false,
    bool Zero = false, bool Unknown = false, long At = 0, [property: JsonPropertyName("day")] string DayKey = "",
    [property: JsonPropertyName("month")] string MonthKey = "", [property: JsonPropertyName("in")] long Input = 0,
    [property: JsonPropertyName("out")] long Output = 0, long Cached = 0, int Status = 0);
public sealed record LedgerHealth(bool Healthy, bool Incomplete, long Seq, int PendingAttempts);
public sealed class LedgerAttempt
{
    internal LedgerAttempt(Guid owner, string id, string key) { Owner = owner; Id = id; Key = key; }
    internal Guid Owner { get; }
    public string Id { get; }
    public string Key { get; }
}
public static class CreditRules
{
    static JsonElement Field(JsonElement e, string key) => e.ValueKind == JsonValueKind.Object && e.TryGetProperty(key, out var v) ? v : default;
    static double? Number(JsonElement e)
    {
        if (e.ValueKind == JsonValueKind.Number && e.TryGetDouble(out double value)) return value;
        if (e.ValueKind == JsonValueKind.String && double.TryParse(e.GetString(), NumberStyles.Float, CultureInfo.InvariantCulture, out value)) return value;
        return null;
    }
    static long? Token(JsonElement e)
    {
        if (e.ValueKind == JsonValueKind.String && long.TryParse(e.GetString(), NumberStyles.Integer, CultureInfo.InvariantCulture, out long n)) return n >= 0 ? n : null;
        if (Number(e) is double value && double.IsFinite(value) && value > -1 && value < long.MaxValue) { long truncated = (long)value; return truncated >= 0 ? truncated : null; }
        return null;
    }
    public static CreditObservation Observe(string? rawUsage)
    {
        if (rawUsage is null) return new(SettlementKind.Unknown, 0, 0, 0, 0);
        try
        {
            using var doc = JsonDocument.Parse(rawUsage); var root = doc.RootElement;
            double? credit = Number(Field(root, "credit"));
            var kind = credit is >= 0 and <= 1e308 ? credit > 0 ? SettlementKind.Charged : SettlementKind.Zero : SettlementKind.Unknown;
            long cached = 0;
            foreach (string key in new[] { "prompt_cache_hit_tokens", "cache_read_input_tokens", "cached_tokens" })
                if (Token(Field(root, key)) is long n) { cached = n; if (n > 0) break; }
            if (cached == 0)
                foreach (string key in new[] { "prompt_tokens_details", "input_tokens_details" })
                    if (Token(Field(Field(root, key), "cached_tokens")) is long n) { cached = n; if (n > 0) break; }
            return new(kind, kind == SettlementKind.Charged ? credit!.Value : 0, Token(Field(root, "prompt_tokens")) ?? 0, Token(Field(root, "completion_tokens")) ?? 0, cached);
        }
        catch (Exception e) when (e is JsonException or InvalidOperationException) { return new(SettlementKind.Unknown, 0, 0, 0, 0); }
    }
    public static (string Day, string Month) Window(DateTimeOffset at)
    { var cst = at.ToOffset(TimeSpan.FromHours(8)); return (cst.ToString("yyyy-MM-dd", CultureInfo.InvariantCulture), cst.ToString("yyyy-MM", CultureInfo.InvariantCulture)); }
    public static CreditUsage Roll(CreditUsage usage, DateTimeOffset now)
    {
        var window = Window(now);
        if (usage.SpentDayKey.Length == 0) usage = usage with { SpentDayKey = usage.DayKey, SpentToday = usage.Daily };
        return usage with
        {
            Daily = usage.DayKey == window.Day ? usage.Daily : 0,
            Monthly = usage.MonthKey == window.Month ? usage.Monthly : 0,
            SpentToday = usage.SpentDayKey == window.Day ? usage.SpentToday : 0,
            DayKey = window.Day,
            MonthKey = window.Month,
            SpentDayKey = window.Day
        };
    }
    public static CreditUsage Apply(CreditUsage usage, LedgerEntry entry, DateTimeOffset now)
    {
        usage = Roll(usage, now); var window = Window(now);
        if (entry.Kind == "reset") return usage with { Total = 0, Daily = 0, Monthly = 0, Unknown = 0 };
        if (entry.Kind == "recovery") return usage with { Unknown = checked(usage.Unknown + 1), LastUsedAt = Math.Max(usage.LastUsedAt, entry.At) };
        if (entry.Kind is "begin" or "cancel") return usage;
        double credit = entry.Known ? entry.Credit : 0;
        var next = usage with
        {
            Total = usage.Total + credit,
            Lifetime = usage.Lifetime + credit,
            Daily = usage.Daily + (entry.DayKey == window.Day ? credit : 0),
            Monthly = usage.Monthly + (entry.MonthKey == window.Month ? credit : 0),
            SpentToday = usage.SpentToday + (entry.DayKey == window.Day ? credit : 0),
            Requests = checked(usage.Requests + 1),
            Input = checked(usage.Input + entry.Input),
            Output = checked(usage.Output + entry.Output),
            Cached = checked(usage.Cached + entry.Cached),
            Unknown = checked(usage.Unknown + (entry.Unknown ? 1 : 0)),
            ZeroCost = checked(usage.ZeroCost + (entry.Zero ? 1 : 0)),
            LastUsedAt = Math.Max(usage.LastUsedAt, entry.At)
        };
        ValidateUsage(next); return next;
    }
    internal static void ValidateUsage(CreditUsage usage)
    {
        foreach (double n in new[] { usage.Total, usage.Daily, usage.Monthly, usage.Lifetime, usage.SpentToday })
            if (!double.IsFinite(n) || n < 0) throw new InvalidDataException("Invalid ledger amount");
        if (usage.Requests < 0 || usage.Input < 0 || usage.Output < 0 || usage.Cached < 0 || usage.Unknown < 0 || usage.ZeroCost < 0 || usage.DayKey is null || usage.MonthKey is null || usage.SpentDayKey is null)
            throw new InvalidDataException("Invalid ledger usage");
    }
}
