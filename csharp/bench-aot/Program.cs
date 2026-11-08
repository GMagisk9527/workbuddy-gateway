using System.Diagnostics;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using StreamingMigration;
using TypedMigration;

var writer=new EventWriter();
StorageChecks.Run();
var texts=new List<string>{"","hello","中文 😀 <>& \"\\\r\n\t","\u2028\u2029","\ud800","\udc00","\ud800\ud800",new('x',65536),string.Concat(Enumerable.Range(0,32).Select(i=>(char)i))};
var random=new Random(11128);
for(int i=0;i<200;i++)texts.Add(new string(Enumerable.Range(0,random.Next(1,256)).Select(_=>(char)random.Next(65536)).ToArray()));
var events=new List<ProtocolEvent>();
foreach(var text in texts)
{
    events.AddRange([new MessageStart(text,text),new TextStart(17),new ThinkingStart(17),new ToolStart(17,new(text),new(text)),
        new TextDelta(17,text),new ThinkingDelta(17,text),new SignatureDelta(17,text),new ToolDelta(17,text),
        new BlockStop(17),new MessageDelta(text,new(0,long.MaxValue,123,456)),new MessageStop(),new StreamError(text)]);
}
using var hash=IncrementalHash.CreateHash(HashAlgorithmName.SHA256);
#if !NATIVEAOT
var legacy=new LegacyEventWriter();
#endif
foreach(var e in events)
{
    var bytes=e.Accept(writer);
#if !NATIVEAOT
    if(!bytes.AsSpan().SequenceEqual(e.Accept(legacy)))throw new Exception("Wire mismatch: "+e.GetType().Name);
#endif
    hash.AppendData(bytes);
}
string fingerprint=Convert.ToHexString(hash.GetHashAndReset());
if(args is ["--expect",var expected]&&fingerprint!=expected)throw new Exception("Native wire fingerprint mismatch");
var sample=new TextDelta(1,"synthetic chunk 中文 <>& \n \" "+new string('x',128));
var results=new List<Measurement>();
#if !NATIVEAOT
Measure("legacy",legacy);
#endif
Measure("direct-utf8",writer);
Console.WriteLine(JsonSerializer.Serialize(new Report(events.Count,fingerprint,results.ToArray()),ReportJson.Default.Report));
void Measure(string name,IEventWriter<byte[]> target)
{
    for(int i=0;i<10000;i++)sample.Accept(target);
    for(int round=0;round<5;round++)
    {
        GC.Collect();GC.WaitForPendingFinalizers();long start=GC.GetAllocatedBytesForCurrentThread();
        var watch=Stopwatch.StartNew();int bytes=0;
        for(int i=0;i<50000;i++)bytes+=sample.Accept(target).Length;
        watch.Stop();results.Add(new(name,round,watch.Elapsed.TotalMilliseconds/50000,(GC.GetAllocatedBytesForCurrentThread()-start)/50000,bytes));
    }
}
internal sealed record Measurement(string Name,int Round,double Milliseconds,long AllocatedBytes,int OutputBytes);
internal sealed record Report(int Checks,string Sha256,Measurement[] Measurements);
[JsonSerializable(typeof(Report))]
internal partial class ReportJson:JsonSerializerContext {}
