using System.Collections.Immutable;
using System.Globalization;
using System.Text;
using System.Text.Json;

namespace StreamingMigration;

public sealed record ToolFragment(int Index, string Id, string Name, string Arguments, bool Named = false);
public sealed record Choice(string Reasoning, string Text, string Refusal, ImmutableArray<ToolFragment> Tools, string Finish, bool OtherOutput = false);
public sealed record UsageObservation(Usage Tokens, string RawJson);
public sealed record ChatChunk(string Model, ImmutableArray<Choice> Choices, UsageObservation? Usage)
{
    public bool HasOutput => Choices.Any(c => c.Text.Length > 0 || c.Reasoning.Length > 0 || c.Refusal.Length > 0 || c.OtherOutput || c.Tools.Any(t => t.Named || t.Arguments.Length > 0));
}
public sealed class ConversionFailure(string code) : Exception(code)
{
    public string Code { get; } = code;
    public string? RawUsageJson { get; init; }
}

// Leniency for unknown fields and wrong optional field types matches the Go adapter.
public static class ChunkReader
{
    static JsonElement Field(JsonElement e, string key) => e.ValueKind == JsonValueKind.Object && e.TryGetProperty(key, out var v) ? v : default;
    static string Text(JsonElement e)
    {
        if (e.ValueKind != JsonValueKind.String) return "";
        try { return e.GetString() ?? ""; }
        catch (InvalidOperationException)
        {
            // Go replaces isolated JSON surrogate escapes with U+FFFD. .NET
            // accepts their JSON syntax but GetString throws; keep this repair
            // at the wire boundary, without touching normal strings or code.
            string raw = e.GetRawText(); var repaired = new StringBuilder(raw.Length);
            for (int i = 0; i < raw.Length;)
            {
                if (raw[i] != '\\') { repaired.Append(raw[i++]); continue; }
                if (raw[i + 1] != 'u') { repaired.Append(raw.AsSpan(i, 2)); i += 2; continue; }
                int n = int.Parse(raw.AsSpan(i + 2, 4), NumberStyles.HexNumber, CultureInfo.InvariantCulture);
                if (n is >= 0xd800 and <= 0xdbff && i + 12 <= raw.Length && raw.AsSpan(i + 6, 2).SequenceEqual("\\u") &&
                    int.Parse(raw.AsSpan(i + 8, 4), NumberStyles.HexNumber, CultureInfo.InvariantCulture) is >= 0xdc00 and <= 0xdfff)
                { repaired.Append(raw.AsSpan(i, 12)); i += 12; }
                else { if (n is >= 0xd800 and <= 0xdfff) repaired.Append("\\ufffd"); else repaired.Append(raw.AsSpan(i, 6)); i += 6; }
            }
            using var doc = JsonDocument.Parse(repaired.ToString()); return doc.RootElement.GetString() ?? "";
        }
    }
    static long? Token(JsonElement e)
    {
        if (e.ValueKind == JsonValueKind.String && long.TryParse(Text(e), NumberStyles.Integer, CultureInfo.InvariantCulture, out long exact)) return exact >= 0 ? exact : null;
        double n;
        if (e.ValueKind == JsonValueKind.Number && e.TryGetDouble(out n) ||
            e.ValueKind == JsonValueKind.String && double.TryParse(Text(e), NumberStyles.Float, CultureInfo.InvariantCulture, out n))
            return double.IsFinite(n) && n >= 0 && n < long.MaxValue ? (long)n : null;
        return null;
    }
    static long Integer(JsonElement e) => e.ValueKind == JsonValueKind.Number && e.TryGetDouble(out double n) && double.IsFinite(n) && n >= long.MinValue && n < long.MaxValue ? (long)n : 0;
    static IEnumerable<JsonElement> Items(JsonElement e) => e.ValueKind == JsonValueKind.Array ? e.EnumerateArray() : [];
    public static ChatChunk Parse(string payload)
    {
        try
        {
            using var doc = JsonDocument.Parse(payload, new JsonDocumentOptions { MaxDepth = 10000 });
            var root = doc.RootElement;
            if (root.ValueKind is not (JsonValueKind.Object or JsonValueKind.Null)) throw new ConversionFailure("invalid_json");
            var error = Field(root, "error");
            var obj = error.ValueKind == JsonValueKind.Object && (Code(Field(error, "code")) == "11128" || Field(root, "code").ValueKind is JsonValueKind.Undefined or JsonValueKind.Null) ? error : root;
            string code = Code(Field(obj, "code"));
            ConversionFailure Failure(string failure) => new(failure) { RawUsageJson = Field(root,"usage").ValueKind==JsonValueKind.Object?Field(root,"usage").GetRawText():null };
            if (code is "11128" or "11140" or "6004" or "14018" or "429") throw Failure(code);
            if (error.ValueKind == JsonValueKind.Object)
            {
                string failure = Code(Field(error, "code"));
                if (failure == "") failure = Text(Field(error, "code"));
                if (failure.Length == 0 || failure.Length > 128 || failure.Any(char.IsControl)) failure = "upstream_error";
                throw Failure(failure);
            }
            var u = Field(root, "usage");
            UsageObservation? usage = null;
            if (u.ValueKind == JsonValueKind.Object)
            {
                long cached = 0;
                foreach (string key in new[] { "prompt_cache_hit_tokens", "cache_read_input_tokens", "cached_tokens" })
                    if (Token(Field(u, key)) is long n) { cached = n; if (n > 0) break; }
                if (cached == 0)
                    foreach (string key in new[] { "prompt_tokens_details", "input_tokens_details" })
                        if (Token(Field(Field(u, key), "cached_tokens")) is long n) { cached = n; if (n > 0) break; }
                long input = Integer(Field(u, "prompt_tokens"));
                cached = Math.Min(cached, input);
                usage = new(new(input - cached, Integer(Field(u, "completion_tokens")), cached, Math.Max(0, Integer(Field(u, "cache_creation_input_tokens")))), u.GetRawText());
            }
            return new(Text(Field(root, "model")), Items(Field(root, "choices")).Where(x => x.ValueKind == JsonValueKind.Object).Select(c =>
            {
                var d = Field(c, "delta");
                var tools = Items(Field(d, "tool_calls")).Where(x => x.ValueKind == JsonValueKind.Object).Select(t =>
                {
                    var fn = Field(t, "function");
                    return new ToolFragment((int)Integer(Field(t, "index")), Text(Field(t, "id")), Text(Field(fn, "name")), Text(Field(fn, "arguments")), Field(fn, "name").ValueKind == JsonValueKind.String);
                }).ToImmutableArray();
                var legacy = Field(d, "function_call");
                return new Choice(Text(Field(d, "reasoning_content")), Text(Field(d, "content")), Text(Field(d, "refusal")), tools, Text(Field(c, "finish_reason")),
                    Text(Field(d, "reasoning")).Length > 0 || Field(legacy, "name").ValueKind == JsonValueKind.String || Text(Field(legacy, "arguments")).Length > 0);
            }).ToImmutableArray(), usage);
        }
        catch (JsonException) { throw new ConversionFailure("invalid_json"); }
    }
    static string Code(JsonElement e) => e.ValueKind == JsonValueKind.String && uint.TryParse(Text(e), NumberStyles.None, CultureInfo.InvariantCulture, out uint v) ? v.ToString(CultureInfo.InvariantCulture) :
        e.ValueKind == JsonValueKind.Number && e.TryGetDouble(out double n) && n is >= 0 and <= 999999999 && n == Math.Truncate(n) ? n.ToString("0", CultureInfo.InvariantCulture) : "";
    public static void ValidateArguments(string args)
    {
        if (args.Length == 0) return;
        try { using var doc = JsonDocument.Parse(args, new JsonDocumentOptions { MaxDepth = 10000 }); if (doc.RootElement.ValueKind != JsonValueKind.Object) throw new ConversionFailure("invalid_tool_json"); }
        catch (JsonException) { throw new ConversionFailure("invalid_tool_json"); }
    }
}
