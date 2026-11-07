using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;
namespace HttpMigration;
internal static class AccountManagementTests
{
    static int checks;
    static void Check(bool b,string why){if(!b)throw new InvalidOperationException(why);checks++;}
    sealed class Store(AccountCredential credential):ICredentialPersistence
    {
        internal PersistedCredential Saved=new(credential,true);internal Action<PersistedCredential>? Saving;internal int Writes;
        public PersistedCredential Load()=>Saved;
        public void Save(PersistedCredential next){Saving?.Invoke(next);Saved=next;Writes++;}
        public void Dispose(){ }
    }
    sealed class Control:IAccountControlPlane
    {
        internal CredentialVerification Verdict=CredentialVerification.Usable;internal Func<CancellationToken,Task>? Verifying;
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken ct)=>throw new InvalidOperationException("Unexpected refresh");
        public async Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken ct){if(Verifying is not null)await Verifying(ct);return Verdict;}
    }
    static AccountCredential Credential(long? expiry=null)=>new(new("uid-secret","enterprise-secret"),"synthetic-access","synthetic-refresh",expiry??DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600);
    static async Task Expect(Func<Task> action,string code)
    {bool match=false;try{await action();}catch(CredentialFailure e){match=e.Code==code;}Check(match,"Expected "+code);}
    static async Task Unavailable(AccountPool pool)
    {bool rejected=false;try{using var ignored=await pool.AcquireAsync("m");}catch(SchedulingFailure e){rejected=e.Code=="no_eligible_account";}Check(rejected,"Disabled account excluded from selection");}
    internal static async Task Run(string scratch)
    {
        checks=0;var id=new AccountId("a");var endpoint=SiteEndpoints.Model(AccountSite.Cn);
        foreach(var verdict in new[]{CredentialVerification.Usable,CredentialVerification.Rejected,CredentialVerification.Unknown})
        {
            var store=new Store(Credential());var control=new Control{Verdict=verdict};var pool=new AccountPool([new(id,endpoint)]);var manager=new CredentialManager(pool,[new(id,endpoint,store,control)]);
            if(verdict==CredentialVerification.Usable)
            {await manager.EnableAsync(id);using var lease=await pool.AcquireAsync("m");Check(!store.Saved.Disabled&&!manager.Status.Single().Disabled&&pool.Status.Active==1,"Enable persists then restores scheduling");}
            else {await Expect(()=>manager.EnableAsync(id),verdict==CredentialVerification.Rejected?"credential_verification_rejected":"credential_verification_unknown");Check(store.Writes==0&&store.Saved.Disabled,"Rejected or unknown verification never persists enable");await Unavailable(pool);}
        }
        {
            var store=new Store(Credential());var pool=new AccountPool([new(id,endpoint)]);var manager=new CredentialManager(pool,[new(id,endpoint,store,new Control(),true)]);
            await Expect(()=>manager.EnableAsync(id),"credential_configured_disabled");Check(store.Writes==0,"Cannot override manifest disable");
        }
        {
            var store=new Store(Credential());var pool=new AccountPool([new(id,endpoint,Disabled:true)]);var manager=new CredentialManager(pool,[new(id,endpoint,store,new Control())]);
            await Expect(()=>manager.EnableAsync(id),"credential_configured_disabled");await Unavailable(pool);
        }
        {
            var store=new Store(Credential(1));var pool=new AccountPool([new(id,endpoint)]);var manager=new CredentialManager(pool,[new(id,endpoint,store,new Control())]);
            await Expect(()=>manager.EnableAsync(id),"credential_expired");Check(store.Writes==0,"Expired access is not blindly restored");
        }
        {
            var store=new Store(Credential()){Saving=_=>throw new IOException("synthetic write failure")};var pool=new AccountPool([new(id,endpoint)]);var manager=new CredentialManager(pool,[new(id,endpoint,store,new Control())]);
            await Expect(()=>manager.EnableAsync(id),"credential_persist_failed");Check(manager.Status.Single().Degraded&&store.Saved.Disabled,"Persistence failure remains blocked/degraded");await Unavailable(pool);await Expect(()=>manager.EnableAsync(id),"credential_store_incomplete");
        }
        {
            var entered=new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);var release=new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var store=new Store(Credential());var control=new Control{Verifying=async ct=>{entered.SetResult();await release.Task.WaitAsync(ct);}};var pool=new AccountPool([new(id,endpoint)]);var manager=new CredentialManager(pool,[new(id,endpoint,store,control)]);
            Task enable=manager.EnableAsync(id);await entered.Task;manager.Block(id);release.SetResult();await Expect(()=>enable,"credential_changed_during_enable");Check(store.Writes==0,"New restriction during verification prevents enable commit");await Unavailable(pool);
        }
        {
            var store=new Store(Credential());var pool=new AccountPool([new(id,endpoint)]);var manager=new CredentialManager(pool,[new(id,endpoint,store,new Control())]);
            store.Saving=s=>{if(!s.Disabled)manager.Block(id);};await Expect(()=>manager.EnableAsync(id),"credential_changed_during_enable");Check(store.Saved.Disabled&&store.Writes==2,"Restriction during write restores durable disable");await Unavailable(pool);
        }
        {
            var store=new Store(Credential());var control=new Control{Verifying=ct=>Task.Delay(Timeout.InfiniteTimeSpan,ct)};var pool=new AccountPool([new(id,endpoint)]);var manager=new CredentialManager(pool,[new(id,endpoint,store,control)]);
            using var cancellation=new CancellationTokenSource(TimeSpan.FromMilliseconds(40));bool cancelled=false;try{await manager.EnableAsync(id,cancellation.Token);}catch(OperationCanceledException){cancelled=true;}
            Check(cancelled&&store.Writes==0&&store.Saved.Disabled,"Cancelled verification does not enable");
        }
        {
            using var cancelled=new CancellationTokenSource();var store=new Store(Credential()){Saving=_=>cancelled.Cancel()};var pool=new AccountPool([new(id,endpoint)]);var manager=new CredentialManager(pool,[new(id,endpoint,store,new Control())]);
            await manager.EnableAsync(id,cancelled.Token);Check(!store.Saved.Disabled&&!manager.Status.Single().Disabled,"Once commit starts, cancellation does not discard committed state");
        }
        foreach(var snapshot in new[]{new AccountSnapshot(id,endpoint,CooldownUntil:DateTimeOffset.UtcNow.AddMinutes(2)),new AccountSnapshot(id,endpoint,Models:new Dictionary<string,AccountModelState>{{"m",new(QuotaBlocked:true)}})})
        {
            var store=new Store(Credential());var pool=new AccountPool([snapshot]);var manager=new CredentialManager(pool,[new(id,endpoint,store,new Control())]);
            await manager.EnableAsync(id);await Unavailable(pool);Check(!store.Saved.Disabled,"Enable preserves independent admission restrictions");
        }
        await Http(scratch);Console.WriteLine(JsonSerializer.Serialize(new{accountManagementChecks=checks,realNetworkCalls=0,realModelCalls=0}));
    }
    sealed class SplitStream:Stream
    {
        internal readonly TaskCompletionSource Finish=new(TaskCreationOptions.RunContinuationsAsynchronously);int stage;
        public override async ValueTask<int> ReadAsync(Memory<byte> buffer,CancellationToken ct=default)
        {
            if(stage==2)return 0;
            if(stage==1)await Finish.Task.WaitAsync(ct);
            byte[] bytes=Encoding.UTF8.GetBytes(stage++==0?"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n":"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0}}\n\ndata: [DONE]\n\n");bytes.CopyTo(buffer);return bytes.Length;
        }
        public override bool CanRead=>true;public override bool CanSeek=>false;public override bool CanWrite=>false;public override long Length=>throw new NotSupportedException();public override long Position{get=>throw new NotSupportedException();set=>throw new NotSupportedException();}
        public override int Read(byte[] b,int o,int n)=>throw new NotSupportedException();public override void Flush(){ }public override void Write(byte[] b,int o,int n)=>throw new NotSupportedException();public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long n)=>throw new NotSupportedException();
    }
    sealed class Handler:HttpMessageHandler
    {
        internal SplitStream? Current;internal int Calls;internal bool AutoFinish;
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken ct)
        {Calls++;Current=new();if(AutoFinish)Current.Finish.SetResult();return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK){Content=new StreamContent(Current){Headers={ContentType=new("text/event-stream")}}});}
    }
    static async Task Http(string directory)
    {
        Directory.CreateDirectory(directory);using var files=CredentialFiles.Create(Path.Combine(directory,"account"),Credential());var id=new AccountId("a");var endpoint=SiteEndpoints.Model(AccountSite.Cn);var pool=new AccountPool([new(id,endpoint)]);var control=new Control();var manager=new CredentialManager(pool,[new(id,endpoint,files,control)]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(directory,"keys"));using var ledger=CreditLedger.Open(Path.Combine(directory,"keys"),[]);keys.AttachAccounting(ledger);var key=keys.Create(new("synthetic",QuotaTotal:100,MaxConcurrent:2));string token=key.TakeSecret();var handler=new Handler();
        await using var app=LocalGateway.Build(new(endpoint,Program.TestKey,Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,UpstreamHandler:handler));await app.StartAsync();
        using var admin=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(10)};admin.DefaultRequestHeaders.Authorization=new AuthenticationHeaderValue("Bearer","synthetic-admin");
        using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=admin.BaseAddress,Timeout=TimeSpan.FromSeconds(10)};client.DefaultRequestHeaders.Authorization=new AuthenticationHeaderValue("Bearer",token);
        using(var forbidden=await client.GetAsync("/admin/api/accounts"))Check((int)forbidden.StatusCode==401,"Model Key cannot manage accounts");
        using(var list=await admin.GetAsync("/admin/api/accounts")){string text=await list.Content.ReadAsStringAsync();Check(list.IsSuccessStatusCode&&!text.Contains("synthetic-access")&&!text.Contains("synthetic-refresh")&&!text.Contains("uid-secret")&&!text.Contains("enterprise-secret")&&list.Headers.CacheControl?.NoStore==true,"Admin list is redacted and not cached");}
        foreach(var command in new[]{new{action="unknown",id="a"},new{action="enable",id=""}}){using var bad=await admin.PostAsJsonAsync("/admin/api/accounts",command);Check((int)bad.StatusCode==400,"Command validation");}
        using(var missing=await admin.PostAsJsonAsync("/admin/api/accounts",new{action="disable",id="missing"}))Check((int)missing.StatusCode==404,"Missing account is explicit");
        using var request=new HttpRequestMessage(HttpMethod.Post,"/v1/messages"){Content=JsonContent.Create(new{model="m",stream=true,messages=new[]{new{role="user",content="hello"}}})};
        using var running=await client.SendAsync(request,HttpCompletionOption.ResponseHeadersRead);Check(running.IsSuccessStatusCode&&pool.Status.Active==1,"Streaming request admitted");
        using(var disable=await admin.PostAsJsonAsync("/admin/api/accounts",new{action="disable",id="a"}))Check(disable.IsSuccessStatusCode&&files.Load().Disabled&&pool.Status.Active==1,"Disable blocks future work while preserving active stream lease");
        using(var blocked=await client.PostAsJsonAsync("/v1/messages",new{model="m",messages=new[]{new{role="user",content="hi"}}}))Check((int)blocked.StatusCode==503&&handler.Calls==1,"Disabled account receives no new model request");
        handler.Current!.Finish.SetResult();string complete=await running.Content.ReadAsStringAsync();Check(complete.Contains("message_stop",StringComparison.Ordinal),"Existing stream completes after administrative disable");
        var entered=new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);var release=new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
        control.Verifying=async ct=>{entered.SetResult();await release.Task.WaitAsync(ct);};
        Task<HttpResponseMessage> enabling=admin.PostAsJsonAsync("/admin/api/accounts",new{action="enable",id="a"});await entered.Task.WaitAsync(TimeSpan.FromSeconds(5));
        using(var busy=await admin.PostAsJsonAsync("/admin/api/accounts",new{action="disable",id="a"}))Check((int)busy.StatusCode==429,"Admin mutations do not build an unbounded queue");
        using(var status=await admin.GetAsync("/admin/api/accounts"))Check(status.IsSuccessStatusCode,"Status remains readable while verification is pending");
        release.SetResult();using(var enable=await enabling)Check(enable.IsSuccessStatusCode&&!files.Load().Disabled&&!manager.Status.Single().Disabled,"Verified administrative enable persists and restores account");
        using(var unknown=await admin.PostAsJsonAsync("/admin/api/accounts",new{action="enable",id="a",token="never-accepted"}))Check((int)unknown.StatusCode==400,"Management endpoint does not accept credential fields");
        handler.AutoFinish=true;using(var restored=await client.PostAsJsonAsync("/v1/messages",new{model="m",messages=new[]{new{role="user",content="hi"}}}))Check(restored.IsSuccessStatusCode&&handler.Calls==2,"Restored account can serve a new model request");
        using(var excessive=await admin.PostAsync("/admin/api/accounts",new StringContent(new string('x',8193))))Check((int)excessive.StatusCode==413,"Admin body bound enforced");
        Check(ledger.Health.PendingAttempts==0&&ledger.GetUsage(key.Item.Id).Total==0,"Account management creates no model billing attempt");
    }
}
