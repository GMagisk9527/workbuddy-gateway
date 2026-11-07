using System.Text.Json;
using KeySecurity;
using LedgerMigration;

namespace LedgerChecks;

internal sealed class MemoryLedger
{
    internal Dictionary<string, byte[]> Data = [];
    internal Action<string, string>? Fault;
    internal int? ShortWrite;
    internal string? LastKind;
    internal ILedgerFiles Open() => new Files(this);
    sealed class Files(MemoryLedger backend) : ILedgerFiles
    {
        public byte[]? Read(string name) => backend.Data.GetValueOrDefault(name)?.ToArray();
        public void Replace(string name, ReadOnlySpan<byte> bytes)
        { backend.Fault?.Invoke("before-replace", name); backend.Data[name] = bytes.ToArray(); backend.Fault?.Invoke("after-replace", name); }
        public int Append(ReadOnlySpan<byte> bytes)
        {
            using var doc = JsonDocument.Parse(bytes.ToArray()); backend.LastKind = doc.RootElement.GetProperty("kind").GetString();
            backend.Fault?.Invoke("before-append", CreditLedger.WalName);
            int count = Math.Min(bytes.Length, backend.ShortWrite ?? bytes.Length);
            backend.Data[CreditLedger.WalName] = [.. backend.Data.GetValueOrDefault(CreditLedger.WalName) ?? [], .. bytes[..count]];
            backend.Fault?.Invoke("after-append", CreditLedger.WalName); return count;
        }
        public void Sync() => backend.Fault?.Invoke("sync", CreditLedger.WalName);
        public void Dispose() { }
    }
}
internal sealed class MemoryKeys : IKeyFiles
{
    readonly Dictionary<string, byte[]> data = [];
    public byte[]? Read(string name) => data.GetValueOrDefault(name)?.ToArray();
    public void WriteAtomic(string name, ReadOnlySpan<byte> bytes) => data[name] = bytes.ToArray();
    public void Delete(string name) => data.Remove(name);
    public void Dispose() { }
}
internal sealed class FakeClock(DateTimeOffset at) : TimeProvider
{
    internal DateTimeOffset Now = at;
    public override DateTimeOffset GetUtcNow() => Now;
}
internal static class Fixtures
{
    internal static readonly JsonSerializerOptions Json = new() { PropertyNamingPolicy = JsonNamingPolicy.CamelCase, PropertyNameCaseInsensitive = true };
    internal static void Check(bool condition, string message) { if (!condition) throw new InvalidOperationException(message); }
    internal static string Failure(Action action) { try { action(); return ""; } catch (KeyFailure e) { return e.Code; } }
    internal static KeyView Policy(double total = 0, double daily = 0, double monthly = 0) => new("key", "key", "wbk_test", true, 1, 0, [], 2, total, daily, monthly, "");
}
