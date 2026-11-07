using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;
using TypedMigration;

namespace HttpMigration;

internal static class Utf8ToolResultTests
{
    internal static void Run()
    {
        int checks=0;
        var encoder=new JsonSerializerOptions{Encoder=JavaScriptEncoder.UnsafeRelaxedJsonEscaping};
        var samples=new List<string>{new('x',8192),new(' ',8192),string.Concat(Enumerable.Repeat("中文 😀 <>& \" \\ \n\t \u2028\u2029 \u0001",1024))};
        var random=new Random(11128);
        for(int i=0;i<12;i++)samples.Add(new string(Enumerable.Range(0,8192).Select(_=>(char)random.Next(65536)).ToArray()));
        var tokens=samples.Select(s=>JsonSerializer.Serialize(s,encoder)).ToList();
        foreach(string escape in new[]{"\\u0041","\\/","\\u000b","\\u2028","\\uD800"})tokens.Add("\""+new string('x',8192)+escape+"\"");
        foreach(string token in tokens)foreach(bool error in new[]{false,true})
        {
            byte[] input=Encoding.UTF8.GetBytes("{\"messages\":[{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"c\",\"is_error\":"+(error?"true":"false")+",\"content\":"+token+"}]}]}");
            ChatRequest owned;byte[] borrowedBytes,expected;
            using(var document=JsonDocument.Parse(input))
            {
                try{owned=MessageMapper.Map(WireReader.Read(document.RootElement));}
                catch(InvalidOperationException)
                {
                    try{MessagesRequestAdapter.Prepare(input,Program.TestKey);}
                    catch(RequestFailure){checks++;continue;}
                    throw new Exception("Borrowed decoding accepted invalid Unicode");
                }
                var borrowed=MessageMapper.Map(WireReader.ReadBorrowed(document.RootElement));
                expected=Encoding.UTF8.GetBytes(GoJson.Format(WireWriter.ToNode(owned)));
                borrowedBytes=GoJson.FormatUtf8(WireWriter.ToNode(borrowed),expected.Length);
                if(!expected.AsSpan().SequenceEqual(borrowedBytes)||!WireWriter.Write(owned).AsSpan().SequenceEqual(WireWriter.Write(borrowed)))
                    throw new Exception("Borrowed tool result changed the wire representation");
            }
            // The default reader is still independently owned; the fast path
            // returns only completed byte arrays before disposing its document.
            if(!expected.AsSpan().SequenceEqual(Encoding.UTF8.GetBytes(GoJson.Format(WireWriter.ToNode(owned)))))
                throw new Exception("Default reader lost document ownership");
            using var result=JsonDocument.Parse(MessagesRequestAdapter.Prepare(input,Program.TestKey).UpstreamBody);
            string actual=result.RootElement.GetProperty("messages")[0].GetProperty("content").GetString()!;
            using var reference=JsonDocument.Parse(expected);
            if(actual!=reference.RootElement.GetProperty("messages")[0].GetProperty("content").GetString())
                throw new Exception("Prepared tool result changed after document disposal");
            checks++;
        }
        Console.WriteLine(JsonSerializer.Serialize(new{utf8ToolResultChecks=checks}));
        foreach(string sample in samples)
        {
            byte[] input=Encoding.UTF8.GetBytes("{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":"+JsonSerializer.Serialize(sample,encoder)+"}]}");
            using var document=JsonDocument.Parse(input);
            var owned=MessageMapper.Map(WireReader.Read(document.RootElement));
            var borrowed=MessageMapper.Map(WireReader.ReadBorrowed(document.RootElement));
            byte[] expected=Encoding.UTF8.GetBytes(GoJson.Format(WireWriter.ToNode(owned)));
            if(!expected.AsSpan().SequenceEqual(GoJson.FormatUtf8(WireWriter.ToNode(borrowed),expected.Length)))throw new Exception("Borrowed user text changed wire bytes");
            using var result=JsonDocument.Parse(MessagesRequestAdapter.Prepare(input,Program.TestKey,prompts:new(Identity:new(Target:"Changed"))).UpstreamBody);
            using var source=JsonDocument.Parse(JsonSerializer.SerializeToUtf8Bytes(sample,encoder));
            if(result.RootElement.GetProperty("messages")[0].GetProperty("content").GetString()!=source.RootElement.GetString())throw new Exception("Borrowed user text was rewritten");
            checks++;
        }
        Console.WriteLine(JsonSerializer.Serialize(new{utf8UserTextChecks=samples.Count}));
    }
}
