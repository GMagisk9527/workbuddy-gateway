using System.Net;
using System.Net.Http.Headers;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class CatalogCacheTests
{
    static int checks;
    static void Check(bool b,string message){if(!b)throw new InvalidOperationException(message);checks++;}
    static byte[] Snapshot(int output)=>JsonSerializer.SerializeToUtf8Bytes(new{schema=5,catalogs=new{cn=new[]{new{id="m",maxInputTokens=1000,maxOutputTokens=output,contextLengths=new[]{128,256},contextDefaultLength=128}},intl=new[]{new{id="m",maxInputTokens=2000,maxOutputTokens=128}}}});
    static ModelCatalog Catalog(int output){var c=new ModelCatalog();c.Load(Snapshot(output));return c;}
    static byte[] Live(int output)=>JsonSerializer.SerializeToUtf8Bytes(new{code=0,data=new{models=new[]{new{id="m",maxInputTokens=1000,maxOutputTokens=output}}}});
    static void Wrap(string path,byte[] snapshot)
    {
        using var buffer=new MemoryStream();using(var writer=new Utf8JsonWriter(buffer))
        {writer.WriteStartObject();writer.WriteNumber("version",1);writer.WriteString("sha256",Convert.ToHexStringLower(SHA256.HashData(snapshot)));writer.WritePropertyName("snapshot");writer.WriteRawValue(snapshot);writer.WriteEndObject();}
        File.WriteAllBytes(path,buffer.ToArray());
    }
    internal static async Task Run(string scratch)
    {
        checks=0;Directory.CreateDirectory(scratch);Check(!Directory.EnumerateFileSystemEntries(scratch).Any(),"Empty scratch required");
        var source=Catalog(32);source.LoadLive(AccountSite.Cn,Live(64));var roundtrip=new ModelCatalog();roundtrip.Load(source.Export());
        Check(source.List().ToJsonString()==roundtrip.List().ToJsonString()&&roundtrip.Source=="cache","Export preserves both sites and restores cache provenance");
        var empty=new ModelCatalog();empty.Load(empty.Export());Check(empty.IsEmpty,"Empty catalog export is valid schema5");
        string foreign=Path.Combine(scratch,"foreign");Directory.CreateDirectory(foreign);File.WriteAllText(Path.Combine(foreign,"wb-models-cache.json"),"untouched");
        bool rejected=false;try{using var ignored=CatalogCache.Open(foreign);}catch(ArgumentException){rejected=true;}
        Check(rejected&&Directory.GetFiles(foreign).Length==1&&File.ReadAllText(Path.Combine(foreign,"wb-models-cache.json"))=="untouched","Never adopt a Go or other nonempty directory");
        string dir=Path.Combine(scratch,"cache"),primary=Path.Combine(dir,"catalog.json"),backup=Path.Combine(dir,"catalog.backup.json");
        using(var cache=CatalogCache.Open(dir))
        {
            var target=Catalog(999);Check(!cache.TryLoad(target)&&target.DefaultOutput("m",AccountSite.Cn)==999&&cache.Status.LoadedFrom=="none","Cache miss retains startup seed");
            rejected=false;try{using var conflict=CatalogCache.Open(dir);}catch(IOException){rejected=true;}
            Check(rejected,"A cache directory has one writer");
            Check(cache.TrySave(source)&&cache.Status.Saves==1&&!cache.Status.WriteFailed,"First complete snapshot saved");
            Check(File.Exists(primary)&&File.Exists(backup),"First save has a primary and recoverable backup");
            Check(cache.TryLoad(target)&&target.DefaultOutput("m",AccountSite.Cn)==64&&target.DefaultOutput("m",AccountSite.Intl)==128&&cache.Status.LoadedFrom=="primary","Both sites restored from disk");
            Check(cache.TrySave(Catalog(96)),"Second generation saved");
            File.WriteAllText(primary,"{broken");target=Catalog(999);
            Check(cache.TryLoad(target)&&target.DefaultOutput("m",AccountSite.Cn)==64&&cache.Status.LoadedFrom=="backup","Corrupt primary falls back to previous valid generation");
            Check(cache.TrySave(Catalog(128)),"Valid new primary can replace corrupt primary");
            File.WriteAllText(primary,"{broken again");target=Catalog(999);
            Check(cache.TryLoad(target)&&target.DefaultOutput("m",AccountSite.Cn)==64,"Corrupt primary was never copied over valid backup");
            File.WriteAllText(backup,"null");target=Catalog(777);
            Check(!cache.TryLoad(target)&&cache.Status.LoadedFrom=="invalid"&&target.DefaultOutput("m",AccountSite.Cn)==777,"Both invalid files retain startup seed");
            Check(cache.TrySave(Catalog(32)),"Repair replaces invalid generations with valid data");
            string bytes=File.ReadAllText(primary);File.WriteAllText(primary,bytes.Replace("\"maxOutputTokens\":32","\"maxOutputTokens\":999",StringComparison.Ordinal));
            target=Catalog(777);Check(cache.TryLoad(target)&&target.DefaultOutput("m",AccountSite.Cn)==32&&cache.Status.LoadedFrom=="backup","Checksum rejects syntactically valid corruption");
            Wrap(primary,"{\"schema\":999,\"catalogs\":{}}"u8.ToArray());
            Check(cache.TryLoad(target)&&cache.Status.LoadedFrom=="backup","Valid checksum does not bypass snapshot schema validation");
            Wrap(primary,"{\"schema\":5,\"catalogs\":{\"cn\":[{\"id\":\"m\",\"maxOutputTokens\":-1}]}}"u8.ToArray());
            Check(cache.TryLoad(target)&&cache.Status.LoadedFrom=="backup","Valid checksum does not bypass capability validation");
            using(var file=new FileStream(primary,FileMode.Create,FileAccess.Write))file.SetLength((8<<20)+4097);
            Check(cache.TryLoad(target)&&cache.Status.LoadedFrom=="backup","Oversize cache rejected before reading into memory");
            Check(cache.TrySave(Catalog(32)),"Cache repaired after bounds rejection");
            byte[] retained=File.ReadAllBytes(primary);Directory.CreateDirectory(primary+".tmp");
            Check(!cache.TrySave(Catalog(64))&&cache.Status.WriteFailed&&retained.SequenceEqual(File.ReadAllBytes(primary)),"Primary write failure preserves previous complete file");
            Directory.Delete(primary+".tmp");Check(cache.TrySave(Catalog(64))&&!cache.Status.WriteFailed,"Later successful write clears cache failure state");
            retained=File.ReadAllBytes(primary);Directory.CreateDirectory(backup+".tmp");
            Check(!cache.TrySave(Catalog(96))&&retained.SequenceEqual(File.ReadAllBytes(primary)),"Backup write failure leaves primary unchanged");Directory.Delete(backup+".tmp");
            File.WriteAllText(primary+".tmp","abandoned partial write");Check(cache.TrySave(Catalog(96))&&!File.Exists(primary+".tmp"),"Next save removes bounded crash debris");
            Parallel.For(0,24,i=>{source.LoadLive(i%2==0?AccountSite.Cn:AccountSite.Intl,Live(32+i));if(!cache.TrySave(source))throw new IOException("Unexpected concurrent save failure");});
            target=new();Check(cache.TryLoad(target)&&source.Export().SequenceEqual(target.Export()),"Concurrent refresh and save retain complete newest state");
            var restricted=new ModelCatalog(allow:["m"],block:["m"]);Check(cache.TryLoad(restricted)&&!restricted.Allows("m")&&restricted.List()["data"]!.AsArray().Count==0,"Cache restore retains runtime model policy");
        }
        using(var reopened=CatalogCache.Open(dir))Check(reopened.TryLoad(new()),"Writer lock is released on disposal");
        string wrongMarker=Path.Combine(scratch,"wrong-marker");using(var owned=CatalogCache.Open(wrongMarker)){ }
        File.WriteAllText(Path.Combine(wrongMarker,".csharp-catalog-cache"),"unrelated store");rejected=false;
        try{using var ignored=CatalogCache.Open(wrongMarker);}catch(ArgumentException){rejected=true;}
        Check(rejected,"Ownership marker contents must match");
        string unavailable=Path.Combine(scratch,"unavailable");using(var cache=CatalogCache.Open(unavailable))
        {Directory.CreateDirectory(Path.Combine(unavailable,"catalog.json"));var seeded=Catalog(444);Check(!cache.TryLoad(seeded)&&cache.Status.LoadedFrom=="unavailable"&&seeded.DefaultOutput("m",AccountSite.Cn)==444,"Unreadable cache file retains seed");}
        await Integration(Path.Combine(scratch,"integration"));
        Console.WriteLine(JsonSerializer.Serialize(new{catalogCacheChecks=checks,realNetworkCalls=0,modelCreditsUsed=0}));
    }
    sealed class Control:IAccountControlPlane,IModelCatalogControlPlane
    {
        internal int Calls;internal bool Invalid;
        public Task<AccountCredential> RefreshAsync(AccountCredential current,CancellationToken ct)=>throw new InvalidOperationException("Unexpected credential refresh");
        public Task<CredentialVerification> VerifyAsync(AccountCredential current,CancellationToken ct)=>Task.FromResult(CredentialVerification.Usable);
        public Task<byte[]> FetchCatalogAsync(AccountCredential current,CancellationToken ct){Interlocked.Increment(ref Calls);return Task.FromResult(Invalid?"{\"code\":11128}"u8.ToArray():Live(64));}
    }
    sealed class ModelHandler:HttpMessageHandler
    {
        internal int Calls;
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken ct)
        {
            Interlocked.Increment(ref Calls);using var doc=JsonDocument.Parse(await request.Content!.ReadAsByteArrayAsync(ct));
            Check(request.RequestUri==SiteEndpoints.Model(AccountSite.Cn)&&doc.RootElement.GetProperty("max_tokens").GetInt32()==64,"Restored cache supplies selected-site default budget");
            return new(HttpStatusCode.OK){Content=new StringContent("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")};
        }
    }
    static async Task Integration(string directory)
    {
        var id=new AccountId("cached-account");var endpoint=SiteEndpoints.Model(AccountSite.Cn);
        using var files=CredentialFiles.Create(Path.Combine(directory,"account"),new(new("synthetic-user"),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600));
        var first=new Control();var spare=new Control();var other=new AccountId("spare");using var otherFiles=CredentialFiles.Create(Path.Combine(directory,"spare"),files.Load().Credential);
        var pool=new AccountPool([new(id,endpoint),new(other,endpoint)]);var credentials=new CredentialManager(pool,[new(id,endpoint,files,first),new(other,endpoint,otherFiles,spare)]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(directory,"keys"));using var ledger=CreditLedger.Open(Path.Combine(directory,"keys"),[]);keys.AttachAccounting(ledger);
        var key=keys.Create(new("synthetic",Models:["m"],QuotaTotal:100));string token=key.TakeSecret();string cacheDirectory=Path.Combine(directory,"cache");
        using(var cache=CatalogCache.Open(cacheDirectory))
        {
            var catalog=Catalog(32);Check(cache.TrySave(catalog),"Integration seed written");
            Directory.CreateDirectory(Path.Combine(cacheDirectory,"catalog.json.tmp"));
            using var refresher=new CatalogRefresher(credentials,catalog,TimeSpan.FromHours(1),cache);await refresher.RefreshOnceAsync();
            Check(refresher.Status.UpdatedSites==1&&refresher.Status.FailedSites==0&&refresher.Status.CacheWriteFailed&&first.Calls+spare.Calls==1&&catalog.DefaultOutput("m",AccountSite.Cn)==64,"Disk failure retains live memory and does not retry another account");
            var restored=new ModelCatalog();Check(cache.TryLoad(restored)&&restored.DefaultOutput("m",AccountSite.Cn)==32,"Failed persistence retains old restart snapshot");
            Directory.Delete(Path.Combine(cacheDirectory,"catalog.json.tmp"));await refresher.RefreshOnceAsync();
            Check(!refresher.Status.CacheWriteFailed&&cache.TryLoad(restored)&&restored.DefaultOutput("m",AccountSite.Cn)==64,"Next refresh recovers persistence");
            first.Invalid=spare.Invalid=true;long saves=cache.Status.Saves;await refresher.RefreshOnceAsync();
            Check(refresher.Status.FailedSites==1&&cache.Status.Saves==saves&&catalog.DefaultOutput("m",AccountSite.Cn)==64,"Invalid upstream data never overwrites the durable cache");
            Check(ledger.Health.PendingAttempts==0&&ledger.GetUsage(key.Item.Id).Total==0&&credentials.Status.All(s=>s.Failures==0&&!s.Disabled),"Cache fetch and write failures do not charge or cool accounts");
        }
        using(var cache=CatalogCache.Open(cacheDirectory))
        {
            var restored=Catalog(999);var handler=new ModelHandler();
            await using var app=LocalGateway.Build(new(endpoint,Program.TestKey,Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:credentials,DirectUpstream:true,Catalog:restored,CatalogCache:cache,UpstreamHandler:handler));
            await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app))};client.DefaultRequestHeaders.Authorization=new AuthenticationHeaderValue("Bearer",token);
            using(var response=await client.GetAsync("/v1/models"))
            {var body=await response.Content.ReadAsStringAsync();Check(response.IsSuccessStatusCode&&response.Headers.GetValues("X-Model-Source").Single()=="cache"&&body.Contains("capabilities_by_site",StringComparison.Ordinal)&&restored.DefaultOutput("m",AccountSite.Cn)==64,"Host restores durable cache before serving model list");}
            foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})
            {
                object request=path=="/v1/responses"?new{model="m",input="hello",stream=false}:(object)new{model="m",messages=new[]{new{role="user",content="hello"}},stream=false};
                using var response=await client.PostAsJsonAsync(path,request);Check(response.IsSuccessStatusCode&&(await response.Content.ReadAsStringAsync()).Contains("ok",StringComparison.Ordinal),"Three protocols work from restored cache");
            }
            Check(handler.Calls==3&&ledger.Health.PendingAttempts==0&&ledger.GetUsage(key.Item.Id).Total==0,"Exactly one synthetic zero-credit model attempt per protocol");
        }
        first.Invalid=spare.Invalid=false;
        using(var cache=CatalogCache.Open(Path.Combine(directory,"hosted-cache")))
        {
            var handler=new ModelHandler();var catalog=Catalog(32);
            await using var app=LocalGateway.Build(new(endpoint,Program.TestKey,Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:credentials,DirectUpstream:true,Catalog:catalog,CatalogCache:cache,CatalogRefreshInterval:TimeSpan.FromHours(1),UpstreamHandler:handler));
            await app.StartAsync();var refresher=app.Services.GetRequiredService<CatalogRefresher>();
            using var deadline=new CancellationTokenSource(TimeSpan.FromSeconds(5));
            while(refresher.Status.Runs==0)await Task.Delay(10,deadline.Token);
            await app.StopAsync();var restored=new ModelCatalog();
            Check(cache.Status.Saves==2&&cache.TryLoad(restored)&&restored.DefaultOutput("m",AccountSite.Cn)==64,"Host injects cache into background refresher and drains on stop");
            Check(handler.Calls==0&&ledger.Health.PendingAttempts==0,"Hosted cache update invokes no model");
        }
    }
}
