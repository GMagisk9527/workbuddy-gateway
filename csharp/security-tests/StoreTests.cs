using System.Collections.Concurrent;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using KeySecurity;
using static SecurityChecks.Fixtures;

namespace SecurityChecks;

internal static class StoreTests
{
    internal static async Task<List<string>> Run(string root)
    {
        var passed = new List<string>();
        void Test(string name, Action test) { test(); passed.Add(name); }
        Test("fresh secure store denies anonymous; open compatibility is explicit", () =>
        {
            using var secure = new KeyStore(new MemoryBackend().Open()); Check(Failure(() => secure.Acquire("")) == "invalid_api_key", "fresh secure");
            using var open = new KeyStore(new MemoryBackend().Open(), allowAnonymousOnFresh: true); using var lease = open.Acquire(""); Check(lease.Identity is null && !open.RequireAuth, "explicit open");
        });
        Test("create stores only hash and publishes secret once", () =>
        {
            var files = new MemoryBackend(); using var store = new KeyStore(files.Open()); var issued = store.Create(new("first")); string secret = issued.TakeSecret();
            Check(secret.Length == 56 && secret.StartsWith("wbk_", StringComparison.Ordinal) && secret[4..].All(c => "abcdefghijklmnopqrstuvwxyz234567".Contains(c)), "secret format");
            Check(store.Authenticate(secret)?.Id == issued.Item.Id && !Encoding.UTF8.GetString(files.Data["apikeys.json"]).Contains(secret, StringComparison.Ordinal), "hash only");
            int rejected = 0; try { issued.TakeSecret(); } catch (InvalidOperationException) { rejected++; }
            Check(rejected == 1 && !issued.ToString().Contains(secret, StringComparison.Ordinal), "one retrieval");
        });
        Test("successful rotation invalidates old key and survives reload", () =>
        {
            var files = new MemoryBackend(); string old, next; string id;
            using (var store = new KeyStore(files.Open())) { var issued = store.Create(new()); id = issued.Item.Id; old = issued.TakeSecret(); next = store.Rotate(id).TakeSecret(); Check(store.Authenticate(old) is null && store.Authenticate(next)?.Id == id, "rotate live"); }
            using var loaded = new KeyStore(files.Open()); Check(loaded.Authenticate(old) is null && loaded.Authenticate(next)?.Id == id, "rotate reload");
        });
        Test("Go omitempty key documents load with empty note and model list", () =>
        {
            var backend = new MemoryBackend();
            backend.Data["apikeys.json"] = JsonSerializer.SerializeToUtf8Bytes(new { version = 1, requireAuth = true, keys = new[] { new { id = "fixture", name = "fixture", prefix = KeyRules.Prefix(Secret), hash = KeyRules.Hash(Secret), enabled = true, createdAt = 1 } } });
            using var store = new KeyStore(backend.Open()); using var lease = store.Acquire(Secret, "any-model");
            Check(store.List().Single().Models.IsEmpty && store.List().Single().Note == "", "Go omitted fields defaults");
        });
        Test("update and enable changes persist without changing identity", () =>
        {
            var files = new MemoryBackend(); string secret, id;
            using (var store = new KeyStore(files.Open())) { var key = store.Create(new()); secret = key.TakeSecret(); id = key.Item.Id; store.Update(id, new("changed", Models: [" B ", "a", "A"], MaxConcurrent: 99)); store.SetEnabled(id, false); Check(Failure(() => store.Acquire(secret)) == "key_disabled", "disable"); }
            using var loaded = new KeyStore(files.Open()); var view = loaded.List().Single(); Check(view.Name == "changed" && view.Models.SequenceEqual(["a", "b"]) && view.MaxConcurrent == 64 && !view.Enabled, "updated fields");
        });
        Test("delete last key never reopens anonymous after restart or key-table loss", () =>
        {
            var files = new MemoryBackend();
            using (var store = new KeyStore(files.Open(), allowAnonymousOnFresh: true)) { var key = store.Create(new()); store.Delete(key.Item.Id); Check(store.RequireAuth && store.List().IsEmpty, "sticky delete"); }
            files.Data.Remove("apikeys.json"); using var loaded = new KeyStore(files.Open(), allowAnonymousOnFresh: true); Check(loaded.RequireAuth && Failure(() => loaded.Acquire("")) == "invalid_api_key", "sticky file loss");
        });
        foreach (string operation in new[] { "create", "rotate", "update", "toggle", "delete" })
        {
            foreach (string stage in new[] { "before-write", "after-replace" })
                Test($"{operation} rollback at {stage} keeps prior committed state", () =>
                {
                    var files = new MemoryBackend(); string original, id;
                    using (var store = new KeyStore(files.Open()))
                    {
                        var issued = store.Create(new("original")); id = issued.Item.Id; original = issued.TakeSecret(); var before = store.List().Single();
                        files.FailOnce(stage, "apikeys.json");
                        string code = Failure(() => { switch (operation) { case "create": store.Create(new("unreturned")); break; case "rotate": store.Rotate(id); break; case "update": store.Update(id, new("bad update")); break; case "toggle": store.SetEnabled(id, false); break; case "delete": store.Delete(id); break; } });
                        Check(code == "keystore_unavailable" && store.Degraded && store.List().Single() == before && store.Authenticate(original)?.Id == id, "rollback snapshot");
                        Check(Failure(() => store.Acquire(original)) == "keystore_unavailable", "degraded admission");
                    }
                    files.Fault = null; using var reloaded = new KeyStore(files.Open()); Check(reloaded.Authenticate(original)?.Id == id && reloaded.List().Length == 1, "rollback disk");
                });
        }
        Test("failed first create persists auth intent without an unreturned key", () =>
        {
            var files = new MemoryBackend(); files.FailOnce("before-write", "keyauth.json");
            using (var store = new KeyStore(files.Open(), allowAnonymousOnFresh: true)) Check(Failure(() => store.Create(new())) == "keystore_unavailable" && store.List().IsEmpty && store.RequireAuth, "first create failure");
            using var loaded = new KeyStore(files.Open(), allowAnonymousOnFresh: true); Check(loaded.RequireAuth && loaded.List().IsEmpty, "durable first intent");
        });
        Test("failed rollback leaves pending marker and refuses restart", () =>
        {
            var files = Store(); files.Fault = (op, name) => { if (name == "apikeys.json" && op == "before-write") throw new IOException("persistent failure"); };
            using (var store = new KeyStore(files.Open())) Check(Failure(() => store.Rotate("fixture")) == "keystore_unavailable" && files.Data.ContainsKey("keystore.pending"), "pending remains");
            int blocked = 0; try { using var reopened = new KeyStore(files.Open()); } catch (InvalidDataException) { blocked++; }
            Check(blocked == 1, "pending restart");
        });
        Test("failure deleting transaction marker rolls back rather than publishing", () =>
        {
            var files = Store(); files.FailOnce("before-delete", "keystore.pending");
            using (var store = new KeyStore(files.Open())) Check(Failure(() => store.Rotate("fixture")) == "keystore_unavailable" && store.Authenticate(Secret) is not null, "delete marker rollback");
            using var loaded = new KeyStore(files.Open()); Check(loaded.Authenticate(Secret) is not null, "marker rollback reload");
        });
        Test("corrupt documents and duplicate key identities fail startup", () =>
        {
            foreach (var pair in new[] { ("keyauth.json", "null"), ("keyauth.json", "{}"), ("keyauth.json", "{\"requireAuth\":1}"), ("apikeys.json", "null"), ("apikeys.json", "{}"), ("apikeys.json", "{\"version\":2,\"requireAuth\":true,\"keys\":[]}") })
            { var files = new MemoryBackend(); files.Data[pair.Item1] = Encoding.UTF8.GetBytes(pair.Item2); int blocked = 0; try { using var store = new KeyStore(files.Open()); } catch (Exception e) when (e is InvalidDataException or JsonException) { blocked++; } Check(blocked == 1, "corrupt startup"); }
            var duplicates = Store(); using var parsed = JsonDocument.Parse(duplicates.Data["apikeys.json"]); string entry = parsed.RootElement.GetProperty("keys")[0].GetRawText(); duplicates.Data["apikeys.json"] = Encoding.UTF8.GetBytes("{\"version\":1,\"requireAuth\":true,\"keys\":[" + entry + "," + entry + "]}");
            int rejected = 0; try { using var store = new KeyStore(duplicates.Open()); } catch (InvalidDataException) { rejected++; }
            Check(rejected == 1, "duplicate rejection");
        });
        Test("disabled expired model denied and quota-bearing keys reject correctly", () =>
        {
            foreach (var item in new[] { (Store(enabled: false), "key_disabled"), (Store(expires: 1), "key_expired"), (Store(models: ["other"]), "model_not_allowed"), (Store(quota: 1), "ledger_unavailable") })
            { using var store = new KeyStore(item.Item1.Open()); Check(Failure(() => store.Acquire(Secret, "test-model")) == item.Item2, "policy reason"); }
        });
        Test("same-prefix wrong hash and whitespace token semantics", () =>
        {
            using var store = new KeyStore(Store().Open()); Check(store.Authenticate(Secret[..^1] + "!") is null && store.Authenticate(" " + Secret + " ") is not null && store.Authenticate("") is null, "hash auth");
        });
        Test("leases are idempotent and rotation does not reset concurrency", () =>
        {
            var files = Store(); using var store = new KeyStore(files.Open()); var lease = store.Acquire(Secret); string next = store.Rotate("fixture").TakeSecret();
            Check(Failure(() => store.Acquire(next)) == "key_concurrency_exceeded", "rotation preserves inflight"); lease.Dispose(); lease.Dispose(); using var again = store.Acquire(next); Check(Failure(() => store.Acquire(next)) == "key_concurrency_exceeded", "release once");
        });
        Test("legacy identity preserves unlimited policy; credentials do not print secrets", () =>
        {
            var credential = new Credential("synthetic-legacy"); using var store = new KeyStore(new MemoryBackend().Open(), legacy: credential);
            using var first = store.Acquire("synthetic-legacy", "any"); using var second = store.Acquire("synthetic-legacy", "any");
            Check(first.Identity?.Legacy == true && !credential.ToString().Contains("synthetic-legacy", StringComparison.Ordinal), "legacy");
        });
        Test("reasoning keys are credential scoped and cleared on release", () =>
        {
            using var store = new KeyStore(Store().Open()); var lease = store.Acquire(Secret); ReadOnlyMemory<byte> key = lease.ReasoningKey;
            Check(key.Span.SequenceEqual(KeyRules.ReasoningKey(Secret)), "scoped key"); lease.Dispose(); Check(key.Span.IndexOfAnyExcept((byte)0) < 0, "cleared key");
        });
        Test("random source failure creates no key and performs no weak fallback", () =>
        {
            var backend = new MemoryBackend(); using var store = new KeyStore(backend.Open(), entropy: new BrokenEntropy()); int rejected = 0;
            try { store.Create(new()); } catch (CryptographicException) { rejected++; }
            Check(rejected == 1 && backend.Data.Count == 0 && store.List().IsEmpty, "entropy failure");
        });
        Test("real file flush/replace reload and exclusive directory ownership", () =>
        {
            string dir = Path.Combine(root, "disk-" + Guid.NewGuid().ToString("N")); string secret;
            using (var store = KeyStore.Open(dir))
            {
                secret = store.Create(new("disk")).TakeSecret(); int rejected = 0; try { using var second = KeyStore.Open(dir); } catch (IOException) { rejected++; }
                Check(rejected == 1 && !Directory.EnumerateFiles(dir).Any(p => p.EndsWith(".tmp", StringComparison.Ordinal)) && !File.ReadAllText(Path.Combine(dir, "apikeys.json")).Contains(secret, StringComparison.Ordinal), "atomic file owner");
            }
            using var loaded = KeyStore.Open(dir); Check(loaded.Authenticate(secret) is not null, "real disk reload");
        });
        Test("real replace failure leaves marker and no published key", () =>
        {
            string dir = Path.Combine(root, "replace-failure-" + Guid.NewGuid().ToString("N")); using var store = KeyStore.Open(dir); Directory.CreateDirectory(Path.Combine(dir, "apikeys.json"));
            Check(Failure(() => store.Create(new())) == "keystore_unavailable" && store.List().IsEmpty && File.Exists(Path.Combine(dir, "keystore.pending")), "real failure");
        });
        Test("preview CLI refuses existing unowned data and reopens its own directory", () =>
        {
            string dir = Path.Combine(root, "unowned-" + Guid.NewGuid().ToString("N")); Directory.CreateDirectory(dir); File.WriteAllText(Path.Combine(dir, "existing.txt"), "unchanged"); int blocked = 0;
            try { using var unexpected = HttpMigration.PreviewKeyDirectory.Open(dir); } catch (ArgumentException) { blocked++; }
            Check(blocked == 1 && File.ReadAllText(Path.Combine(dir, "existing.txt")) == "unchanged" && !File.Exists(Path.Combine(dir, ".csharp-preview")), "existing directory untouched");
            string own = Path.Combine(root, "owned-" + Guid.NewGuid().ToString("N")); string secret;
            using (var store = HttpMigration.PreviewKeyDirectory.Open(own)) secret = store.Create(new()).TakeSecret();
            using var reloaded = HttpMigration.PreviewKeyDirectory.Open(own); Check(reloaded.Authenticate(secret) is not null, "owned preview reload");
        });
        var concurrentFiles = Store(concurrent: 3); using (var store = new KeyStore(concurrentFiles.Open()))
        {
            var releases = new ConcurrentBag<KeyLease>(); var start = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            await Task.WhenAll(Enumerable.Range(0, 64).Select(async _ => { await start.Task; try { releases.Add(store.Acquire(Secret)); } catch (KeyFailure e) when (e.Code == "key_concurrency_exceeded") { } }).Append(Task.Run(() => start.SetResult())));
            Check(releases.Count == 3, "parallel bound"); foreach (var lease in releases) lease.Dispose(); using var next = store.Acquire(Secret);
            passed.Add("64 parallel admissions honor exact per-key bound and release");
        }
        return passed;
    }
    sealed class BrokenEntropy : IKeyEntropy { public byte[] Bytes(int count) => throw new CryptographicException("Synthetic entropy failure"); }
}
