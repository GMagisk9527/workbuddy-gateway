using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using System.Security.Cryptography;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;
internal static class StandaloneAccountsTests
{
    static int checks;
    static void Check(bool b,string reason){if(!b)throw new InvalidOperationException(reason);checks++;}
    static void Create(string root,string directory,string uid,AccountSite site,bool disabled=false)
    {
        using var files=CredentialFiles.Create(Path.Combine(root,directory),new(new(uid,Site:site),"synthetic-access-"+uid,"synthetic-refresh-"+uid,DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600));
        if(disabled)files.Save(files.Load() with{Disabled=true});
    }
    static string Write(string root,object value){string path=Path.Combine(root,"accounts.json");File.WriteAllBytes(path,JsonSerializer.SerializeToUtf8Bytes(value));return path;}
    sealed class Control:IAccountControlPlane,IDisposable,IModelCatalogControlPlane
    {
        internal int Disposed;
        public Task<AccountCredential> RefreshAsync(AccountCredential current,CancellationToken ct)=>throw new InvalidOperationException("Unexpected refresh");
        public Task<CredentialVerification> VerifyAsync(AccountCredential current,CancellationToken ct)=>Task.FromResult(CredentialVerification.Usable);
        public Task<byte[]> FetchCatalogAsync(AccountCredential current,CancellationToken ct)=>Task.FromResult("{\"code\":0,\"data\":{\"models\":[{\"id\":\"m\"}]}}"u8.ToArray());
        public void Dispose()=>Disposed++;
    }
    static Dictionary<string,string> Digests(string root)=>Directory.EnumerateFiles(root,"*.json",SearchOption.AllDirectories).ToDictionary(p=>p,p=>Convert.ToHexString(SHA256.HashData(File.ReadAllBytes(p))));
    internal static async Task Run(string scratch)
    {
        checks=0;Directory.CreateDirectory(scratch);Check(!Directory.EnumerateFileSystemEntries(scratch).Any(),"Empty scratch required");
        Create(scratch,"cn","user-cn",AccountSite.Cn);Create(scratch,"intl","user-intl",AccountSite.Intl);
        Create(scratch,"configured-off","user-off",AccountSite.Cn);Create(scratch,"persisted-off","user-disabled",AccountSite.Intl,true);
        var entries=new[]{new{id="cn",directory="cn",disabled=false},new{id="intl",directory="intl",disabled=false},new{id="off",directory="configured-off",disabled=true},new{id="stored-off",directory="persisted-off",disabled=false}};
        string manifest=Write(scratch,new{version=1,accounts=entries});var before=Digests(scratch);var controls=new List<Control>();
        using(var runtime=StandaloneAccounts.OpenManifest(manifest,_=>{var c=new Control();controls.Add(c);return c;}))
        {
            Check(runtime.Pool.Count==4&&controls.Count==2,"Multiple accounts share one control plane per site");
            Check(runtime.Credentials.Status.Count(s=>s.Disabled)==2&&runtime.Credentials.CatalogSources.Count==2,"Config and stored disable both exclude catalog refresh");
            for(int i=0;i<8;i++)
            {using var lease=await runtime.Pool.AcquireAsync("m");Check(lease.Account.Id.Value is "cn" or "intl","Only active manifest accounts scheduled");using var credential=await runtime.Credentials.AcquireForHeadersAsync(lease);using var message=new HttpRequestMessage(HttpMethod.Post,lease.Account.Upstream);credential.ApplyTo(message);Check(message.Headers.Authorization?.Parameter=="synthetic-access-"+(lease.Account.Id.Value=="cn"?"user-cn":"user-intl"),"Selected account applies its own credentials");}
            Check(before.All(p=>p.Value==Convert.ToHexString(SHA256.HashData(File.ReadAllBytes(p.Key)))) ,"Startup does not modify account/config files");
            await runtime.Credentials.DisableAsync(new("cn"));
        }
        Check(controls.All(c=>c.Disposed==1),"Shared controls disposed once");
        using(var reopened=StandaloneAccounts.OpenManifest(manifest,_=>new Control()))
        {Check(reopened.Credentials.Status.Count(s=>s.Disabled)==3,"Restriction disable survives manifest restart");using var lease=await reopened.Pool.AcquireAsync("m");Check(lease.Account.Id.Value=="intl","Restart excludes restricted and configured-disabled accounts");}
        foreach(object invalid in new object[]{new{version=2,accounts=entries},new{version=1,accounts=new[]{new{id="x",directory="../cn"}}},new{version=1,accounts=new[]{new{id="x",directory=Path.Combine(scratch,"cn")}}},new{version=1,accounts=new[]{new{id="x",directory="cn"},new{id="x",directory="intl"}}},new{version=1,accounts=new[]{new{id="x",directory="cn"},new{id="y",directory="cn"}}},new{version=1,accounts=new[]{new{id="x",directory="cn",disabled="false"}}},new{version=1,accounts=new[]{new{id="x",directory="cn",typo=true}}},new{version=1,accounts=Enumerable.Range(0,1025).Select(i=>new{id="a"+i,directory="cn"}).ToArray()}})
        {bool rejected=false;try{using var ignored=StandaloneAccounts.OpenManifest(Write(scratch,invalid),_=>new Control());}catch(Exception e) when(e is ArgumentException or JsonException){rejected=true;}Check(rejected,"Reject invalid manifest before startup");}
        Create(scratch,"copy-cn","user-cn",AccountSite.Cn);
        bool duplicate=false;try{using var ignored=StandaloneAccounts.OpenManifest(Write(scratch,new{version=1,accounts=new[]{new{id="a",directory="cn"},new{id="b",directory="copy-cn"}}}),_=>new Control());}catch(ArgumentException){duplicate=true;}
        Check(duplicate,"Duplicate declared identity rejected across directories");
        using(var reacquire=CredentialFiles.Open(Path.Combine(scratch,"cn")))Check(reacquire.Load().Disabled,"Failure releases earlier account lock");
        Directory.CreateDirectory(Path.Combine(scratch,"unowned"));File.Copy(Path.Combine(scratch,"intl/credential.json"),Path.Combine(scratch,"unowned/credential.json"));File.Copy(Path.Combine(scratch,"intl/state.json"),Path.Combine(scratch,"unowned/state.json"));
        bool unowned=false;try{using var ignored=StandaloneAccounts.OpenManifest(Write(scratch,new{version=1,accounts=new[]{new{id="a",directory="intl"},new{id="b",directory="unowned"}}}),_=>new Control());}catch(CredentialFailure){unowned=true;}
        Check(unowned,"Go/unowned files are not adopted");using(var reacquire=CredentialFiles.Open(Path.Combine(scratch,"intl")))Check(!reacquire.Load().Disabled,"Partial load releases lock before failure");
        var constructed=new Control();bool factoryFailed=false;
        try{using var ignored=StandaloneAccounts.OpenManifest(Write(scratch,new{version=1,accounts=entries}),s=>s==AccountSite.Cn?constructed:throw new IOException("synthetic control factory failure"));}catch(IOException){factoryFailed=true;}
        Check(factoryFailed&&constructed.Disposed==1,"Control initialization failure disposes previously constructed shared client");
        using(var reacquire=CredentialFiles.Open(Path.Combine(scratch,"cn")))Check(reacquire.Load().Disabled,"Control factory failure releases all file owners");
        using(var allOff=StandaloneAccounts.OpenManifest(Write(scratch,new{version=1,accounts=entries.Select(e=>new{e.id,e.directory,disabled=true})}),_=>new Control()))
        {
            bool unavailable=false;try{using var ignored=await allOff.Pool.AcquireAsync("m");}catch(SchedulingFailure e){unavailable=e.Code=="no_eligible_account";}
            Check(unavailable&&allOff.Credentials.CatalogSources.Count==0,"All disabled is a valid pool with no schedulable or catalog account");
        }
        foreach(string protocol in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})foreach(bool stream in new[]{false,true})await Http(Path.Combine(scratch,Guid.NewGuid().ToString("N")),protocol,stream);
        Console.WriteLine(JsonSerializer.Serialize(new{accountChecks=checks,realNetworkCalls=0,modelCreditsUsed=0}));
    }
    sealed class Handler:HttpMessageHandler
    {
        internal readonly List<string> Users=[];
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken ct)
        {
            Users.Add(request.Headers.GetValues("X-User-Id").Single());
            if(Users.Count==1)return Task.FromResult(new HttpResponseMessage(HttpStatusCode.Forbidden){Content=new StringContent("{\"code\":11140,\"msg\":\"synthetic restriction\"}")});
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK){Content=new StringContent("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":2}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")});
        }
    }
    static async Task Http(string directory,string protocol,bool stream)
    {
        Directory.CreateDirectory(directory);Create(directory,"a","first-user",AccountSite.Cn);Create(directory,"b","second-user",AccountSite.Intl);
        string manifest=Write(directory,new{version=1,accounts=new[]{new{id="a",directory="a"},new{id="b",directory="b"}}});
        using(var runtime=StandaloneAccounts.OpenManifest(manifest,_=>new Control()))
        {
            using var keys=PreviewKeyDirectory.Open(Path.Combine(directory,"keys"));using var ledger=CreditLedger.Open(Path.Combine(directory,"keys"),[]);keys.AttachAccounting(ledger);var issued=keys.Create(new("synthetic",QuotaTotal:100));
            var handler=new Handler();await using var app=LocalGateway.Build(new(runtime.Upstream,Program.TestKey,Security:new(keys,new("synthetic-admin"),ledger),Accounts:runtime.Pool,Credentials:runtime.Credentials,DirectUpstream:true,UpstreamHandler:handler){RestrictedReplayDelay=TimeSpan.FromMilliseconds(20)});await app.StartAsync();
            using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app))};client.DefaultRequestHeaders.Authorization=new AuthenticationHeaderValue("Bearer",issued.TakeSecret());
            object body=protocol=="/v1/responses"?new{model="m",input="hello",stream}:(object)new{model="m",messages=new[]{new{role="user",content="hello"}},stream};
            using var response=await client.PostAsJsonAsync(protocol,body);string text=await response.Content.ReadAsStringAsync();
            Check(response.IsSuccessStatusCode&&text.Contains("ok",StringComparison.Ordinal)&&handler.Users.Count==2&&handler.Users[0]!=handler.Users[1],"Manifest accounts support pre-output restriction fallback for all protocols/modes");
            Check(runtime.Credentials.Status.Count(s=>s.Disabled)==1&&ledger.GetUsage(issued.Item.Id).Total==2&&ledger.Health.PendingAttempts==0,"Only restricted account disabled; exactly one successful settlement");
        }
        using(var reopened=StandaloneAccounts.OpenManifest(manifest,_=>new Control()))Check(reopened.Credentials.Status.Count(s=>s.Disabled)==1,"HTTP restriction persisted across manifest restart");
    }
}
