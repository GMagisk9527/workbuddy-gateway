using System.IO.Compression;
using System.Text;
using System.Text.Json;
using LedgerMigration;

namespace HttpMigration;

internal static class CountTokensTests
{
    sealed class NoNetwork:HttpMessageHandler
    {
        internal int Calls;
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage r,CancellationToken c){Calls++;throw new InvalidOperationException("Count must stay local");}
    }
    internal static async Task Run(string scratch)
    {
        int checks=0;void Check(bool b,string name){if(!b)throw new InvalidOperationException(name);checks++;}
        using var keys=PreviewKeyDirectory.Open(scratch);using var ledger=CreditLedger.Open(scratch,keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);
        var issued=keys.Create(new("count",QuotaTotal:10,MaxConcurrent:2,Models:["m"]));string token=issued.TakeSecret();var fake=new NoNetwork();
        await using var app=LocalGateway.Build(new(new("http://127.0.0.1:1"),new byte[32],MaxRequestBytes:2048,MaxConcurrent:2,MaxInFlightBodyBytes:4096,Security:new(keys,new("admin-synthetic"),ledger),UpstreamHandler:fake));
        await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(10)};
        var resources=app.Services.GetRequiredService<PreparationAdmission>();var gate=app.Services.GetRequiredService<SemaphoreSlim>();
        const string good="{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"你好abc\"}]}";
        async Task Send(string body,int status,int? count=null,string auth="valid",string? coding=null,HttpMethod? method=null)
        {
            byte[] bytes=Encoding.UTF8.GetBytes(body);
            if(coding=="gzip"){using var ms=new MemoryStream();using(var zip=new GZipStream(ms,CompressionLevel.Fastest,true))zip.Write(bytes);bytes=ms.ToArray();}
            if(coding=="zstd-raw")
            {
                // Valid single-segment zstd frame with one uncompressed block (<256 bytes).
                int header=(bytes.Length<<3)|1;
                bytes=new byte[]{0x28,0xb5,0x2f,0xfd,0x20,(byte)bytes.Length,(byte)header,(byte)(header>>8),(byte)(header>>16)}.Concat(bytes).ToArray();coding="zstd";
            }
            using var req=new HttpRequestMessage(method??HttpMethod.Post,"/v1/messages/count_tokens"){Content=new ByteArrayContent(bytes)};
            if(auth!="none")req.Headers.Add("x-api-key",auth=="valid"?token:auth);
            if(coding is not null)req.Content.Headers.ContentEncoding.Add(coding);
            using var response=await client.SendAsync(req);string text=await response.Content.ReadAsStringAsync();
            Check((int)response.StatusCode==status,$"HTTP expected {status}: {text}");
            Check(response.Headers.CacheControl?.NoStore==true,"No response caching");
            using var doc=JsonDocument.Parse(text);
            if(count is not null)Check(doc.RootElement.GetProperty("input_tokens").GetInt32()==count,"Count parity");
            else Check(doc.RootElement.GetProperty("type").GetString()=="error","Anthropic error envelope");
        }
        await Send(good,200,3);await Send(good,200,3,coding:"gzip");
        await Send(good,200,3,coding:"zstd-raw");
        await Send("{\"model\":\"m\",\"messages\":[]}",200,0);
        await Send(good,401,auth:"none");await Send(good,401,auth:"invalid");await Send(good,405,method:HttpMethod.Get);
        await Send(good.Replace("\"m\"","\"forbidden\""),403);
        await Send("{",400);await Send("null",400);await Send("[]",400);await Send("{\"messages\":{}}",400);
        await Send(good.Replace("你好abc","\\ud800"),400);
        await Send("{\"model\":\"m\",\"messages\":[],\"\\udfff\":1}",400);
        await Send(good.Replace("\"user\"","\"invalid\""),400);
        await Send(good.Replace("你好abc",new string('a',2200)),413);
        await Send(good.Replace("你好abc",new string('a',2200)),413,coding:"gzip");
        await Send(good,415,coding:"br");await Send(good,400,coding:"zstd");
        using(var held=resources.Acquire()){held.Reserve(4096);await Send(good,503);}
        await resources.Preparing.WaitAsync();await resources.Preparing.WaitAsync();
        try{await Send(good,503);}finally{resources.Preparing.Release(2);}
        await gate.WaitAsync();await gate.WaitAsync();try{await Send(good,503);}finally{gate.Release(2);}
        await Send(good,200,3);
        Check(resources.Used==0&&resources.Preparing.CurrentCount==2&&gate.CurrentCount==2,"Admission released after failures");
        Check(fake.Calls==0,"No upstream requests");Check(ledger.Health.PendingAttempts==0&&ledger.GetUsage(issued.Item.Id).Total==0,"No ledger attempts or charges");
        await app.StopAsync();Console.WriteLine(JsonSerializer.Serialize(new{countTokenChecks=checks,realModelCalls=0,productionChanged=false}));
    }
}
