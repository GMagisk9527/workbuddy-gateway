using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;
using System.Text.Json.Nodes;
using OpenAiProtocols;

namespace OpenAiChecks;

internal static class EncodingTests
{
    internal static int Run()
    {
        int checks=0;var options=new JsonSerializerOptions{Encoder=JavaScriptEncoder.UnsafeRelaxedJsonEscaping,MaxDepth=256};
        var samples=new List<string>{new('x',1024),string.Concat(Enumerable.Repeat("中文 😀 <>& \" \\ \n\t",100)),new(' ',1024)};
        var random=new Random(11128);
        for(int i=0;i<64;i++)samples.Add(new string(Enumerable.Range(0,1024).Select(_=>(char)random.Next(65536)).ToArray()));
        foreach(string sample in samples)
        {
            byte[] encoded=JsonSerializer.SerializeToUtf8Bytes(sample,options);
            Compare(JsonNode.Parse(encoded));
            Compare(JsonValue.Create(sample));
            var original=new JsonObject{["type"]="function_call_output",["call_id"]="call",["output"]=JsonNode.Parse(encoded)};
            byte[] request=JsonSerializer.SerializeToUtf8Bytes(new JsonObject{["model"]="m",["input"]=new JsonArray(original)},options);
            var converted=ResponsesRequestAdapter.Prepare(request,Program.Key);
            using var result=JsonDocument.Parse(converted.Body);
            string actual=result.RootElement.GetProperty("messages")[0].GetProperty("content").GetString()!;
            using var expected=JsonDocument.Parse(encoded);
            if(actual!=expected.RootElement.GetString())throw new Exception("Scalar tool result changed");
            checks++;
        }
        foreach(string escape in new[]{"\\/","\\u0041","\\u000b","\\u2028","\\u2029","\\uD800","\\uDBFF\\uDFFF"})
            Compare(JsonNode.Parse("\""+new string('x',512)+escape+"\""));
        foreach(JsonNode? value in new JsonNode?[]{null,JsonValue.Create(1.234m),JsonValue.Create(ulong.MaxValue),new JsonObject{["control"]=JsonValue.Create("\u0001"),["nested"]=new JsonArray(JsonNode.Parse("\""+new string('z',2048)+"\""),true,null)}})
            Compare(value);
        return checks;

        void Compare(JsonNode? node)
        {
            using var bytes=new MemoryStream();
            try
            {
                using var writer=new Utf8JsonWriter(bytes,new(){Encoder=options.Encoder,MaxDepth=256});
                if(node is null)writer.WriteNullValue();else node.WriteTo(writer,options);
            }
            catch(InvalidOperationException)
            {
                try{OpenWire.Encode(node,1<<20);}
                catch(InvalidOperationException){checks++;return;}
                throw new Exception("Invalid UTF-16 must follow the reference encoder's rejection");
            }
            byte[] expected=bytes.ToArray(),actual=OpenWire.Encode(node,expected.Length);
            if(!expected.AsSpan().SequenceEqual(actual))throw new Exception("UTF-8 writer changed the encoded JSON");
            try{OpenWire.Encode(node,expected.Length-1);throw new Exception("Output budget was bypassed");}
            catch(OpenAiFailure failure)when(failure.Code=="output_budget_exceeded"){}
            checks++;
        }
    }
}
