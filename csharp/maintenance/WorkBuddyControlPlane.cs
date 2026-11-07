using AccountScheduling;
using System.Text.Json;

namespace AccountMaintenance;

// No client identity emulation. Channel rejection remains an upstream error.
public sealed class WorkBuddyControlPlane : IAccountControlPlane, IModelCatalogControlPlane,IAccountResourcesControlPlane,IAccountCheckinControlPlane, IDisposable
{
    readonly AccountSite site;
    readonly LoopbackControlPlane core;
    public WorkBuddyControlPlane(AccountSite site, HttpMessageHandler? handler = null, TimeSpan? timeout = null, TimeProvider? time = null)
    {
        this.site = site;
        core = new(SiteEndpoints.Refresh(site), SiteEndpoints.Verify(site), handler, timeout, time);
    }
    void Check(AccountCredential c) { if (c.Identity.Site != site) throw new CredentialFailure("credential_site_mismatch"); }
    public Task<AccountCredential> RefreshAsync(AccountCredential current, CancellationToken cancellation)
    { Check(current); return core.RefreshAsync(current,cancellation); }
    public Task<CredentialVerification> VerifyAsync(AccountCredential candidate, CancellationToken cancellation)
    { Check(candidate); return core.VerifyAsync(candidate,cancellation); }
    public Task<byte[]> FetchCatalogAsync(AccountCredential credential, CancellationToken cancellation)
    { Check(credential); return core.FetchCatalogAsync(SiteEndpoints.Catalog(site),credential,cancellation); }
    public void Dispose() => core.Dispose();
    public Task<CheckinResult> CheckinAsync(AccountCredential credential,CancellationToken cancellation)
    {
        Check(credential);
        if(site!=AccountSite.Cn||!string.IsNullOrWhiteSpace(credential.Identity.EnterpriseId))throw new CredentialFailure("checkin_unsupported");
        return core.CheckinAsync(credential,cancellation);
    }
    public async Task<AccountResourceObservation> FetchResourcesAsync(AccountCredential credential,CancellationToken cancellation,bool fetchPackages=true)
    {
        Check(credential);using var deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);deadline.CancelAfter(TimeSpan.FromSeconds(10));
        bool enterprise=!string.IsNullOrWhiteSpace(credential.Identity.EnterpriseId);
        var summary=await core.ResourceDataAsync(enterprise?SiteEndpoints.EnterpriseUsage(site):SiteEndpoints.Verify(site),credential,new{},enterprise,deadline.Token);
        var quota=ResourceParser.Quota(summary,enterprise);
        if(enterprise||!fetchPackages)return new(quota,[],false);
        var codes=ResourceParser.PackageCodes(summary);if(codes.Length==0)return new(quota,[],false);
        var packages=new List<CreditPackage>();var ids=new HashSet<string>(StringComparer.Ordinal);
        try
        {
            var free=await core.ResourceDataAsync(SiteEndpoints.CreditPackages(site,false),credential,new{PackageCodes=codes,PageNumber=1,PageSize=200,Status=new[]{0}},false,deadline.Token);ResourceParser.AddPackages(packages,ids,free);
            if(quota.Paid)
            {var paid=await core.ResourceDataAsync(SiteEndpoints.CreditPackages(site,true),credential,new{PackageCodes=codes,PageNumber=1,PageSize=200,Status=new[]{0,3},NeedRenewInfo=true},false,deadline.Token);ResourceParser.AddPackages(packages,ids,paid);}
            return new(quota,packages.ToArray(),true);
        }
        catch(OperationCanceledException) when(!cancellation.IsCancellationRequested){return new(quota,[],false);}
        catch(Exception e) when(e is CredentialFailure or JsonException or InvalidOperationException){return new(quota,[],false);}
    }
}
