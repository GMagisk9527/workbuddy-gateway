using System.Collections.Frozen;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using System.Security.Cryptography;
using AccountScheduling;

namespace AccountMaintenance;

public interface IAccountControlPlane
{
    Task<AccountCredential> RefreshAsync(AccountCredential current, CancellationToken cancellation);
    Task<CredentialVerification> VerifyAsync(AccountCredential candidate, CancellationToken cancellation);
}
public interface IModelCatalogControlPlane
{
    Task<byte[]> FetchCatalogAsync(AccountCredential credential, CancellationToken cancellation);
}
public sealed record CatalogCredentialSource(AccountId Id, AccountSite Site);
public sealed record CredentialRegistration(AccountId Id, Uri ModelEndpoint, ICredentialPersistence Persistence, IAccountControlPlane Control,bool Disabled=false);
public sealed record CredentialStatus(AccountId Id, long ExpiresAt, bool Disabled, bool Degraded, int Failures, long CooldownUntil);
public sealed record AccountManagementStatus(string Id,AccountSite Site,long ExpiresAt,bool Disabled,bool ConfiguredDisabled,bool Degraded,int Failures,long CooldownUntil);
public sealed record AccountResourceStatus(string Id,bool Known,bool Stale,long FetchedAt,double? Total,double? Used,double? Remaining,bool Unlimited,bool Paid,bool PackagesKnown,int Packages,long CreditExpiresAt,double ExpiringCredit,string Error);
public sealed class CredentialLease : IDisposable
{
    IDisposable? permit;
    readonly AccountCredential credential;
    readonly Uri endpoint;
    internal CredentialLease(AccountCredential credential, Uri endpoint, IDisposable permit) { this.credential = credential; this.endpoint = endpoint; this.permit = permit; }
    public void ApplyTo(HttpRequestMessage message)
    {
        if (Volatile.Read(ref permit) is null) throw new ObjectDisposedException(nameof(CredentialLease));
        if (message.RequestUri != endpoint) throw new CredentialFailure("credential_endpoint_mismatch");
        ApplyHeaders(message);
    }
    public void ApplyToAgentTool(HttpRequestMessage message,bool fetch)
    {
        if(Volatile.Read(ref permit) is null)throw new ObjectDisposedException(nameof(CredentialLease));
        if(endpoint!=SiteEndpoints.Model(credential.Identity.Site)||message.RequestUri!=SiteEndpoints.AgentTool(credential.Identity.Site,fetch))
            throw new CredentialFailure("credential_endpoint_mismatch");
        ApplyHeaders(message);
    }
    void ApplyHeaders(HttpRequestMessage message)
    {
        message.Headers.Authorization = new AuthenticationHeaderValue("Bearer", credential.Access);
        if (credential.Identity.Uid.Length != 0) message.Headers.Add("X-User-Id", credential.Identity.Uid);
        if (credential.Identity.EnterpriseId.Length != 0) message.Headers.Add("X-Enterprise-Id", credential.Identity.EnterpriseId);
        if (credential.Domain.Length != 0) message.Headers.Add("X-Domain", credential.Domain);
    }
    public void Dispose() => Interlocked.Exchange(ref permit, null)?.Dispose();
    public override string ToString() => "CredentialLease (redacted)";
    internal Task<byte[]> FetchCatalogAsync(IModelCatalogControlPlane control, CancellationToken cancellation)
    {
        if (Volatile.Read(ref permit) is null) throw new ObjectDisposedException(nameof(CredentialLease));
        return control.FetchCatalogAsync(credential,cancellation);
    }
    internal Task<AccountResourceObservation> FetchResourcesAsync(IAccountResourcesControlPlane control,CancellationToken cancellation,bool fetchPackages)
    {if(Volatile.Read(ref permit) is null)throw new ObjectDisposedException(nameof(CredentialLease));return control.FetchResourcesAsync(credential,cancellation,fetchPackages);}
}
public sealed partial class CredentialManager
{
    sealed record State(AccountCredential Credential, bool Disabled = false, bool Degraded = false, int Failures = 0, long CooldownUntil = 0);
    sealed class Cell(CredentialRegistration registration, PersistedCredential saved)
    {
        public CredentialRegistration Registration { get; } = registration;
        public AsyncCredentialGate Gate { get; } = new();
        public State Current = new(saved.Credential, saved.Disabled||registration.Disabled);
        public int Blocked;
        public long BlockVersion;
        public bool Retired;
        public string AffinityGeneration{get;}=Guid.NewGuid().ToString("N");
        public readonly object Barrier=new();
        public AccountResourceStatus Resources=new(registration.Id.Value,false,false,0,null,null,null,false,false,false,0,0,0,"not_fetched");
        public readonly SemaphoreSlim ResourceGate=new(1,1);
        public IReadOnlyList<CreditPackage> Packages=[];
    }
    volatile FrozenDictionary<AccountId, Cell> cells;
    readonly TimeProvider time;
    readonly AccountPool pool;
    public bool FetchCreditPackages{get=>Volatile.Read(ref fetchCreditPackages);set=>Volatile.Write(ref fetchCreditPackages,value);}
    bool fetchCreditPackages=true;
    public CredentialManager(AccountPool pool, IEnumerable<CredentialRegistration> registrations, TimeProvider? time = null)
    {
        this.pool = pool; this.time = time ?? TimeProvider.System;
        var entries = registrations.Take(1025).ToArray();
        if (entries.Length > 1024 || entries.Select(x => x.Id).Distinct().Count() != entries.Length) throw new ArgumentException("Invalid credential registrations");
        cells = entries.ToFrozenDictionary(r => r.Id, r => new Cell(r, r.Persistence.Load()));
        foreach (var cell in cells.Values)
            if (!SiteEndpoints.Allowed(cell.Registration.ModelEndpoint, cell.Current.Credential.Identity.Site)) throw new ArgumentException("Credential endpoint/site mismatch");
        foreach (var cell in cells.Values) PublishAvailability(cell, cell.Current);
    }
    Cell Find(AccountId id) => cells.GetValueOrDefault(id) ?? throw new CredentialFailure("credential_missing");
    public IReadOnlyList<CatalogCredentialSource> CatalogSources => cells.Values.Where(cell =>
    {
        var s=Volatile.Read(ref cell.Current);
        return cell.Registration.Control is IModelCatalogControlPlane && Volatile.Read(ref cell.Blocked)==0 && !s.Disabled && !s.Degraded && s.CooldownUntil<=time.GetUtcNow().ToUnixTimeSeconds() && s.Credential.Access.Length>0;
    }).Select(cell=>new CatalogCredentialSource(cell.Registration.Id,Volatile.Read(ref cell.Current).Credential.Identity.Site)).ToArray();
    public async Task<byte[]> FetchCatalogAsync(AccountId id, CancellationToken cancellation=default)
    {
        var cell=Find(id);
        if(cell.Registration.Control is not IModelCatalogControlPlane control)throw new CredentialFailure("catalog_not_supported");
        // Keep the read permit through the complete RPC; token rotation cannot race the fetch.
        using var lease=await AcquireForHeadersAsync(id,cell.Registration.ModelEndpoint,cancellation);
        return await lease.FetchCatalogAsync(control,cancellation);
    }
    void PublishAvailability(Cell cell, State state) => pool.SetCredentialAvailability(cell.Registration.Id, state.Credential.Access.Length != 0,
        state.Disabled || state.Degraded || Volatile.Read(ref cell.Blocked) != 0, state.CooldownUntil == 0 ? default : DateTimeOffset.FromUnixTimeSeconds(state.CooldownUntil));
    public IReadOnlyList<CredentialStatus> Status => cells.Select(p =>
    {
        var s = Volatile.Read(ref p.Value.Current); return new CredentialStatus(p.Key, s.Credential.ExpiresAt, s.Disabled || Volatile.Read(ref p.Value.Blocked)!=0, s.Degraded, s.Failures, s.CooldownUntil);
    }).ToArray();
    public IReadOnlyList<AccountManagementStatus> ManagementStatus=>cells.Select(p=>
    {var s=Volatile.Read(ref p.Value.Current);bool configured=p.Value.Registration.Disabled||pool.IsConfiguredDisabled(p.Key);return new AccountManagementStatus(p.Key.Value,s.Credential.Identity.Site,s.Credential.ExpiresAt,s.Disabled||Volatile.Read(ref p.Value.Blocked)!=0||configured,configured,s.Degraded,s.Failures,s.CooldownUntil);}).ToArray();
    public IReadOnlyList<AccountResourceStatus> ResourceStatus=>cells.Values.Select(c=>Volatile.Read(ref c.Resources)).ToArray();
    public IReadOnlyDictionary<AccountId,(string Name,long LastRefresh)> DisplayInfo=>cells.ToDictionary(p=>p.Key,p=>{var c=Volatile.Read(ref p.Value.Current).Credential;return(c.Identity.Nickname,c.LastRefreshTime);});
    public IReadOnlyList<CreditPackage> PackagesFor(AccountId id)=>cells.TryGetValue(id,out var cell)?Volatile.Read(ref cell.Packages):Array.Empty<CreditPackage>();
    static string CheckinKey(Cell cell)
    {var c=Volatile.Read(ref cell.Current).Credential.Identity;return Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes((c.Site==AccountSite.Cn?"cn":"intl")+":"+(c.Uid.Length>0?"uid:"+c.Uid:"file:"+cell.Registration.Id.Value))));}
    public IReadOnlyList<CheckinTarget> CheckinTargets=>cells.Values.Select(cell=>
    {var s=Volatile.Read(ref cell.Current);bool supported=s.Credential.Identity.Site==AccountSite.Cn&&string.IsNullOrWhiteSpace(s.Credential.Identity.EnterpriseId);return new CheckinTarget(CheckinKey(cell),cell.Registration.Id,supported,supported&&cell.Registration.Control is IAccountCheckinControlPlane&&!s.Disabled&&!s.Degraded&&Volatile.Read(ref cell.Blocked)==0&&!pool.IsConfiguredDisabled(cell.Registration.Id)&&s.Credential.Access.Length>0);}).ToArray();
    public async Task<CheckinResult> CheckinAsync(AccountId id,string expected,CancellationToken cancellation=default)
    {
        var cell=Find(id);if(cell.Registration.Control is not IAccountCheckinControlPlane control)throw new CredentialFailure("checkin_unsupported");
        AccountCredential credential;
        using(await cell.Gate.ReadAsync(cancellation))
        {
            var s=Volatile.Read(ref cell.Current);CheckUsable(s,time.GetUtcNow().ToUnixTimeSeconds());
            if(Volatile.Read(ref cell.Blocked)!=0||pool.IsConfiguredDisabled(id)||CheckinKey(cell)!=expected)throw new CredentialFailure("checkin_identity_unavailable");
            credential=s.Credential;
        }
        // Immutable credential snapshot; network I/O does not hold the model credential lock.
        return await control.CheckinAsync(credential,cancellation);
    }
    public IReadOnlyList<AccountId> KeepaliveCandidates(bool overdueOnly)=>cells.Where(p=>
    {var s=Volatile.Read(ref p.Value.Current);long now=time.GetUtcNow().ToUnixTimeSeconds();return !s.Disabled&&!s.Degraded&&Volatile.Read(ref p.Value.Blocked)==0&&!pool.IsConfiguredDisabled(p.Key)&&s.CooldownUntil<=now&&s.Credential.Access.Length>0&&s.Credential.CanRefresh&&(!overdueOnly||s.Credential.LastRefreshTime<=0||now-s.Credential.LastRefreshTime>=72000);}).Select(p=>p.Key).OrderBy(id=>id.Value,StringComparer.Ordinal).ToArray();
    public Task<bool> KeepaliveAsync(AccountId id,bool overdueOnly,CancellationToken cancellation=default)=>RefreshAsync(Find(id),cancellation,true,overdueOnly);
    public IReadOnlyList<AccountId> ResourceCandidates=>cells.Where(p=>
    {var s=Volatile.Read(ref p.Value.Current);return p.Value.Registration.Control is IAccountResourcesControlPlane&&!s.Disabled&&!s.Degraded&&Volatile.Read(ref p.Value.Blocked)==0&&!pool.IsConfiguredDisabled(p.Key)&&s.Credential.Access.Length>0&&s.CooldownUntil<=time.GetUtcNow().ToUnixTimeSeconds();}).Select(p=>p.Key).OrderBy(id=>id.Value,StringComparer.Ordinal).ToArray();
    public async Task FetchResourcesAsync(AccountId id,CancellationToken cancellation=default)
    {
        var cell=Find(id);if(cell.Registration.Control is not IAccountResourcesControlPlane control)throw new CredentialFailure("resources_not_supported");
        await cell.ResourceGate.WaitAsync(cancellation);
        try{await FetchResourcesCore(cell,id,control,cancellation);}
        catch(Exception e) when(e is CredentialFailure or HttpRequestException or IOException or OperationCanceledException){MarkResourceFailure(id);throw;}
        finally{cell.ResourceGate.Release();}
    }
    async Task FetchResourcesCore(Cell cell,AccountId id,IAccountResourcesControlPlane control,CancellationToken cancellation)
    {
        using var lease=await AcquireForHeadersAsync(id,cell.Registration.ModelEndpoint,cancellation);
        bool fetchPackages=FetchCreditPackages;
        var observation=await lease.FetchResourcesAsync(control,cancellation,fetchPackages);cancellation.ThrowIfCancellationRequested();
        var quota=observation.Quota;
        if(!double.IsFinite(quota.Total)||!double.IsFinite(quota.Used)||!double.IsFinite(quota.Remaining)||quota.Used<0||(!quota.Unlimited&&(quota.Total<0||quota.Remaining<0))||(quota.Unlimited&&(quota.Total!=-1||quota.Remaining!=-1))||observation.Packages.Count>64)throw new CredentialFailure("invalid_resource_response");
        long now=time.GetUtcNow().ToUnixTimeSeconds();
        var packages=observation.Packages.ToArray();foreach(var p in packages)if(!double.IsFinite(p.Remain)||p.ExpiresAt<0||p.ExpiresAt>253402300799)throw new CredentialFailure("invalid_resource_response");
        var soonest=observation.PackagesKnown?packages.Where(p=>p.ExpiryKnown&&p.Remain>0&&p.ExpiresAt>now).OrderBy(p=>p.ExpiresAt).FirstOrDefault():null;
        lock(cell.Barrier)
        {
            var state=Volatile.Read(ref cell.Current);
            if(state.Disabled||state.Degraded||Volatile.Read(ref cell.Blocked)!=0||pool.IsConfiguredDisabled(id))throw new CredentialFailure("credential_disabled");
            var next=new AccountResourceStatus(id.Value,true,false,now,quota.Total,quota.Used,quota.Remaining,quota.Unlimited,quota.Paid,observation.PackagesKnown,observation.PackagesKnown?packages.Length:0,soonest?.ExpiresAt??0,soonest?.Remain??0,observation.PackagesKnown||quota.Unlimited?"":"packages_unavailable");
            pool.ObserveQuota(id,!quota.Unlimited&&quota.Remaining<=0,soonest is null?default:DateTimeOffset.FromUnixTimeSeconds(soonest.ExpiresAt),soonest?.Remain??0);Volatile.Write(ref cell.Packages,observation.PackagesKnown?packages:[]);Volatile.Write(ref cell.Resources,next);
        }
    }
    public void MarkResourceFailure(AccountId id)
    {var cell=Find(id);lock(cell.Barrier){var old=Volatile.Read(ref cell.Resources);Volatile.Write(ref cell.Resources,old with{Stale=true,Error="resource_unavailable",CreditExpiresAt=0,ExpiringCredit=0});pool.ClearCreditPriority(id);}}
    sealed record StoredResource(string Identity,AccountResourceStatus Observation);
    sealed record ResourceDocument(int Version, StoredResource[] Accounts);

    [JsonSourceGenerationOptions(JsonSerializerDefaults.Web)]
    [JsonSerializable(typeof(ResourceDocument))]
    [JsonSerializable(typeof(StoredResource[]))]
    partial class ResourceJson : JsonSerializerContext { }
    static string ResourceIdentity(Cell cell)
    {var c=Volatile.Read(ref cell.Current).Credential.Identity;return Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes(((int)c.Site)+"\0"+c.Uid+"\0"+c.EnterpriseId)));}
    public IReadOnlyList<RuntimeIdentity> RuntimeIdentities=>cells.Values.Select(c=>new RuntimeIdentity(c.Registration.Id,Volatile.Read(ref c.Current).Credential.Identity.Site,ResourceIdentity(c))).ToArray();
    public string HistoricalIdentity(AccountId id,string file)
    {var c=Volatile.Read(ref Find(id).Current).Credential.Identity;return Convert.ToHexStringLower(SHA256.HashData(Encoding.UTF8.GetBytes(file+"\0"+(c.Site==AccountSite.Cn?"cn":"intl")+"\0"+c.Uid+"\0"+c.EnterpriseId.Trim())));}
    public byte[] ExportResources()=>JsonSerializer.SerializeToUtf8Bytes(new ResourceDocument(1, cells.Values.Where(c=>Volatile.Read(ref c.Resources).Known).Select(c=>new StoredResource(ResourceIdentity(c),Volatile.Read(ref c.Resources))).ToArray()), ResourceJson.Default.ResourceDocument);
    public void ValidateResourceSnapshot(ReadOnlyMemory<byte> bytes)=>ParseResources(bytes);
    StoredResource[] ParseResources(ReadOnlyMemory<byte> bytes)
    {
        if(bytes.Length>4<<20)throw new ArgumentException("Resource snapshot exceeds bounds");
        using var doc=JsonDocument.Parse(bytes,new JsonDocumentOptions{MaxDepth=8});var root=doc.RootElement;
        if(root.GetProperty("version").GetInt32()!=1||root.GetProperty("accounts").ValueKind!=JsonValueKind.Array||root.GetProperty("accounts").GetArrayLength()>1024)throw new ArgumentException("Invalid resource snapshot");
        var records=JsonSerializer.Deserialize(root.GetProperty("accounts"), ResourceJson.Default.StoredResourceArray)!;var seen=new HashSet<string>(StringComparer.Ordinal);long now=time.GetUtcNow().ToUnixTimeSeconds();
        foreach(var row in records)
        {
            var s=row?.Observation;
            if(row is null||row.Identity is null||row.Identity.Length!=64||row.Identity.Any(c=>!char.IsAsciiHexDigit(c))||s is null||string.IsNullOrWhiteSpace(s.Id)||s.Id.Length>256||s.Id.Any(char.IsControl)||!seen.Add(s.Id)||!s.Known||s.Total is null||s.Used is null||s.Remaining is null||!double.IsFinite(s.Total.Value)||!double.IsFinite(s.Used.Value)||!double.IsFinite(s.Remaining.Value)||s.Used<0||s.FetchedAt<=0||s.FetchedAt>now+300||(!s.Unlimited&&(s.Total<0||s.Remaining<0))||(s.Unlimited&&(s.Total!=-1||s.Remaining!=-1)))throw new ArgumentException("Invalid resource observation");
        }
        return records;
    }
    public void ImportResources(ReadOnlyMemory<byte> bytes)
    {
        var records=ParseResources(bytes);
        foreach(var row in records)
        {
            if(!cells.TryGetValue(new(row.Observation.Id),out var cell)||!StringComparer.Ordinal.Equals(row.Identity,ResourceIdentity(cell)))continue;
            lock(cell.Barrier)
            {
                if(Volatile.Read(ref cell.Resources).Known)continue;
                var state=row.Observation with{Stale=true,PackagesKnown=false,Packages=0,CreditExpiresAt=0,ExpiringCredit=0,Error="cached_observation"};
                Volatile.Write(ref cell.Resources,state);if(!state.Unlimited&&state.Remaining<=0)pool.RestoreQuotaBlock(cell.Registration.Id);
            }
        }
    }
    public Task<CredentialLease> AcquireForHeadersAsync(AccountLease account, CancellationToken cancellation = default)
    {
        var cell = Find(account.Account.Id);
        if (Volatile.Read(ref cell.Current).Credential.Identity.Site != account.Account.Site) throw new CredentialFailure("credential_site_mismatch");
        return AcquireForHeadersAsync(account.Account.Id, account.Account.Upstream, cancellation);
    }
    public async Task<CredentialLease> AcquireForHeadersAsync(AccountId id, Uri endpoint, CancellationToken cancellation = default)
    {
        var cell = Find(id);
        if (endpoint != cell.Registration.ModelEndpoint) throw new CredentialFailure("credential_endpoint_mismatch");
        bool refreshed = false;
        while (true)
        {
            var read = await cell.Gate.ReadAsync(cancellation);
            try
            {
                var s = Volatile.Read(ref cell.Current); long now = time.GetUtcNow().ToUnixTimeSeconds();
                if (Volatile.Read(ref cell.Blocked) != 0) throw new CredentialFailure("credential_disabled");
                CheckUsable(s, now);
                if (!refreshed && CredentialRules.NeedsRefresh(s.Credential.ExpiresAt, now)) { read.Dispose(); await RefreshAsync(cell, cancellation, false); refreshed = true; continue; }
                if (now >= s.Credential.ExpiresAt) throw new CredentialFailure("credential_expired");
                return new(s.Credential, endpoint, read);
            }
            catch { read.Dispose(); throw; }
        }
    }
    static void CheckUsable(State s, long now)
    {
        if (s.Degraded) throw new CredentialFailure("credential_store_incomplete");
        if (s.Disabled) throw new CredentialFailure("credential_disabled");
        if (s.CooldownUntil > now) throw new CredentialFailure("credential_cooling");
        if (s.Credential.Access.Length == 0) throw new CredentialFailure("credential_missing_access");
    }
    public Task RefreshAsync(AccountId id, CancellationToken cancellation = default) => RefreshAsync(Find(id), cancellation, true);
    public IReadOnlyList<AccountId> MaintenanceCandidates=>cells.Where(p=>
    {
        var s=Volatile.Read(ref p.Value.Current);long now=time.GetUtcNow().ToUnixTimeSeconds();
        return !s.Disabled&&!s.Degraded&&Volatile.Read(ref p.Value.Blocked)==0&&!pool.IsConfiguredDisabled(p.Key)&&s.CooldownUntil<=now&&s.Credential.Access.Length>0&&s.Credential.CanRefresh&&CredentialRules.NeedsRefresh(s.Credential.ExpiresAt,now);
    }).OrderBy(p=>Volatile.Read(ref p.Value.Current).Credential.ExpiresAt).ThenBy(p=>p.Key.Value,StringComparer.Ordinal).Select(p=>p.Key).ToArray();
    public Task RefreshIfDueAsync(AccountId id,CancellationToken cancellation=default)=>RefreshAsync(Find(id),cancellation,false);
    public void Block(AccountId id)
    {
        var cell=Find(id);lock(cell.Barrier){cell.BlockVersion++;Interlocked.Exchange(ref cell.Blocked,1);PublishAvailability(cell,Volatile.Read(ref cell.Current));}
    }
    public async Task EnableAsync(AccountId id,CancellationToken cancellation=default)
    {
        var cell=Find(id);
        using var deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);deadline.CancelAfter(TimeSpan.FromSeconds(30));
        using var permit=await cell.Gate.WriteAsync(deadline.Token);
        if(cell.Retired)throw new CredentialFailure("credential_missing");
        var old=Volatile.Read(ref cell.Current);
        if(cell.Registration.Disabled||pool.IsConfiguredDisabled(id))throw new CredentialFailure("credential_configured_disabled");
        if(old.Degraded)throw new CredentialFailure("credential_store_incomplete");
        if(old.Credential.Access.Length==0)throw new CredentialFailure("credential_missing_access");
        if(old.Credential.ExpiresAt<=time.GetUtcNow().ToUnixTimeSeconds())throw new CredentialFailure("credential_expired");
        long version;lock(cell.Barrier)version=cell.BlockVersion;
        var verified=await cell.Registration.Control.VerifyAsync(old.Credential,deadline.Token);deadline.Token.ThrowIfCancellationRequested();
        if(verified!=CredentialVerification.Usable)throw new CredentialFailure(verified==CredentialVerification.Rejected?"credential_verification_rejected":"credential_verification_unknown");
        lock(cell.Barrier)if(version!=cell.BlockVersion)throw new CredentialFailure("credential_changed_during_enable");
        try{cell.Registration.Persistence.Save(new(old.Credential,false));}
        catch
        {
            lock(cell.Barrier){Interlocked.Exchange(ref cell.Blocked,1);var failed=old with{Disabled=true,Degraded=true};Volatile.Write(ref cell.Current,failed);PublishAvailability(cell,failed);}
            throw new CredentialFailure("credential_persist_failed");
        }
        lock(cell.Barrier)
        {
            if(version==cell.BlockVersion)
            {
                var next=new State(old.Credential);Volatile.Write(ref cell.Current,next);Interlocked.Exchange(ref cell.Blocked,0);
                pool.RestoreCredentialAvailability(id,true);return;
            }
        }
        // A newer restriction won while the enabled state was being written.
        // Keep it blocked and restore the durable disabled flag before returning.
        bool failedSave=false;try{cell.Registration.Persistence.Save(new(old.Credential,true));}catch{failedSave=true;}
        lock(cell.Barrier){var stopped=old with{Disabled=true,Degraded=failedSave};Volatile.Write(ref cell.Current,stopped);PublishAvailability(cell,stopped);}
        throw new CredentialFailure(failedSave?"credential_persist_failed":"credential_changed_during_enable");
    }
    public async Task DisableAsync(AccountId id)
    {
        Block(id); var cell=Find(id);
        using var deadline=new CancellationTokenSource(TimeSpan.FromSeconds(30));
        try
        {
            using var permit=await cell.Gate.WriteAsync(deadline.Token);
            if(cell.Retired)throw new CredentialFailure("credential_missing");
            var old=Volatile.Read(ref cell.Current); var next=old with { Disabled=true };
            try { cell.Registration.Persistence.Save(new(old.Credential,true)); }
            catch { next=next with { Degraded=true }; }
            Volatile.Write(ref cell.Current,next); PublishAvailability(cell,next);
        }
        catch (OperationCanceledException)
        {
            var stopped=Volatile.Read(ref cell.Current) with { Disabled=true,Degraded=true };
            Volatile.Write(ref cell.Current,stopped); PublishAvailability(cell,stopped);
        }
    }
    async Task<bool> RefreshAsync(Cell cell, CancellationToken cancellation, bool force,bool overdueOnly=false)
    {
        using var write = await cell.Gate.WriteAsync(cancellation);
        if(cell.Retired)throw new CredentialFailure("credential_missing");
        var old = Volatile.Read(ref cell.Current); long now = time.GetUtcNow().ToUnixTimeSeconds(); CheckUsable(old, now);
        if(Volatile.Read(ref cell.Blocked)!=0||pool.IsConfiguredDisabled(cell.Registration.Id))throw new CredentialFailure("credential_disabled");
        if(overdueOnly&&old.Credential.LastRefreshTime>0&&now-old.Credential.LastRefreshTime<72000)return false;
        if (!force && !CredentialRules.NeedsRefresh(old.Credential.ExpiresAt, now)) return false;
        if (!old.Credential.CanRefresh) { Cooldown(cell, old, now); throw new CredentialFailure("credential_missing_refresh"); }
        // A refresh may rotate the old refresh token. Once started, finish verification
        // and persistence under a separate bounded deadline, even if the caller leaves.
        using var maintenanceDeadline = new CancellationTokenSource(TimeSpan.FromSeconds(30));
        AccountCredential next;
        try { next = await cell.Registration.Control.RefreshAsync(old.Credential, maintenanceDeadline.Token); }
        catch (CredentialFailure e)
        {
            var failed = old with { Failures = old.Failures + 1, Disabled = old.Failures >= 2, CooldownUntil = now + 60 };
            if (failed.Disabled)
            {
                try { cell.Registration.Persistence.Save(new(old.Credential, true)); }
                catch { failed = failed with { Degraded = true }; }
            }
            Volatile.Write(ref cell.Current, failed); PublishAvailability(cell, failed);
            throw new CredentialFailure(failed.Degraded ? "credential_persist_failed" : "credential_refresh_failed", e.Kind);
        }
        if (!CredentialRules.SameIdentity(old.Credential, next)) { Cooldown(cell, old, now); throw new CredentialFailure("credential_identity_changed"); }
        if (await cell.Registration.Control.VerifyAsync(next, maintenanceDeadline.Token) == CredentialVerification.Rejected)
        { Cooldown(cell, old, now); throw new CredentialFailure("credential_verification_rejected"); }
        try { cell.Registration.Persistence.Save(new(next)); }
        catch
        {
            var failed = old with { Degraded = true }; Volatile.Write(ref cell.Current, failed); PublishAvailability(cell, failed);
            throw new CredentialFailure("credential_persist_failed");
        }
        // Once the write succeeds, publish regardless of a racing caller cancellation.
        var updated = new State(next); Volatile.Write(ref cell.Current, updated); PublishAvailability(cell, updated);
        cancellation.ThrowIfCancellationRequested();
        return true;
    }
    void Cooldown(Cell cell, State old, long now)
    {
        var state = old with { CooldownUntil = now + 60 }; Volatile.Write(ref cell.Current, state); PublishAvailability(cell, state);
    }
}
