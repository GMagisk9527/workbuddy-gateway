using System.Net;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class LiveCatalogTests
{
    static void Check(bool b,string reason){if(!b)throw new InvalidOperationException(reason);}
    static byte[] Live(int output=64)=>JsonSerializer.SerializeToUtf8Bytes(new{code=0,data=new{models=new[]{new{id="m",maxInputTokens=1000,maxOutputTokens=output,contextWindow=new{supportedLengths=new[]{128,256,256,2000},defaultLength=2000}}}}});
    sealed class Handler(AccountSite site):HttpMessageHandler
    {
        internal string Mode="ok";
        internal int Catalogs,Refreshes,Verifies;
        internal TaskCompletionSource Entered=new(TaskCreationOptions.RunContinuationsAsynchronously),Release=new(TaskCreationOptions.RunContinuationsAsynchronously);
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken cancellation)
        {
            if(request.RequestUri==SiteEndpoints.Refresh(site))
            {
                Interlocked.Increment(ref Refreshes);Check(request.Headers.GetValues("X-Refresh-Token").Single() is "synthetic-refresh" or "fresh-refresh","Refresh token confined to refresh RPC");
                return Json("{\"code\":0,\"data\":{\"accessToken\":\"fresh-access\",\"refreshToken\":\"fresh-refresh\",\"expiresIn\":3600}}");
            }
            if(request.RequestUri==SiteEndpoints.Verify(site)){Interlocked.Increment(ref Verifies);return Json("{\"code\":0,\"data\":{}}");}
            Interlocked.Increment(ref Catalogs);
            Check(request.RequestUri==SiteEndpoints.Catalog(site)&&request.Method==HttpMethod.Get&&request.Content is null,"Catalog GET has pinned site and no body");
            Check(request.Headers.Authorization?.Parameter is "old-access" or "fresh-access" && request.Headers.GetValues("X-User-Id").Single()=="synthetic-user" && request.Headers.GetValues("X-Enterprise-Id").Single()=="synthetic-enterprise" && request.Headers.GetValues("X-Domain").Single()=="synthetic-domain","Account headers on catalog only");
            Check(!request.Headers.Contains("Cookie")&&!request.Headers.Contains("X-Refresh-Token")&&!request.Headers.Contains("X-Client-ID")&&request.Headers.UserAgent.ToString()=="WorkBuddy-Gateway-CSharp/0.1","No cookie, refresh secret, or official-client identity");
            switch(Mode)
            {
                case "wait":Entered.TrySetResult();await Release.Task.WaitAsync(cancellation);break;
                case "timeout":Entered.TrySetResult();await Task.Delay(Timeout.InfiniteTimeSpan,cancellation);break;
                case "transport":throw new HttpRequestException("synthetic failure");
                case "redirect":return new(HttpStatusCode.Redirect){Headers={Location=new("https://example.invalid/")},Content=new StringContent("secret must not follow")};
                case "403":return new(HttpStatusCode.Forbidden){Content=new StringContent("synthetic-secret-body")};
                case "oversize":return new(HttpStatusCode.OK){Content=new ByteArrayContent(new byte[(8<<20)+1])};
                case "stream-oversize":return new(HttpStatusCode.OK){Content=new StreamContent(new UnknownLengthStream((8<<20)+1))};
                case "body-timeout":return new(HttpStatusCode.OK){Content=new StreamContent(new DelayedStream())};
                case "bad":return Json("{\"code\":11128,\"data\":{\"models\":[{\"id\":\"rejected\"}]}}");
                case "invalid":return Json("{\"code\":0,\"data\":{\"models\":[{\"id\":\"m\",\"maxOutputTokens\":-1}]}}");
            }
            return new(HttpStatusCode.OK){Content=new ByteArrayContent(Live(site==AccountSite.Cn?64:128))};
        }
        static HttpResponseMessage Json(string body)=>new(HttpStatusCode.OK){Content=new StringContent(body,Encoding.UTF8,"application/json")};
    }
    sealed class UnknownLengthStream(int length):Stream
    {
        int remaining=length;
        public override bool CanRead=>true;public override bool CanSeek=>false;public override bool CanWrite=>false;
        public override long Length=>throw new NotSupportedException();public override long Position{get=>throw new NotSupportedException();set=>throw new NotSupportedException();}
        public override int Read(byte[] b,int o,int count){int n=Math.Min(remaining,count);b.AsSpan(o,n).Fill((byte)'x');remaining-=n;return n;}
        public override ValueTask<int> ReadAsync(Memory<byte> b,CancellationToken ct=default){ct.ThrowIfCancellationRequested();int n=Math.Min(remaining,b.Length);b.Span[..n].Fill((byte)'x');remaining-=n;return ValueTask.FromResult(n);}
        public override void Flush()=>throw new NotSupportedException();public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long l)=>throw new NotSupportedException();public override void Write(byte[] b,int o,int c)=>throw new NotSupportedException();
    }
    sealed class DelayedStream:Stream
    {
        public override bool CanRead=>true;public override bool CanSeek=>false;public override bool CanWrite=>false;
        public override long Length=>throw new NotSupportedException();public override long Position{get=>throw new NotSupportedException();set=>throw new NotSupportedException();}
        public override int Read(byte[] b,int o,int c)=>throw new NotSupportedException();
        public override async ValueTask<int> ReadAsync(Memory<byte> b,CancellationToken ct=default){await Task.Delay(Timeout.InfiniteTimeSpan,ct);return 0;}
        public override void Flush()=>throw new NotSupportedException();public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long l)=>throw new NotSupportedException();public override void Write(byte[] b,int o,int c)=>throw new NotSupportedException();
    }
    internal static async Task Run(string scratch)
    {
        Directory.CreateDirectory(scratch);Check(!Directory.EnumerateFileSystemEntries(scratch).Any(),"Empty scratch required");int checks=0;
        var catalog=new ModelCatalog();catalog.Load("{\"schema\":5,\"catalogs\":{\"intl\":[{\"id\":\"old-intl\",\"maxOutputTokens\":99}]}}"u8.ToArray());
        catalog.LoadLive(AccountSite.Cn,Live());
        Check(catalog.DefaultOutput("m",AccountSite.Cn)==64&&catalog.DefaultOutput("old-intl",AccountSite.Intl)==99&&catalog.Source=="mixed","Live update preserves other site cache");checks++;
        var m=(JsonObject)catalog.List()["data"]![0]!;Check(m["context_length"]!.GetValue<int>()==1000&&m["default_context_length"]!.GetValue<int>()==1000&&m["context_length_options"]!.ToJsonString()=="[128,256]","Live contextWindow capability normalization");checks++;
        string before=catalog.List().ToJsonString();
        foreach(string bad in new[]{"null","{}","{\"code\":11128,\"data\":{}}","{\"code\":0}","{\"data\":{\"models\":[{\"id\":\"m\"}]}}","{\"code\":0,\"data\":{\"models\":[]}}","{\"code\":0,\"data\":{\"models\":[null]}}","{\"code\":0,\"data\":{\"models\":[{\"id\":\"x\",\"maxOutputTokens\":-1}]}}","{\"code\":0,\"data\":{\"models\":[{\"id\":\"x\",\"contextWindow\":\"wrong\"}]}}"})
        {
            bool rejected=false;try{catalog.LoadLive(AccountSite.Cn,Encoding.UTF8.GetBytes(bad));}catch(Exception e) when(e is ArgumentException or JsonException or InvalidOperationException){rejected=true;}
            Check(rejected&&before==catalog.List().ToJsonString(),"Malformed or rejected reload preserves last good catalog");checks++;
        }
        foreach(int size in new[]{501})
        {bool rejected=false;try{catalog.LoadLive(AccountSite.Cn,JsonSerializer.SerializeToUtf8Bytes(new{code=0,data=new{models=Enumerable.Range(0,size).Select(i=>new{id="m"+i})}}));}catch(ArgumentException){rejected=true;}Check(rejected&&before==catalog.List().ToJsonString(),"Model count bounded");checks++;}
        Parallel.Invoke(()=>catalog.LoadLive(AccountSite.Cn,Live(32)),()=>catalog.LoadLive(AccountSite.Intl,Live(128)));
        Check(catalog.DefaultOutput("m",AccountSite.Cn)==32&&catalog.DefaultOutput("m",AccountSite.Intl)==128&&catalog.Source=="live","Concurrent site updates do not overwrite each other");checks++;
        catalog.LoadLive(AccountSite.Cn,"{\"code\":0,\"data\":{\"models\":[{\"id\":\"m\",\"contextLengths\":\"ignored\",\"contextDefaultLength\":\"ignored\",\"contextWindow\":{\"supportedLengths\":[128,256],\"defaultLength\":128}}]}}"u8.ToArray());
        Check(catalog.List()["data"]![0]!["capabilities_by_site"]!["cn"]!["default_context_length"]!.GetValue<int>()==128,"Live parser ignores cache-shaped capability extras");checks++;
        foreach(var site in new[]{AccountSite.Cn,AccountSite.Intl})
        {
            var credential=new AccountCredential(new("synthetic-user","synthetic-enterprise",Site:site),"old-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+60,"synthetic-domain");
            using var files=CredentialFiles.Create(Path.Combine(scratch,site.ToString()),credential);var id=new AccountId("account");var pool=new AccountPool([new(id,SiteEndpoints.Model(site),site)]);
            var handler=new Handler(site);using var control=new WorkBuddyControlPlane(site,handler,TimeSpan.FromSeconds(1));var manager=new CredentialManager(pool,[new(id,SiteEndpoints.Model(site),files,control)]);
            var local=new ModelCatalog();using var refresh=new CatalogRefresher(manager,local,TimeSpan.FromHours(1));await refresh.RefreshOnceAsync();
            Check(local.DefaultOutput("m",site)==(site==AccountSite.Cn?64:128)&&handler.Catalogs==1&&handler.Refreshes==1&&handler.Verifies==1&&refresh.Status==new CatalogRefreshStatus(1,1,0),"Refresh stale credential once before catalog fetch");checks++;
            foreach(string mode in new[]{"bad","invalid","403","redirect","transport","oversize","stream-oversize","timeout","body-timeout"})
            {
                before=local.List().ToJsonString();handler.Mode=mode;int calls=handler.Catalogs;await refresh.RefreshOnceAsync();
                Check(before==local.List().ToJsonString()&&handler.Catalogs==calls+1&&refresh.Status.FailedSites==1&&!manager.Status.Single().Disabled&&manager.Status.Single().Failures==0,"Catalog failure retains state and does not cool/disable model account: "+mode);checks++;
            }
            handler.Mode="timeout";using(var cancel=new CancellationTokenSource(TimeSpan.FromMilliseconds(30)))
            {
                bool cancelled=false;try{await refresh.RefreshOnceAsync(cancel.Token);}catch(OperationCanceledException){cancelled=true;}
                Check(cancelled,"Caller cancellation propagated");checks++;
            }
            handler.Mode="wait";handler.Entered=new(TaskCreationOptions.RunContinuationsAsynchronously);handler.Release=new(TaskCreationOptions.RunContinuationsAsynchronously);
            Task running=refresh.RefreshOnceAsync();await handler.Entered.Task.WaitAsync(TimeSpan.FromSeconds(2));int started=handler.Catalogs;
            await Task.WhenAll(Enumerable.Range(0,64).Select(_=>refresh.RefreshOnceAsync()));
            Check(handler.Catalogs==started,"Concurrent triggers do not queue duplicate catalog RPCs");checks++;
            Task rotation=manager.RefreshAsync(id);await Task.Delay(25);Check(handler.Refreshes==1&&!rotation.IsCompleted,"Catalog read lease excludes credential rotation");checks++;
            handler.Release.TrySetResult();await Task.WhenAll(running,rotation);Check(handler.Refreshes==2,"Rotation proceeds after catalog response is consumed");checks++;
            handler.Mode="ok";await refresh.RefreshOnceAsync();Check(refresh.Status.UpdatedSites==1,"Cancellation and failures release refresh gate");checks++;
            manager.Block(id);int previous=handler.Catalogs;await refresh.RefreshOnceAsync();Check(manager.CatalogSources.Count==0&&previous==handler.Catalogs,"Blocked accounts never fetch catalog");checks++;
        }
        await Hosted(Path.Combine(scratch,"host"));checks+=3;
        await BoundedFallback(Path.Combine(scratch,"bounded"));checks+=3;
        Console.WriteLine(JsonSerializer.Serialize(new{liveCatalogChecks=checks,realNetworkCalls=0,modelCreditsUsed=0}));
    }
    static async Task Hosted(string directory)
    {
        var id=new AccountId("hosted");var site=AccountSite.Cn;var endpoint=SiteEndpoints.Model(site);
        using var files=CredentialFiles.Create(Path.Combine(directory,"account"),new(new("synthetic-user","synthetic-enterprise",Site:site),"old-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600,"synthetic-domain"));
        var pool=new AccountPool([new(id,endpoint,site)]);var handler=new Handler(site){Mode="timeout"};using var control=new WorkBuddyControlPlane(site,handler);
        var manager=new CredentialManager(pool,[new(id,endpoint,files,control)]);using var keys=PreviewKeyDirectory.Open(Path.Combine(directory,"keys"));using var ledger=CreditLedger.Open(Path.Combine(directory,"keys"),[]);
        var catalog=new ModelCatalog();await using var app=LocalGateway.Build(new(endpoint,Program.TestKey,Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,Catalog:catalog,CatalogRefreshInterval:TimeSpan.FromHours(1)));
        await app.StartAsync();await handler.Entered.Task.WaitAsync(TimeSpan.FromSeconds(2));using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false});
        using(var health=await client.GetAsync(LocalGateway.Address(app)+"/readyz"))Check(health.IsSuccessStatusCode,"Catalog startup runs in background, health remains available");
        await app.StopAsync().WaitAsync(TimeSpan.FromSeconds(2));Check(handler.Catalogs==1,"Host stops pending catalog fetch without extra requests");
        Check(ledger.Health.PendingAttempts==0&&manager.Status.Single().Failures==0,"Catalog refresh never starts billing attempts");
    }
    static async Task BoundedFallback(string directory)
    {
        var stores=new List<CredentialFiles>();var controls=new List<WorkBuddyControlPlane>();var handlers=new List<Handler>();var registrations=new List<CredentialRegistration>();var snapshots=new List<AccountSnapshot>();
        try
        {
            foreach(int i in Enumerable.Range(0,4))
            {
                var id=new AccountId("bounded"+i);var endpoint=SiteEndpoints.Model(AccountSite.Cn);
                var store=CredentialFiles.Create(Path.Combine(directory,i.ToString()),new(new("synthetic-user","synthetic-enterprise"),"old-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600,"synthetic-domain"));stores.Add(store);
                var handler=new Handler(AccountSite.Cn){Mode="403"};handlers.Add(handler);var control=new WorkBuddyControlPlane(AccountSite.Cn,handler);controls.Add(control);
                snapshots.Add(new(id,endpoint,AccountSite.Cn));registrations.Add(new(id,endpoint,store,control));
            }
            var manager=new CredentialManager(new(snapshots),registrations);using var refresh=new CatalogRefresher(manager,new(),TimeSpan.FromHours(1));await refresh.RefreshOnceAsync();
            Check(handlers.Sum(h=>h.Catalogs)==3&&refresh.Status.FailedSites==1,"At most three credential candidates per site per tick");
            handlers.Single(h=>h.Catalogs==0).Mode="ok";await refresh.RefreshOnceAsync();
            Check(handlers.Sum(h=>h.Catalogs)==4&&refresh.Status.UpdatedSites==1,"Later tick reaches candidates beyond the first batch");
            foreach(var h in handlers)h.Mode="ok";int before=handlers.Sum(h=>h.Catalogs);await refresh.RefreshOnceAsync();
            Check(handlers.Sum(h=>h.Catalogs)==before+1&&refresh.Status.UpdatedSites==1,"First successful account stops catalog fallback");
        }
        finally{foreach(var c in controls)c.Dispose();foreach(var s in stores)s.Dispose();}
    }
}
