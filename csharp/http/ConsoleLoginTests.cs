using System.Net;
using System.Text.Json;
using AccountLogin;
using AccountMaintenance;
using AccountScheduling;
namespace HttpMigration;

internal static class ConsoleLoginTests
{
    static int checks;
    static void Check(bool condition,string reason){if(!condition)throw new InvalidOperationException(reason);checks++;}
    sealed class Transport(AccountSite site,string uid):HttpMessageHandler
    {
        public int Calls;public bool Redirect,ForeignLink,Pending;
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken token)
        {
            Calls++;Check(request.RequestUri!.Authority==LoginOrigins.Control(site).Authority,"Login requests stay on exact control host");
            Check(!request.Headers.Contains("X-Client-ID")&&!request.Headers.Contains("X-Client-Version")&&!request.Headers.Contains("X-Device-ID")&&request.Headers.UserAgent.ToString()=="WorkBuddy-Gateway-CSharp/0.1","Login does not assert official client or device identity");
            if(Redirect)return Task.FromResult(new HttpResponseMessage(HttpStatusCode.Redirect){Headers={Location=new("https://untrusted.invalid/")}});
            string path=request.RequestUri.AbsolutePath;object data;
            if(path.EndsWith("/state"))
            {Check(request.Method==HttpMethod.Post&&request.Headers.Authorization is null,"Challenge has no account credentials");data=new{state="synthetic & state",authUrl=ForeignLink?"https://untrusted.invalid/login":new Uri(LoginOrigins.Control(site),"login").ToString()};}
            else if(path.EndsWith("/token"))
            {Check(request.Headers.Authorization is null&&request.RequestUri.Query.Contains("%26"),"Token polling encodes state and has no existing account token");data=Pending?(object)new{}:new{accessToken="synthetic-login-token",refreshToken="synthetic-login-refresh",expiresIn=3600};}
            else
            {Check(path.EndsWith("/account")&&request.Headers.Authorization?.Parameter=="synthetic-login-token","Only account lookup uses freshly authorized token");data=new{uid,nickname="synthetic account"};}
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK){Content=new StringContent(JsonSerializer.Serialize(new{code=0,data}))});
        }
    }
    sealed class Control:IAccountControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential credential,CancellationToken token)=>throw new InvalidOperationException();
        public Task<CredentialVerification> VerifyAsync(AccountCredential credential,CancellationToken token)=>Task.FromResult(CredentialVerification.Usable);
    }
    internal static async Task Run(string path)
    {
        Directory.CreateDirectory(path);string manifest=Path.Combine(path,"accounts.json");File.WriteAllText(manifest,"{\"version\":1,\"accounts\":[]}");
        using var owner=StandaloneAccounts.OpenManifest(manifest,_=>new Control());
        foreach(var site in new[]{AccountSite.Cn,AccountSite.Intl})
        {
            var transport=new Transport(site,"synthetic-"+site);await using var service=new ConsoleLoginService(owner,new Dictionary<string,string>(),s=>LoopbackLoginFlow.ForWorkBuddy(s,transport),new(PollInterval:TimeSpan.FromMilliseconds(2)));
            string name=service.SuggestedFile;var pending=await service.Start(name,site,CancellationToken.None);
            LoginSnapshot? snapshot=null;
            for(int i=0;i<1000;i++){snapshot=service.Find(pending.Id);if(snapshot?.Status is not("pending" or "saving"))break;await Task.Delay(5);}
            Check(snapshot?.Status=="done"&&owner.Pool.Snapshot(new(name)) is not null,"Authorized account is persisted and enters running pool");
            Check(transport.Calls==3,"Exactly challenge, token and account requests, no model invocation");
            Check(!JsonSerializer.Serialize(snapshot).Contains("synthetic-login-token"),"Management result never exposes tokens");
        }
        foreach(bool redirect in new[]{false,true})
        {
            var transport=new Transport(AccountSite.Cn,"unused"){Redirect=redirect,ForeignLink=!redirect};
            await using var service=new ConsoleLoginService(owner,new Dictionary<string,string>(),s=>LoopbackLoginFlow.ForWorkBuddy(s,transport));bool rejected=false;
            try{await service.Start(service.SuggestedFile,AccountSite.Cn,CancellationToken.None);}catch(LoginFailure){rejected=true;}
            Check(rejected&&transport.Calls==1&&owner.Pool.Count==2,"Redirect and unapproved authorization link cannot begin polling");
        }
        var wait=new Transport(AccountSite.Cn,"unused"){Pending=true};
        await using(var service=new ConsoleLoginService(owner,new Dictionary<string,string>(),s=>LoopbackLoginFlow.ForWorkBuddy(s,wait)))
        {var pending=await service.Start(service.SuggestedFile,AccountSite.Cn,CancellationToken.None);Check(service.Cancel(pending.Id)&&service.Find(pending.Id)?.Status=="cancelled"&&owner.Pool.Count==2,"Cancellation does not create an account");}
        bool invalid=false;try{owner.LoginDestination("../workbuddy.json");}catch(LoginFailure){invalid=true;}Check(invalid,"Login labels cannot select arbitrary filesystem paths");
        string existing=owner.Credentials.ManagementStatus.Single(s=>s.Site==AccountSite.Cn).Id;
        await owner.Credentials.DisableAsync(new(existing));
        var renewalTransport=new Transport(AccountSite.Cn,"synthetic-Cn");
        Action shuttingDown=()=>throw new ObjectDisposedException("synthetic-observer");owner.Changed+=shuttingDown;
        await using(var renewal=new ConsoleLoginService(owner,new Dictionary<string,string>{{existing,"workbuddy-original.json"}},s=>LoopbackLoginFlow.ForWorkBuddy(s,renewalTransport),new(PollInterval:TimeSpan.FromMilliseconds(2))))
        {
            var start=await renewal.Start("workbuddy-original.json",AccountSite.Cn,CancellationToken.None);LoginSnapshot? done=null;
            for(int i=0;i<1000;i++){done=renewal.Find(start.Id);if(done?.Status is not("pending" or "saving"))break;await Task.Delay(5);}
            Check(done?.Status=="done"&&owner.Pool.Count==2&&!owner.Credentials.Status.Single(s=>s.Id.Value==existing).Disabled,"Explicit successful re-login replaces existing migrated alias and clears old restriction");
            Check(renewalTransport.Calls==3,"Re-login only performs normal authorization flow, no model or extra verification calls");
        }
        owner.Changed-=shuttingDown;
        var interrupted=owner.LoginDestination(existing);owner.Credentials.Block(new(existing));
        bool conflict=false;
        try{interrupted.Commit(new(new("synthetic-Cn"),"fresh-authorized-access","fresh-authorized-refresh",DateTimeOffset.UtcNow.AddHours(2).ToUnixTimeSeconds()));}
        catch(CredentialFailure e){conflict=e.Code=="credential_revision_conflict";}
        Check(conflict&&owner.Pool.Snapshot(new(existing))!.Disabled,"Restriction observed during pending login wins over later login commit");
        await owner.Credentials.DisableAsync(new(existing));
        var duplicate=owner.LoginDestination(existing);bool duplicateRejected=false;
        try{duplicate.Commit(new(new("synthetic-Intl",Site:AccountSite.Intl),"duplicate-access","duplicate-refresh",DateTimeOffset.UtcNow.AddHours(2).ToUnixTimeSeconds()));}
        catch(CredentialFailure e){duplicateRejected=e.Code=="credential_identity_duplicate";}
        Check(duplicateRejected&&owner.Pool.Count==2&&owner.Pool.Snapshot(new(existing))!.Site==AccountSite.Cn,"Re-login cannot duplicate another loaded identity or change site on failure");
        owner.LoginDestination("workbuddy-delete-test.json").Commit(new(new("delete-uid"),"delete-access","delete-refresh",DateTimeOffset.UtcNow.AddHours(2).ToUnixTimeSeconds()));
        var deleted=owner.LoginDestination("workbuddy-delete-test.json");await owner.DeleteAsync("workbuddy-delete-test.json");bool missing=false;
        try{deleted.Commit(new(new("delete-uid"),"late-access","late-refresh",DateTimeOffset.UtcNow.AddHours(2).ToUnixTimeSeconds()));}catch(KeyNotFoundException){missing=true;}
        Check(missing&&owner.Pool.Count==2,"Deleted login target is not recreated by a late authorization callback");
        owner.Dispose();
        using(var reopened=StandaloneAccounts.OpenManifest(manifest,_=>new Control()))Check(reopened.Pool.Count==2,"Login additions survive manifest reload");
        Console.WriteLine(JsonSerializer.Serialize(new{consoleLoginChecks=checks,realNetworkCalls=0,modelCreditsUsed=0}));
    }
}
