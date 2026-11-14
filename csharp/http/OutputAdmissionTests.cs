using System.Net;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;
using StreamingMigration;

namespace HttpMigration;

internal static class OutputAdmissionTests
{
    static int checks;static void Check(bool ok,string reason){if(!ok)throw new InvalidOperationException(reason);checks++;}
    static void Reject(Action action){try{action();throw new InvalidOperationException("Output budget accepted overflow");}catch(ConversionFailure e){Check(e.Code=="output_budget_exceeded","Stable budget rejection code");}}
    internal static async Task<int> Run(string root)
    {
        checks=0;var sample=MemorySample.Empty;var gate=new OutputAdmission(()=>sample);
        Check(gate.Status.Request==16L<<20&&gate.Status.Shared==32L<<20,"Go default retained-output budgets");
        sample=new(new(0,32L<<20),[new(0,16L<<20)]);Check(gate.Status.Request==2L<<20&&gate.Status.Shared==4L<<20,"Output budget respects runtime and ancestor cgroup limits");
        gate.Configure(new(4096,4096));using var first=gate.Acquire();using var second=gate.Acquire();first.Add(3000);Reject(()=>second.Add(1500));
        Check(gate.Status.Used<=4096&&gate.Status.Rejected==1,"Concurrent responses share bounded output reservations");first.Dispose();second.Add(1500);
        second.Scratch(500);long reserved=gate.Status.Used;second.Scratch(400);second.Remove(1500);Check(gate.Status.Used==reserved,"Scratch only grows and blocks remain reserved until response ends");
        gate.Configure(new(512,4096));second.Add(1000);using var fresh=gate.Acquire();Reject(()=>fresh.Add(513));
        Check(gate.Status.Request==512,"Hot shrink affects new request budgets without terminating old lease");second.Dispose();fresh.Dispose();Check(gate.Status.Used==0,"Output leases idempotently release all global reservations");
        using(var settings=new ConsoleSettings(new())){settings.AttachOutput(gate);settings.Update("""{"output":{"maxRequestBytes":2048,"maxSharedBytes":8192}}""",settings.View().Revision);Check(gate.Options==new OutputOptions(2048,8192),"Output budget hot reload");}
        await Reservations();
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})foreach(bool stream in new[]{false,true})foreach(bool shared in new[]{false,true})
            await Http(Path.Combine(root,Guid.NewGuid().ToString("N")),path,stream,shared);
        return checks;
    }
    static async Task Reservations()
    {
        int samples=0;var gate=new OutputAdmission(()=>{Interlocked.Increment(ref samples);return MemorySample.Empty;});
        gate.Configure(new(65536,131072));
        using(var lease=gate.Acquire())
        {
            for(int i=0;i<4096;i++)lease.Add(1);
            Check(samples==1,"Small appends only sample/refill once per reserved block");
            lease.Scratch(512);lease.Remove(4096);lease.Add(3584);
            Check(samples==2,"Scratch and remove reuse the existing reservation");
            // An in-flight request keeps its old per-request limit and can use
            // already reserved shared blocks after a hot shrink, as before.
            gate.Configure(new(64,128));lease.Add(1);
            Reject(()=>lease.Add(8192));
            using var fresh=gate.Acquire();Reject(()=>fresh.Add(65));Reject(()=>fresh.Add(1));
            Check(gate.Status.Used==8192,"Shrinking does not revoke an existing block or admit a new request");
        }
        Check(gate.Status.Used==0&&gate.Status.Rejected==3,"Refill rejection and release remain exact after hot shrink");
        gate.Configure(new(4096,16384));
        var leases=Enumerable.Range(0,32).Select(_=>gate.Acquire()).ToArray();
        int admitted=0,denied=0;
        await Task.WhenAll(leases.Select(lease=>Task.Run(()=>
        {
            try{lease.Add(4096);Interlocked.Increment(ref admitted);}
            catch(ConversionFailure e)when(e.Code=="output_budget_exceeded"){Interlocked.Increment(ref denied);}
        })));
        Check(admitted==4&&denied==28&&gate.Status.Used==16384&&gate.Status.Rejected==31,"Concurrent refills never over-admit the shared budget");
        foreach(var lease in leases)lease.Dispose();
        Check(gate.Status.Used==0,"Concurrent leases release all reservations");
        gate.Configure(new(1<<20,1<<22));
        for(int round=0;round<24;round++)
        {
            var lease=gate.Acquire();lease.Scratch(16);
            var workers=Enumerable.Range(0,4).Select(_=>Task.Run(()=>
            {
                for(int i=0;i<256;i++)
                {
                    try{lease.Add(8);lease.Scratch(32);lease.Remove(8);}
                    catch(ObjectDisposedException){break;}
                }
            })).Append(Task.Run(()=>{lease.Dispose();lease.Dispose();}));
            await Task.WhenAll(workers).WaitAsync(TimeSpan.FromSeconds(5));lease.Dispose();
        }
        Check(gate.Status.Used==0,"Concurrent scratch/remove/cancellation disposal neither leaks nor deadlocks");
    }
    sealed class Control:IAccountControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken token)=>throw new InvalidOperationException();
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken token)=>Task.FromResult(CredentialVerification.Usable);
    }
    sealed class Backend:HttpMessageHandler
    {
        public int Calls;
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken token)
        {
            Calls++;string delta=JsonSerializer.Serialize(new{choices=new[]{new{delta=new{tool_calls=new[]{new{index=0,id="call_synthetic",type="function",function=new{name="synthetic",arguments="{\"payload\":\""+new string('x',4096)+"\"}"}}}},finish_reason="tool_calls"}},usage=new{credit=2}});
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK){Content=new StringContent("data: "+delta+"\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")});
        }
    }
    static async Task Http(string root,string path,bool stream,bool shared)
    {
        Directory.CreateDirectory(root);var endpoint=SiteEndpoints.Model(AccountSite.Cn);var id=new AccountId("a");
        using var files=CredentialFiles.Create(Path.Combine(root,"credential"),new(new("synthetic-user"),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.AddHours(2).ToUnixTimeSeconds()));
        var pool=new AccountPool([new AccountSnapshot(id,endpoint)]);var credentials=new CredentialManager(pool,[new(id,endpoint,files,new Control())]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(root,"keys"));using var ledger=CreditLedger.Open(Path.Combine(root,"keys"),[]);keys.AttachAccounting(ledger);var key=keys.Create(new("synthetic",QuotaTotal:100));
        var backend=new Backend();await using var app=LocalGateway.Build(new(endpoint,new byte[32],Security:new(keys,new("admin"),ledger),Accounts:pool,Credentials:credentials,DirectUpstream:true,UpstreamHandler:backend));await app.StartAsync();
        var gate=app.Services.GetRequiredService<OutputAdmission>();gate.Configure(shared?new(65536,512):new(512,65536));
        using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(4)};using var request=new HttpRequestMessage(HttpMethod.Post,path);request.Headers.Authorization=new("Bearer",key.TakeSecret());
        request.Content=new StringContent(JsonSerializer.Serialize(path.EndsWith("responses",StringComparison.Ordinal)?(object)new{model="m",input="synthetic",stream}:new{model="m",messages=new[]{new{role="user",content="synthetic"}},stream}),Encoding.UTF8,"application/json");
        using var response=await client.SendAsync(request);string body=await response.Content.ReadAsStringAsync();
        Check(body.Contains("output_budget_exceeded")&&!body.Contains("[DONE]")&&!body.Contains("response.completed")&&!body.Contains("message_stop"),$"Over-budget output fails honestly: {path} stream={stream} shared={shared}");
        Check(stream||response.StatusCode==HttpStatusCode.BadGateway,"Aggregated overflow returns 502");
        for(int i=0;i<100&&(app.Services.GetRequiredService<RequestAdmission>().Status.Active>0||gate.Status.Used>0);i++)await Task.Delay(5);
        Check(backend.Calls==1&&gate.Status.Used==0&&pool.Status.Active==0&&ledger.Health.PendingAttempts==0&&ledger.GetUsage(key.Item.Id).Total==2,"Output failure releases budgets and retains actual upstream usage exactly once");
    }
}
