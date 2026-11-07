using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class FailoverTests
{
    static void Check(bool value,string text) { if(!value) throw new InvalidOperationException(text); }
    sealed class Fake(string mode) : HttpMessageHandler
    {
        internal List<string> Calls=[];
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage r,CancellationToken cancellation)
        {
            string uid=r.Headers.GetValues("X-User-Id").Single(); Calls.Add(uid);
            using var request=JsonDocument.Parse(r.Content!.ReadAsByteArrayAsync(cancellation).GetAwaiter().GetResult());
            Check(r.RequestUri==SiteEndpoints.Model(uid=="first"?AccountSite.Cn:AccountSite.Intl) && request.RootElement.GetProperty("max_tokens").GetInt32()==(uid=="first"?64:128),"Recompute default output for selected site");
            if(uid=="next" && mode!="all") return Task.FromResult(Sse("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":2}}\n\ndata: [DONE]\n\n"));
            if(mode is "http" or "all" or "noeligible" or "11128" or "400") return Task.FromResult(new HttpResponseMessage(mode is "11128" or "400"?HttpStatusCode.BadRequest:HttpStatusCode.Forbidden){Content=new StringContent("{\"code\":"+(mode=="11128"?"11128":"11140")+"}",Encoding.UTF8,"application/json")});
            if(mode is "11128usage" or "usageframe") return Task.FromResult(Sse("data: {\"code\":"+(mode=="11128usage"?"11128":"11140")+",\"usage\":{\"credit\":3}}\n\n"));
            string head=mode is "usage" or "quota"?"data: {\"usage\":{\"credit\":3}}\n\n":mode=="partial"?"data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}],\"usage\":{\"credit\":3}}\n\n":mode=="toolpartial"?"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call\",\"function\":{\"name\":\"run\",\"arguments\":\"{}\"}}]}}],\"usage\":{\"credit\":3}}\n\n":"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n";
            return Task.FromResult(Sse(head+"data: {\"code\":11140}\n\n"));
        }
        static HttpResponseMessage Sse(string text)=>new(HttpStatusCode.OK){Content=new StringContent(text,Encoding.UTF8,"text/event-stream")};
    }
    internal static async Task Run(string scratch)
    {
        Directory.CreateDirectory(scratch); Check(!Directory.EnumerateFileSystemEntries(scratch).Any(),"Empty scratch required"); int checks=0;
        using(var cancellation=new CancellationTokenSource())
        using(var pending=new PendingStream())
        {
            var observed=new TaskCompletionSource<string>(TaskCreationOptions.RunContinuationsAsynchronously);
            var probe=RestrictionProbe.Run(pending,cancellation.Token,raw=>observed.TrySetResult(raw));
            string usage=await observed.Task.WaitAsync(TimeSpan.FromSeconds(5)); cancellation.Cancel();
            try { await probe; throw new InvalidOperationException("Expected probe cancellation"); } catch(OperationCanceledException) { }
            Check(usage.Contains("3"),"Preserve observed usage when client cancels during probe");
        }
        Check(RestrictedReplay.DefaultDelay==TimeSpan.FromMinutes(2),"Production restriction cooldown is two minutes");
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"}) foreach(bool stream in new[]{false,true}) foreach(string mode in new[]{"http","sse","partial","toolpartial","11128","all","usage","quota","noeligible","11128usage","usageframe","400"})
        {
            string root=Path.Combine(scratch,Guid.NewGuid().ToString("N"));
            var endpoint=SiteEndpoints.Model(AccountSite.Cn); var a=new AccountId("first");var b=new AccountId("next");
            using var fa=CredentialFiles.Create(Path.Combine(root,"first"),new(new("first"),"synthetic-access","refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+7200));
            var nextEndpoint=SiteEndpoints.Model(AccountSite.Intl);
            using var fb=CredentialFiles.Create(Path.Combine(root,"next"),new(new("next",Site:AccountSite.Intl),"synthetic-access","refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+7200));
            if(mode=="noeligible") fb.Save(fb.Load() with {Disabled=true});
            var pool=new AccountPool([new(a,endpoint),new(b,nextEndpoint,AccountSite.Intl)]);
            using var control=new WorkBuddyControlPlane(AccountSite.Cn,new Fake(mode));
            using var intlControl=new WorkBuddyControlPlane(AccountSite.Intl,new Fake(mode));
            var credentials=new CredentialManager(pool,[new(a,endpoint,fa,control),new(b,nextEndpoint,fb,intlControl)]);
            using var keys=PreviewKeyDirectory.Open(Path.Combine(root,"keys")); using var ledger=CreditLedger.Open(Path.Combine(root,"keys"),keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);
            var issued=keys.Create(new("synthetic",QuotaTotal:mode=="quota"?3:100)); var fake=new Fake(mode);
            var catalog=new ModelCatalog();catalog.Load("{\"schema\":5,\"catalogs\":{\"cn\":[{\"id\":\"test-model\",\"maxOutputTokens\":64}],\"intl\":[{\"id\":\"test-model\",\"maxOutputTokens\":128}]}}"u8.ToArray());
            await using var app=LocalGateway.Build(new(endpoint,new byte[32],Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:credentials,DirectUpstream:true,UpstreamHandler:fake,Catalog:catalog){RestrictedReplayDelay=TimeSpan.FromMilliseconds(20)});
            await app.StartAsync(); using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(10)};client.DefaultRequestHeaders.Authorization=new AuthenticationHeaderValue("Bearer",issued.TakeSecret());
            object body=path=="/v1/responses"?new{model="test-model",input="hello",stream}:(object)new{model="test-model",messages=new[]{new{role="user",content="hello"}},stream};
            using var response=await client.PostAsJsonAsync(path,body); string text=await response.Content.ReadAsStringAsync();
            bool fallback=mode=="http";
            if(fallback) Check(response.IsSuccessStatusCode && (text.Contains("\"text\":\"ok\"") || text.Contains("\"content\":\"ok\"") || text.Contains("\"delta\":\"ok\"")) && fake.Calls.SequenceEqual(new[]{"first","next"}),"Recover on next account");
            else Check(fake.Calls.Count==(mode=="all"?2:1),"Never replay partial output or unrelated rejection");
            if(mode=="quota") Check((int)response.StatusCode==400,"In-stream restriction is returned without replay even with exhausted key quota");
            if(mode is "all" or "noeligible" or "11128" or "11128usage") Check(text.Contains(mode.StartsWith("11128",StringComparison.Ordinal)?"11128":"11140"),"Keep final business rejection");
            if(stream && mode is "partial" or "toolpartial") Check(!text.Contains("message_stop") && !text.Contains("[DONE]") && !text.Contains("response.completed"),"No false completion after partial rejection");
            double expected=mode is "usage" or "usageframe" or "partial" or "toolpartial" or "quota" or "11128usage"?3:fallback?2:0;
            Check(ledger.GetUsage(issued.Item.Id).Total==expected && ledger.Health.PendingAttempts==0 && pool.Status.Active==0,"Exactly settle each attempt and release all leases");
            Check(fa.Load().Disabled==(!mode.StartsWith("11128",StringComparison.Ordinal)),"Only actual restricted account is disabled");
            Check(fb.Load().Disabled==(mode is "all" or "noeligible"),"Healthy fallback is preserved"); checks++;
        }
        int cooldownChecks=await RestrictedReplayTests.Run(Path.Combine(scratch,"cooldown"));
        Console.WriteLine(JsonSerializer.Serialize(new{failoverChecks=checks,cooldownChecks,probeCancellationChecks=1,realNetworkCalls=0}));
    }
    sealed class PendingStream() : MemoryStream(Encoding.UTF8.GetBytes("data: {\"usage\":{\"credit\":3}}\n\n"))
    {
        public override ValueTask<int> ReadAsync(Memory<byte> buffer,CancellationToken cancellationToken=default)=>Position<Length?base.ReadAsync(buffer,cancellationToken):new(Wait(cancellationToken));
        static async Task<int> Wait(CancellationToken cancellation) { await Task.Delay(Timeout.InfiniteTimeSpan,cancellation); return 0; }
    }
}
