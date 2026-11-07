using System.Net.Http;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using static MaintenanceChecks.Fixtures;

namespace MaintenanceChecks;

internal static class ManagerTests
{
    public static async Task<List<string>> Run(string directory)
    {
        var passed = new List<string>(); async Task Test(string name, Func<Task> action) { await action(); passed.Add(name); }
        await Test("credential JSON and ToString views do not expose tokens", async () =>
        {
            var f = Create(); using var lease = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl);
            Check(!JsonSerializer.Serialize(f.Files.Saved.Credential).Contains("old-access", StringComparison.Ordinal) && !lease.ToString().Contains("old-access", StringComparison.Ordinal), "redacted views");
            using var request = new HttpRequestMessage(HttpMethod.Post, ModelUrl); lease.ApplyTo(request);
            Check(request.Headers.Authorization?.Parameter == "old-access" && !request.Headers.Contains("X-Refresh-Token"), "access-only model headers");
            lease.Dispose(); try { lease.ApplyTo(request); throw new InvalidOperationException("disposed"); } catch (ObjectDisposedException) { }
        });
        await Test("64 concurrent near-expiry requests coalesce to one refresh", async () =>
        {
            var f = Create(Cred(1800000100)); var entered = Signal(); var release = Signal();
            f.Control.Action = async (_, ct) => { entered.SetResult(); await release.Task.WaitAsync(ct); return Cred(); };
            var tasks = Enumerable.Range(0, 64).Select(_ => f.Manager.AcquireForHeadersAsync(Id, ModelUrl)).ToArray();
            await entered.Task.WaitAsync(TimeSpan.FromSeconds(5)); release.SetResult(); var leases = await Task.WhenAll(tasks);
            Check(f.Control.Calls == 1 && f.Files.Saves == 1, "one durable refresh");
            Parallel.ForEach(leases, l => { l.Dispose(); l.Dispose(); });
            using var next = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); Check(f.Control.Calls == 1, "read locks released");
        });
        await Test("shared readers coexist and queued refresh prevents writer starvation", async () =>
        {
            var f = Create(); using var a = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); using var b = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl);
            var entered = Signal(); var release = Signal(); f.Control.Action = async (_, ct) => { entered.SetResult(); await release.Task.WaitAsync(ct); return Cred(1800007200); };
            var writer = f.Manager.RefreshAsync(Id); var later = f.Manager.AcquireForHeadersAsync(Id, ModelUrl);
            Check(!writer.IsCompleted && !later.IsCompleted && f.Control.Calls == 0, "writer waits for active headers");
            a.Dispose(); Check(f.Control.Calls == 0, "second reader still owns"); b.Dispose(); await entered.Task.WaitAsync(TimeSpan.FromSeconds(5));
            Check(!later.IsCompleted, "new reader cannot bypass writer"); release.SetResult(); await writer; using var c = await later;
        });
        await Test("cancelled queued writer is removed and unblocks subsequent readers", async () =>
        {
            var f = Create(); using var a = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); using var cancel = new CancellationTokenSource();
            var writer = f.Manager.RefreshAsync(Id, cancel.Token); var reader = f.Manager.AcquireForHeadersAsync(Id, ModelUrl);
            Check(!reader.IsCompleted, "reader queued behind writer"); cancel.Cancel(); await Cancelled(writer); using var b = await reader.WaitAsync(TimeSpan.FromSeconds(5));
            Check(f.Control.Calls == 0 && f.Manager.Status.Single().Failures == 0, "cancelled before RPC");
        });
        await Test("cancel during refresh still commits rotated tokens before cancellation completes", async () =>
        {
            var f = Create(Cred(1800000100)); var entered = Signal(); var release = Signal();
            f.Control.Action = async (_, ct) => { entered.SetResult(); await release.Task.WaitAsync(ct); return Cred(access: "rotated-access", refresh: "rotated-refresh"); };
            using var cancel = new CancellationTokenSource(); var pending = f.Manager.AcquireForHeadersAsync(Id, ModelUrl, cancel.Token);
            await entered.Task.WaitAsync(TimeSpan.FromSeconds(5)); cancel.Cancel(); release.SetResult(); await Cancelled(pending);
            Check(Access(f.Files.Saved.Credential) == "rotated-access" && f.Manager.Status.Single().Failures == 0, "rotation saved");
            using var lease = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl);
            using var request = new HttpRequestMessage(HttpMethod.Post, ModelUrl); lease.ApplyTo(request); Check(request.Headers.Authorization?.Parameter == "rotated-access", "published token");
        });
        await Test("cancel racing a successful disk commit cannot revert in-memory credential", async () =>
        {
            var f = Create(Cred(1800000100)); using var cancel = new CancellationTokenSource(); f.Files.OnSave = cancel.Cancel;
            await Cancelled(f.Manager.AcquireForHeadersAsync(Id, ModelUrl, cancel.Token));
            using var lease = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); using var request = new HttpRequestMessage(HttpMethod.Post, ModelUrl); lease.ApplyTo(request);
            Check(request.Headers.Authorization?.Parameter == "new-access", "commit published despite cancellation");
        });
        await Test("pre-write and ambiguous post-write failures block further admission", async () =>
        {
            foreach (bool after in new[] { false, true })
            {
                var f = Create(Cred(1800000100)); f.Files.Before = !after; f.Files.After = after;
                await Fail(async () => { using var lease = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); }, "credential_persist_failed");
                Check(f.Manager.Status.Single().Degraded, "fail closed");
                await Fail(async () => { using var lease = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); }, "credential_store_incomplete");
                try { using var lease = await f.Pool.AcquireAsync("m"); throw new InvalidOperationException("pool must block"); } catch (SchedulingFailure) { }
            }
        });
        await Test("changed JWT account or realm rejects before disk replacement", async () =>
        {
            foreach (bool realm in new[] { false, true })
            {
                var f = Create(Cred(1800000100, Jwt(new { sub = "a", iss = "https://issuer/auth/realms/cn" })));
                f.Control.Action = (_, _) => Task.FromResult(Cred(access: Jwt(new { sub = realm ? "a" : "b", iss = realm ? "https://issuer/auth/realms/intl" : "https://issuer/auth/realms/cn" })));
                await Fail(async () => { using var lease = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); }, "credential_identity_changed");
                Check(f.Files.Saves == 0 && f.Control.Verifications == 0 && f.Manager.Status.Single().CooldownUntil > f.Clock.Now, "no wrong-account write and no immediate refresh loop");
            }
        });
        await Test("read-only verification rejection preserves old credential", async () =>
        {
            var f = Create(Cred(1800000100)); f.Control.Verification = CredentialVerification.Rejected;
            await Fail(async () => { using var lease = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); }, "credential_verification_rejected");
            Check(f.Files.Saves == 0 && Access(f.Files.Saved.Credential) == "old-access", "old retained");
        });
        await Test("unknown verification does not discard a successfully refreshed token", async () =>
        {
            var f = Create(Cred(1800000100)); f.Control.Verification = CredentialVerification.Unknown;
            using var lease = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); Check(f.Files.Saves == 1, "unknown accepted like Go");
        });
        await Test("unchanged expired expiry fails after one refresh rather than looping", async () =>
        {
            var f = Create(Cred(1799999999)); f.Control.Action = (old, _) => Task.FromResult(CredentialRules.Merge(old, "{\"accessToken\":\"still-expired\"}"u8, f.Clock.Now));
            await Fail(async () => { using var lease = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); }, "credential_expired"); Check(f.Control.Calls == 1, "one attempt");
        });
        await Test("missing refresh token does not send a control request", async () =>
        {
            var f = Create(Cred(1800000100, refresh: ""));
            await Fail(async () => { using var lease = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); }, "credential_missing_refresh"); Check(f.Control.Calls == 0, "no RPC");
        });
        await Test("three refresh failures persist disabled state and retain original tokens", async () =>
        {
            var f = Create(); f.Control.Action = (_, _) => throw new CredentialFailure("refresh_rejected", RefreshFailureKind.SessionDead);
            for (int i = 1; i <= 3; i++)
            {
                await Fail(() => f.Manager.RefreshAsync(Id), "credential_refresh_failed");
                Check(f.Manager.Status.Single().Failures == i && f.Manager.Status.Single().Disabled == (i == 3), "threshold"); f.Clock.Now += 61;
            }
            Check(f.Files.Saved.Disabled && Access(f.Files.Saved.Credential) == "old-access", "credentials retained on disable");
            var restored = new CredentialManager(f.Pool, [new(Id, ModelUrl, f.Files, f.Control)], f.Clock);
            await Fail(async () => { using var lease = await restored.AcquireForHeadersAsync(Id, ModelUrl); }, "credential_disabled");
        });
        await Test("successful refresh resets consecutive failures", async () =>
        {
            var f = Create(); f.Control.Action = (_, _) => throw new CredentialFailure("refresh_timeout");
            await Fail(() => f.Manager.RefreshAsync(Id), "credential_refresh_failed"); f.Clock.Now += 61; f.Control.Action = null;
            await f.Manager.RefreshAsync(Id); Check(f.Manager.Status.Single() is { Failures: 0, Disabled: false, CooldownUntil: 0 }, "reset failure count");
        });
        await Test("ModelUrl pinning rejects mismatches before refresh or token application", async () =>
        {
            var f = Create(Cred(1800000100));
            await Fail(async () => { using var lease = await f.Manager.AcquireForHeadersAsync(Id, new("http://127.0.0.1:2/model")); }, "credential_endpoint_mismatch"); Check(f.Control.Calls == 0, "no RPC");
            f = Create(); using var lease = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); using var request = new HttpRequestMessage(HttpMethod.Post, "http://127.0.0.1:1/other");
            await Fail(() => { lease.ApplyTo(request); return Task.CompletedTask; }, "credential_endpoint_mismatch"); Check(request.Headers.Authorization is null, "no header leak");
        });
        await Test("strict 15-minute threshold matches Unix-second boundary", () =>
        {
            Check(!CredentialRules.NeedsRefresh(1800000900, 1800000000) && CredentialRules.NeedsRefresh(1800000899, 1800000000), "strict threshold"); return Task.CompletedTask;
        });
        await Test("100 queued reader cancellation races leave the gate usable", async () =>
        {
            var f = Create();
            for (int i = 0; i < 100; i++)
            {
                using var held = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); using var cancelled = new CancellationTokenSource();
                var writer = f.Manager.RefreshAsync(Id); var reader = f.Manager.AcquireForHeadersAsync(Id, ModelUrl, cancelled.Token);
                await Task.WhenAll(Task.Run(cancelled.Cancel), Task.Run(held.Dispose)); await writer;
                try { using var raced = await reader; } catch (OperationCanceledException) { }
            }
            using var final = await f.Manager.AcquireForHeadersAsync(Id, ModelUrl); Check(f.Manager.Status.Single().Failures == 0, "gate remained usable");
        });
        await Test("owned disk store roundtrips Go-shaped data and disabled state", () =>
        {
            string path = Path.Combine(directory, "roundtrip-" + Guid.NewGuid().ToString("N"));
            using (var store = CredentialFiles.Create(path, Cred())) { store.Save(new(Cred(access: "disk-new"), true)); try { using var other = CredentialFiles.Open(path); throw new InvalidOperationException("two owners"); } catch (IOException) { } }
            using var reopened = CredentialFiles.Open(path); Check(reopened.Load().Disabled && Access(reopened.Load().Credential) == "disk-new", "restart preserved state"); return Task.CompletedTask;
        });
        await Test("unfinished persistence marker refuses restart", () =>
        {
            string path = Path.Combine(directory, "pending-" + Guid.NewGuid().ToString("N")); using (var store = CredentialFiles.Create(path, Cred())) { }
            File.WriteAllText(Path.Combine(path, ".pending"), "1");
            try { using var store = CredentialFiles.Open(path); throw new InvalidOperationException("pending accepted"); } catch (CredentialFailure e) { Check(e.Code == "credential_store_incomplete", "pending failure"); }
            return Task.CompletedTask;
        });
        await Test("unowned nonempty credential directory is never adopted", () =>
        {
            string path = Path.Combine(directory, "foreign-" + Guid.NewGuid().ToString("N")); Directory.CreateDirectory(path); File.WriteAllText(Path.Combine(path, "foreign.json"), "{}");
            try { using var store = CredentialFiles.Create(path, Cred()); throw new InvalidOperationException("foreign accepted"); } catch (CredentialFailure e) { Check(e.Code == "credential_directory_not_empty", "ownership"); }
            Check(File.ReadAllText(Path.Combine(path, "foreign.json")) == "{}", "untouched"); return Task.CompletedTask;
        });
        return passed;
    }
}
