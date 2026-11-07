using AccountLogin;
using AccountMaintenance;
using AccountScheduling;

namespace LoginChecks;

internal sealed class Clock : TimeProvider
{
    public DateTimeOffset Now = DateTimeOffset.FromUnixTimeSeconds(1800000000);
    public override DateTimeOffset GetUtcNow() => Now;
}
internal sealed class Destination(string key = "synthetic", string label = "workbuddy1.json") : ILoginDestination
{
    public string Key { get; } = key;
    public string Label { get; } = label;
    public int Writes;
    public AccountCredential? Saved;
    public Action? Action;
    public void Commit(AccountCredential credential) { Action?.Invoke(); Saved = credential; Interlocked.Increment(ref Writes); }
}
internal sealed class Flow(AccountSite site = AccountSite.Cn) : ILoginFlow
{
    public int Begins, Polls, Disposals;
    public Func<CancellationToken, Task<LoginChallenge>>? Begin;
    public Func<CancellationToken, Task<AccountCredential?>>? Poll;
    public Task<LoginChallenge> BeginAsync(CancellationToken cancellation)
    {
        Interlocked.Increment(ref Begins); return Begin?.Invoke(cancellation) ?? Task.FromResult(new LoginChallenge("synthetic-state", new("http://127.0.0.1:1/authorize")));
    }
    public Task<AccountCredential?> PollAsync(LoginChallenge challenge, CancellationToken cancellation)
    {
        Interlocked.Increment(ref Polls); return Poll?.Invoke(cancellation) ?? Task.FromResult<AccountCredential?>(Fixtures.Cred(site));
    }
    public void Dispose() => Interlocked.Increment(ref Disposals);
}
internal static class Fixtures
{
    internal static readonly LoginOptions Fast = new(PollInterval: TimeSpan.FromMilliseconds(5), Timeout: TimeSpan.FromSeconds(3));
    internal static AccountCredential Cred(AccountSite site = AccountSite.Cn, string uid = "synthetic-user") => new(new(uid, "enterprise", "test", site), "synthetic-access", "synthetic-refresh", 1800003600, "domain");
    internal static TaskCompletionSource Signal() => new(TaskCreationOptions.RunContinuationsAsynchronously);
    internal static void Check(bool condition, string name) { if (!condition) throw new InvalidOperationException(name); }
    internal static async Task<LoginSnapshot> Done(LoginOperation op) => await op.Completion.WaitAsync(TimeSpan.FromSeconds(6));
    internal static async Task Fail(Func<Task> action, string code)
    {
        try { await action().WaitAsync(TimeSpan.FromSeconds(5)); throw new InvalidOperationException("Expected " + code); }
        catch (LoginFailure e) { Check(e.Code == code, e.Code + " != " + code); }
    }
}
