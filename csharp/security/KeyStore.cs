using System.Collections.Immutable;
using System.Security.Cryptography;
using System.Text.Json;
using System.Text.Json.Serialization;

namespace KeySecurity;

public interface IKeyEntropy { byte[] Bytes(int count); }
public sealed class CryptoKeyEntropy : IKeyEntropy { public byte[] Bytes(int count) => RandomNumberGenerator.GetBytes(count); }

public sealed class KeyLease : IDisposable
{
    Action? release;
    readonly Action<string> check;
    readonly byte[] reasoningKey;
    internal KeyLease(KeyIdentity? identity, Action release, Action<string> check, byte[] reasoningKey)
    { Identity = identity; this.release = release; this.check = check; this.reasoningKey = reasoningKey; }
    public KeyIdentity? Identity { get; }
    public ReadOnlyMemory<byte> ReasoningKey => reasoningKey;
    public void CheckModel(string model) { if (Volatile.Read(ref release) is null) throw new ObjectDisposedException(nameof(KeyLease)); check(model); }
    public void Dispose() { var action = Interlocked.Exchange(ref release, null); if (action is not null) { action(); CryptographicOperations.ZeroMemory(reasoningKey); } }
}

// Key metadata only. Quota-bearing keys fail closed until the WAL module is
// attached. Store records are immutable and published after durable replacement.
public sealed partial class KeyStore : IDisposable
{
    sealed class Runtime { public int Inflight; }
    sealed record Entry(KeyView View, string Hash, Runtime Runtime);
    sealed record DiskKey(string Id, string Name, string Prefix, string Hash, bool Enabled, long CreatedAt,
        long ExpiresAt, string[] Models, int MaxConcurrent, double QuotaTotal, double QuotaDaily, double QuotaMonthly, string Note);
    sealed record DiskDocument(int Version, bool RequireAuth, DiskKey[] Keys);
    sealed record DiskAuth(bool RequireAuth, long UpdatedAt);
    [JsonSourceGenerationOptions(PropertyNamingPolicy = JsonKnownNamingPolicy.CamelCase, PropertyNameCaseInsensitive = false)]
    [JsonSerializable(typeof(DiskDocument))]
    [JsonSerializable(typeof(DiskAuth))]
    partial class KeyJson : JsonSerializerContext {}
    readonly object gate = new();
    readonly IKeyFiles files;
    readonly IKeyEntropy entropy;
    readonly TimeProvider clock;
    readonly bool allowAnonymousOnFresh;
    readonly Credential? legacy;
    IKeyAccounting? accounting;
    Dictionary<string, Entry> entries = [];
    Dictionary<string, Entry> byPrefix = [];
    bool required, degraded, disposed;

    public KeyStore(IKeyFiles files, bool allowAnonymousOnFresh = false, Credential? legacy = null, IKeyEntropy? entropy = null, TimeProvider? clock = null)
    {
        this.files = files; this.allowAnonymousOnFresh = allowAnonymousOnFresh; this.legacy = legacy;
        this.entropy = entropy ?? new CryptoKeyEntropy(); this.clock = clock ?? TimeProvider.System;
        try { Load(); } catch { files.Dispose(); throw; }
    }
    public static KeyStore Open(string directory, bool allowAnonymousOnFresh = false, Credential? legacy = null) => new(new KeyFiles(directory), allowAnonymousOnFresh, legacy);
    void EnsureOpen() { if (disposed) throw new ObjectDisposedException(nameof(KeyStore)); }
    public bool RequireAuth { get { lock (gate) { EnsureOpen(); return required || degraded || legacy is not null || !allowAnonymousOnFresh; } } }
    public bool HasLegacy => legacy is not null;
    public bool Degraded { get { lock (gate) return degraded; } }
    public void AttachAccounting(IKeyAccounting value)
    { ArgumentNullException.ThrowIfNull(value); lock (gate) { EnsureOpen(); if (accounting is not null && !ReferenceEquals(accounting, value)) throw new InvalidOperationException("Accounting already attached"); accounting = value; } }
    public KeyView GetView(string id) { lock (gate) { EnsureOpen(); return entries.TryGetValue(id, out var e) ? e.View : throw Missing(); } }
    static KeyFailure Missing() => new(404, "key_not_found", "密钥不存在");
    static KeyFailure StorageFailure() => new(503, "keystore_unavailable", "密钥存储不可用，已暂停请求");
    void Load()
    {
        if (files.Read("keystore.pending") is not null) throw new InvalidDataException("Unfinished key transaction; verified recovery is required");
        if (files.Read("keyauth.json") is { } switchBytes)
        {
            using var doc = JsonDocument.Parse(switchBytes);
            if (doc.RootElement.ValueKind != JsonValueKind.Object || !doc.RootElement.TryGetProperty("requireAuth", out var on) || on.ValueKind is not (JsonValueKind.True or JsonValueKind.False)) throw new InvalidDataException("Invalid keyauth document");
            required = on.GetBoolean();
        }
        if (files.Read("apikeys.json") is not { } raw) return;
        using var parsed = JsonDocument.Parse(raw);
        var root = parsed.RootElement;
        if (root.ValueKind != JsonValueKind.Object || !root.TryGetProperty("version", out var version) || version.ValueKind != JsonValueKind.Number || !version.TryGetInt32(out int v) || v != 1 ||
            !root.TryGetProperty("requireAuth", out var auth) || auth.ValueKind is not (JsonValueKind.True or JsonValueKind.False) || !root.TryGetProperty("keys", out var keys) || keys.ValueKind != JsonValueKind.Array)
            throw new InvalidDataException("Invalid API key document");
        var data = JsonSerializer.Deserialize(raw, KeyJson.Default.DiskDocument) ?? throw new InvalidDataException("Invalid API key document");
        var prefixes = new HashSet<string>();
        foreach (var k in data.Keys)
        {
            if (k is null || string.IsNullOrEmpty(k.Id) || string.IsNullOrEmpty(k.Prefix) || k.Hash is null || k.Hash.Length != 64 || k.Hash.Any(c => !char.IsAsciiHexDigit(c) || char.IsAsciiLetterUpper(c)) ||
                k.Models?.Any(m => m is null) == true || k.Name is null || !k.Prefix.StartsWith("wbk_", StringComparison.Ordinal) || k.Prefix.Length > 12 || !prefixes.Add(k.Prefix) || entries.ContainsKey(k.Id) ||
                k.MaxConcurrent is < 0 or > 64 || !double.IsFinite(k.QuotaTotal) || !double.IsFinite(k.QuotaDaily) || !double.IsFinite(k.QuotaMonthly) || k.QuotaTotal < 0 || k.QuotaDaily < 0 || k.QuotaMonthly < 0)
                throw new InvalidDataException("Invalid or duplicate key entry");
            var entry = new Entry(new(k.Id, k.Name, k.Prefix, k.Enabled, k.CreatedAt, k.ExpiresAt, (k.Models ?? []).ToImmutableArray(), k.MaxConcurrent, k.QuotaTotal, k.QuotaDaily, k.QuotaMonthly, k.Note ?? ""), k.Hash, new());
            entries.Add(k.Id, entry); byPrefix.Add(k.Prefix, entry);
        }
        required |= data.RequireAuth || entries.Count > 0;
    }
    byte[] Encode(Dictionary<string, Entry> table)
    {
        var keys = table.Values.OrderBy(e => e.View.CreatedAt).ThenBy(e => e.View.Id, StringComparer.Ordinal).Select(e =>
        {
            var k = e.View; return new DiskKey(k.Id, k.Name, k.Prefix, e.Hash, k.Enabled, k.CreatedAt, k.ExpiresAt, k.Models.ToArray(), k.MaxConcurrent, k.QuotaTotal, k.QuotaDaily, k.QuotaMonthly, k.Note);
        }).ToArray();
        return JsonSerializer.SerializeToUtf8Bytes(new DiskDocument(1, required, keys), KeyJson.Default.DiskDocument);
    }
    // The pending marker makes an ambiguous replace/rollback fail closed across
    // restart. Rollback preserves sticky auth even when first-key creation fails.
    void Commit(Dictionary<string, Entry> candidate)
    {
        EnsureOpen(); if (degraded) throw StorageFailure(); required = true;
        byte[] old = Encode(entries), next = Encode(candidate);
        byte[] auth = JsonSerializer.SerializeToUtf8Bytes(new DiskAuth(true, clock.GetUtcNow().ToUnixTimeMilliseconds()), KeyJson.Default.DiskAuth);
        try
        {
            files.WriteAtomic("keystore.pending", "{\"version\":1}"u8);
            files.WriteAtomic("keyauth.json", auth);
            files.WriteAtomic("apikeys.json", next);
            files.Delete("keystore.pending");
        }
        catch (Exception e) when (e is IOException or UnauthorizedAccessException)
        {
            degraded = true;
            try { files.WriteAtomic("keystore.pending", "{\"version\":1}"u8); files.WriteAtomic("keyauth.json", auth); files.WriteAtomic("apikeys.json", old); files.Delete("keystore.pending"); }
            catch (Exception rollback) when (rollback is IOException or UnauthorizedAccessException) { /* Keep any pending marker for fail-closed restart. */ }
            throw StorageFailure();
        }
        entries = candidate;
        byPrefix = candidate.Values.ToDictionary(e => e.View.Prefix, StringComparer.Ordinal);
    }
    (string Secret, string Prefix, string Hash) NewSecret()
    {
        byte[] random = entropy.Bytes(32); if (random.Length != 32) throw new CryptographicException("Invalid entropy source");
        string secret = "wbk_" + KeyRules.Base32(random); CryptographicOperations.ZeroMemory(random);
        return (secret, KeyRules.Prefix(secret), KeyRules.Hash(secret));
    }
    static KeyView View(string id, KeyOptions options, string prefix, long now) => new(id, options.Name, prefix, true, now, options.ExpiresAt,
        (options.Models ?? []).ToImmutableArray(), options.MaxConcurrent, options.QuotaTotal, options.QuotaDaily, options.QuotaMonthly, options.Note);
    public IssuedKey Create(KeyOptions options)
    {
        options = KeyRules.Normalize(options); var generated = NewSecret();
        byte[] randomId = entropy.Bytes(8); if (randomId.Length != 8) throw new CryptographicException("Invalid entropy source");
        string id = "key_" + Convert.ToHexStringLower(randomId);
        lock (gate)
        {
            EnsureOpen();
            if (entries.ContainsKey(id) || entries.Values.Any(e => e.View.Prefix == generated.Prefix)) throw new KeyFailure(409, "key_duplicate", "密钥前缀冲突，请重试");
            var view = View(id, options, generated.Prefix, clock.GetUtcNow().ToUnixTimeMilliseconds());
            var candidate = new Dictionary<string, Entry>(entries) { [id] = new(view, generated.Hash, new()) };
            Commit(candidate); return new(view, generated.Secret);
        }
    }
    public IssuedKey Rotate(string id)
    {
        var generated = NewSecret();
        lock (gate)
        {
            EnsureOpen(); if (!entries.TryGetValue(id, out var old)) throw Missing();
            if (entries.Values.Any(e => e.View.Id != id && e.View.Prefix == generated.Prefix)) throw new KeyFailure(409, "key_duplicate", "密钥前缀冲突，请重试");
            var next = old with { View = old.View with { Prefix = generated.Prefix }, Hash = generated.Hash };
            var candidate = new Dictionary<string, Entry>(entries) { [id] = next }; Commit(candidate); return new(next.View, generated.Secret);
        }
    }
    public KeyView Update(string id, KeyOptions options)
    {
        options = KeyRules.Normalize(options);
        lock (gate)
        {
            EnsureOpen(); if (!entries.TryGetValue(id, out var old)) throw Missing();
            var view = View(id, options, old.View.Prefix, old.View.CreatedAt) with { Enabled = old.View.Enabled };
            var candidate = new Dictionary<string, Entry>(entries) { [id] = old with { View = view } }; Commit(candidate); return view;
        }
    }
    public void SetEnabled(string id, bool enabled)
    {
        lock (gate)
        {
            EnsureOpen(); if (!entries.TryGetValue(id, out var old)) throw Missing();
            var candidate = new Dictionary<string, Entry>(entries) { [id] = old with { View = old.View with { Enabled = enabled } } }; Commit(candidate);
        }
    }
    public void Delete(string id)
    {
        lock (gate) { EnsureOpen(); if (!entries.ContainsKey(id)) throw Missing(); var candidate = new Dictionary<string, Entry>(entries); candidate.Remove(id); Commit(candidate); }
        // No usage exists in this module. WAL/history ownership remains outside it.
    }
    public ImmutableArray<KeyView> List() { lock (gate) { EnsureOpen(); return entries.Values.Select(x => x.View).OrderBy(x => x.CreatedAt).ThenBy(x => x.Id, StringComparer.Ordinal).ToImmutableArray(); } }
    Entry? Find(string token)
    {
        token = token.Trim(); if (token.Length is 0 or > 4096 || !token.StartsWith("wbk_", StringComparison.Ordinal)) return null;
        string prefix = KeyRules.Prefix(token);
        byPrefix.TryGetValue(prefix, out var found);
        return found is not null && CryptographicOperations.FixedTimeEquals(Convert.FromHexString(found.Hash), SHA256.HashData(System.Text.Encoding.UTF8.GetBytes(token))) ? found : null;
    }
    public KeyIdentity? Authenticate(string token)
    {
        lock (gate) { EnsureOpen(); if (legacy?.Matches(token) == true) return new("legacy", "config.apiKey", "兼容旧 Key", true); var found = Find(token); return found is null ? null : Identity(found); }
    }
    static KeyIdentity Identity(Entry entry) => new(entry.View.Id, entry.View.Prefix, entry.View.Name);
    void CheckPolicy(Entry entry, string model)
    {
        var view = entry.View;
        if (!view.Enabled) throw new KeyFailure(403, "key_disabled", "该 API Key 已被禁用");
        if (view.ExpiresAt > 0 && clock.GetUtcNow().ToUnixTimeMilliseconds() >= view.ExpiresAt) throw new KeyFailure(403, "key_expired", "该 API Key 已过期");
        if (model.Length > 0 && !KeyRules.Allows(view.Models, model)) throw new KeyFailure(403, "model_not_allowed", "该 API Key 无权使用模型 " + KeyRules.Model(model));
        if (degraded) throw StorageFailure();
        if (accounting is not null) accounting.Check(view);
        else if (view.Limited) throw new KeyFailure(503, "ledger_unavailable", "额度账本尚未接入，已暂停该 Key 的请求");
    }
    public KeyLease Acquire(string token, string model = "")
    {
        lock (gate)
        {
            EnsureOpen();
            if (legacy?.Matches(token) == true) { if (degraded) throw StorageFailure(); return new(new("legacy", "config.apiKey", "兼容旧 Key", true), () => { }, _ => { }, KeyRules.ReasoningKey(token)); }
            var found = Find(token);
            if (found is null)
            {
                if (RequireAuth) throw new KeyFailure(401, "invalid_api_key", "未提供有效 API 密钥");
                return new(null, () => { }, _ => { }, []);
            }
            CheckPolicy(found, model);
            if (found.Runtime.Inflight >= found.View.Concurrency) throw new KeyFailure(429, "key_concurrency_exceeded", $"该 API Key 并发上限为 {found.View.Concurrency}，请稍后重试");
            found.Runtime.Inflight++;
            return new(Identity(found), () => { lock (gate) found.Runtime.Inflight--; }, requested =>
            {
                lock (gate) { EnsureOpen(); if (!entries.TryGetValue(found.View.Id, out var current)) throw new KeyFailure(403, "key_deleted", "该 API Key 已被删除"); CheckPolicy(current, requested); }
            }, KeyRules.ReasoningKey(token.Trim()));
        }
    }
    public void Dispose() { lock (gate) { if (disposed) return; disposed = true; files.Dispose(); } }
}
