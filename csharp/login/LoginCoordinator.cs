using System.Security.Cryptography;
using AccountMaintenance;
using AccountScheduling;

namespace AccountLogin;

public sealed class LoginOperation
{
    readonly Func<LoginSnapshot> snapshot;
    readonly Func<bool> cancel;
    public Task<LoginSnapshot> Completion { get; }
    internal LoginOperation(Func<LoginSnapshot> snapshot, Func<bool> cancel, Task<LoginSnapshot> completion)
    { this.snapshot = snapshot; this.cancel = cancel; Completion = completion; }
    public LoginSnapshot Snapshot => snapshot();
    public bool Cancel() => cancel();
}

public sealed class LoginCoordinator : IAsyncDisposable
{
    readonly object sync = new();
    readonly Func<AccountSite, ILoginFlow> factory;
    readonly LoginOptions options;
    readonly TimeProvider time;
    readonly CancellationTokenSource stopping = new();
    readonly Dictionary<string, Session> sessions = new(StringComparer.Ordinal);
    readonly HashSet<string> targets = new(OperatingSystem.IsWindows() ? StringComparer.OrdinalIgnoreCase : StringComparer.Ordinal);
    int active, starting;
    bool disposed;
    TaskCompletionSource drained = Signal();
    static TaskCompletionSource Signal() => new(TaskCreationOptions.RunContinuationsAsynchronously);
    public LoginCoordinator(Func<AccountSite, ILoginFlow> factory, LoginOptions? options = null, TimeProvider? time = null)
    {
        this.factory = factory; this.options = options ?? new(); this.options.Validate(); this.time = time ?? TimeProvider.System;
        drained.SetResult();
    }
    public int Active { get { lock (sync) return active; } }
    void Prune()
    {
        var now = time.GetUtcNow();
        foreach (var id in sessions.Where(p => p.Value.FinishedAt is { } at && now - at >= options.Keep).Select(p => p.Key).ToArray()) sessions.Remove(id);
    }
    public LoginSnapshot? Find(string id)
    {
        lock (sync) { Prune(); return sessions.GetValueOrDefault(id)?.Snapshot(); }
    }
    public bool Cancel(string id)
    {
        Session? session; lock (sync) session = sessions.GetValueOrDefault(id); return session?.Cancel() ?? false;
    }
    public async Task<LoginOperation> StartAsync(AccountSite site, ILoginDestination destination, CancellationToken cancellation = default)
    {
        if (!Enum.IsDefined(site) || destination.Label.Length > 255 || !LoginRules.ValidFile(destination.Label) || string.IsNullOrEmpty(destination.Key) || destination.Key.Length > 4096)
            throw new LoginFailure("invalid_login_target");
        lock (sync)
        {
            ObjectDisposedException.ThrowIf(disposed, this); cancellation.ThrowIfCancellationRequested(); Prune();
            if (active >= options.MaxActive || sessions.Count + starting >= options.MaxRetained) throw new LoginFailure("login_capacity_full");
            if (!targets.Add(destination.Key)) throw new LoginFailure("login_target_busy");
            active++; if (starting++ == 0) drained = Signal();
        }
        ILoginFlow? flow = null; bool published = false;
        try
        {
            using var begin = CancellationTokenSource.CreateLinkedTokenSource(cancellation, stopping.Token);
            flow = factory(site); var challenge = await flow.BeginAsync(begin.Token); begin.Token.ThrowIfCancellationRequested();
            if (!challenge.AuthUrl.IsAbsoluteUri || !(challenge.AuthUrl.Scheme=="http"&&challenge.AuthUrl.IsLoopback||LoginOrigins.AllowedChallenge(challenge.AuthUrl,site)) || challenge.AuthUrl.UserInfo.Length != 0)
                throw new LoginFailure("invalid_login_challenge");
            lock (sync)
            {
                ObjectDisposedException.ThrowIf(disposed, this);
                begin.Token.ThrowIfCancellationRequested();
                string id = "login-" + Convert.ToHexStringLower(RandomNumberGenerator.GetBytes(16));
                var session = new Session(this, id, site, challenge, destination, flow, time.GetUtcNow() + options.Ttl(site));
                sessions.Add(id, session); published = true;
                session.Work = Task.Run(() => Run(session));
                return new(session.Snapshot, session.Cancel, session.Completion.Task);
            }
        }
        catch (OperationCanceledException) { throw; }
        catch (LoginFailure) { throw; }
        catch { throw new LoginFailure("login_begin_failed"); }
        finally
        {
            if (!published) { try { flow?.Dispose(); } catch { } }
            lock (sync)
            {
                if (!published) { active--; targets.Remove(destination.Key); }
                if (--starting == 0) drained.TrySetResult();
            }
        }
    }
    async Task Run(Session s)
    {
        using var expires = new CancellationTokenSource(options.Ttl(s.Site), time);
        using var linked = CancellationTokenSource.CreateLinkedTokenSource(s.Stop.Token, stopping.Token, expires.Token);
        try
        {
            while (true)
            {
                await Task.Delay(options.Poll, time, linked.Token);
                if (time.GetUtcNow() >= s.Deadline) { s.End(LoginStatus.Expired, "login_expired"); return; }
                var candidate = await s.Flow.PollAsync(s.Challenge, linked.Token);
                linked.Token.ThrowIfCancellationRequested();
                if (candidate is null) continue;
                if (candidate.Identity.Site != s.Site || string.IsNullOrEmpty(candidate.Identity.Uid) || candidate.ExpiresAt <= time.GetUtcNow().ToUnixTimeSeconds())
                    throw new LoginFailure("invalid_login_credential");
                if (!s.BeginSaving(time.GetUtcNow())) return;
                // Cancellation cannot interrupt a commit that has begun. Its durable outcome wins.
                s.Destination.Commit(candidate);
                s.Done(candidate); return;
            }
        }
        catch (OperationCanceledException)
        { s.End(expires.IsCancellationRequested || time.GetUtcNow() >= s.Deadline ? LoginStatus.Expired : LoginStatus.Cancelled, null); }
        catch (LoginFailure e) { s.End(LoginStatus.Failed, e.Code); }
        catch (CredentialFailure e) { s.End(LoginStatus.Failed, e.Code); }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException or HttpRequestException)
        { s.End(LoginStatus.Failed, "login_commit_or_transport_failed"); }
        catch { s.End(LoginStatus.Failed, "login_failed"); }
        finally
        {
            try { s.Flow.Dispose(); } catch { /* Resource cleanup cannot hide the terminal result. */ }
            lock (sync) { active--; targets.Remove(s.Destination.Key); s.FinishedAt = time.GetUtcNow(); }
            s.Completion.TrySetResult(s.Snapshot());
            s.Stop.Dispose();
        }
    }
    public async ValueTask DisposeAsync()
    {
        Task[] work; Task starts;
        lock (sync)
        {
            if (disposed) return; disposed = true; work = sessions.Values.Select(s => s.Work).ToArray(); starts = drained.Task;
        }
        stopping.Cancel(); await starts; await Task.WhenAll(work); stopping.Dispose();
    }
    sealed class Session(LoginCoordinator owner, string id, AccountSite site, LoginChallenge challenge, ILoginDestination destination, ILoginFlow flow, DateTimeOffset deadline)
    {
        readonly object sync = new();
        LoginStatus status = LoginStatus.Pending;
        string? error;
        LoginAccount? account;
        public AccountSite Site { get; } = site;
        public LoginChallenge Challenge { get; } = challenge;
        public ILoginDestination Destination { get; } = destination;
        public ILoginFlow Flow { get; } = flow;
        public DateTimeOffset Deadline { get; } = deadline;
        public CancellationTokenSource Stop { get; } = new();
        public TaskCompletionSource<LoginSnapshot> Completion { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);
        public Task Work = Task.CompletedTask;
        public DateTimeOffset? FinishedAt;
        public LoginSnapshot Snapshot()
        {
            lock (sync) return new(id, Site == AccountSite.Intl ? "intl" : "cn", Destination.Label, Challenge.AuthUrl,
                status.ToString().ToLowerInvariant(), Deadline.ToUnixTimeSeconds(), LoginRules.Remaining(status, Deadline, owner.time.GetUtcNow()), error,
                status == LoginStatus.Done ? Destination.Label : null, account);
        }
        public bool Cancel()
        {
            lock (sync) { if (status != LoginStatus.Pending) return false; status = LoginStatus.Cancelled; }
            try { Stop.Cancel(); } catch (ObjectDisposedException) { }
            return true;
        }
        public bool BeginSaving(DateTimeOffset now)
        {
            lock (sync)
            {
                if (status != LoginStatus.Pending) return false;
                if (now >= Deadline) { status = LoginStatus.Expired; error = "login_expired"; return false; }
                status = LoginStatus.Saving; return true;
            }
        }
        public void Done(AccountCredential credential)
        {
            lock (sync)
            {
                status = LoginStatus.Done;
                account = new(credential.Identity.Nickname, credential.Identity.Uid, credential.Identity.EnterpriseId, credential.ExpiresAt);
            }
        }
        public void End(LoginStatus terminal, string? reason)
        {
            lock (sync)
            {
                if (status is not (LoginStatus.Pending or LoginStatus.Saving)) return;
                status = terminal; error = reason;
            }
        }
    }
}
