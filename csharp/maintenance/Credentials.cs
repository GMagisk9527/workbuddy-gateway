using System.Text;
using System.Text.Json;
using AccountScheduling;

namespace AccountMaintenance;

public enum RefreshFailureKind { Transient, SessionDead }
public enum CredentialVerification { Usable, Rejected, Unknown }
public sealed class CredentialFailure(string code, RefreshFailureKind kind = RefreshFailureKind.Transient) : Exception(code)
{ public string Code { get; } = code; public RefreshFailureKind Kind { get; } = kind; }
public sealed record AccountIdentity(string Uid, string EnterpriseId = "", string Nickname = "", AccountSite Site = AccountSite.Cn);

// Public JSON/debug views never include token material. Export requires an explicit codec call.
public sealed class AccountCredential
{
    internal string Access { get; }
    internal string Refresh { get; }
    internal string Domain { get; }
    public AccountIdentity Identity { get; }
    public long ExpiresAt { get; }
    public long RefreshExpiresAt { get; }
    public long LastRefreshTime { get; }
    public bool CanRefresh => Refresh.Length != 0;
    public AccountCredential(AccountIdentity identity, string access, string refresh, long expiresAt, string domain = "", long refreshExpiresAt = 0, long lastRefreshTime = 0)
    {
        ArgumentNullException.ThrowIfNull(identity);
        foreach (string v in new[] { access, refresh, domain, identity.Uid, identity.EnterpriseId, identity.Nickname })
            if (v is null || v.Length > 32768 || v.Any(char.IsControl)) throw new CredentialFailure("invalid_credential");
        if (!Enum.IsDefined(identity.Site) || expiresAt < 0 || refreshExpiresAt < 0 || lastRefreshTime < 0) throw new CredentialFailure("invalid_credential");
        Identity = identity; Access = access; Refresh = refresh; Domain = domain; ExpiresAt = expiresAt; RefreshExpiresAt = refreshExpiresAt; LastRefreshTime = lastRefreshTime;
    }
    public override string ToString() => "AccountCredential (redacted)";
}

public static class CredentialRules
{
    public static bool NeedsRefresh(long expiresAt, long now) => now > expiresAt - 900;
    public static RefreshFailureKind Classify(string body)
    {
        string low = body.ToLowerInvariant();
        return new[] { "12153", "offline user session not found", "invalid_grant", "token is not active", "refresh token failed", "session not found" }.Any(low.Contains)
            ? RefreshFailureKind.SessionDead : RefreshFailureKind.Transient;
    }
    public static AccountSite Site(string edition) => edition.Trim().ToLowerInvariant() is "intl" or "international" or "global" or "workbuddy.ai" ? AccountSite.Intl : AccountSite.Cn;
    public static bool AuthenticationRejected(int status, string body)
    {
        string low = body.ToLowerInvariant();
        return status == 401 || new[] { "invalid token", "unauthorized", "登录已过期", "登录失效", "token 已失效", "authentication required" }.Any(low.Contains);
    }
    internal static JsonElement? Claims(string token)
    {
        var parts = token.Trim().Split('.'); if (parts.Length != 3 || parts[1].Length > 65536) return null;
        if (parts[1].TrimEnd('=').Any(c => !char.IsAsciiLetterOrDigit(c) && c is not ('_' or '-'))) return null;
        try
        {
            string raw = parts[1].TrimEnd('=').Replace('-', '+').Replace('_', '/'); raw += new string('=', (4 - raw.Length % 4) % 4);
            using var doc = JsonDocument.Parse(Convert.FromBase64String(raw)); return doc.RootElement.ValueKind == JsonValueKind.Object ? doc.RootElement.Clone() : null;
        }
        catch (FormatException) { return null; }
        catch (JsonException) { return null; }
    }
    public static long JwtExpiry(string token)
    {
        var claims = Claims(token);
        if (claims is { } c && c.TryGetProperty("exp", out var exp) && exp.ValueKind == JsonValueKind.Number && exp.TryGetDouble(out var n) && n > 0 && n < long.MaxValue) return (long)n;
        return 0;
    }
    internal static (string Id, string Realm) JwtIdentity(string token)
    {
        var c = Claims(token); string Get(string key) => c is { } value && value.TryGetProperty(key, out var v) && v.ValueKind == JsonValueKind.String ? v.GetString()! : "";
        string iss = Get("iss"); int index = iss.IndexOf("/auth/realms/", StringComparison.Ordinal);
        return (Get("sub"), index >= 0 ? iss[(index + 13)..] : iss);
    }
    public static bool SameIdentity(AccountCredential old, AccountCredential next)
    {
        if (old.Identity.Uid != next.Identity.Uid || old.Identity.EnterpriseId != next.Identity.EnterpriseId || old.Identity.Site != next.Identity.Site) return false;
        var a = JwtIdentity(old.Access); var b = JwtIdentity(next.Access);
        return (a.Id == "" || b.Id == "" || a.Id == b.Id) && (a.Realm == "" || b.Realm == "" || a.Realm == b.Realm);
    }
    // Matches refreshTokenPayload field preservation; claims are metadata, never proof of authentication.
    public static AccountCredential Merge(AccountCredential old, ReadOnlySpan<byte> data, long now)
    {
        var wire = JsonSerializer.Deserialize<TokenWire>(data, CredentialCodec.Json) ?? throw new CredentialFailure("invalid_refresh_response");
        if (string.IsNullOrEmpty(wire.AccessToken)) throw new CredentialFailure("invalid_refresh_response");
        long Add(long seconds) { try { return checked(now + seconds); } catch (OverflowException) { throw new CredentialFailure("invalid_refresh_expiry"); } }
        string refresh = string.IsNullOrEmpty(wire.RefreshToken) ? old.Refresh : wire.RefreshToken;
        return new(old.Identity, wire.AccessToken, refresh, wire.ExpiresIn > 0 ? Add(wire.ExpiresIn) : old.ExpiresAt,
            string.IsNullOrEmpty(wire.Domain) ? old.Domain : wire.Domain,
            wire.RefreshExpiresIn > 0 ? Add(wire.RefreshExpiresIn) : JwtExpiry(refresh) is > 0 and var exp ? exp : old.RefreshExpiresAt, now);
    }
    sealed class TokenWire
    {
        public string? AccessToken { get; set; }
        public string? RefreshToken { get; set; }
        public string? Domain { get; set; }
        public long ExpiresIn { get; set; }
        public long RefreshExpiresIn { get; set; }
    }
}

public static class CredentialCodec
{
    internal static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web) { PropertyNameCaseInsensitive = true };
    sealed class Wire
    {
        public Tokens? Auth { get; set; }
        public User? Account { get; set; }
        public string? Edition { get; set; }
    }
    sealed class Tokens
    {
        public string AccessToken { get; set; } = "";
        public string RefreshToken { get; set; } = "";
        public long ExpiresAt { get; set; }
        public string Domain { get; set; } = "";
        public long RefreshExpiresAt { get; set; }
        public long LastRefreshTime { get; set; }
    }
    sealed class User
    {
        public string Uid { get; set; } = "";
        public string EnterpriseId { get; set; } = "";
        public string Nickname { get; set; } = "";
    }
    public static AccountCredential Decode(ReadOnlySpan<byte> bytes)
    {
        if (bytes.Length > 1 << 20) throw new CredentialFailure("credential_document_too_large");
        try
        {
            var wire = JsonSerializer.Deserialize<Wire>(bytes, Json);
            if (wire?.Auth is not { } a || wire.Account is not { } u) throw new CredentialFailure("invalid_credential_document");
            return new(new(u.Uid, u.EnterpriseId, u.Nickname, CredentialRules.Site(wire.Edition ?? "")), a.AccessToken, a.RefreshToken, a.ExpiresAt, a.Domain, a.RefreshExpiresAt, a.LastRefreshTime);
        }
        catch (JsonException) { throw new CredentialFailure("invalid_credential_document"); }
    }
    public static byte[] Export(AccountCredential credential) => JsonSerializer.SerializeToUtf8Bytes(new Wire
    {
        Auth = new() { AccessToken = credential.Access, RefreshToken = credential.Refresh, ExpiresAt = credential.ExpiresAt, Domain = credential.Domain, RefreshExpiresAt = credential.RefreshExpiresAt, LastRefreshTime = credential.LastRefreshTime },
        Account = new() { Uid = credential.Identity.Uid, EnterpriseId = credential.Identity.EnterpriseId, Nickname = credential.Identity.Nickname },
        Edition = credential.Identity.Site == AccountSite.Intl ? "intl" : "cn"
    }, Json);
}
