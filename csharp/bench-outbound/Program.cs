using System.Diagnostics;
using System.Text;
using System.Text.Json;
using OpenAiProtocols;
using StreamingMigration;

// Synthetic loopback-free CPU/allocation benchmark. Use the same executable
// against two GatewayAssemblies builds; no model, production data or network.
byte[] key=new byte[32];
string delta="data: {\"choices\":[{\"delta\":{\"content\":\""+new string('x',256)+"\"}}]}\n\n";
byte[] wire=Encoding.UTF8.GetBytes(string.Concat(Enumerable.Repeat(delta,2048))+"data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":2,\"prompt_tokens\":20,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n");
var rows=new List<object>();
void Measure(string name,Func<CountingOutput,Task> pump)
{
    for(int i=0;i<2;i++)pump(new()).GetAwaiter().GetResult();
    for(int round=0;round<5;round++)
    {
        GC.Collect();GC.WaitForPendingFinalizers();GC.Collect();
        var output=new CountingOutput();long before=GC.GetTotalAllocatedBytes(true);var clock=Stopwatch.StartNew();
        pump(output).GetAwaiter().GetResult();clock.Stop();
        rows.Add(new{name,round,allocatedBytes=GC.GetTotalAllocatedBytes(true)-before,elapsedMs=clock.Elapsed.TotalMilliseconds,output.Bytes,output.Writes,output.Flushes});
    }
}
foreach(bool responses in new[]{false,true})foreach(bool stream in new[]{true,false})
{
    var request=responses?ResponsesRequestAdapter.Prepare("{\"model\":\"m\",\"input\":\"hi\"}"u8.ToArray(),key)
        :ChatRequestAdapter.Prepare("{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"u8.ToArray());
    request=request with{Stream=stream};
    Measure((responses?"Responses":"Chat")+(stream?" stream":" aggregate"),async output=>
    {
        var result=await OpenAiPump.RunAsync(new MemoryStream(wire),output,request,key);
        if(result.Outcome!=StreamOutcome.Complete)throw new InvalidOperationException(result.Code);
    });
}
Measure("Messages stream",async output=>
{
    var result=await StreamPump.RunAsync(new MemoryStream(wire),output,new("m"));
    if(result.Outcome!=StreamOutcome.Complete)throw new InvalidOperationException(result.Code);
});
Console.WriteLine(JsonSerializer.Serialize(new{runtime=Environment.Version.ToString(),os=System.Runtime.InteropServices.RuntimeInformation.OSDescription,mode="Release JIT synthetic 512KiB/2048 deltas; not AOT or production latency",rows}));

sealed class CountingOutput : Stream
{
    internal long Bytes,Writes,Flushes;
    public override ValueTask WriteAsync(ReadOnlyMemory<byte> value,CancellationToken cancellationToken=default)
    {Bytes+=value.Length;Writes++;return ValueTask.CompletedTask;}
    public override Task FlushAsync(CancellationToken cancellationToken){Flushes++;return Task.CompletedTask;}
    public override bool CanRead=>false;public override bool CanWrite=>true;public override bool CanSeek=>false;
    public override long Length=>Bytes;public override long Position{get=>Bytes;set=>throw new NotSupportedException();}
    public override void Flush(){}public override int Read(byte[] b,int o,int c)=>throw new NotSupportedException();
    public override void Write(byte[] b,int o,int c)=>throw new NotSupportedException();public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long v)=>throw new NotSupportedException();
}
