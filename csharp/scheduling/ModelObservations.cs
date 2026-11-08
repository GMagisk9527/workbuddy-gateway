using System.Globalization;
using System.Text.Json;
using System.Text.RegularExpressions;

namespace AccountScheduling;

public enum ModelRejection { None, Quota, ModelRate, AccountRate }
public sealed record RejectionObservation(ModelRejection Kind, DateTimeOffset Until = default);

// Observations are evidence, never authorization to replay a request or disable a credential.
public static partial class ModelObservations
{
    static JsonElement Field(JsonElement e, string name) => e.ValueKind == JsonValueKind.Object && e.TryGetProperty(name, out var v) ? v : default;
    static string Text(JsonElement e) => e.ValueKind == JsonValueKind.String ? e.GetString()! : "";
    static string Code(JsonElement e) => e.ValueKind==JsonValueKind.Number&&e.TryGetInt32(out int n)?n.ToString(CultureInfo.InvariantCulture):Text(e);
    static double? Number(JsonElement e) => e.ValueKind == JsonValueKind.Number && e.TryGetDouble(out double n) ? n :
        e.ValueKind == JsonValueKind.String && double.TryParse(e.GetString(), NumberStyles.Float, CultureInfo.InvariantCulture, out n) ? n : null;
    static long? Tokens(JsonElement e)
    {
        if (e.ValueKind == JsonValueKind.String && long.TryParse(e.GetString(), NumberStyles.Integer, CultureInfo.InvariantCulture, out long n)) return n >= 0 ? n : null;
        return Number(e) is double d && double.IsFinite(d) && d >= 0 && d < long.MaxValue ? (long)d : null;
    }
    public static ModelCost Cost(string? raw)
    {
        if (raw is null || raw.Length > 65536) return ModelCost.Unknown;
        try
        {
            using var doc = JsonDocument.Parse(raw); var r = doc.RootElement;
            if (Number(Field(r, "credit")) is not double credit || !double.IsFinite(credit) || credit < 0) return ModelCost.Unknown;
            if (credit > 0) return ModelCost.Paid;
            long? total = Tokens(Field(r, "total_tokens"));
            if (total is null)
            {
                long? input = Tokens(Field(r, "prompt_tokens")), output = Tokens(Field(r, "completion_tokens"));
                if (input is not null || output is not null) total = input.GetValueOrDefault() > long.MaxValue - output.GetValueOrDefault() ? long.MaxValue : input.GetValueOrDefault() + output.GetValueOrDefault();
            }
            return total >= 100 ? ModelCost.Free : ModelCost.Unknown;
        }
        catch (JsonException) { return ModelCost.Unknown; }
    }
    public static RejectionObservation Failure(int status, string? code, string? raw, DateTimeOffset now)
    {
        if(code is "11128" or "11140")return new(ModelRejection.None);
        string message = "";
        if (raw is { Length: <= 65536 })
        {
            try
            {
                using var doc = JsonDocument.Parse(raw); var r = doc.RootElement;
                if(Code(Field(r,"code")) is "11128" or "11140")return new(ModelRejection.None);
                var error = Field(r, "error"); if (error.ValueKind == JsonValueKind.Object) r = error;
                string parsed=Code(Field(r,"code"));if(parsed.Length>0)code=parsed;
                message = Text(Field(r, "message")); if (message.Length == 0) message = Text(Field(r, "msg"));
            }
            catch (JsonException) { }
        }
        if (code is "11128" or "11140") return new(ModelRejection.None);
        bool Has(string text) => message.Contains(text, StringComparison.OrdinalIgnoreCase);
        if (code == "14018" || status == 429 && (Has("额度已用尽") || Has("credits exhausted"))) return new(ModelRejection.Quota);
        // A model-specific cooldown is justified only by a 429 response. Some
        // upstream 500/11134 bodies reuse the same suggestion text while the
        // model is merely unavailable; cooling it would drain the whole pool.
        bool model = status == 429 && (code == "6004" || code == "14003" || Has("切换其他模型") || Has("switch to another model") || Has("模型繁忙") || Has("model busy"));
        bool rate = model || status == 429 || code == "429" || Has("频率限制") || Has("frequency limit") || Has("rate limit") || Has("ratelimit") || Has("too many requests");
        if (!rate) return new(ModelRejection.None);
        var until = Reset(message);
        // A malformed, expired or unbounded server date must not permanently remove capacity.
        return new(model ? ModelRejection.ModelRate : ModelRejection.AccountRate,
            until is { } end && end > now ? (end > now.AddDays(1) ? now.AddDays(1) : end) : now.AddSeconds(60));
    }
    [GeneratedRegex(@"(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2}:\d{2})(?:\s*(UTC[+-]\d+(?::\d{2})?))?", RegexOptions.CultureInvariant, 100)]
    private static partial Regex ResetPattern();
    public static DateTimeOffset? Reset(string message)
    {
        if (message.Length > 65536) return null;
        var m = ResetPattern().Match(message); if (!m.Success) return null;
        if (!DateTime.TryParseExact(m.Groups[1].Value + " " + m.Groups[2].Value, "yyyy-MM-dd HH:mm:ss", CultureInfo.InvariantCulture, DateTimeStyles.None, out var date)) return null;
        int offset = 480; string zone = m.Groups[3].Value;
        if (zone.Length > 0)
        {
            string[] parts = zone[4..].Split(':');
            if (!int.TryParse(parts[0], NumberStyles.None, CultureInfo.InvariantCulture, out int hour) || hour > 14 ||
                parts.Length > 1 && (!int.TryParse(parts[1], NumberStyles.None, CultureInfo.InvariantCulture, out int minute) || minute > 59)) return null;
            int mins = parts.Length > 1 ? int.Parse(parts[1], CultureInfo.InvariantCulture) : 0;
            offset = (zone[3] == '-' ? -1 : 1) * (hour * 60 + mins); if (Math.Abs(offset) > 840) return null;
        }
        try { return new DateTimeOffset(date, TimeSpan.FromMinutes(offset)); } catch (ArgumentException) { return null; }
    }
}
