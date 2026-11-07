using System.Buffers;
using System.Collections.Immutable;
using System.Security.Cryptography;
using System.Text;

namespace KeySecurity;

public sealed class KeyFailure(int status, string code, string message) : Exception(message)
{ public int Status { get; } = status; public string Code { get; } = code; }
public sealed record KeyOptions(string Name = "", double QuotaTotal = 0, double QuotaDaily = 0, double QuotaMonthly = 0,
    long ExpiresAt = 0, string[]? Models = null, int MaxConcurrent = 0, string Note = "");
public sealed record KeyView(string Id, string Name, string Prefix, bool Enabled, long CreatedAt, long ExpiresAt,
    ImmutableArray<string> Models, int MaxConcurrent, double QuotaTotal, double QuotaDaily, double QuotaMonthly, string Note)
{
    public int Concurrency => MaxConcurrent > 0 ? MaxConcurrent : 1;
    public bool Limited => QuotaTotal > 0 || QuotaDaily > 0 || QuotaMonthly > 0;
}
public sealed record KeyIdentity(string Id, string Prefix, string Name, bool Legacy = false);
public sealed class IssuedKey(KeyView item, string secret)
{
    string? secret = secret;
    public KeyView Item { get; } = item;
    public string TakeSecret() => Interlocked.Exchange(ref secret, null) ?? throw new InvalidOperationException("Secret already retrieved");
    public override string ToString() => "IssuedKey (secret redacted)";
}
public sealed class Credential
{
    readonly byte[] hash;
    public Credential(string token)
    {
        if (string.IsNullOrWhiteSpace(token) || token.Length > 4096) throw new ArgumentException("Invalid credential");
        hash = SHA256.HashData(Encoding.UTF8.GetBytes(token));
    }
    public bool Matches(string token) => token.Length <= 4096 && CryptographicOperations.FixedTimeEquals(hash, SHA256.HashData(Encoding.UTF8.GetBytes(token)));
    public override string ToString() => "Credential (redacted)";
}

public static class KeyRules
{
    public static string Model(string name) => string.Concat(name.Trim().EnumerateRunes().Select(r => (r.Value == 0x130 ? new Rune('i') : Rune.ToLowerInvariant(r)).ToString()));
    public static bool Allows(IEnumerable<string> models, string model)
    { string[] values = models.ToArray(); return values.Length == 0 || values.Any(m => Model(m) == Model(model)); }
    static string BytePrefix(string text, int length)
    {
        byte[] utf8 = Encoding.UTF8.GetBytes(text);
        if (utf8.Length <= length) return text;
        var result = new StringBuilder(); ReadOnlySpan<byte> bytes = utf8.AsSpan(0, length);
        while (bytes.Length > 0)
        {
            if (Rune.DecodeFromUtf8(bytes, out var rune, out int consumed) == OperationStatus.Done) { result.Append(rune); bytes = bytes[consumed..]; }
            else { result.Append('\ufffd'); bytes = bytes[1..]; }
        }
        return result.ToString();
    }
    static int Utf8Compare(string a, string b)
    {
        var x = a.EnumerateRunes().GetEnumerator(); var y = b.EnumerateRunes().GetEnumerator();
        while (true) { bool p = x.MoveNext(), q = y.MoveNext(); if (!p || !q) return p ? 1 : q ? -1 : 0; int c = x.Current.Value.CompareTo(y.Current.Value); if (c != 0) return c; }
    }
    public static KeyOptions Normalize(KeyOptions input)
    {
        if (input.Name is null || input.Note is null || input.Models?.Any(x => x is null) == true ||
            !double.IsFinite(input.QuotaTotal) || !double.IsFinite(input.QuotaDaily) || !double.IsFinite(input.QuotaMonthly))
            throw new KeyFailure(400, "invalid_key_options", "密钥选项无效");
        string name = input.Name.Trim(); if (name.Length == 0) name = "未命名密钥";
        var models = (input.Models ?? []).Select(Model).Where(x => x.Length > 0).Distinct(StringComparer.Ordinal).Order(Comparer<string>.Create(Utf8Compare)).ToArray();
        return input with
        {
            Name = BytePrefix(name, 64),
            Note = BytePrefix(input.Note, 200),
            Models = models,
            QuotaTotal = Math.Max(0, input.QuotaTotal),
            QuotaDaily = Math.Max(0, input.QuotaDaily),
            QuotaMonthly = Math.Max(0, input.QuotaMonthly),
            MaxConcurrent = Math.Clamp(input.MaxConcurrent, 0, 64)
        };
    }
    public static string Base32(ReadOnlySpan<byte> input)
    {
        const string alphabet = "abcdefghijklmnopqrstuvwxyz234567"; var output = new StringBuilder(); uint acc = 0; int bits = 0;
        foreach (byte b in input) { acc = (acc << 8) | b; bits += 8; while (bits >= 5) { bits -= 5; output.Append(alphabet[(int)((acc >> bits) & 31)]); } }
        if (bits > 0) output.Append(alphabet[(int)((acc << (5 - bits)) & 31)]);
        return output.ToString();
    }
    public static string Hash(string secret) => Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes(secret)));
    public static string Prefix(string secret) => secret[..Math.Min(12, secret.Length)];
    public static byte[] ReasoningKey(string token) => SHA256.HashData(Encoding.UTF8.GetBytes("workbuddy-gateway/reasoning/key/v1\0" + token));
    public static string Token(string authorization, string alternate)
    {
        string token = authorization.StartsWith("Bearer ", StringComparison.Ordinal) ? authorization[7..] : authorization;
        return token.Length == 0 ? alternate : token;
    }
}
