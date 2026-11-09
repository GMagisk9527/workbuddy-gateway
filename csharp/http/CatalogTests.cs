using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class CatalogTests
{
    static void Check(bool b,string reason){if(!b)throw new InvalidOperationException(reason);}
    static byte[] Snapshot()=>Encoding.UTF8.GetBytes("{\"schema\":5,\"catalogs\":{\"cn\":[{\"id\":\"m\",\"maxInputTokens\":1000,\"maxOutputTokens\":64,\"contextLengths\":[100,1000,100,2000,-1],\"contextDefaultLength\":2000},{\"id\":\"other\"}],\"intl\":[{\"id\":\"m\",\"maxInputTokens\":2000,\"maxOutputTokens\":128,\"contextDefaultLength\":500}]}}");
    internal static async Task Run()
    {
        var catalog=new ModelCatalog();catalog.Load(Snapshot());int checks=0;
        var first=(JsonObject)catalog.List()["data"]![0]!;
        Check(!first.ContainsKey("context_length")&&first.ContainsKey("capabilities_by_site"),"Never merge conflicting site capabilities by maximum");checks++;
        Check(catalog.DefaultOutput("m")==128&&catalog.DefaultOutput("m",AccountSite.Cn)==64&&catalog.DefaultOutput("unknown")==0,"Site output limits and unknown models");checks++;
        string before=catalog.List().ToJsonString();
        try{catalog.Load("{\"schema\":5,\"catalogs\":{\"cn\":[null]}}"u8.ToArray());throw new InvalidOperationException();}catch(ArgumentException){}
        Check(catalog.List().ToJsonString()==before,"Failed reload preserves prior complete snapshot");checks++;
        foreach(string body in new[]{"{\"model\":\"m\",\"messages\":[]}","{\"max_tokens\":null,\"max_completion_tokens\":null}","{}"})
        {
            int reserved=0;var bytes=catalog.PrepareOutput(Encoding.UTF8.GetBytes(body),"m",AccountSite.Cn,n=>reserved=n,1024);
            using var doc=JsonDocument.Parse(bytes);Check(doc.RootElement.GetProperty("max_tokens").GetInt32()==64&&reserved==bytes.Length,"Default output injected after reservation");checks++;
        }
        foreach(string body in new[]{"{\"max_tokens\":0}","{\"max_tokens\":100000}","{\"max_completion_tokens\":1}","{\"max_tokens\":false}"})
        {var bytes=Encoding.UTF8.GetBytes(body);Check(ReferenceEquals(bytes,catalog.PrepareOutput(bytes,"m",AccountSite.Cn,_=>throw new InvalidOperationException(),1024)),"Explicit output budget untouched");checks++;}
        byte[] large=JsonSerializer.SerializeToUtf8Bytes(new{messages=new[]{new{role="user",content=new string('x',1<<20)}}});
        _=catalog.PrepareOutput(large,"m",AccountSite.Cn,_=>{},32<<20);
        long allocated=GC.GetAllocatedBytesForCurrentThread();byte[] patched=catalog.PrepareOutput(large,"m",AccountSite.Cn,_=>{},32<<20);allocated=GC.GetAllocatedBytesForCurrentThread()-allocated;
        Check(allocated<large.Length+65536&&patched.Length>large.Length,"Budget insertion does not construct another JSON tree");checks++;
        long failed=GC.GetAllocatedBytesForCurrentThread();
        try{catalog.PrepareOutput(large,"m",AccountSite.Cn,_=>throw new RequestFailure("budget",503),32<<20);throw new InvalidOperationException();}catch(RequestFailure e){Check(e.Status==503,"Reservation refusal");}
        Check(GC.GetAllocatedBytesForCurrentThread()-failed<65536,"Reserve before allocating large outbound copy");checks++;
        var blocked=new ModelCatalog(allow:["m"],block:["m"]);blocked.Load(Snapshot());Check(((JsonArray)blocked.List()["data"]!).Count==0&&!blocked.Allows("m"),"Global block cannot be overridden by whitelist");checks++;
        Check(new ModelCatalog(allow:[]).Allows("m"),"Empty whitelist leaves model access unchanged");checks++;
        byte[] nested="{\"messages\":[{\"role\":\"user\",\"content\":\"用户正常正文\"}],\"tool\":{\"max_tokens\":999}}"u8.ToArray();
        using(var d=JsonDocument.Parse(catalog.PrepareOutput(nested,"m",AccountSite.Cn,_=>{},1024)))Check(d.RootElement.GetProperty("tool").GetProperty("max_tokens").GetInt32()==999&&d.RootElement.GetProperty("messages")[0].GetProperty("content").GetString()=="用户正常正文","Do not edit nested tool arguments or user text");checks++;
        foreach(string body in new[]{
            """{"max_completion_tokens":128, "tool":{"max_tokens":999}, "max_tokens":512}""",
            """{"max_tokens":512,"max_completion_tokens":128, "tool":{"max_tokens":999}}"""})
        {
            int reserved=0;
            var capped=ModelCatalog.PrepareCappedOutput(Encoding.UTF8.GetBytes(body),32768,0,n=>reserved=n,1024);
            using var d=JsonDocument.Parse(capped.Body);
            Check(capped.Requested==128 && capped.Effective==128 && reserved==capped.Body.Length &&
                d.RootElement.GetProperty("max_tokens").GetInt32()==128 &&
                !d.RootElement.TryGetProperty("max_completion_tokens",out _) &&
                d.RootElement.GetProperty("tool").GetRawText()=="""{"max_tokens":999}""",
                "Combining both top-level limits keeps the smaller value and preserves nested bytes");
            checks++;
        }
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})
        {
            var b=WebApplication.CreateSlimBuilder();b.Logging.ClearProviders();b.WebHost.ConfigureKestrel(k=>k.Listen(IPAddress.Loopback,0));await using var backend=b.Build();int calls=0;
            backend.MapPost("/model",async c=>{using var d=await JsonDocument.ParseAsync(c.Request.Body);Check(d.RootElement.GetProperty("max_tokens").GetInt32()==128,"Mapped requests have default output budget");Interlocked.Increment(ref calls);c.Response.ContentType="text/event-stream";await c.Response.WriteAsync("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":1}}\n\ndata: [DONE]\n\n");});
            await backend.StartAsync();string dir=Path.Combine(Path.GetTempPath(),"synthetic-catalog-"+Guid.NewGuid().ToString("N"));
            using var keys=PreviewKeyDirectory.Open(dir);using var ledger=CreditLedger.Open(dir,keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);
            var key=keys.Create(new("synthetic",Models:["m"],MaxConcurrent:2));
            await using var app=LocalGateway.Build(new(new Uri(LocalGateway.Address(backend)+"/model"),Program.TestKey,Security:new(keys,new("synthetic-admin"),ledger),Catalog:catalog));await app.StartAsync();
            using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(10)};
            using(var noKey=await client.GetAsync("/v1/models"))Check((int)noKey.StatusCode==401,"Models require model Key");checks++;
            client.DefaultRequestHeaders.Authorization=new AuthenticationHeaderValue("Bearer",key.TakeSecret());
            using(var list=await client.GetAsync("/v1/models")){using var doc=JsonDocument.Parse(await list.Content.ReadAsStringAsync());Check(list.IsSuccessStatusCode&&doc.RootElement.GetProperty("data").GetArrayLength()==1,"Models obey Key whitelist");}checks++;
            using(var bad=await client.PostAsJsonAsync("/v1/models",new{}))Check((int)bad.StatusCode==405,"Models method validation");checks++;
            var gate=app.Services.GetRequiredService<SemaphoreSlim>(); for(int i=0;i<4;i++)await gate.WaitAsync();
            try { using var busy=await client.GetAsync("/v1/models");Check((int)busy.StatusCode==503,"Model listing shares global admission"); }
            finally { gate.Release(4); } checks++;
            object request=path=="/v1/responses"?new{model="m",input="hello",stream=false}:(object)new{model="m",messages=new[]{new{role="user",content="hello"}},stream=false};
            using(var response=await client.PostAsJsonAsync(path,request))Check(response.IsSuccessStatusCode&&calls==1,"Independent models and three protocols");checks++;
            keys.Update(key.Item.Id,new("restricted",Models:["other"]));
            using(var rejected=await client.PostAsJsonAsync(path,request))Check((int)rejected.StatusCode==403&&calls==1,"Key policy checked before send");checks++;
        }
        Console.WriteLine(JsonSerializer.Serialize(new{catalogChecks=checks,budgetPatchAllocatedBytes=allocated}));
    }
}
