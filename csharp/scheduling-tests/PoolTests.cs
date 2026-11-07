using AccountScheduling;

namespace SchedulingChecks;

internal static class PoolTests
{
    internal static AccountSnapshot A(string id = "a", bool exhausted = false, ModelCost? cost = null) => Program.Snapshot(new(id, Exhausted: exhausted, Cost: cost));
    internal static void Check(bool condition, string name) { if (!condition) throw new InvalidOperationException(name); }
    internal static async Task Until(Func<bool> predicate)
    {
        using var deadline = new CancellationTokenSource(TimeSpan.FromSeconds(5));
        while (!predicate()) await Task.Delay(5, deadline.Token);
    }
    internal static async Task Failure(Task<AccountLease> task, string code)
    {
        try { using var unexpected = await task.WaitAsync(TimeSpan.FromSeconds(5)); throw new InvalidOperationException("Expected " + code); }
        catch (SchedulingFailure e) { Check(e.Code == code, e.Code + " != " + code); }
    }
    public static async Task<List<string>> Run()
    {
        var passed = new List<string>();
        async Task Test(string name, Func<Task> test) { await test(); passed.Add(name); }
        await Test("missing credentials cannot busy-loop or shadow a healthy account", async () =>
        {
            var pool = new AccountPool([A() with { CredentialsPresent = false }, A("b")]);
            using var lease = await pool.AcquireAsync("m"); Check(lease.Account.Id.Value == "b", "healthy fallback");
        });
        await Test("free site dominates a paid sticky binding", async () =>
        {
            var pool = new AccountPool([A(), A("b") with { Site = AccountSite.Intl }], policies: new Dictionary<string, ModelPolicy> { ["m"] = new(new HashSet<AccountSite> { AccountSite.Intl }, new HashSet<AccountSite> { AccountSite.Cn }) });
            using var lease = await pool.AcquireAsync("m", new("a")); Check(lease.Account.Id.Value == "b", "free site fallback");
        });
        await Test("disabled free observations cannot steer site selection", async () =>
        {
            var pool = new AccountPool([A("old", cost: ModelCost.Free) with { Disabled = true, Site = AccountSite.Intl }, A(), A("b") with { Site = AccountSite.Intl }],
                policies: new Dictionary<string, ModelPolicy> { ["m"] = new(PaidSites: new HashSet<AccountSite> { AccountSite.Cn }) });
            using var lease = await pool.AcquireAsync("m"); Check(lease.Account.Id.Value == "a", "disabled observation");
        });
        await Test("exhausted free tier wins over a normal paid sticky account", async () =>
        {
            var pool = new AccountPool([A(), A("b", true, ModelCost.Free)]);
            using var lease = await pool.AcquireAsync("m", new("a")); Check(lease.Account.Id.Value == "b" && lease.Kind == SelectionKind.FreeExhausted, "tier");
        });
        await Test("160 concurrent arrivals reserve load atomically", async () =>
        {
            var pool = new AccountPool([A(), A("b")], new(MaxAccount: 100));
            var start = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var tasks = Enumerable.Range(0, 160).Select(_ => Task.Run(async () => { await start.Task; return await pool.AcquireAsync("m"); })).ToArray();
            start.SetResult(); var leases = await Task.WhenAll(tasks);
            Check(leases.Count(l => l.Account.Id.Value == "a") == 80 && pool.Status.Active == 160, "balanced reservation");
            Parallel.ForEach(leases, l => { l.Dispose(); l.Dispose(); }); Check(pool.Status.Active == 0 && pool.Status.Loads.Count == 0, "idempotent release");
        });
        await Test("account and global limits reserve together", async () =>
        {
            var pool = new AccountPool([A(), A("b")], new(MaxAccount: 2, MaxGlobal: 3));
            using var a = await pool.AcquireAsync("m"); using var b = await pool.AcquireAsync("m"); using var c = await pool.AcquireAsync("m");
            var task = pool.AcquireAsync("m"); await Until(() => pool.Status.Waiting == 1);
            Check(pool.Status.Active == 3 && pool.Status.Loads.Values.All(n => n <= 2), "limits"); a.Dispose(); using var d = await task;
        });
        await Test("waiting on a sticky account consumes no global permit", async () =>
        {
            var pool = new AccountPool([A(), A("b")], new(MaxAccount: 1, MaxGlobal: 2, AffinityWait: TimeSpan.FromSeconds(3)));
            using var a = await pool.AcquireAsync("m", new("a")); using var cancel = new CancellationTokenSource();
            var pending = pool.AcquireAsync("m", new("a"), cancellation: cancel.Token); await Until(() => pool.Status.Waiting == 1);
            using var b = await pool.AcquireAsync("m", new("b")); Check(pool.Status.Active == 2, "unrelated account slot"); cancel.Cancel();
            try { using var unexpected = await pending; throw new InvalidOperationException("Expected cancellation"); } catch (OperationCanceledException) { }
            Check(pool.Status.Waiting == 0, "waiter cleaned");
        });
        await Test("soft affinity falls back after grace and preserves free-site filtering", async () =>
        {
            var pool = new AccountPool([A(), A("b"), A("paid") with { Site = AccountSite.Intl }], new(MaxAccount: 1, AffinityWait: TimeSpan.FromMilliseconds(100)),
                new Dictionary<string, ModelPolicy> { ["m"] = new(new HashSet<AccountSite> { AccountSite.Cn }, new HashSet<AccountSite> { AccountSite.Intl }) });
            using var a = await pool.AcquireAsync("m", new("a")); var task = pool.AcquireAsync("m", new("a"));
            await Until(() => pool.Status.Waiting == 1); using var b = await task.WaitAsync(TimeSpan.FromSeconds(5));
            Check(b.Account.Id.Value == "b", "grace fallback");
        });
        await Test("queue is bounded and zero means immediate busy rejection", async () =>
        {
            var pool = new AccountPool([A()], new(MaxAccount: 1, MaxQueued: 1)); using var held = await pool.AcquireAsync("m");
            var pending = pool.AcquireAsync("m"); await Until(() => pool.Status.Waiting == 1);
            await Failure(pool.AcquireAsync("m"), "scheduling_queue_full"); held.Dispose(); using var next = await pending;
            pool.Configure(new(MaxAccount: 1, MaxQueued: 0)); await Failure(pool.AcquireAsync("m"), "scheduling_queue_full");
        });
        await Test("timeout and already-cancelled requests leave no permits", async () =>
        {
            var pool = new AccountPool([A()], new(MaxAccount: 1, WaitTimeout: TimeSpan.FromMilliseconds(80))); using var held = await pool.AcquireAsync("m");
            await Failure(pool.AcquireAsync("m"), "scheduling_timeout"); Check(pool.Status.Waiting == 0 && pool.Status.Active == 1, "timeout cleanup");
            held.Dispose(); using var cancel = new CancellationTokenSource(); cancel.Cancel();
            try { using var unexpected = await pool.AcquireAsync("m", cancellation: cancel.Token); throw new InvalidOperationException("cancel"); } catch (OperationCanceledException) { }
            Check(pool.Status.Active == 0, "cancel did not reserve");
        });
        await Test("probe only commits on admission and blocks another probe for five minutes", async () =>
        {
            var pool = new AccountPool([A(exhausted: true)], new(MaxAccount: 1)); using var held = await pool.AcquireAsync("m");
            Check(held.Kind == SelectionKind.ProbeExhausted, "probe kind"); await Failure(pool.AcquireAsync("m"), "no_eligible_account");
            Check(held.Account.Models!["m"].NextProbeAt > DateTimeOffset.UtcNow.AddMinutes(4), "reservation");
        });
        await Test("a globally blocked probe does not consume the probe window", async () =>
        {
            var pool = new AccountPool([A(), A("b", true)], new(MaxGlobal: 1)); using var held = await pool.AcquireAsync("other", new("a"));
            var pending = pool.AcquireAsync("m", attempted: new HashSet<AccountId> { new("a") }); await Until(() => pool.Status.Waiting == 1);
            held.Dispose(); using var probe = await pending; Check(probe.Kind == SelectionKind.ProbeExhausted, "probe reserved only once");
        });
        await Test("same-ID account reload cannot erase a committed probe reservation", async () =>
        {
            var pool = new AccountPool([A(exhausted: true)]); using var first = await pool.AcquireAsync("m");
            pool.Replace([A(exhausted: true)]);
            await Failure(pool.AcquireAsync("m"), "no_eligible_account");
        });
        await Test("reload refuses a full protected state table rather than dropping a probe", async () =>
        {
            var pool = new AccountPool([A(exhausted: true)]); using var first = await pool.AcquireAsync("m");
            var protectedStates = Enumerable.Range(0, 1024).ToDictionary(i => "model" + i, _ => new AccountModelState(QuotaBlocked: true));
            try { pool.Replace([A(exhausted: true) with { Models = protectedStates }]); throw new InvalidOperationException("discarded reservation"); } catch (ArgumentException) { }
            await Failure(pool.AcquireAsync("m"), "no_eligible_account");
            using var other = await pool.AcquireAsync("other"); Check(other.Account.Models!.Count == 2, "failed reload left original states intact");
        });
        await Test("reload preserves live counts including same-ID replacement", async () =>
        {
            var pool = new AccountPool([A()], new(MaxAccount: 1)); using var held = await pool.AcquireAsync("m");
            pool.Replace([A() with { Upstream = new("http://127.0.0.1:2/") }]); var pending = pool.AcquireAsync("m"); await Until(() => pool.Status.Waiting == 1);
            Check(pool.Status.Active == 1, "count retained"); held.Dispose(); using var next = await pending; Check(next.Account.Upstream.Port == 2, "updated snapshot");
        });
        await Test("shrink drains naturally and growing limits wakes waiters", async () =>
        {
            var pool = new AccountPool([A()], new(MaxAccount: 2)); using var a = await pool.AcquireAsync("m"); using var b = await pool.AcquireAsync("m");
            pool.Configure(new(MaxAccount: 1)); var task = pool.AcquireAsync("m"); await Until(() => pool.Status.Waiting == 1);
            a.Dispose(); Check(pool.Status.Active == 1 && !task.IsCompleted, "shrink retained remaining request");
            pool.Configure(new(MaxAccount: 2)); using var c = await task;
        });
        await Test("policy updates and account disable are rechecked while waiting", async () =>
        {
            var pool = new AccountPool([A()], new(MaxAccount: 1)); using var held = await pool.AcquireAsync("m");
            var task = pool.AcquireAsync("m"); await Until(() => pool.Status.Waiting == 1); pool.Replace([A() with { Disabled = true }]);
            await Failure(task, "no_eligible_account"); Check(pool.Status.Active == 1 && pool.Status.Waiting == 0, "no extra reservation");
        });
        await Test("empty or wholly blocked pool fails immediately", async () =>
        {
            await Failure(new AccountPool([]).AcquireAsync("m"), "no_eligible_account");
            await Failure(new AccountPool([A() with { CooldownUntil = DateTimeOffset.MaxValue }]).AcquireAsync("m"), "no_eligible_account");
        });
        await Test("all and model allow/block rules intersect and attempted accounts stay excluded", async () =>
        {
            var pool = new AccountPool([A(), A("b"), A("c")], policies: new Dictionary<string, ModelPolicy> { ["m"] = new(Allow: new HashSet<AccountId> { new("a"), new("b") }) },
                global: new(Block: new HashSet<AccountId> { new("a") }));
            using var b = await pool.AcquireAsync("m"); Check(b.Account.Id.Value == "b", "intersection");
            await Failure(pool.AcquireAsync("m", attempted: new HashSet<AccountId> { new("b") }), "no_eligible_account");
        });
        await Test("caller mutation cannot change published model states or policies", async () =>
        {
            var states = new Dictionary<string, AccountModelState> { ["m"] = new(QuotaBlocked: true) }; var allow = new HashSet<AccountId> { new("a") };
            var pool = new AccountPool([A() with { Models = states }, A("b")], policies: new Dictionary<string, ModelPolicy> { ["m"] = new(Allow: allow) });
            states.Clear(); allow.Add(new("b")); await Failure(pool.AcquireAsync("m"), "no_eligible_account");
        });
        await Test("invalid reload is atomic and non-loopback transport is rejected", async () =>
        {
            var pool = new AccountPool([A()]);
            foreach (var candidate in new[] { new[] { A(), A() }, new[] { A() with { Upstream = new("https://example.invalid/") } } })
                try { pool.Replace(candidate); throw new InvalidOperationException("reload accepted"); } catch (ArgumentException) { }
            using var lease = await pool.AcquireAsync("m"); Check(lease.Account.Id.Value == "a", "old snapshot retained");
        });
        await Test("model state cap retains protected blocks and can evict inactive observations", async () =>
        {
            var states = Enumerable.Range(0, 1024).ToDictionary(i => "model" + i, _ => new AccountModelState(QuotaBlocked: true));
            var pool = new AccountPool([A(exhausted: true) with { Models = states }]); await Failure(pool.AcquireAsync("new"), "no_eligible_account");
            states["model0"] = new(); pool.Replace([A(exhausted: true) with { Models = states }]); using var lease = await pool.AcquireAsync("new");
            Check(lease.Account.Models!.Count == 1024 && !lease.Account.Models.ContainsKey("model0") && lease.Account.Models["model1"].QuotaBlocked, "bounded protected states");
        });
        await Test("expiring credit selection respects load and existing affinity", async () =>
        {
            var pool = new AccountPool([A(), A("b") with { CreditExpiresAt = DateTimeOffset.UtcNow.AddDays(1), ExpiringCredit = 1 }], new(ExpiringFirst: true));
            using var b = await pool.AcquireAsync("m"); Check(b.Account.Id.Value == "b", "expiring first");
            using var a = await pool.AcquireAsync("m"); Check(a.Account.Id.Value == "a", "load before expiry");
            using var sticky = await pool.AcquireAsync("m", new("a")); Check(sticky.Account.Id.Value == "a", "affinity before expiry");
        });
        await Test("100 cancellations racing release cannot leak or over-admit", async () =>
        {
            var pool = new AccountPool([A()], new(MaxAccount: 1));
            for (int i = 0; i < 100; i++)
            {
                using var held = await pool.AcquireAsync("m"); using var cancel = new CancellationTokenSource();
                var task = pool.AcquireAsync("m", cancellation: cancel.Token); await Until(() => pool.Status.Waiting == 1);
                await Task.WhenAll(Task.Run(held.Dispose), Task.Run(cancel.Cancel));
                try { using var raced = await task; } catch (OperationCanceledException) { }
                Check(pool.Status.Active == 0 && pool.Status.Waiting == 0, "race cleanup");
            }
        });
        await Test("revalidation rejects foreign or released permits", async () =>
        {
            var pool = new AccountPool([A()]); using var lease = await pool.AcquireAsync("m");
            pool.Revalidate(lease, "m"); var foreign = new AccountPool([A()]);
            try { foreign.Revalidate(lease, "m"); throw new InvalidOperationException("foreign lease"); } catch (SchedulingFailure) { }
            lease.Dispose(); try { pool.Revalidate(lease, "m"); throw new InvalidOperationException("released lease"); } catch (SchedulingFailure) { }
        });
        await Test("revalidation catches removal endpoint changes and policy changes", async () =>
        {
            for (int mode = 0; mode < 3; mode++)
            {
                var pool = new AccountPool([A()]); using var lease = await pool.AcquireAsync("m");
                if (mode == 0) pool.Replace([]);
                else if (mode == 1) pool.Replace([A() with { Upstream = new("http://127.0.0.1:2/") }]);
                else pool.Replace([A()], global: new(Block: new HashSet<AccountId> { new("a") }));
                try { pool.Revalidate(lease, "m"); throw new InvalidOperationException("stale lease"); } catch (SchedulingFailure) { }
            }
        });
        await Test("revalidation never promotes a normal reservation into an exhausted probe", async () =>
        {
            var pool = new AccountPool([A()]); using var lease = await pool.AcquireAsync("m");
            pool.Replace([A(exhausted: true)]); try { pool.Revalidate(lease, "m"); throw new InvalidOperationException("promoted probe"); } catch (SchedulingFailure) { }
            pool.Replace([A(exhausted: true, cost: ModelCost.Free)]); pool.Revalidate(lease, "m");
        });
        await Test("a committed probe can revalidate its own reservation after maintenance", async () =>
        {
            var pool = new AccountPool([A(exhausted: true)]); using var lease = await pool.AcquireAsync("m"); pool.Revalidate(lease, "m");
            pool.SetCredentialAvailability(new("a"), true, true, default);
            try { pool.Revalidate(lease, "m"); throw new InvalidOperationException("disabled after maintenance"); } catch (SchedulingFailure) { }
        });
        return passed;
    }
}
