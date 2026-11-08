using System.IO.Compression;
using System.Net;
using System.Text;
using System.Text.Json;

namespace HttpMigration;

internal static class ResourceTests
{
    static void Check(bool ok) { if (!ok) throw new InvalidOperationException("Resource admission check failed"); }
    internal static async Task Run()
    {
        var budget = new PreparationAdmission(8, 1);
        using (var lease = budget.Acquire())
        {
            var wire = lease.Wire(4); wire(3); wire(1); Check(budget.Used == 4);
            lease.Reserve(4); Check(budget.Used == 8);
            try { lease.Reserve(1); throw new InvalidOperationException(); } catch (RequestFailure e) { Check(e.Status == 503); }
            lease.Retain(4);Check(budget.Used==4);lease.Retain(4);Check(budget.Used==4);
            try{lease.Retain(5);throw new InvalidOperationException();}catch(ArgumentOutOfRangeException){}
            lease.Dispose(); lease.Dispose(); Check(budget.Used == 0);
        }
        int checks = 1;
        foreach (string path in new[] { "/v1/messages", "/v1/chat/completions", "/v1/responses" })
        {
            var b = WebApplication.CreateSlimBuilder(); b.Logging.ClearProviders(); b.WebHost.ConfigureKestrel(k => k.Listen(IPAddress.Loopback,0));
            await using var backend = b.Build(); var entered = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously);
            var release = new TaskCompletionSource(TaskCreationOptions.RunContinuationsAsynchronously); int count = 0;
            backend.MapPost("/upstream", async c =>
            {
                using var doc = await JsonDocument.ParseAsync(c.Request.Body);
                if (Interlocked.Increment(ref count)==1) { entered.TrySetResult(); await release.Task.WaitAsync(c.RequestAborted); }
                c.Response.ContentType="text/event-stream";
                await c.Response.WriteAsync("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n");
            });
            await backend.StartAsync();
            await using var gateway = LocalGateway.Build(new(new Uri(LocalGateway.Address(backend)+"/upstream"), Program.TestKey, MaxInFlightBodyBytes: 4096));
            await gateway.StartAsync();
            using var client = new HttpClient(new SocketsHttpHandler { UseProxy=false }) { BaseAddress=new Uri(LocalGateway.Address(gateway)), Timeout=TimeSpan.FromSeconds(10) };
            string text = new('x',1600);
            var obj = path=="/v1/responses" ? (object)new { model="test-model",input=text,stream=false } : new { model="test-model",messages=new[]{new {role="user",content=text}},stream=false };
            byte[] body=JsonSerializer.SerializeToUtf8Bytes(obj);
            var first=client.PostAsync(path,new ByteArrayContent(body));
            try
            {
                await entered.Task.WaitAsync(TimeSpan.FromSeconds(5));
                using var rejected=await client.PostAsync(path,new ByteArrayContent(body));
                Check((int)rejected.StatusCode==503 && count==1);
            }
            finally { release.TrySetResult(); using var done=await first; Check(done.IsSuccessStatusCode); }
            using (var recovered=await client.PostAsync(path,new ByteArrayContent(body))) Check(recovered.IsSuccessStatusCode && count==2);
            // Compressed bytes fit, decoded bytes exceed the shared budget.
            using var packed=new MemoryStream(); using(var gzip=new GZipStream(packed,CompressionMode.Compress,true)) gzip.Write(Encoding.UTF8.GetBytes(new string('x',5000)));
            using var content=new ByteArrayContent(packed.ToArray()); content.Headers.ContentEncoding.Add("gzip");
            using(var rejected=await client.PostAsync(path,content)) Check((int)rejected.StatusCode==503 && count==2);
            var resources=gateway.Services.GetRequiredService<PreparationAdmission>(); Check(resources.Used==0);
            using(var invalid=await client.PostAsync(path,new StringContent("{bad"))) Check((int)invalid.StatusCode==400);
            Check(resources.Used==0 && resources.Preparing.CurrentCount==2);
            checks+=4;
        }
        AllocationTests.Run();
        Console.WriteLine(JsonSerializer.Serialize(new {resourceChecks=checks,allocationChecks=1}));
    }
}
