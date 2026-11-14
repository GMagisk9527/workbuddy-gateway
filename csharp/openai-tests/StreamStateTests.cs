using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;
using System.Text.Json.Nodes;
using OpenAiProtocols;
using StreamingMigration;
using static OpenAiChecks.StreamTests;

namespace OpenAiChecks;

internal static class StreamStateTests
{
    static string Payload(string text)=>JsonSerializer.Serialize(new{choices=new[]{new{delta=new{content=text,reasoning_content=text,refusal=text}}}});
    sealed class Budget:IRetainedOutputBudget
    {
        internal long Added,ScratchBytes;
        public void Add(long bytes)=>Added+=bytes;
        public void Remove(long bytes)=>Added-=bytes;
        public void Scratch(long bytes)=>ScratchBytes=Math.Max(bytes,ScratchBytes);
    }
    internal static async Task<List<string>> Run()
    {
        var tests=new List<string>();string text="中文 😀 \"\\ <>&\u2028\u2029\n\r\t\0";
        byte[] payload=Encoding.UTF8.GetBytes(Payload(text));
        var budget=new Budget();var forward=new ChatState("m",1<<20,budget,retainText:false);var aggregate=new ChatState("m",1<<20);
        for(int i=0;i<256;i++)
        {
            var (raw,chunk)=forward.Feed(payload);aggregate.FeedResponses(payload);
            Check(OpenWire.Text(raw["choices"]?[0]?["delta"]?["content"])==text&&chunk.Choices[0].Reasoning==text,"Forwarded text and reasoning preserved");
        }
        Check(forward.Content.Length==0&&forward.Reasoning.Length==0&&forward.Refusal.Length==0&&budget.Added==0,"Streaming-only text does not accumulate or reserve already emitted content");
        Check(aggregate.Content.ToString()==string.Concat(Enumerable.Repeat(text,256))&&aggregate.Reasoning.Length==256*text.Length&&aggregate.Refusal.Length==256*text.Length,"Default aggregate state still retains all channels");
        forward.Feed(Encoding.UTF8.GetBytes(ToolArgumentTests.Frame(ToolArgumentTests.Fragment(0,"{}","call_a","run")).Split('\n')[0][6..]));
        forward.Feed("{\"choices\":[{\"finish_reason\":\"tool_calls\"}],\"usage\":{\"credit\":2}}"u8.ToArray());forward.Validate();
        Check(forward.Calls.Single().Arguments.ToString()=="{}"&&forward.Usage?["credit"]?.GetValue<int>()==2&&budget.Added>512,"Tool identity, arguments and measured usage remain retained");
        bool refused=false;try{forward.Completion();}catch(InvalidOperationException){refused=true;}
        Check(refused,"Streaming-only state cannot accidentally produce an empty aggregate completion");
        tests.Add("Chat streaming avoids retained text while preserving tools, usage and aggregate controls");

        int bytes=Encoding.UTF8.GetByteCount(text)*3;
        var limited=new ChatState("m",bytes,retainText:false);limited.Feed(payload);
        bool exceeded=false;try{limited.Feed(payload);}catch(ConversionFailure e)when(e.Code=="output_budget_exceeded"){exceeded=true;}
        Check(exceeded,"Cumulative logical text limit still applies without retained builders");
        var small=new Budget();var shorter=new Budget();using(var output=new MemoryStream())
        {
            string delta="data: "+Payload(new string('x',256))+"\n\n";
            var result=await OpenAiPump.RunAsync(new MemoryStream(Encoding.UTF8.GetBytes(string.Concat(Enumerable.Repeat(delta,64))+Finish)),output,Request(false),Program.Key,budget:small);
            using var shortOutput=new MemoryStream();
            var shortResult=await OpenAiPump.RunAsync(new MemoryStream(Encoding.UTF8.GetBytes(delta+Finish)),shortOutput,Request(false),Program.Key,budget:shorter);
            Check(result.Outcome==StreamOutcome.Complete&&shortResult.Outcome==StreamOutcome.Complete&&small.Added==shorter.Added&&small.ScratchBytes==shorter.ScratchBytes&&output.Length>48000,"Emitting 64 chunks reserves the same memory as one chunk");
        }
        tests.Add("Chat stream keeps cumulative limits with bounded retained-memory admission");

        var restore=new ResponseIdentityPlan([new("GatewayAgent","OriginalAgent")]);
        const string restoreWire="data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Gateway\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Agent thinks\",\"content\":\"GatewayAgent says hi\"}}]}\n\n";
        var (restored,restoredWire)=await Pump(restoreWire+Finish,Request(false) with{ResponseRestore=restore});
        var restoredEvents=Events(restoredWire);
        Check(restored.Outcome==StreamOutcome.Complete&&string.Concat(restoredEvents.Select(e=>OpenWire.Text(e["choices"]?[0]?["delta"]?["reasoning_content"])))=="OriginalAgent thinks"&&string.Concat(restoredEvents.Select(e=>OpenWire.Text(e["choices"]?[0]?["delta"]?["content"])))=="OriginalAgent says hi","Chat streaming preserves split-prefix identity restoration without aggregate text");
        tests.Add("Chat streaming retains split-prefix response restoration");

        // Compare exact JSON property order and escaping against the former DOM
        // representation for all four typed deltas, not only parsed equality.
        string tools=ToolArgumentTests.Frame(ToolArgumentTests.Fragment(0,text,"call_a","run"));
        var (result2,wire)=await Pump("data: "+Payload(text)+"\n\n"+tools+Finish,Request());
        Check(result2.Outcome==StreamOutcome.Complete,"All typed Responses deltas complete");
        var options=new JsonSerializerOptions{Encoder=JavaScriptEncoder.UnsafeRelaxedJsonEscaping};
        var expectedTypes=new HashSet<string>{"response.output_text.delta","response.refusal.delta","response.reasoning_summary_text.delta","response.function_call_arguments.delta"};
        int sequence=0;
        foreach(string line in wire.Split('\n').Where(s=>s.StartsWith("data: {",StringComparison.Ordinal)))
        {
            string json=line[6..];var e=(JsonObject)JsonNode.Parse(json)!;string type=OpenWire.Text(e["type"]);
            Check(OpenWire.Int(e["sequence_number"])==sequence++,"Sequence stays contiguous through typed and complex events");
            if(!expectedTypes.Remove(type))continue;
            var old=new JsonObject{["item_id"]=OpenWire.Text(e["item_id"]),["output_index"]=OpenWire.Int(e["output_index"])};
            if(type=="response.reasoning_summary_text.delta")old["summary_index"]=0;
            else if(type!="response.function_call_arguments.delta")old["content_index"]=type=="response.refusal.delta"?1:0;
            old["delta"]=text;old["type"]=type;old["sequence_number"]=sequence-1;
            Check(json==old.ToJsonString(options),"Typed delta matches prior DOM bytes: "+type);
            Check(wire.Contains("event: "+type+"\ndata: "+json+"\n\n",StringComparison.Ordinal),"Event header matches payload type");
        }
        Check(expectedTypes.Count==0,"Every typed delta exercised");
        var completed=Events(wire).Single(e=>OpenWire.Text(e["type"])=="response.completed")["response"]!;
        Check(completed["output"]!.AsArray().Count==3&&wire.EndsWith("data: [DONE]\n\n",StringComparison.Ordinal),"Lifecycle and complete aggregate output remain present");
        tests.Add("Responses typed deltas preserve exact UTF-8 escaping, field order and sequence");
        return tests;
    }
}
