using System.Net.Http.Json;
using System.Text.Json;
using System.Text;
using AccountMaintenance;
using AccountScheduling;
using StreamingMigration;

namespace HttpMigration;

// Explicit opt-in only. Reserves the durable twelve-hour window before sending,
// so a crash or ambiguous reply never immediately duplicates a paid request.
public sealed class ModelPriceProbeWorker:BackgroundService
{
    public sealed record Probe(string Site,string Model,long LastAt,ModelCost Cost=ModelCost.Unknown);
    readonly CredentialManager credentials;readonly ModelCatalog catalog;readonly ConsoleTelemetry telemetry;readonly HttpClient client;
    readonly string path;readonly TimeProvider clock;readonly SemaphoreSlim gate=new(1,1);List<Probe> probes=[];
    public bool Blocked{get;private set;}
    Probe[] published=[];
    public IReadOnlyList<Probe> Results=>Volatile.Read(ref published).ToArray();
    public ScheduledTaskState TaskState{get;}
    public ModelPriceProbeWorker(CredentialManager credentials,ModelCatalog catalog,ConsoleRuntime console,HttpClient client,TimeProvider? clock=null)
    {
        this.credentials=credentials;this.catalog=catalog;telemetry=console.Telemetry;this.client=client;this.clock=clock??TimeProvider.System;TaskState=new(this.clock);
        path=Path.Combine(console.Settings.DirectoryPath??throw new ArgumentException("Price probes require owned durable state"),"price-probes.json");
        try
        {
            probes=PriceProbeState.Read(path,this.clock.GetUtcNow());
        }
        catch(Exception e)when(e is IOException or UnauthorizedAccessException or JsonException or InvalidDataException or ArgumentException){Blocked=true;}
        if(!Blocked){published=probes.ToArray();foreach(var p in probes)catalog.ObserveProbeCost(p.Site=="cn"?AccountSite.Cn:AccountSite.Intl,p.Model,p.Cost,p.LastAt);}
    }
    IEnumerable<(AccountSite Site,string Model,AccountId Account)> Pending()
    {
        var requested=telemetry.Rows().Select(r=>r.Model).ToHashSet();long now=clock.GetUtcNow().ToUnixTimeSeconds();var resources=credentials.ResourceStatus.ToDictionary(r=>r.Id);
        foreach(var site in new[]{AccountSite.Cn,AccountSite.Intl})foreach(string model in catalog.ModelIds())
        {
            string key=site==AccountSite.Cn?"cn":"intl";
            if(!catalog.NeedsProbe(site,model,requested.Contains(model))||probes.Any(p=>p.Site==key&&p.Model==model&&now-p.LastAt<43200))continue;
            var account=credentials.CatalogSources.Where(a=>a.Site==site&&credentials.AllowsModel(a.Id,model))
                .Where(a=>!resources.TryGetValue(a.Id.Value,out var r)||!r.Known||r.Unlimited||r.Remaining>0)
                .OrderByDescending(a=>resources.TryGetValue(a.Id.Value,out var r)&&r.Known).FirstOrDefault();
            if(account is not null)yield return(site,model,account.Id);
        }
    }
    void Save(){ConsoleSettings.Atomic(path,JsonSerializer.SerializeToUtf8Bytes(probes, StorageJson.Default.ListProbe));Volatile.Write(ref published,probes.ToArray());}
    public async Task RunOnceAsync(CancellationToken token=default)
    {
        if(!await gate.WaitAsync(0,token))return;string trace=TaskState.Begin();int attempted=0,ok=0,failed=0;
        try
        {
            if(Blocked){TaskState.Finish(trace,1,0,1,TimeSpan.FromMinutes(30));return;}
            foreach(var item in Pending().Take(probes.Count==0?30:5).ToArray())
            {
                token.ThrowIfCancellationRequested();string site=item.Site==AccountSite.Cn?"cn":"intl";
                var old=probes.SingleOrDefault(p=>p.Site==site&&p.Model==item.Model);
                if(old is null&&probes.Count==1000)break;
                var reserved=new Probe(site,item.Model,clock.GetUtcNow().ToUnixTimeSeconds(),old?.Cost??ModelCost.Unknown);
                if(old is not null)probes.Remove(old);probes.Add(reserved);
                try{Save();}catch(Exception e)when(e is IOException or UnauthorizedAccessException){Blocked=true;failed++;break;}
                attempted++;
                using var deadline=CancellationTokenSource.CreateLinkedTokenSource(token);deadline.CancelAfter(TimeSpan.FromSeconds(60));
                bool restricted=false;
                try
                {
                    using var credential=await credentials.AcquireForHeadersAsync(item.Account,SiteEndpoints.Model(item.Site),deadline.Token);
                    if(!catalog.Allows(item.Model)||!credentials.AllowsModel(item.Account,item.Model)){failed++;continue;}
                    using var request=new HttpRequestMessage(HttpMethod.Post,SiteEndpoints.Model(item.Site)){Content=JsonContent.Create<object>(new Dictionary<string, object?> { ["model"] = item.Model, ["stream"] = true, ["max_tokens"] = 300, ["messages"] = new[]{new Dictionary<string, object?> { ["role"] = "system", ["content"] = "You are a helpful assistant." },new Dictionary<string, object?> { ["role"] = "user", ["content"] = "Please write a factual paragraph of about 150 words about the water cycle, without markdown." }} }, GatewayJson.Default.Object)};
                    credential.ApplyTo(request);
                    using var response=await client.SendAsync(request,HttpCompletionOption.ResponseHeadersRead,deadline.Token);
                    if(!response.IsSuccessStatusCode)
                    {
                        byte[] raw=await LocalGateway.ReadBounded(await response.Content.ReadAsStreamAsync(deadline.Token),65536,deadline.Token);
                        try{ChunkReader.Parse(Encoding.UTF8.GetString(raw));}
                        catch(ConversionFailure e){restricted=e.Code=="11140";}
                        if(restricted)credentials.Block(item.Account);
                        failed++;continue;
                    }
                    var result=await StreamPump.RunAsync(await response.Content.ReadAsStreamAsync(deadline.Token),Stream.Null,new(item.Model,RetainedLimit:1<<20),deadline.Token);
                    restricted=result.Code=="11140";
                    if(restricted)credentials.Block(item.Account);
                    var cost=result.Outcome==StreamOutcome.Complete?ModelObservations.Cost(result.RawUsageJson):ModelCost.Unknown;
                    if(cost==ModelCost.Unknown){failed++;continue;}
                    probes[probes.IndexOf(reserved)]=reserved with{Cost=cost};Save();catalog.ObserveProbeCost(item.Site,item.Model,cost,reserved.LastAt);ok++;
                }
                catch(OperationCanceledException)when(!token.IsCancellationRequested){failed++;}
                catch(Exception e)when(e is CredentialFailure or HttpRequestException or IOException or UnauthorizedAccessException or RequestFailure){failed++;}
                finally
                {
                    // Release the credential reader before acquiring the writer used to persist restriction.
                    if(restricted)
                    {
                        try{await credentials.DisableAsync(item.Account);}
                        catch(CredentialFailure e)when(e.Code=="credential_missing"){/* Explicit deletion won after the reader drained. */}
                    }
                }
            }
            TaskState.Finish(trace,attempted,ok,failed,Pending().Any()?TimeSpan.FromMinutes(2):TimeSpan.FromMinutes(30),cacheFailed:Blocked,outcome:Blocked?"failed":null);
        }
        catch(OperationCanceledException){TaskState.Finish(trace,attempted,ok,failed,TimeSpan.Zero,outcome:"cancelled");throw;}
        finally{gate.Release();}
    }
    protected override async Task ExecuteAsync(CancellationToken token)
    {
        TaskState.Schedule(clock.GetUtcNow().AddSeconds(20).ToUnixTimeSeconds());await Task.Delay(TimeSpan.FromSeconds(20),clock,token);
        while(!token.IsCancellationRequested){await RunOnceAsync(token);await Task.Delay(Pending().Any()?TimeSpan.FromMinutes(2):TimeSpan.FromMinutes(30),clock,token);}
    }
}
