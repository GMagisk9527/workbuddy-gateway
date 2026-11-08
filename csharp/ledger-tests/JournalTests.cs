using System.Text;
using System.Text.Json;
using KeySecurity;
using LedgerMigration;
using static LedgerChecks.Fixtures;

namespace LedgerChecks;

internal static class JournalTests
{
    internal static async Task<List<string>> Run(string directory)
    {
        var passed = new List<string>(); void Test(string name, Action action) { action(); passed.Add(name); }
        Test("begin is durable before return; measured credit settles once", () =>
        {
            var files = new MemoryLedger(); using var ledger = new CreditLedger(files.Open()); var attempt = ledger.Begin("key");
            Check(Encoding.UTF8.GetString(files.Data[CreditLedger.WalName]).Contains("begin", StringComparison.Ordinal) && ledger.Health.PendingAttempts == 1, "durable intent");
            Check(ledger.Complete(attempt, "{\"credit\":2.5,\"prompt_tokens\":20,\"completion_tokens\":3,\"cached_tokens\":7}", 200) == SettlementKind.Charged, "charged");
            Check(ledger.Complete(attempt, "{\"credit\":999}", 200) == SettlementKind.Duplicate && ledger.GetUsage("key") is { Total: 2.5, Requests: 1, Input: 20, Output: 3, Cached: 7 } && ledger.Health.PendingAttempts == 0, "idempotence");
        });
        Test("zero and unknown remain distinct and unknown tolerance blocks limited keys", () =>
        {
            using var ledger = new CreditLedger(new MemoryLedger().Open()); ledger.Complete(ledger.Begin("key"), "{\"credit\":0}", 200);
            for (int i = 0; i < 10; i++) ledger.Complete(ledger.Begin("key"), i % 2 == 0 ? null : "{\"credit\":-1}", 502);
            Check(ledger.GetUsage("key") is { Total: 0, ZeroCost: 1, Unknown: 10, Requests: 11 }, "classification");
            Check(Failure(() => ledger.Check(Policy(total: 1))) == "credit_quota_exhausted", "unknown cap"); ledger.Check(Policy());
        });
        Test("confirmed uncharged cancel is durable and never increments usage", () =>
        {
            var files = new MemoryLedger(); using (var ledger = new CreditLedger(files.Open())) { var a = ledger.Begin("key"); ledger.CancelUncharged(a); Check(ledger.Complete(a, null, 500) == SettlementKind.Duplicate, "cancel idempotent"); }
            using var loaded = new CreditLedger(files.Open(), ["key"]); Check(loaded.Health.Healthy && loaded.GetUsage("key").Requests == 0, "cancel replay");
        });
        Test("credit on failed status remains chargeable", () =>
        {
            using var ledger = new CreditLedger(new MemoryLedger().Open()); ledger.Complete(ledger.Begin("key"), "{\"credit\":3}", 502); Check(ledger.GetUsage("key").Total == 3, "failed measured credit");
        });
        Test("total daily and monthly limits stop new admissions", () =>
        {
            using var ledger = new CreditLedger(new MemoryLedger().Open()); ledger.Complete(ledger.Begin("key"), "{\"credit\":3}", 200);
            foreach (var policy in new[] { Policy(total: 3), Policy(daily: 3), Policy(monthly: 3) }) Check(Failure(() => ledger.Check(policy)) == "credit_quota_exhausted", "quota cap");
            ledger.Check(Policy(total: 4, daily: 4, monthly: 4));
        });
        Test("CST day and month rollover preserve total lifetime and reset windows", () =>
        {
            var time = new FakeClock(new DateTimeOffset(2026, 11, 30, 15, 59, 59, TimeSpan.Zero)); var files = new MemoryLedger();
            using (var ledger = new CreditLedger(files.Open(), clock: time))
            {
                ledger.Complete(ledger.Begin("key"), "{\"credit\":4}", 200); time.Now = time.Now.AddSeconds(1);
                var u = ledger.GetUsage("key"); Check(u is { Total: 4, Lifetime: 4, Daily: 0, Monthly: 0, SpentToday: 0, DayKey: "2026-12-01", MonthKey: "2026-12" }, "CST rollover");
                ledger.Complete(ledger.Begin("key"), "{\"credit\":2}", 200);
            }
            using var loaded = new CreditLedger(files.Open(), ["key"], time); Check(loaded.GetUsage("key") is { Total: 6, Daily: 2, Monthly: 2, SpentToday: 2 }, "rollover replay");
        });
        Test("reset clears only admission amounts and unknowns; factual history survives", () =>
        {
            var files = new MemoryLedger(); using (var ledger = new CreditLedger(files.Open()))
            {
                ledger.Complete(ledger.Begin("key"), "{\"credit\":5,\"prompt_tokens\":2}", 200); ledger.Complete(ledger.Begin("key"), "{\"credit\":0}", 200); ledger.Complete(ledger.Begin("key"), null, 500);
                ledger.Reset("key"); Check(ledger.GetUsage("key") is { Total: 0, Daily: 0, Monthly: 0, Unknown: 0, Lifetime: 5, SpentToday: 5, Requests: 3, Input: 2, ZeroCost: 1 }, "reset facts");
            }
            using var loaded = new CreditLedger(files.Open(), ["key"]); Check(loaded.GetUsage("key") is { Lifetime: 5, Total: 0, Requests: 3, ZeroCost: 1 }, "reset replay");
        });
        Test("inflight settlement after reset belongs to new quota window", () =>
        {
            using var ledger = new CreditLedger(new MemoryLedger().Open()); var pending = ledger.Begin("key"); ledger.Complete(ledger.Begin("key"), "{\"credit\":3}", 200); ledger.Reset("key"); ledger.Complete(pending, "{\"credit\":2}", 200);
            Check(ledger.GetUsage("key") is { Total: 2, Lifetime: 5, Requests: 2, SpentToday: 5 }, "reset then settle");
        });
        Test("unmatched begin after restart blocks without inventing zero cost", () =>
        {
            var files = new MemoryLedger(); using (var ledger = new CreditLedger(files.Open())) ledger.Begin("key");
            using var loaded = new CreditLedger(files.Open(), ["key"]); Check(!loaded.Health.Healthy && loaded.Health.Incomplete && loaded.Health.PendingAttempts == 1 && Failure(() => loaded.Check(Policy(total: 1))) == "ledger_unavailable", "unresolved intent");
        });
        foreach (string stage in new[] { "before-append", "after-append", "sync" })
            Test($"settlement {stage} failure publishes no usage and blocks restart", () =>
            {
                var files = new MemoryLedger(); using (var ledger = new CreditLedger(files.Open()))
                {
                    var attempt = ledger.Begin("key"); files.Fault = (op, _) => { if (op == stage) throw new IOException("injected"); };
                    Check(Failure(() => ledger.Complete(attempt, "{\"credit\":2}", 200)) == "ledger_unavailable" && ledger.GetUsage("key").Requests == 0 && !ledger.Health.Healthy, "no memory publication");
                    Check(Failure(() => ledger.Complete(attempt, "{\"credit\":2}", 200)) == "ledger_unavailable", "no blind retry");
                }
                files.Fault = null; using var loaded = new CreditLedger(files.Open(), ["key"]); Check(!loaded.Health.Healthy, "sticky storage fault");
            });
        Test("partial WAL plus failure to persist error marker retains safe restart", () =>
        {
            var files = new MemoryLedger(); using (var ledger = new CreditLedger(files.Open()))
            {
                var a = ledger.Begin("key"); files.ShortWrite = 15; files.Fault = (op, name) => { if (op == "before-replace" && name == CreditLedger.StateName) throw new IOException("marker unavailable"); };
                Check(Failure(() => ledger.Complete(a, "{\"credit\":2}", 200)) == "ledger_unavailable", "short write");
            }
            files.Fault = null; files.ShortWrite = null; byte[] damaged = files.Data[CreditLedger.WalName].ToArray();
            using var loaded = new CreditLedger(files.Open(), ["key"]); Check(!loaded.Health.Healthy && loaded.Health.PendingAttempts == 1 && damaged.SequenceEqual(files.Data[CreditLedger.WalName]), "preserved ambiguous tail");
        });
        Test("complete last JSON row without newline is recovered exactly once", () =>
        {
            var files = new MemoryLedger(); using (var ledger = new CreditLedger(files.Open())) ledger.Complete(ledger.Begin("key"), "{\"credit\":2}", 200);
            files.Data[CreditLedger.WalName] = files.Data[CreditLedger.WalName][..^1];
            using (var loaded = new CreditLedger(files.Open(), ["key"])) Check(loaded.Health.Healthy && loaded.GetUsage("key").Total == 2 && files.Data[CreditLedger.WalName][^1] == '\n', "newline recovery");
            using var again = new CreditLedger(files.Open(), ["key"]); Check(again.GetUsage("key").Requests == 1, "no duplicate");
        });
        Test("checkpoint snapshot and stale WAL never double apply costs", () =>
        {
            var files = new MemoryLedger(); byte[] old;
            using (var ledger = new CreditLedger(files.Open())) { ledger.Complete(ledger.Begin("key"), "{\"credit\":4}", 200); old = files.Data[CreditLedger.WalName].ToArray(); ledger.Compact(); }
            files.Data[CreditLedger.WalName] = old;
            using var loaded = new CreditLedger(files.Open(), ["key"]); Check(loaded.Health.Healthy && loaded.GetUsage("key") is { Total: 4, Requests: 1 }, "snapshot duplicate suppression");
        });
        foreach (string stage in new[] { "before-replace", "after-replace" })
            foreach (string file in new[] { CreditLedger.SnapshotName, CreditLedger.StateName, CreditLedger.WalName })
                Test($"checkpoint {stage} {file} failure preserves consumption", () =>
                {
                    var backend = new MemoryLedger(); using (var ledger = new CreditLedger(backend.Open()))
                    {
                        ledger.Complete(ledger.Begin("key"), "{\"credit\":4}", 200); bool fired = false;
                        backend.Fault = (op, path) => { if (!fired && op == stage && path == file) { fired = true; throw new IOException("checkpoint failure"); } };
                        Check(Failure(ledger.Compact) == "ledger_unavailable", "checkpoint failure");
                    }
                    backend.Fault = null; using var loaded = new CreditLedger(backend.Open(), ["key"]); Check(loaded.GetUsage("key").Total == 4 && !loaded.Health.Healthy, "cost retained");
                });
        Test("missing snapshot or missing WAL cannot reopen a limited key", () =>
        {
            foreach (string missing in new[] { CreditLedger.WalName, CreditLedger.SnapshotName })
            {
                var files = new MemoryLedger(); using (var ledger = new CreditLedger(files.Open())) { ledger.Complete(ledger.Begin("key"), "{\"credit\":2}", 200); ledger.Compact(); }
                files.Data.Remove(missing); using var loaded = new CreditLedger(files.Open(), ["key"]); Check(!loaded.Health.Healthy, "missing history");
            }
            using var untrusted = new CreditLedger(new MemoryLedger().Open(), ["existing-key"]); Check(!untrusted.Health.Healthy, "no initial history for existing keys");
        });
        Test("corrupt middle rows sequence gaps duplicates and bad state reject startup", () =>
        {
            foreach (string wal in new[] { "{}\n", "{bad}\n{}\n", "{\"seq\":2,\"key\":\"key\",\"kind\":\"reset\"}\n", "{\"seq\":1,\"key\":\"key\",\"kind\":\"reset\"}\n{\"seq\":1,\"key\":\"key\",\"kind\":\"reset\"}\n" })
            { var files = new MemoryLedger(); files.Data[CreditLedger.WalName] = Encoding.UTF8.GetBytes(wal); int rejected = 0; try { using var ledger = new CreditLedger(files.Open()); } catch (InvalidDataException) { rejected++; } Check(rejected == 1, "corrupt WAL"); }
            foreach (string state in new[] { "null", "{}", "{\"version\":2}", "{bad}" })
            { var files = new MemoryLedger(); files.Data[CreditLedger.StateName] = Encoding.UTF8.GetBytes(state); int rejected = 0; try { using var ledger = new CreditLedger(files.Open()); } catch (InvalidDataException) { rejected++; } Check(rejected == 1, "bad state"); }
        });
        Test("snapshot-to-WAL gap is rejected", () =>
        {
            var files = new MemoryLedger(); using (var ledger = new CreditLedger(files.Open())) { ledger.Complete(ledger.Begin("key"), "{\"credit\":2}", 200); ledger.Compact(); }
            files.Data[CreditLedger.WalName] = "{\"seq\":4,\"key\":\"key\",\"kind\":\"reset\"}\n"u8.ToArray();
            int blocked = 0; try { using var loaded = new CreditLedger(files.Open(), ["key"]); } catch (InvalidDataException) { blocked++; }
            Check(blocked == 1, "snapshot sequence gap");
        });
        Test("delete during inflight preserves consumption independently of metadata", () =>
        {
            using var store = new KeyStore(new MemoryKeys()); var key = store.Create(new(QuotaTotal: 10)); using var ledger = new CreditLedger(new MemoryLedger().Open()); store.AttachAccounting(ledger);
            var a = ledger.Begin(key.Item.Id); store.Delete(key.Item.Id); ledger.Complete(a, "{\"credit\":3}", 200);
            Check(store.List().IsEmpty && ledger.AllUsage()[key.Item.Id].Lifetime == 3, "deleted history");
        });
        Test("real files checkpoint restart and exclusive writer ownership", () =>
        {
            string path = Path.Combine(directory, "journal-" + Guid.NewGuid().ToString("N"));
            using (var ledger = CreditLedger.Open(path))
            {
                ledger.Complete(ledger.Begin("key"), "{\"credit\":2}", 200); ledger.Compact(); int rejected = 0;
                try { using var second = CreditLedger.Open(path); } catch (IOException) { rejected++; }
                Check(rejected == 1, "exclusive writer");
            }
            using var loaded = CreditLedger.Open(path, ["key"]); Check(loaded.Health.Healthy && loaded.GetUsage("key").Total == 2, "real recovery");
        });
        using (var source = new ObservedThenBlocked())
        using (var cts = new CancellationTokenSource())
        using (var ledger = new CreditLedger(new MemoryLedger().Open()))
        {
            var attempt = ledger.Begin("key"); var task = HttpMigration.MessageAggregate.RunAsync(source, new("test-model"), 1 << 20, cts.Token);
            await source.Blocked.Task.WaitAsync(TimeSpan.FromSeconds(5)); cts.Cancel(); var result = await task;
            ledger.Complete(attempt, result.RawUsageJson, 499);
            Check(result.Code == "cancelled" && ledger.GetUsage("key").Total == 2, "observed aggregate evidence survives cancellation");
            passed.Add("deterministic aggregate cancellation retains parsed usage evidence");
        }
        using (var ledger = new CreditLedger(new MemoryLedger().Open()))
        {
            await Task.WhenAll(Enumerable.Range(0, 64).Select(_ => Task.Run(() => { var a = ledger.Begin("key"); ledger.Complete(a, "{\"credit\":1}", 200); ledger.Complete(a, "{\"credit\":1}", 200); })));
            Check(ledger.GetUsage("key") is { Total: 64, Requests: 64 } && ledger.Health.PendingAttempts == 0, "parallel exact once");
            passed.Add("64 parallel attempts settle once without sequence gaps");
        }
        return passed;
    }
    sealed class ObservedThenBlocked : MemoryStream
    {
        bool sent;
        readonly byte[] prefix = Encoding.UTF8.GetBytes("data: {\"choices\":[{\"delta\":{\"content\":\"text\"}}],\"usage\":{\"credit\":2}}\n\n");
        internal TaskCompletionSource Blocked { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);
        public override async ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default)
        {
            if (!sent) { sent = true; prefix.CopyTo(buffer); return prefix.Length; }
            Blocked.TrySetResult(); await Task.Delay(Timeout.InfiniteTimeSpan, cancellationToken); return 0;
        }
    }
}
