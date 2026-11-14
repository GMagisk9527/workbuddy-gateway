using System.Text;
using System.Text.Json;
using OpenAiProtocols;
using StreamingMigration;
using static OpenAiChecks.StreamTests;

namespace OpenAiChecks;

internal static class FrameBufferTests
{
    internal static async Task<List<string>> Run()
    {
        var tests=new List<string>();
        string text="中文 😀 \" \\ <>&\u2028\n";
        string delta="data: "+JsonSerializer.Serialize(new{choices=new[]{new{delta=new{content=text}}}})+"\n\n";
        foreach(bool responses in new[]{false,true})
        {
            using var output=new DelayedOutput();int settlements=0;
            var result=await OpenAiPump.RunAsync(new MemoryStream(Encoding.UTF8.GetBytes(delta+delta+Finish)),output,Request(responses),Program.Key,
                beforeTerminal:_=>
                {
                    settlements++;
                    string sent=Encoding.UTF8.GetString(output.ToArray());
                    Check(!sent.Contains("[DONE]",StringComparison.Ordinal)&&!sent.Contains("response.completed",StringComparison.Ordinal)&&!sent.Contains("\"finish_reason\":\"stop\"",StringComparison.Ordinal),"Terminal success waits for settlement");
                });
            string wire=Encoding.UTF8.GetString(output.ToArray());var events=Events(wire);
            string combined=responses?string.Concat(events.Where(e=>OpenWire.Text(e["type"])=="response.output_text.delta").Select(e=>OpenWire.Text(e["delta"])))
                :string.Concat(events.Select(e=>OpenWire.Text(e["choices"]?[0]?["delta"]?["content"])));
            Check(result.Outcome==StreamOutcome.Complete&&settlements==1&&combined==text+text,"Borrowed frames survive delayed writes and flushes");
            Check(output.Writes==output.Flushes&&wire.EndsWith("data: [DONE]\n\n",StringComparison.Ordinal),"Every frame flushes before reuse");
            if(responses)
            {
                Check(events.Count(e=>OpenWire.Text(e["type"])=="response.output_text.done")==1&&events.Count(e=>OpenWire.Text(e["type"])=="response.completed")==1,"Deferred terminal frames have independent storage");
                Check(OpenWire.Text(events.Single(e=>OpenWire.Text(e["type"])=="response.completed")["response"]?["output"]?[0]?["content"]?[0]?["text"])==text+text,"Final accumulated text retained");
            }
        }
        tests.Add("borrowed Chat and Responses frames survive asynchronous downstream and settlement barrier");
        // Unknown Chat fields are forwarded and can exceed the output budget
        // without first triggering accumulated-text admission.
        string huge="data: "+JsonSerializer.Serialize(new{choices=new[]{new{delta=new{content="ok"}}},extension=new string('x',8192)})+"\n\n";
        var (failure,failedWire)=await Pump(huge+Finish,Request(false),2048);
        Check(failure.Code=="output_budget_exceeded"&&Events(failedWire).Count==1&&Events(failedWire)[0]["error"] is not null,"Rejected partial frame never reaches downstream, disposal does not retry flush");
        tests.Add("oversized encoded frame fails before any partial write");
        foreach(bool responses in new[]{false,true})
        {
            using var output=new MemoryStream();
            var result=await OpenAiPump.RunAsync(new MemoryStream(Encoding.UTF8.GetBytes(Text+Finish)),output,Request(responses),Program.Key,budget:new DenyScratch());
            string wire=Encoding.UTF8.GetString(output.ToArray());var last=Events(wire).Last();
            Check(result.Code=="output_budget_exceeded"&&!wire.Contains("[DONE]",StringComparison.Ordinal)&&(responses?OpenWire.Text(last["type"])=="response.failed":last["error"] is not null),"Error frame survives exhausted shared scratch budget");
        }
        tests.Add("shared budget failure remains a visible error with no success marker");
        return tests;
    }
    sealed class DenyScratch : IRetainedOutputBudget
    {
        public void Add(long bytes){} public void Remove(long bytes){}
        public void Scratch(long bytes)=>throw new ConversionFailure("output_budget_exceeded");
    }
    sealed class DelayedOutput : MemoryStream
    {
        ReadOnlyMemory<byte> borrowed;byte[] snapshot=[];
        internal int Writes,Flushes;
        public override async ValueTask WriteAsync(ReadOnlyMemory<byte> buffer,CancellationToken cancellationToken=default)
        {
            borrowed=buffer;snapshot=buffer.ToArray();await Task.Yield();
            Check(buffer.Span.SequenceEqual(snapshot),"Frame changed before WriteAsync completed");
            await base.WriteAsync(buffer,cancellationToken);Writes++;
        }
        public override async Task FlushAsync(CancellationToken cancellationToken)
        {
            await Task.Yield();Check(borrowed.Span.SequenceEqual(snapshot),"Frame changed before FlushAsync completed");Flushes++;
        }
    }
}
