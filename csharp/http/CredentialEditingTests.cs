using System.Net.Http.Json;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class CredentialEditingTests
{
    sealed class Control:IAccountControlPlane,IAccountResourcesControlPlane
    {
        public int Calls;
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken token){Calls++;throw new InvalidOperationException("No upstream calls allowed");}
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken token){Calls++;throw new InvalidOperationException("No upstream calls allowed");}
        public Task<AccountResourceObservation> FetchResourcesAsync(AccountCredential c,CancellationToken token,bool fetchPackages=true)=>Task.FromResult(new AccountResourceObservation(new(100,20,80),[],false));
    }
    internal static async Task<int> Run(string root)
    {
        int checks=0;void Check(bool value,string why){if(!value)throw new InvalidOperationException(why);checks++;}
        Directory.CreateDirectory(root);
        foreach(string id in new[]{"a","b","disabled"})
        {
            using var file=CredentialFiles.Create(Path.Combine(root,id),new(new("uid-"+id),"private-access-"+id,"private-refresh-"+id,DateTimeOffset.UtcNow.AddHours(3).ToUnixTimeSeconds()));
            if(id=="disabled")file.Save(file.Load() with{Disabled=true});
        }
        string manifest=Path.Combine(root,"accounts.json");File.WriteAllText(manifest,"""{"version":1,"accounts":[{"id":"a","directory":"a"},{"id":"b","directory":"b"},{"id":"disabled","directory":"disabled"}]}""");
        var control=new Control();using var owner=StandaloneAccounts.OpenManifest(manifest,_=>control);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(root,"keys"),new("synthetic-key"));using var ledger=CreditLedger.Open(Path.Combine(root,"keys"),keys.List().Select(x=>x.Id));keys.AttachAccounting(ledger);
        await using var app=LocalGateway.Build(new(owner.Upstream,new byte[32],Security:new(keys,new("synthetic-admin"),ledger),Accounts:owner.Pool,Credentials:owner.Credentials,DirectUpstream:true,AccountOwner:owner,ConsoleDirectory:Path.Combine(root,"keys/console")));
        await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(5)};
        async Task<JsonElement> Send(string route,object? body=null,int expected=200,string auth="synthetic-admin")
        {
            using var request=new HttpRequestMessage(body is null?HttpMethod.Get:HttpMethod.Post,"/admin/api/"+route);request.Headers.Authorization=new("Bearer",auth);
            if(body is not null)request.Content=JsonContent.Create(body);
            using var response=await client.SendAsync(request);Check((int)response.StatusCode==expected,"Credential route status "+route+" got "+(int)response.StatusCode);
            Check(response.Headers.CacheControl?.NoStore==true,"Credential response never cached");
            using var json=JsonDocument.Parse(await response.Content.ReadAsStringAsync());return json.RootElement.Clone();
        }
        string Replace(JsonElement document,string token,string? uid=null,string? edition=null)
        {
            var node=JsonNode.Parse(document.GetProperty("content").GetString()!)!;node["auth"]!["accessToken"]=token;
            if(uid is not null)node["account"]!["uid"]=uid;if(edition is not null)node["edition"]=edition;
            node["customMetadata"]=new JsonObject{["kept"]=true};return node.ToJsonString();
        }
        async Task<JsonElement> Edit(string id,JsonElement document,string content,int status=200)=>await Send("credential-edit",new{path=id,revision=document.GetProperty("revision").GetString(),content},status);
        await Send("credentials",expected:401,auth:"synthetic-key");await Send("credential-edit?file=a",expected:401,auth:"synthetic-key");
        await Send("credential-edit",new{path="a",revision="x",content="{}"},401,"synthetic-key");
        var listed=await Send("credentials");Check(listed.GetProperty("files").GetArrayLength()==3&&!listed.GetRawText().Contains("private-access")&&!listed.GetRawText().Contains("private-refresh"),"Credential list is masked including short tokens");
        await Send("credential-edit?file=..%2Fa",expected:404);
        var original=await Send("credential-edit?file=a");Check(original.GetProperty("content").GetString()!.Contains("private-access-a"),"Only explicit admin editor returns full credential");
        await owner.Credentials.FetchResourcesAsync(new("a"));owner.Pool.ObserveModelFailure(new("a"),"m",429,"6004");
        string updated=Replace(original,"private-new-access-a");await Edit("a",original,updated);
        var current=await Send("credential-edit?file=a");Check(current.GetProperty("content").GetString()==updated,"Edited JSON including unknown metadata preserved byte for byte");
        Check(owner.Credentials.ResourceStatus.Single(x=>x.Id=="a").Known&&owner.Pool.Snapshot(new("a"))!.Models!["m"].CooldownUntil>DateTimeOffset.UtcNow,"Same account edit preserves quota and model cooldown");
        await Edit("a",original,Replace(original,"must-not-write"),409);
        Check((await Send("credential-edit?file=a")).GetProperty("content").GetString()==updated,"Stale revision cannot overwrite new token");
        foreach(string invalid in new[]{Replace(current,"****"),Replace(current,"[REDACTED]"),Replace(current,""),Replace(current,"ok",edition:"unexpected"),"{\"auth\":{},\"auth\":{}}"})await Edit("a",current,invalid,409);
        await Edit("a",current,Replace(current,"private-duplicate",uid:"uid-b"),409);
        using(var held=await owner.Credentials.AcquireForHeadersAsync(new("a"),SiteEndpoints.Model(AccountSite.Cn)))
        {
            using var cancel=new CancellationTokenSource(TimeSpan.FromMilliseconds(80));bool cancelled=false;
            try{await owner.EditCredentialAsync("a",Replace(current,"cancelled-write"),current.GetProperty("revision").GetString()!,cancel.Token);}catch(OperationCanceledException){cancelled=true;}
            Check(cancelled&&File.ReadAllText(Path.Combine(root,"a/credential.json"))==updated,"Cancelled edit waits for active readers and writes nothing");
        }
        var disabled=await Send("credential-edit?file=disabled");await Edit("disabled",disabled,Replace(disabled,"private-renewed-disabled"));
        Check(owner.Credentials.Status.Single(x=>x.Id.Value=="disabled").Disabled,"Manual editing does not silently revive an upstream restriction");
        await Edit("a",current,Replace(current,"private-intl",uid:"uid-new-intl",edition:"intl"));
        Check(owner.Pool.Snapshot(new("a")) is{Site:AccountSite.Intl,QuotaExhausted:false} snapshot&&snapshot.Upstream==SiteEndpoints.Model(AccountSite.Intl)&&snapshot.Models?.Count==0,"Identity/site replacement resets quota/model state and updates upstream");
        Check(!owner.Credentials.ResourceStatus.Single(x=>x.Id=="a").Known,"New identity never inherits old credit package observation");
        var afterIdentity=await Send("credential-edit?file=a");
        string external=Replace(afterIdentity,"externally-rotated-token");File.WriteAllText(Path.Combine(root,"a/credential.json"),external);
        await Edit("a",afterIdentity,Replace(afterIdentity,"would-overwrite-external"),409);
        Check(File.ReadAllText(Path.Combine(root,"a/credential.json"))==external,"External credential rotation participates in CAS");
        var externalDisabled=await Send("credential-edit?file=b");File.WriteAllText(Path.Combine(root,"b/state.json"),"{\"version\":1,\"disabled\":true}");
        await Edit("b",externalDisabled,Replace(externalDisabled,"private-new-b"));
        Check(owner.Credentials.Status.Single(s=>s.Id.Value=="b").Disabled,"Manual editing preserves a newer disabled marker not yet picked up by scanner");
        Check(control.Calls==0&&ledger.Health.Healthy&&ledger.Health.PendingAttempts==0,"Admin editing uses no upstream calls and leaves ledger healthy");
        var console=app.Services.GetRequiredService<ConsoleRuntime>();Check(!string.Join("\n",console.Telemetry.Logs(200)).Contains("private-"),"Credential content excluded from audit logs");
        await app.StopAsync();await app.DisposeAsync();owner.Dispose();
        using(var reopened=StandaloneAccounts.OpenManifest(manifest,_=>control))Check(reopened.Pool.Snapshot(new("a"))!.Site==AccountSite.Intl&&reopened.Credentials.Status.Single(x=>x.Id.Value=="disabled").Disabled,"Edited site and restriction survive restart");
        return checks;
    }
}
