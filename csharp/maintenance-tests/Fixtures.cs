using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;

namespace MaintenanceChecks;

internal sealed class Clock : TimeProvider
{
    public long Now = 1800000000;
    public override DateTimeOffset GetUtcNow() => DateTimeOffset.FromUnixTimeSeconds(Now);
}
internal sealed class MemoryPersistence(AccountCredential credential) : ICredentialPersistence
{
    public PersistedCredential Saved = new(credential);
    public int Saves;
    public bool Before, After;
    public Action? OnSave;
    public PersistedCredential Load() => Saved;
    public void Save(PersistedCredential state)
    {
        if (Before) throw new IOException("synthetic pre-write fault");
        Saved = state; Saves++; OnSave?.Invoke();
        if (After) throw new IOException("synthetic post-write fault");
    }
    public void Dispose() { }
}
internal sealed class Control(Clock clock) : IAccountControlPlane
{
    public int Calls, Verifications;
    public Func<AccountCredential, CancellationToken, Task<AccountCredential>>? Action;
    public CredentialVerification Verification = CredentialVerification.Usable;
    public async Task<AccountCredential> RefreshAsync(AccountCredential current, CancellationToken cancellation)
    {
        Interlocked.Increment(ref Calls);
        return Action is null ? Fixtures.Cred(clock.Now + 3600, "new-access", "new-refresh") : await Action(current, cancellation);
    }
    public Task<CredentialVerification> VerifyAsync(AccountCredential candidate, CancellationToken cancellation)
    { Interlocked.Increment(ref Verifications); return Task.FromResult(Verification); }
}
internal static class Fixtures
{
    internal static readonly AccountId Id = new("synthetic");
    internal static readonly Uri ModelUrl = new("http://127.0.0.1:1/model");
    internal static AccountCredential Cred(long expires = 1800003600, string access = "old-access", string refresh = "old-refresh") => new(new("user", "enterprise", "test"), access, refresh, expires, "domain", 1801000000, 1799999000);
    internal static string Jwt(object claims) => "e30." + Convert.ToBase64String(JsonSerializer.SerializeToUtf8Bytes(claims)).TrimEnd('=').Replace('+', '-').Replace('/', '_') + ".signature";
    internal static void Check(bool value, string message) { if (!value) throw new InvalidOperationException(message); }
    internal static async Task Fail(Func<Task> action, string code)
    {
        try { await action().WaitAsync(TimeSpan.FromSeconds(5)); throw new InvalidOperationException("Expected " + code); }
        catch (CredentialFailure e) { Check(e.Code == code, e.Code + " != " + code); }
    }
    internal static TaskCompletionSource Signal() => new(TaskCreationOptions.RunContinuationsAsynchronously);
    internal static (AccountPool Pool, CredentialManager Manager, MemoryPersistence Files, Control Control, Clock Clock) Create(AccountCredential? credential = null)
    {
        var clock = new Clock(); var control = new Control(clock); var files = new MemoryPersistence(credential ?? Cred());
        var pool = new AccountPool([new(Id, ModelUrl)], time: clock);
        return (pool, new(pool, [new(Id, ModelUrl, files, control)], clock), files, control, clock);
    }
    internal static string Access(AccountCredential c)
    {
        using var doc = JsonDocument.Parse(CredentialCodec.Export(c)); return doc.RootElement.GetProperty("auth").GetProperty("accessToken").GetString()!;
    }
    internal static async Task Cancelled(Task task)
    {
        try { await task.WaitAsync(TimeSpan.FromSeconds(5)); throw new InvalidOperationException("Expected cancellation"); } catch (OperationCanceledException) { }
    }
}
