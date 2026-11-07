using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;

namespace HttpMigration;

public sealed record CatalogRefreshStatus(long Runs,int UpdatedSites,int FailedSites,bool CacheWriteFailed=false);

// Sequential, bounded work; no model invocations or model scheduling permits.
public sealed class CatalogRefresher : BackgroundService
{
    readonly CredentialManager credentials;
    readonly ModelCatalog catalog;
    readonly TimeSpan interval;
    readonly CatalogCache? cache;
    readonly NpmCatalogSource? npm;
    int refreshing;
    readonly Dictionary<AccountSite,int> nextCandidate=new();
    CatalogRefreshStatus status=new(0,0,0);
    public CatalogRefreshStatus Status=>Volatile.Read(ref status);
    public ScheduledTaskState TaskState{get;}=new();
    public CatalogRefresher(CredentialManager credentials,ModelCatalog catalog,TimeSpan interval,CatalogCache? cache=null,NpmCatalogSource? npm=null)
    {
        if(interval<TimeSpan.FromSeconds(10)||interval>TimeSpan.FromDays(1))throw new ArgumentException("Catalog interval must be between 10 seconds and one day");
        this.credentials=credentials;this.catalog=catalog;this.interval=interval;this.cache=cache;this.npm=npm;
    }
    public async Task RefreshOnceAsync(CancellationToken cancellation=default)
    {
        if(Interlocked.CompareExchange(ref refreshing,1,0)!=0)return;
        string trace=TaskState.Begin();int updated=0,failed=0;
        try
        {
            string? version=npm is null?null:await npm.Version(cancellation);
            var sources=credentials.CatalogSources.GroupBy(s=>s.Site).ToDictionary(g=>g.Key,g=>g.ToArray());
            foreach(var site in npm is null?sources.Keys.Order().ToArray():new[]{AccountSite.Cn,AccountSite.Intl})
            {
                bool success=false;var candidates=sources.GetValueOrDefault(site)??[];int start=candidates.Length==0?0:nextCandidate.GetValueOrDefault(site)%candidates.Length;
                for(int i=0;i<Math.Min(3,candidates.Length);i++)
                {
                    int index=(start+i)%candidates.Length;var source=candidates[index];
                    cancellation.ThrowIfCancellationRequested();
                    using var deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);deadline.CancelAfter(TimeSpan.FromMinutes(1));
                    try { var bytes=await credentials.FetchCatalogAsync(source.Id,deadline.Token);deadline.Token.ThrowIfCancellationRequested();catalog.LoadLive(source.Site,bytes);success=true;nextCandidate[site]=index;break; }
                    catch(OperationCanceledException) when(!cancellation.IsCancellationRequested) { }
                    catch(Exception e) when(e is CredentialFailure or HttpRequestException or IOException or JsonException or ArgumentException or InvalidOperationException or FormatException or OverflowException) { }
                    nextCandidate[site]=(index+1)%candidates.Length;
                }
                if(version is not null&&npm is not null&&await npm.Fetch(version,site,cancellation) is{} fallback)
                {try{catalog.LoadNpm(site,fallback,success);success=true;}catch(Exception e)when(e is ArgumentException or JsonException or InvalidOperationException){}}
                if(success)updated++;else failed++;
            }
            cancellation.ThrowIfCancellationRequested();
            if(updated>0)cache?.TrySave(catalog);
            Volatile.Write(ref status,new(Status.Runs+1,updated,failed,cache?.Status.WriteFailed??false));
            TaskState.Finish(trace,updated+failed,updated,failed,interval,cache?.Status.WriteFailed??false);
        }
        catch(OperationCanceledException){TaskState.Finish(trace,updated+failed,updated,failed,interval,outcome:"cancelled");throw;}
        catch{TaskState.Finish(trace,updated+failed,updated,failed,interval,outcome:"failed");throw;}
        finally { Volatile.Write(ref refreshing,0); }
    }
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        using var timer=new PeriodicTimer(interval);
        do{await RefreshOnceAsync(stoppingToken);}while(await timer.WaitForNextTickAsync(stoppingToken));
    }
}
