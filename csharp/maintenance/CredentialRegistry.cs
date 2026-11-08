using System.Collections.Frozen;
using AccountScheduling;

namespace AccountMaintenance;

public sealed partial class CredentialManager
{
    readonly SemaphoreSlim registryGate=new(1,1);
    public bool AllowsModel(AccountId id,string model)=>pool.Allows(id,model);
    public bool ContainsIdentity(AccountCredential credential)=>cells.Values.Any(c=>CredentialRules.SameIdentity(Volatile.Read(ref c.Current).Credential,credential));
    public string? AffinityIdentity(AccountId id)=>cells.TryGetValue(id,out var cell)&&Volatile.Read(ref cell.Blocked)==0&&!Volatile.Read(ref cell.Current).Disabled?cell.AffinityGeneration:null;
    static bool SameCredential(AccountCredential a,AccountCredential b)=>a.Identity==b.Identity&&a.Access==b.Access&&a.Refresh==b.Refresh&&a.Domain==b.Domain&&a.ExpiresAt==b.ExpiresAt&&a.RefreshExpiresAt==b.RefreshExpiresAt&&a.LastRefreshTime==b.LastRefreshTime;

    // Called only by the manifest owner. Validate every entry before publication,
    // and drain credential readers before replacing or removing registrations.
    public async Task<bool> ReconcileAsync(IReadOnlyList<CredentialRegistration> registrations,Action? durableCommit=null,CancellationToken cancellation=default)
    {
        await registryGate.WaitAsync(cancellation);var permits=new List<IDisposable>();
        try
        {
            if(registrations.Count>1024||registrations.Select(r=>r.Id).Distinct().Count()!=registrations.Count)throw new ArgumentException("Invalid credential registrations");
            var previous=cells;
            foreach(var cell in previous.OrderBy(p=>p.Key.Value,StringComparer.Ordinal).Select(p=>p.Value))permits.Add(await cell.Gate.WriteAsync(cancellation));
            var staged=new Dictionary<AccountId,Cell>();var identities=new HashSet<(AccountSite,string,string)>();var changedIds=new HashSet<AccountId>();bool changed=previous.Count!=registrations.Count;
            foreach(var registration in registrations)
            {
                var saved=registration.Persistence.Load();var identity=saved.Credential.Identity;
                if(identity.Uid.Length==0||!identities.Add((identity.Site,identity.Uid,identity.EnterpriseId))||!SiteEndpoints.Allowed(registration.ModelEndpoint,identity.Site))throw new CredentialFailure("credential_identity_invalid");
                if(previous.TryGetValue(registration.Id,out var old))
                {
                    if(!ReferenceEquals(old.Registration.Persistence,registration.Persistence)||!CredentialRules.SameIdentity(old.Current.Credential,saved.Credential))throw new CredentialFailure("credential_identity_changed");
                    if(SameCredential(old.Current.Credential,saved.Credential)&&old.Current.Disabled==(saved.Disabled||registration.Disabled)&&old.Registration.Disabled==registration.Disabled)
                    {staged.Add(registration.Id,old);continue;}
                    // Do not resurrect a restriction observed concurrently while draining.
                    if(Volatile.Read(ref old.Blocked)!=0&&!saved.Disabled)throw new CredentialFailure("credential_disabled");
                    changedIds.Add(registration.Id);
                }
                changed=true;staged.Add(registration.Id,new Cell(registration,saved));
            }
            if(!changed&&durableCommit is null)return false;
            cancellation.ThrowIfCancellationRequested();
            var next=staged.ToFrozenDictionary();
            durableCommit?.Invoke();
            // After durable commit publication cannot be cancelled. Retired cells
            // stay blocked even for a maintenance operation already queued on them.
            foreach(var pair in previous)
                if(!next.TryGetValue(pair.Key,out var same)||!ReferenceEquals(same,pair.Value))
                {lock(pair.Value.Barrier){pair.Value.Retired=true;pair.Value.BlockVersion++;Volatile.Write(ref pair.Value.Blocked,1);}}
            cells=next;
            pool.ReconcileAccounts(registrations.Select(r=>new AccountSnapshot(r.Id,r.ModelEndpoint,next[r.Id].Current.Credential.Identity.Site,Disabled:r.Disabled)),changedIds,
                ()=>{foreach(var cell in next.Values)PublishAvailability(cell,cell.Current);});
            return true;
        }
        finally{for(int i=permits.Count-1;i>=0;i--)permits[i].Dispose();registryGate.Release();}
    }
}
