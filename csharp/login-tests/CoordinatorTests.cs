using System.Text.Json;
using AccountLogin;
using AccountMaintenance;
using AccountScheduling;
using static LoginChecks.Fixtures;

namespace LoginChecks;

internal static class CoordinatorTests
{
    public static async Task<List<string>> Run(string root)
    {
        var tests = new List<string>(); async Task Test(string name, Func<Task> action) { await action(); tests.Add(name); }
        await Test("success is published only after a complete credential commit", async () =>
        {
            var flow = new Flow(); var destination = new Destination(); await using var coordinator = new LoginCoordinator(_ => flow, Fast, new Clock());
            var op = await coordinator.StartAsync(AccountSite.Cn, destination); var done = await Done(op);
            Check(done.Status == "done" && destination.Writes == 1 && done.Account?.Uid == "synthetic-user" && done.RemainingSeconds == 0 && flow.Disposals == 1 && coordinator.Active == 0, "committed success");
            Check(!JsonSerializer.Serialize(done).Contains("synthetic-access", StringComparison.Ordinal) && !JsonSerializer.Serialize(done).Contains("synthetic-refresh", StringComparison.Ordinal), "no tokens in snapshot");
        });
        await Test("cancellation before candidate arrival cannot write credentials", async () =>
        {
            var entered = Signal(); var flow = new Flow { Poll = async ct => { entered.SetResult(); await Task.Delay(Timeout.Infinite, ct); return null; } };
            var destination = new Destination(); await using var coordinator = new LoginCoordinator(_ => flow, Fast, new Clock());
            var op = await coordinator.StartAsync(AccountSite.Cn, destination); await entered.Task; Check(op.Cancel(), "cancel accepted");
            Check((await Done(op)).Status == "cancelled" && destination.Writes == 0 && flow.Disposals == 1, "cancelled without commit");
        });
        await Test("late candidate from a cancelled poll is discarded", async () =>
        {
            var entered = Signal(); var release = Signal(); var flow = new Flow { Poll = async _ => { entered.SetResult(); await release.Task; return Cred(); } };
            var destination = new Destination(); await using var coordinator = new LoginCoordinator(_ => flow, Fast, new Clock());
            var op = await coordinator.StartAsync(AccountSite.Cn, destination); await entered.Task; op.Cancel(); release.SetResult();
            Check((await Done(op)).Status == "cancelled" && destination.Writes == 0, "late response discarded");
        });
        await Test("cancel while saving cannot misreport a completed commit as cancelled", async () =>
        {
            var entered = Signal(); using var release = new ManualResetEventSlim(); var destination = new Destination { Action = () => { entered.SetResult(); Check(release.Wait(TimeSpan.FromSeconds(5)), "save release"); } };
            await using var coordinator = new LoginCoordinator(_ => new Flow(), Fast, new Clock()); var op = await coordinator.StartAsync(AccountSite.Cn, destination);
            await entered.Task; Check(op.Snapshot.Status == "saving" && !op.Cancel(), "commit cannot be cancelled"); release.Set();
            Check((await Done(op)).Status == "done" && destination.Writes == 1, "durable result wins");
        });
        await Test("deadline cancels an in-flight token poll and frees capacity", async () =>
        {
            var flow = new Flow { Poll = async ct => { await Task.Delay(Timeout.Infinite, ct); return null; } }; var destination = new Destination();
            await using var coordinator = new LoginCoordinator(_ => flow, Fast with { Timeout = TimeSpan.FromMilliseconds(80) }, new Clock());
            var op = await coordinator.StartAsync(AccountSite.Cn, destination); Check((await Done(op)).Status == "expired" && destination.Writes == 0 && coordinator.Active == 0, "bounded poll");
        });
        await Test("clock expiry after a successful poll is checked before committing", async () =>
        {
            var clock = new Clock(); var flow = new Flow { Poll = _ => { clock.Now += TimeSpan.FromMinutes(1); return Task.FromResult<AccountCredential?>(Cred()); } };
            var destination = new Destination(); await using var coordinator = new LoginCoordinator(_ => flow, Fast, clock);
            var op = await coordinator.StartAsync(AccountSite.Cn, destination); Check((await Done(op)).Status == "expired" && destination.Writes == 0, "late credential not saved");
        });
        await Test("active and same-target limits reject before allocating another flow", async () =>
        {
            int created = 0; var flow = new Flow { Poll = async ct => { await Task.Delay(Timeout.Infinite, ct); return null; } };
            await using var coordinator = new LoginCoordinator(_ => { created++; return flow; }, Fast with { MaxActive = 2, MaxRetained = 2 }, new Clock());
            var op = await coordinator.StartAsync(AccountSite.Cn, new Destination());
            await Fail(async () => { await coordinator.StartAsync(AccountSite.Cn, new Destination()); }, "login_target_busy"); Check(created == 1, "no duplicate flow");
            await using var second = new LoginCoordinator(_ => new Flow { Poll = async ct => { await Task.Delay(Timeout.Infinite, ct); return null; } }, Fast with { MaxActive = 1, MaxRetained = 1 }, new Clock());
            var held = await second.StartAsync(AccountSite.Cn, new Destination("one"));
            await Fail(async () => { await second.StartAsync(AccountSite.Cn, new Destination("two")); }, "login_capacity_full"); held.Cancel(); op.Cancel(); await Done(held); await Done(op);
        });
        await Test("capacity includes an in-flight challenge request", async () =>
        {
            var entered = Signal(); var flow = new Flow { Begin = async ct => { entered.SetResult(); await Task.Delay(Timeout.Infinite, ct); throw new InvalidOperationException(); } };
            await using var coordinator = new LoginCoordinator(_ => flow, Fast with { MaxActive = 1, MaxRetained = 1 });
            using var cancel = new CancellationTokenSource(); var starting = coordinator.StartAsync(AccountSite.Cn, new Destination("one"), cancel.Token); await entered.Task;
            await Fail(async () => { await coordinator.StartAsync(AccountSite.Cn, new Destination("two")); }, "login_capacity_full"); cancel.Cancel();
            try { await starting; throw new InvalidOperationException("start cancellation"); } catch (OperationCanceledException) { }
            Check(coordinator.Active == 0 && flow.Disposals == 1, "start cleaned up");
        });
        await Test("failed challenge releases reservation for a later login", async () =>
        {
            int count = 0; await using var coordinator = new LoginCoordinator(_ => ++count == 1 ? new Flow { Begin = _ => throw new LoginFailure("synthetic_begin_failure") } : new Flow(), Fast, new Clock());
            await Fail(async () => { await coordinator.StartAsync(AccountSite.Cn, new Destination()); }, "synthetic_begin_failure");
            var op = await coordinator.StartAsync(AccountSite.Cn, new Destination()); Check((await Done(op)).Status == "done", "reservation released");
        });
        await Test("completed sessions are retained then evicted with a fixed bound", async () =>
        {
            var clock = new Clock(); await using var coordinator = new LoginCoordinator(_ => new Flow(), Fast with { MaxActive = 1, MaxRetained = 1, Retention = TimeSpan.FromSeconds(2) }, clock);
            var op = await coordinator.StartAsync(AccountSite.Cn, new Destination()); await Done(op);
            Check(coordinator.Find(op.Snapshot.Id)?.Status == "done", "terminal visible");
            await Fail(async () => { await coordinator.StartAsync(AccountSite.Cn, new Destination("two")); }, "login_capacity_full");
            clock.Now += TimeSpan.FromSeconds(3); Check(coordinator.Find(op.Snapshot.Id) is null, "expired retention removed");
            var next = await coordinator.StartAsync(AccountSite.Cn, new Destination("two")); await Done(next);
        });
        await Test("commit failure never exposes done or candidate tokens", async () =>
        {
            var destination = new Destination { Action = () => throw new IOException("synthetic-access must not appear in output") };
            await using var coordinator = new LoginCoordinator(_ => new Flow(), Fast, new Clock()); var op = await coordinator.StartAsync(AccountSite.Cn, destination); var result = await Done(op);
            Check(result.Status == "failed" && destination.Writes == 0 && result.Account is null && !JsonSerializer.Serialize(result).Contains("synthetic-access", StringComparison.Ordinal), "failed sanitized");
        });
        await Test("invalid site or expired credential cannot reach storage", async () =>
        {
            foreach (bool site in new[] { false, true })
            {
                var flow = new Flow { Poll = _ => Task.FromResult<AccountCredential?>(site ? Cred(AccountSite.Intl) : new(new("user"), "a", "r", 1)) };
                var destination = new Destination(); await using var coordinator = new LoginCoordinator(_ => flow, Fast, new Clock()); var op = await coordinator.StartAsync(AccountSite.Cn, destination);
                Check((await Done(op)).Status == "failed" && destination.Writes == 0, "invalid candidate");
            }
        });
        await Test("shutdown cancels pending polls and waits for resource cleanup", async () =>
        {
            var flow = new Flow { Poll = async ct => { await Task.Delay(Timeout.Infinite, ct); return null; } }; var coordinator = new LoginCoordinator(_ => flow, Fast);
            var op = await coordinator.StartAsync(AccountSite.Cn, new Destination()); await coordinator.DisposeAsync();
            Check((await Done(op)).Status == "cancelled" && coordinator.Active == 0 && flow.Disposals == 1, "shutdown drained");
        });
        await Test("shutdown also cancels a challenge that has not been published", async () =>
        {
            var entered = Signal(); var flow = new Flow { Begin = async ct => { entered.SetResult(); await Task.Delay(Timeout.Infinite, ct); throw new InvalidOperationException(); } };
            var coordinator = new LoginCoordinator(_ => flow, Fast); var begin = coordinator.StartAsync(AccountSite.Cn, new Destination()); await entered.Task; await coordinator.DisposeAsync();
            try { await begin; throw new InvalidOperationException("cancelled start"); } catch (OperationCanceledException) { }
            Check(coordinator.Active == 0 && flow.Disposals == 1, "unpublished flow disposed");
        });
        await Test("new disk login roundtrips and same-identity relogin re-enables the credential", async () =>
        {
            string path = Path.Combine(root, Guid.NewGuid().ToString("N")); await using var coordinator = new LoginCoordinator(_ => new Flow(), Fast, new Clock());
            var first = await coordinator.StartAsync(AccountSite.Cn, new PreviewLoginDestination(path)); Check((await Done(first)).Status == "done", "created disk credential");
            using (var files = CredentialFiles.Open(path)) files.Save(new(Cred(), true));
            var again = await coordinator.StartAsync(AccountSite.Cn, new PreviewLoginDestination(path, replaceExisting: true)); Check((await Done(again)).Status == "done", "relogin");
            using var reopened = CredentialFiles.Open(path); Check(!reopened.Load().Disabled && reopened.Load().Credential.Identity.Uid == "synthetic-user", "disabled cleared");
        });
        await Test("relogin into another identity leaves the old file intact", async () =>
        {
            string path = Path.Combine(root, Guid.NewGuid().ToString("N")); using (var files = CredentialFiles.Create(path, Cred())) { }
            var flow = new Flow { Poll = _ => Task.FromResult<AccountCredential?>(Cred(uid: "different")) };
            await using var coordinator = new LoginCoordinator(_ => flow, Fast, new Clock()); var op = await coordinator.StartAsync(AccountSite.Cn, new PreviewLoginDestination(path, replaceExisting: true));
            Check((await Done(op)).Error == "login_identity_changed", "wrong identity rejected"); using var reopened = CredentialFiles.Open(path); Check(reopened.Load().Credential.Identity.Uid == "synthetic-user", "old intact");
        });
        await Test("cancelled challenge result never publishes a worker", async () =>
        {
            using var cancel = new CancellationTokenSource(); var flow = new Flow
            {
                Begin = _ =>
            {
                cancel.Cancel(); return Task.FromResult(new LoginChallenge("state", new("http://127.0.0.1:1/login")));
            }
            };
            var dest = new Destination(); await using var coordinator = new LoginCoordinator(_ => flow, Fast);
            try { await coordinator.StartAsync(AccountSite.Cn, dest, cancel.Token); throw new InvalidOperationException("cancelled start published"); } catch (OperationCanceledException) { }
            Check(coordinator.Active == 0 && dest.Writes == 0 && flow.Polls == 0 && flow.Disposals == 1, "unpublished result disposed");
        });
        await Test("unexpected commit errors produce a sanitized failed terminal state", async () =>
        {
            var dest = new Destination { Action = () => throw new InvalidOperationException("synthetic-access") };
            await using var coordinator = new LoginCoordinator(_ => new Flow(), Fast, new Clock()); var op = await coordinator.StartAsync(AccountSite.Cn, dest);
            var result = await Done(op); Check(result.Status == "failed" && result.Error == "login_failed" && coordinator.Active == 0, "no stuck saving state");
        });
        await Test("oversized labels are rejected before allocating a login flow", async () =>
        {
            int created = 0; await using var coordinator = new LoginCoordinator(_ => { created++; return new Flow(); }, Fast);
            await Fail(async () => { await coordinator.StartAsync(AccountSite.Cn, new Destination(label: "workbuddy" + new string('a', 256) + ".json")); }, "invalid_login_target");
            Check(created == 0, "bounded retained labels");
        });
        return tests;
    }
}
