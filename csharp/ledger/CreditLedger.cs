using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Text.Json.Serialization.Metadata;
using KeySecurity;

namespace LedgerMigration;

public sealed partial class CreditLedger : IKeyAccounting, IDisposable
{
    public const string WalName = "keyledger.wal", SnapshotName = "keyledger.snapshot.json", StateName = "keyledger.state.json";
    sealed record DiskState(int Version = 0, bool SnapshotRequired = false, bool Incomplete = false);
    sealed record DiskSnapshot(int Version, long LastSeq, Dictionary<string, CreditUsage>? Keys,
        Dictionary<string, CreditUsage>? Archived = null, long LostSettlements = 0, Dictionary<string, string>? PendingAttempts = null, string[]? ResolvedAttempts = null);
    [JsonSourceGenerationOptions(PropertyNamingPolicy = JsonKnownNamingPolicy.CamelCase, DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingDefault)]
    [JsonSerializable(typeof(DiskState))]
    [JsonSerializable(typeof(DiskSnapshot))]
    [JsonSerializable(typeof(LedgerEntry))]
    partial class LedgerJson : JsonSerializerContext {}
    readonly object gate = new();
    readonly object ioGate = new();
    readonly object waitGate = new();
    readonly ILedgerFiles files;
    readonly TimeProvider clock;
    readonly Guid owner = Guid.NewGuid();
    readonly Dictionary<string, CreditUsage> usage = [];
    readonly Dictionary<string, string> pending = [];
    readonly HashSet<string> resolved = [];
    readonly Queue<string> resolvedOrder = [];
    readonly Queue<LedgerEntry> pendingWrites = [];
    readonly HashSet<string> submitted = [];
    Thread? flusher;
    DiskState state = new(1);
    long seq;
    long appliedSeq;
    int writes;
    bool stopping, flushActive, disposed;
    volatile bool failed;
    // Compaction rewrites snapshot/state and truncates the WAL under the gate.
    // A larger batch keeps the occasional multi-file fsync off the hot path;
    // replaying a few thousand WAL rows on restart remains bounded and cheap.
    const int CompactEvery = 4096;
    bool blocked;

    public CreditLedger(ILedgerFiles files, IEnumerable<string>? knownKeys = null, TimeProvider? clock = null)
    {
        this.files = files; this.clock = clock ?? TimeProvider.System;
        try { Load((knownKeys ?? []).ToHashSet(StringComparer.Ordinal)); } catch { files.Dispose(); throw; }
        flusher = new Thread(FlushLoop) { IsBackground = true, Name = "ledger-flusher" };
        flusher.Start();
    }
    public static CreditLedger Open(string directory, IEnumerable<string>? knownKeys = null, TimeProvider? clock = null) => new(new LedgerFiles(directory), knownKeys, clock);
    void EnsureOpen() { if (disposed) throw new ObjectDisposedException(nameof(CreditLedger)); }
    void RequireHealthy() { EnsureOpen(); if (blocked || failed || state.Incomplete) throw new KeyFailure(503, "ledger_unavailable", "记账存储不可用，已暂停请求"); }
    public LedgerHealth Health { get { lock (gate) return new(!disposed && !blocked && !failed && !state.Incomplete, state.Incomplete, appliedSeq, pending.Count); } }
    static T Read<T>(byte[] raw, JsonTypeInfo<T> type)
    {
        try { return JsonSerializer.Deserialize(raw, type) ?? throw new InvalidDataException("Invalid ledger document"); }
        catch (JsonException e) { throw new InvalidDataException("Invalid ledger JSON", e); }
    }
    void StateToDisk() { lock (ioGate) files.Replace(StateName, JsonSerializer.SerializeToUtf8Bytes(state, LedgerJson.Default.DiskState)); }
    void Fault()
    {
        failed = true; blocked = true; state = state with { Incomplete = true };
        try { StateToDisk(); } catch (Exception e) when (e is IOException or UnauthorizedAccessException) { /* A durable unmatched begin remains before any upstream send. */ }
        lock (gate) Monitor.PulseAll(gate);
        lock (waitGate) Monitor.PulseAll(waitGate);
    }
    void Remember(string? attempt)
    {
        if (string.IsNullOrEmpty(attempt)) return;
        if (resolved.Add(attempt)) resolvedOrder.Enqueue(attempt);
        while (resolvedOrder.Count > 8192) resolved.Remove(resolvedOrder.Dequeue());
    }
    static void Validate(LedgerEntry e)
    {
        if (e.Seq <= 0 || e.Key is null || e.Key.Length > 256 || e.Attempt?.Length > 128 || e.Kind is not ("" or "settle" or "begin" or "cancel" or "reset" or "recovery") ||
            !double.IsFinite(e.Credit) || e.Credit < 0 || e.Credit > 1e308 || e.Input < 0 || e.Output < 0 || e.Cached < 0)
            throw new InvalidDataException("Invalid ledger entry");
        if (e.Kind != "recovery" && e.Key.Length == 0) throw new InvalidDataException("Missing ledger key");
        if (e.Kind is "begin" or "cancel" && string.IsNullOrEmpty(e.Attempt)) throw new InvalidDataException("Missing ledger attempt");
        if (e.Kind is "" or "settle" && ((e.Known ? 1 : 0) + (e.Zero ? 1 : 0) + (e.Unknown ? 1 : 0) != 1 || e.Known && e.Credit <= 0))
            throw new InvalidDataException("Ambiguous ledger settlement");
        if (!e.Known && e.Credit != 0 || e.Kind is "begin" or "cancel" or "reset" && (e.Known || e.Zero || e.Unknown || e.Credit != 0 || e.Input != 0 || e.Output != 0 || e.Cached != 0))
            throw new InvalidDataException("Conflicting ledger entry fields");
    }
    void Load(HashSet<string> knownKeys)
    {
        byte[]? stateRaw = files.Read(StateName), snapshotRaw = files.Read(SnapshotName), walRaw = files.Read(WalName);
        state = stateRaw is null ? new(1) : Read(stateRaw, LedgerJson.Default.DiskState);
        if (state.Version != 1) throw new InvalidDataException("Unsupported ledger state version");
        if (snapshotRaw is not null)
        {
            var snap = Read(snapshotRaw, LedgerJson.Default.DiskSnapshot);
            if (snap.Version != 1 || snap.LastSeq < 0 || snap.LostSettlements < 0) throw new InvalidDataException("Invalid ledger snapshot");
            foreach (var pair in (snap.Keys ?? []).Concat(snap.Archived ?? []))
            {
                if (pair.Value is null || usage.ContainsKey(pair.Key) || string.IsNullOrEmpty(pair.Key)) throw new InvalidDataException("Duplicate or invalid snapshot key");
                CreditRules.ValidateUsage(pair.Value); usage.Add(pair.Key, CreditRules.Roll(pair.Value, clock.GetUtcNow()));
            }
            foreach (var pair in snap.PendingAttempts ?? [])
            { if (string.IsNullOrEmpty(pair.Key) || string.IsNullOrEmpty(pair.Value)) throw new InvalidDataException("Invalid pending attempt"); pending.Add(pair.Key, pair.Value); }
            foreach (string attempt in snap.ResolvedAttempts ?? []) Remember(attempt);
            seq = snap.LastSeq; state = state with { SnapshotRequired = true, Incomplete = state.Incomplete || snap.LostSettlements > 0 };
        }
        else if (state.SnapshotRequired) state = state with { Incomplete = true };
        if (walRaw is null && (stateRaw is not null || snapshotRaw is not null || knownKeys.Count > 0)) state = state with { Incomplete = true };
        string raw;
        try { raw = new UTF8Encoding(false, true).GetString(walRaw ?? []); }
        catch (DecoderFallbackException e) { throw new InvalidDataException("Invalid WAL encoding", e); }
        bool addNewline = false;
        int lastNewline = raw.LastIndexOf('\n');
        if (lastNewline + 1 < raw.Length)
        {
            try { Read(Encoding.UTF8.GetBytes(raw[(lastNewline + 1)..]), LedgerJson.Default.LedgerEntry); addNewline = true; }
            catch (InvalidDataException) { raw = raw[..(lastNewline + 1)]; state = state with { Incomplete = true }; }
        }
        long previous = 0, nextReplay = checked(seq + 1), snapshotSeq = seq;
        foreach (string line in raw.Split('\n'))
        {
            if (string.IsNullOrWhiteSpace(line)) continue;
            var entry = Read(Encoding.UTF8.GetBytes(line), LedgerJson.Default.LedgerEntry);
            if (entry.Seq <= 0 || previous > 0 && entry.Seq != previous + 1) throw new InvalidDataException("WAL sequence gap or duplicate");
            previous = entry.Seq;
            if (entry.Seq <= snapshotSeq) continue;
            if (entry.Seq != nextReplay++) throw new InvalidDataException("WAL missing sequence after snapshot");
            Validate(entry); Apply(entry); seq = entry.Seq; writes++;
        }
        if (snapshotRaw is null && stateRaw is null && knownKeys.Count > 0 && previous == 0) state = state with { Incomplete = true };
        if (pending.Count > 0) state = state with { Incomplete = true };
        if (walRaw is null && !state.Incomplete) files.Replace(WalName, []);
        if (addNewline && !state.Incomplete) files.Replace(WalName, Encoding.UTF8.GetBytes(raw + "\n"));
        StateToDisk(); blocked = state.Incomplete; appliedSeq = seq;
    }
    CreditUsage Current(string key) => CreditRules.Roll(usage.GetValueOrDefault(key) ?? new(), clock.GetUtcNow());
    CreditUsage? NextUsage(LedgerEntry entry) => entry.Kind is "begin" or "cancel" ? null : CreditRules.Apply(Current(entry.Key), entry, clock.GetUtcNow());
    void Apply(LedgerEntry entry)
    {
        if (entry.Kind == "begin")
        {
            if (pending.ContainsKey(entry.Attempt!) || resolved.Contains(entry.Attempt!)) throw new InvalidDataException("Duplicate attempt begin");
            pending.Add(entry.Attempt!, entry.Key); return;
        }
        if (entry.Attempt is { Length: > 0 } id)
        {
            if (resolved.Contains(id)) throw new InvalidDataException("Duplicate attempt settlement");
            if (pending.TryGetValue(id, out var key) && key != entry.Key) throw new InvalidDataException("Attempt key mismatch");
            pending.Remove(id); Remember(id);
        }
        if (entry.Kind == "cancel") return;
        if (entry.Kind == "recovery" && entry.Key.Length == 0) { state = state with { Incomplete = true }; return; }
        usage[entry.Key] = NextUsage(entry)!;
    }
    // One flusher thread fsyncs all rows submitted since the previous batch and
    // then applies them in sequence order. Callers still block until their own
    // row is durable, so Begin keeps its "durable before upstream" contract.
    void Submit(LedgerEntry template, LedgerAttempt? attempt, out long target)
    {
        target = 0;
        lock (gate)
        {
            RequireHealthy();
            if (attempt is not null)
            {
                if (attempt.Owner != owner) throw new ArgumentException("Attempt belongs to another ledger");
                if (resolved.Contains(attempt.Id) || submitted.Contains(attempt.Id)) { target = -1; return; }
                if (!pending.TryGetValue(attempt.Id, out var key) || key != attempt.Key) throw new InvalidOperationException("Unknown ledger attempt");
            }
            var entry = template with { Seq = checked(seq + 1) };
            Validate(entry);
            byte[] row = Encoding.UTF8.GetBytes(JsonSerializer.Serialize(entry, LedgerJson.Default.LedgerEntry) + "\n");
            try { lock (ioGate) { if (files.Append(row) != row.Length) throw new IOException("Short WAL write"); } }
            catch (Exception e) when (e is IOException or UnauthorizedAccessException or OverflowException)
            { Fault(); throw new KeyFailure(503, "ledger_unavailable", "账本写入失败，未发布结算，已暂停请求"); }
            seq = entry.Seq; pendingWrites.Enqueue(entry);
            if (attempt is not null) submitted.Add(attempt.Id);
            if (!flushActive) { flushActive = true; Monitor.PulseAll(gate); }
            target = entry.Seq;
        }
        lock (waitGate)
        {
            while (appliedSeq < target)
            {
                if (failed || blocked) throw new KeyFailure(503, "ledger_unavailable", "账本写入失败，未发布结算，已暂停请求");
                Monitor.Wait(waitGate);
            }
        }
    }
    public LedgerAttempt Begin(string key)
    {
        string id = Guid.NewGuid().ToString("N");
        Submit(new(0, key, id, "begin", At: clock.GetUtcNow().ToUnixTimeMilliseconds()), null, out _);
        return new(owner, id, key);
    }
    public SettlementKind Complete(LedgerAttempt attempt, string? rawUsage, int status)
    {
        var observed = CreditRules.Observe(rawUsage); var window = CreditRules.Window(clock.GetUtcNow());
        Submit(new(0, attempt.Key, attempt.Id, Credit: observed.Credit, Known: observed.Kind == SettlementKind.Charged,
            Zero: observed.Kind == SettlementKind.Zero, Unknown: observed.Kind == SettlementKind.Unknown, At: clock.GetUtcNow().ToUnixTimeMilliseconds(), DayKey: window.Day, MonthKey: window.Month,
            Input: observed.Input, Output: observed.Output, Cached: observed.Cached, Status: status), attempt, out var target);
        return target < 0 ? SettlementKind.Duplicate : observed.Kind;
    }
    public SettlementKind CancelUncharged(LedgerAttempt attempt)
    {
        Submit(new(0, attempt.Key, attempt.Id, "cancel", At: clock.GetUtcNow().ToUnixTimeMilliseconds()), attempt, out var target);
        return target < 0 ? SettlementKind.Duplicate : SettlementKind.Cancelled;
    }
    public CreditUsage GetUsage(string key) { lock (gate) { EnsureOpen(); return Current(key); } }
    public Dictionary<string, CreditUsage> AllUsage() { lock (gate) { EnsureOpen(); return usage.Keys.ToDictionary(k => k, Current, StringComparer.Ordinal); } }
    public void Check(KeyView policy)
    {
        lock (gate)
        {
            RequireHealthy(); DrainLocked(); if (!policy.Limited) return; var u = Current(policy.Id);
            if (policy.QuotaTotal > 0 && u.Total >= policy.QuotaTotal || policy.QuotaDaily > 0 && u.Daily >= policy.QuotaDaily || policy.QuotaMonthly > 0 && u.Monthly >= policy.QuotaMonthly || u.Unknown >= 10)
                throw new KeyFailure(429, "credit_quota_exhausted", u.Unknown >= 10 ? "上游长期未返回计费字段，该 Key 已进入安全状态" : "该 API Key 的积分额度已用尽");
        }
    }
    void DrainLocked()
    {
        while (appliedSeq < seq || pendingWrites.Count > 0)
        {
            if (failed || blocked) throw new KeyFailure(503, "ledger_unavailable", "记账存储不可用，已暂停请求");
            if (!flushActive) { flushActive = true; Monitor.PulseAll(gate); }
            Monitor.Wait(gate);
        }
    }
    public void Reset(string key)
    {
        Submit(new(0, key, Kind: "reset", At: clock.GetUtcNow().ToUnixTimeMilliseconds()), null, out _);
        Compact();
    }
    void FlushLoop()
    {
        while (true)
        {
            long target;
            lock (gate)
            {
                while (!stopping && !flushActive) Monitor.Wait(gate);
                if (stopping) return;
                flushActive = false;
                target = pendingWrites.Count > 0 ? pendingWrites.Last().Seq : seq;
            }
            bool ok = true;
            try { lock (ioGate) files.Sync(); }
            catch (Exception e) when (e is IOException or UnauthorizedAccessException or OverflowException) { ok = false; }
            lock (gate)
            {
                if (ok)
                {
                    while (pendingWrites.Count > 0 && pendingWrites.Peek().Seq <= target)
                    {
                        var entry = pendingWrites.Dequeue();
                        try { Apply(entry); }
                        catch (InvalidDataException) { ok = false; failed = true; Fault(); break; }
                        appliedSeq = entry.Seq; writes++;
                        if (entry.Attempt is { Length: > 0 } attempt) submitted.Remove(attempt);
                    }
                }
                else { failed = true; Fault(); }
                lock (waitGate) Monitor.PulseAll(waitGate);
                if (ok && writes >= CompactEvery && pendingWrites.Count == 0)
                {
                    try { CompactLocked(); }
                    catch (Exception e) when (e is IOException or UnauthorizedAccessException) { failed = true; Fault(); }
                    catch (KeyFailure) { failed = true; }
                }
                if (pendingWrites.Count > 0) flushActive = true;
                Monitor.PulseAll(gate);
            }
        }
    }
    void CompactLocked()
    {
        RequireHealthy();
        try
        {
            var snap = new DiskSnapshot(1, appliedSeq, usage.Keys.ToDictionary(k => k, Current, StringComparer.Ordinal), PendingAttempts: new(pending), ResolvedAttempts: resolvedOrder.ToArray());
            lock (ioGate)
            {
                files.Replace(SnapshotName, JsonSerializer.SerializeToUtf8Bytes(snap, LedgerJson.Default.DiskSnapshot));
                state = state with { SnapshotRequired = true }; StateToDisk();
                files.Sync(); files.Replace(WalName, []);
            }
            writes = 0;
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException) { Fault(); throw new KeyFailure(503, "ledger_unavailable", "账本压缩失败，已暂停请求"); }
    }
    public void Compact()
    {
        lock (gate)
        {
            RequireHealthy(); DrainLocked(); CompactLocked();
        }
    }
    public void Dispose()
    {
        lock (gate) { if (disposed) return; disposed = true; stopping = true; Monitor.PulseAll(gate); }
        try { flusher?.Join(TimeSpan.FromSeconds(30)); } catch (ThreadStateException) { }
        lock (ioGate) files.Dispose();
    }
}
