using System.Text.Json;
using System.Text.RegularExpressions;
using AccountMaintenance;
using AccountScheduling;

namespace AccountLogin;

public enum LoginStatus { Pending, Saving, Done, Failed, Expired, Cancelled }
public sealed class LoginFailure(string code) : Exception(code) { public string Code { get; } = code; }
public sealed class LoginChallenge
{
    internal string State { get; }
    public Uri AuthUrl { get; }
    public LoginChallenge(string state, Uri authUrl)
    {
        if (string.IsNullOrEmpty(state) || state.Length > 4096 || state.Any(char.IsControl)) throw new LoginFailure("invalid_login_state");
        if (authUrl is null || authUrl.OriginalString.Length > 8192) throw new LoginFailure("invalid_login_challenge");
        State = state; AuthUrl = authUrl;
    }
    public override string ToString() => "LoginChallenge (redacted)";
}
public sealed record LoginAccount(string Nickname, string Uid, string EnterpriseId, long TokenExpiresAt);
public sealed record LoginSnapshot(string Id, string Site, string File, Uri AuthUrl, string Status, long ExpiresAt,
    int RemainingSeconds, string? Error = null, string? SavedAs = null, LoginAccount? Account = null);
public sealed record LoginOptions(int MaxActive = 16, int MaxRetained = 128, TimeSpan? PollInterval = null,
    TimeSpan? Timeout = null, TimeSpan? Retention = null)
{
    internal TimeSpan Poll => PollInterval ?? TimeSpan.FromSeconds(2);
    internal TimeSpan Keep => Retention ?? TimeSpan.FromMinutes(2);
    internal TimeSpan Ttl(AccountSite site) => Timeout ?? (site == AccountSite.Intl ? TimeSpan.FromMinutes(15) : TimeSpan.FromMinutes(5));
    internal void Validate()
    {
        if (MaxActive is < 1 or > 128 || MaxRetained < MaxActive || MaxRetained > 4096 || Poll < TimeSpan.FromMilliseconds(1) || Poll > TimeSpan.FromSeconds(30) ||
            Keep < TimeSpan.Zero || Keep > TimeSpan.FromHours(1) || Ttl(AccountSite.Cn) <= TimeSpan.Zero || Ttl(AccountSite.Cn) > TimeSpan.FromMinutes(15))
            throw new ArgumentException("Invalid login limits");
    }
}
public interface ILoginFlow : IDisposable
{
    Task<LoginChallenge> BeginAsync(CancellationToken cancellation);
    Task<AccountCredential?> PollAsync(LoginChallenge challenge, CancellationToken cancellation);
}
public interface ILoginDestination
{
    string Key { get; }
    string Label { get; }
    void Commit(AccountCredential credential);
}
public static partial class LoginRules
{
    internal static JsonElement Field(JsonElement value, string name)
    {
        JsonElement result = default;
        if (value.ValueKind == JsonValueKind.Object)
            foreach (var property in value.EnumerateObject()) if (string.Equals(property.Name, name, StringComparison.OrdinalIgnoreCase)) result = property.Value;
        return result;
    }
    internal static string Text(JsonElement root, string name) => Field(root, name) is { ValueKind: JsonValueKind.String } value ? value.GetString()! : "";
    [GeneratedRegex(@"\Aworkbuddy[A-Za-z0-9._-]*\.json\z", RegexOptions.CultureInvariant)]
    private static partial Regex FilePattern();
    public static bool ValidFile(string? value)
    {
        if (value is null) return false;
        value = value.Trim();
        return value.Length != 0 && !value.Any(c => "/\\*?[]".Contains(c)) && FilePattern().IsMatch(value);
    }
    public static string Platform(AccountSite site) => site == AccountSite.Intl ? "workbuddy-ai" : "VSCode";
    public static int Remaining(LoginStatus status, DateTimeOffset deadline, DateTimeOffset now) =>
        status == LoginStatus.Pending ? (int)Math.Clamp((deadline - now).TotalSeconds, 0, int.MaxValue) : 0;
    public static AccountCredential Candidate(JsonElement token, JsonElement account, AccountSite site, long issuedAt)
    {
        var expiry = Field(token, "expiresIn");
        long ttl = expiry.ValueKind == JsonValueKind.Number && expiry.TryGetInt64(out var n) ? n : 0;
        long expires; try { expires = checked(issuedAt + ttl); } catch (OverflowException) { throw new LoginFailure("invalid_login_expiry"); }
        string access = Text(token, "accessToken"), uid = Text(account, "uid");
        if (access.Length == 0 || uid.Length == 0 || ttl <= 0) throw new LoginFailure("incomplete_login_credential");
        return new(new(uid, Text(account, "enterpriseId"), Text(account, "nickname"), site),
            access, Text(token, "refreshToken"), expires, Text(token, "domain"));
    }
}
