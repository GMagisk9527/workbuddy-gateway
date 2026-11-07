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
        byte[] requestBytes=Encoding.UTF8.GetBytes("{\"model\":\"m\",\"metadata\":{\"session_id\":\"synthetic\",\"label\":\"中文\"},\"input\":[{\"type\":\"function_call_output\",\"call_id\":\"c\",\"output\":\""+new string('x',8192)+"\"}]}");
        var responseRequest=ResponsesRequestAdapter.Prepare(requestBytes,Program.Key);
        Array.Fill(requestBytes,(byte)' ');
        if(responseRequest.Metadata["session_id"]?.GetValue<string>()!="synthetic"||responseRequest.Metadata["label"]?.GetValue<string>()!="中文")
            throw new Exception("Response metadata borrowed disposed request memory");
        using(var owned=JsonDocument.Parse(responseRequest.Body))
            if(owned.RootElement.GetProperty("messages")[0].GetProperty("content").GetString()!=new string('x',8192))throw new Exception("Encoded response request borrowed input memory");
        checks++;
        byte[] chatBytes=Encoding.UTF8.GetBytes("{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\""+new string('y',8192)+"\"}]}");
        var chatRequest=ChatRequestAdapter.Prepare(chatBytes);Array.Fill(chatBytes,(byte)' ');
        using(var owned=JsonDocument.Parse(chatRequest.Body))
            if(owned.RootElement.GetProperty("messages")[0].GetProperty("content").GetString()!=new string('y',8192))throw new Exception("Encoded chat request borrowed input memory");
        checks++;
        foreach(string escape in new[]{"\\/","\\u0041","\\u000b","\\u2028"})
        {
            byte[] special=Encoding.UTF8.GetBytes("{\"model\":\"m\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\""+new string('z',4096)+escape+"\"}]}");
            var original=OpenWire.Parse(special);original["model"]="m";original["stream"]=true;
            var normalized=OpenWire.Encode(original,1<<20);
            var converted=ChatRequestAdapter.Prepare(special);
            if(!normalized.AsSpan().SequenceEqual(converted.Body))throw new Exception("Chat fast path changed canonical string escaping");
            checks++;
        }
        foreach(string text in new[]{" ","\\u0020","\\u00a0","\\n\\t","\\b","abc","中文"})
        {
            byte[] request=Encoding.UTF8.GetBytes("{\"model\":\"m\",\"instructions\":\"system\",\"input\":\""+text+"\"}");
            using var source=JsonDocument.Parse("\""+text+"\"");
            using var result=JsonDocument.Parse(ResponsesRequestAdapter.Prepare(request,Program.Key).Body);
            int expected=string.IsNullOrWhiteSpace(source.RootElement.GetString())?1:2;
            if(result.RootElement.GetProperty("messages").GetArrayLength()!=expected)throw new Exception("UTF-8 user input changed whitespace semantics");
            checks++;
        }
        var prefixSamples=samples.Take(8).Concat(new[]{new string('x',1<<20),"first\\nsecond\\t"+new string('z',8192)});
        foreach(string sample in prefixSamples)
        {
            var tree=OpenWire.Parse(JsonSerializer.SerializeToUtf8Bytes(new JsonObject{
                ["role"]="user",["content"]=new JsonArray(new JsonObject{["type"]="text",["text"]=sample}),
                ["after"]=new JsonArray(true,null,JsonValue.Create(1.23m))},options));
            byte[] expected=OpenWire.Encode(tree,OpenWire.RequestLimit);
            foreach(int limit in new[]{1,2,3,7,31,127,256,1024,2048,4096,65536})
            {
                byte[] output=new byte[limit];
                if(OpenWire.TryEncodePrefix(tree,output,out int written)&&
                    (written!=Math.Min(limit,expected.Length)||!output.AsSpan(0,written).SequenceEqual(expected.AsSpan(0,written))))
                    throw new Exception("Bounded JSON prefix changed the encoded bytes");
                checks++;
            }
        }
        var large=OpenWire.Parse(Encoding.UTF8.GetBytes("{\"role\":\"user\",\"content\":\""+new string('x',1<<20)+"\"}"));
        if(!OpenWire.TryEncodePrefix(large,new byte[65536],out int prefixLength)||prefixLength!=65536)
            throw new Exception("Large canonical user prefix did not take the bounded path");
        checks++;
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
            byte[] expected=bytes.ToArray(),actual=OpenWire.Encode(node,expected.Length,optimizeLarge:true);
            if(!expected.AsSpan().SequenceEqual(actual))throw new Exception("UTF-8 writer changed the encoded JSON");
            int reserved=0;byte[] admitted=OpenWire.Encode(node,expected.Length,optimizeLarge:true,reserve:n=>reserved=n);
            if(reserved!=expected.Length||!expected.AsSpan().SequenceEqual(admitted))throw new Exception("Preallocation reservation changed the exact encoded size");
            try{OpenWire.Encode(node,expected.Length-1,optimizeLarge:true);throw new Exception("Output budget was bypassed");}
            catch(OpenAiFailure failure)when(failure.Code=="output_budget_exceeded"){}
            checks++;
        }
    }
}
