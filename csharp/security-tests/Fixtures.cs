using System.Text.Json;
using KeySecurity;

namespace SecurityChecks;

internal sealed class MemoryBackend
{
    public Dictionary<string, byte[]> Data { get; } = [];
    public Action<string, string>? Fault;
    public IKeyFiles Open() => new Files(this);
    sealed class Files(MemoryBackend backend) : IKeyFiles
    {
        public byte[]? Read(string name) => backend.Data.TryGetValue(name, out var bytes) ? bytes.ToArray() : null;
        public void WriteAtomic(string name, ReadOnlySpan<byte> bytes) { backend.Fault?.Invoke("before-write", name); backend.Data[name] = bytes.ToArray(); backend.Fault?.Invoke("after-replace", name); }
        public void Delete(string name) { backend.Fault?.Invoke("before-delete", name); backend.Data.Remove(name); backend.Fault?.Invoke("after-delete", name); }
        public void Dispose() { }
    }
    public void FailOnce(string operation, string name)
    {
        bool fired = false; Fault = (op, path) => { if (!fired && op == operation && path == name) { fired = true; throw new IOException("Injected key transaction failure"); } };
    }
}
internal static class Fixtures
{
    internal const string Secret = "wbk_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrstuvwxyz";
    internal static readonly JsonSerializerOptions Json = new() { PropertyNamingPolicy = JsonNamingPolicy.CamelCase, PropertyNameCaseInsensitive = true };
    internal static MemoryBackend Store(string secret = Secret, bool enabled = true, long expires = 0, int concurrent = 1, string[]? models = null, double quota = 0)
    {
        var backend = new MemoryBackend();
        backend.Data["apikeys.json"] = JsonSerializer.SerializeToUtf8Bytes(new
        {
            version = 1,
            requireAuth = true,
            keys = new[] { new {
            id = "fixture", name = "fixture", prefix = KeyRules.Prefix(secret), hash = KeyRules.Hash(secret), enabled, createdAt = 1,
            expiresAt = expires, models = models ?? [], maxConcurrent = concurrent, quotaTotal = quota, quotaDaily = 0, quotaMonthly = 0, note = "" } }
        }, Json);
        return backend;
    }
    internal static string Failure(Action action) { try { action(); return ""; } catch (KeyFailure e) { return e.Code; } }
    internal static void Check(bool condition, string message) { if (!condition) throw new InvalidOperationException(message); }
}
