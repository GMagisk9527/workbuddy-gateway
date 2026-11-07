using System.Diagnostics;
using System.Net;
using System.Net.Http.Json;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class RestrictedReplayTests
{
    sealed class Control:IAccountControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken token)=>throw new InvalidOperationException();
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken token)=>Task.FromResult(CredentialVerification.Usable);
    }
    sealed class Backend:HttpMessageHandler
    {
        public int Calls;public readonly TaskCompletionSource First=new(TaskCreationOptions.RunContinuationsAsynchronously);
        public long FirstAt,SecondAt;public bool RejectSecond;
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken token)
        {
            int call=Interlocked.Increment(ref Calls);
            if(call==1){FirstAt=Stopwatch.GetTimestamp();First.TrySetResult();}
            else SecondAt=Stopwatch.GetTimestamp();
            return Task.FromResult(call==1||RejectSecond?new HttpResponseMessage(HttpStatusCode.Forbidden){Content=new StringContent("{\"code\":11140}",Encoding.UTF8,"application/json")}:new HttpResponseMessage(HttpStatusCode.OK){Content=new StringContent("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":1}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")});
        }
    }
    internal static async Task<int> Run(string root)
    {
        int checks=0;void Check(bool ok,string reason){if(!ok)throw new InvalidOperationException(reason);checks++;}
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})foreach(bool stream in new[]{false,true})foreach(string mode in new[]{"success","twice","cancel","budget"})
        {
            string dir=Path.Combine(root,Guid.NewGuid().ToString("N"));Directory.CreateDirectory(dir);
            var files=new List<CredentialFiles>();
            try
            {
                var endpoint=SiteEndpoints.Model(AccountSite.Cn);var ids=new[]{new AccountId("a"),new AccountId("b"),new AccountId("c")};
                foreach(var id in ids)files.Add(CredentialFiles.Create(Path.Combine(dir,id.Value),new(new(id.Value),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.AddHours(2).ToUnixTimeSeconds())));
                var pool=new AccountPool(ids.Select(id=>new AccountSnapshot(id,endpoint)));var credentials=new CredentialManager(pool,ids.Select((id,i)=>new CredentialRegistration(id,endpoint,files[i],new Control())));
                using var keys=PreviewKeyDirectory.Open(Path.Combine(dir,"keys"));using var ledger=CreditLedger.Open(Path.Combine(dir,"keys"),[]);keys.AttachAccounting(ledger);var issued=keys.Create(new("synthetic",QuotaTotal:100));
                var backend=new Backend{RejectSecond=mode=="twice"};var options=new LocalGatewayOptions(endpoint,new byte[32],Security:new(keys,new("admin"),ledger),Accounts:pool,Credentials:credentials,DirectUpstream:true,UpstreamHandler:backend);
                // Cancellation verifies the real default without actually waiting two minutes.
                if(mode!="cancel")options=options with{RestrictedReplayDelay=TimeSpan.FromMilliseconds(300)};
                else Check(options.RestrictedReplayDelay==TimeSpan.FromMinutes(2),"Real server default cooldown is two minutes");
                await using var app=LocalGateway.Build(options);await app.StartAsync();
                if(mode=="budget")app.Services.GetRequiredService<RetryAdmission>().Configure(new(MaxExtraAttempts:0));
                using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false,MaxResponseDrainSize=0}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(5)};
                using var cancel=new CancellationTokenSource();using var request=new HttpRequestMessage(HttpMethod.Post,path);
                request.Headers.Authorization=new("Bearer",issued.TakeSecret());
                request.Content=JsonContent.Create(path.EndsWith("responses",StringComparison.Ordinal)?(object)new{model="m",input="hello",stream}:new{model="m",messages=new[]{new{role="user",content="hello"}},stream});
                var pending=client.SendAsync(request,HttpCompletionOption.ResponseHeadersRead,cancel.Token);await backend.First.Task.WaitAsync(TimeSpan.FromSeconds(3));
                for(int i=0;i<100&&(pool.Status.Active!=0||!credentials.Status.Single(s=>s.Id==ids[0]).Disabled);i++)await Task.Delay(2);
                Check(pool.Status.Active==0&&files[0].Load().Disabled&&ledger.Health.PendingAttempts==0&&backend.Calls==1,"Cooldown persists restriction and releases account/WAL attempt before waiting");
                if(mode=="cancel")
                {
                    HttpResponseMessage? response=null;
                    if(stream){response=await pending;Check(response.StatusCode==HttpStatusCode.OK&&response.Content.Headers.ContentType?.MediaType=="text/event-stream","Cooldown starts streaming transport before any model output");}
                    cancel.Cancel();response?.Dispose();try{using var ignored=await pending;}catch(OperationCanceledException){}
                    for(int i=0;i<200&&app.Services.GetRequiredService<RequestAdmission>().Status.Active!=0;i++)await Task.Delay(10);
                    Check(backend.Calls==1&&pool.Status.Active==0&&ledger.Health.PendingAttempts==0&&app.Services.GetRequiredService<RequestAdmission>().Status.Active==0,$"Disconnected request releases admission: {path}, stream={stream}, calls={backend.Calls}, active={pool.Status.Active}, admission={app.Services.GetRequiredService<RequestAdmission>().Status.Active}");
                }
                else if(mode=="budget")
                {
                    using var response=await pending;string content=await response.Content.ReadAsStringAsync();
                    Check(content.Contains("retry_budget_exhausted")&&!content.Contains("[DONE]")&&!content.Contains("response.completed")&&!content.Contains("message_stop"),"Exhausted retry budget returns honest protocol failure");
                    Check(backend.Calls==1&&ledger.Health.PendingAttempts==0&&pool.Status.Active==0&&!files[1].Load().Disabled,"Disabled retries never send second request or affect another account");
                }
                else
                {
                    using var response=await pending;string content=await response.Content.ReadAsStringAsync();
                    Check(backend.Calls==2&&Stopwatch.GetElapsedTime(backend.FirstAt,backend.SecondAt)>=TimeSpan.FromMilliseconds(280),"No second call before cooldown, and no third account replay");
                    Check(stream?content.Contains(": keepalive"):!content.Contains(": keepalive"),"Only streaming clients receive cooldown comments");
                    if(mode=="twice")Check(content.Contains("11140")&&!content.Contains("[DONE]")&&!content.Contains("response.completed")&&!content.Contains("message_stop"),"Second rejection is an honest failure, never false success");
                    else Check(content.Contains("ok")&&ledger.GetUsage(issued.Item.Id).Total==1,"Successful fallback accounts for exactly its actual usage");
                    Check(!files[2].Load().Disabled&&ledger.Health.PendingAttempts==0&&pool.Status.Active==0,"Third account and ledger remain untouched");
                }
            }
            finally{foreach(var file in files)file.Dispose();}
        }
        return checks;
    }
}
