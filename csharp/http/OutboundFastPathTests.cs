using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Runtime.InteropServices;
using OpenAiProtocols;

namespace HttpMigration;

internal static class OutboundFastPathTests
{
    internal static void Run()
    {
        int checks=0;var settings=new SystemPromptOptions();
        JsonObject Base()=>JsonNode.Parse("{\"model\":\"m\",\"messages\":[{\"role\":\"system\",\"content\":\"Keep these rules.\"},{\"role\":\"assistant\",\"tool_calls\":[{\"id\":\"a\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]},{\"role\":\"tool\",\"tool_call_id\":\"a\",\"content\":\"result\"}],\"stream\":true}")!.AsObject();
        void Verify(JsonObject root,bool expected,SystemPromptOptions? options=null)
        {
            options??=settings;
            string original=root.ToJsonString();int reserved=0;string? digest=null;
            var affinity=new SessionAffinity(new(true,Mode:AffinityMode.Prefix));
            byte[] input=OpenWire.Encode(root,OpenWire.RequestLimit,optimizeLarge:true);
            byte[] reference=OutboundPreparation.Prepare(input,options,_=>{},OpenWire.RequestLimit);
            byte[]? parsed=OutboundPreparation.PrepareParsed(root,options,n=>reserved=n,OpenWire.RequestLimit,
                normalized=>digest=affinity.PrefixDigest(normalized,false,null));
            if(!reference.AsSpan().SequenceEqual(parsed??input)||root.ToJsonString()!=original||parsed is not null&&reserved!=parsed.Length)
                throw new Exception("Parsed preparation changed normalization, ownership or reservation");
            if(digest is not null&&affinity.KeyFromPrefix("caller","m",digest,65536)!=affinity.Key("caller","m",null,reference))
                throw new Exception("Normalized tree prefix changed the original request affinity");
            if(OutboundPreparation.IsCompatible(root,options)!=expected)throw new Exception("Compatibility proof disagreed with expected eligibility");
            if(expected)
            {
                byte[] body=JsonSerializer.SerializeToUtf8Bytes(root);
                if(!body.AsSpan().SequenceEqual(OutboundPreparation.Prepare(body,options,_=>{},32<<20)))
                    throw new Exception("Compatibility proof skipped an actual normalization");
            }
            checks++;
        }
        Verify(Base(),true);
        Verify(OpenWire.Parse(Encoding.UTF8.GetBytes("{\"model\":\"m\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\""+new string('x',1<<20)+"\"}]}")),false);
        Verify(OpenWire.Parse(Encoding.UTF8.GetBytes("{\"model\":\"m\",\"stream\":true,\"messages\":[{\"role\":\"developer\",\"content\":\"Keep all rules.\"},{\"role\":\"user\",\"content\":\""+new string('x',65536)+"\\u4e2d\\n\\\"\"}]}")),false);
        var small=JsonNode.Parse("{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"u\"}]}")!.AsObject();
        bool limited=false;try{OutboundPreparation.PrepareParsed(small,new(Force:new string('x',1024)),_=>throw new Exception("Oversize body reserved memory"),128);}
        catch(RequestFailure failure)when(failure.Status==413&&failure.ErrorType=="request_too_large"){limited=true;}
        if(!limited)throw new Exception("Parsed preparation changed the request limit error");checks++;
        var root=Base();root["messages"]![0]!["role"]="developer";Verify(root,false);
        root=Base();root["messages"]!.AsArray().RemoveAt(0);Verify(root,false);
        root=Base();root["messages"]![0]!["content"]="You are Claude Code.";Verify(root,false);
        root=Base();root["reasoning_effort"]="high";Verify(root,false);
        root=Base();root["reasoning_summary"]=null;Verify(root,false);
        root=Base();root["model"]="deepseek-v4";Verify(root,true);
        Verify(Base(),false,settings with{Force="Always keep this instruction."});
        root=Base();root["messages"]!.AsArray().RemoveAt(2);Verify(root,false);
        root=Base();root["messages"]!.AsArray().Insert(2,new JsonObject{["role"]="user",["content"]="interleaved"});Verify(root,true);
        root=Base();root["messages"]!.AsArray().Add(root["messages"]![2]!.DeepClone());Verify(root,false);
        root=Base();root["messages"]![1]!["tool_calls"]!.AsArray().Add(root["messages"]![1]!["tool_calls"]![0]!.DeepClone());Verify(root,false);
        root=Base();var second=root["messages"]![1]!["tool_calls"]![0]!.DeepClone();second["id"]="b";
        root["messages"]![1]!["tool_calls"]!.AsArray().Add(second);
        root["messages"]!.AsArray().Add(new JsonObject{["role"]="tool",["tool_call_id"]="b",["content"]="second"});Verify(root,true);
        root["messages"]![2]!["tool_call_id"]="b";root["messages"]![3]!["tool_call_id"]="a";Verify(root,true);
        byte[] input=JsonSerializer.SerializeToUtf8Bytes(Base());
        var chat=ChatRequestAdapter.Prepare(input,r=>OutboundPreparation.IsCompatible(r,settings),consumeInput:true);
        if(!chat.OutboundCompatible)throw new Exception("Chat compatibility proof was not retained");checks++;
        if(!chat.Body.AsSpan().SequenceEqual(input))throw new Exception("Minified unchanged Chat request was re-encoded");
        if(!MemoryMarshal.TryGetArray<byte>(chat.Body.AsMemory(),out var shared)||!ReferenceEquals(shared.Array,input))throw new Exception("Chat request copied the input body despite retaining an exact full-array view");checks++;
        var spacedInput=Encoding.UTF8.GetBytes("{ \"model\": \"m\", \"stream\": true, \"messages\": [{\"role\":\"user\",\"content\":\"hi\"}] }");
        var spaced=ChatRequestAdapter.Prepare(spacedInput);
        if(spaced.Body.AsSpan().SequenceEqual(spacedInput))throw new Exception("Whitespace Chat request took raw bypass");checks++;
        var defaultModel=Encoding.UTF8.GetBytes("{\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}");
        var defaulted=ChatRequestAdapter.Prepare(defaultModel);
        if(JsonDocument.Parse(defaulted.Body).RootElement.GetProperty("model").GetString()!="hy4-preview")throw new Exception("Missing Chat model bypassed default insertion");checks++;
        var unstreamed=Encoding.UTF8.GetBytes("{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}");
        if(ChatRequestAdapter.Prepare(unstreamed).Body.AsSpan().SequenceEqual(unstreamed))throw new Exception("Non-streaming Chat request skipped mandatory upstream streaming");checks++;
        input=Encoding.UTF8.GetBytes("{\"model\":\"m\",\"instructions\":\"Keep these rules.\",\"input\":[{\"type\":\"function_call\",\"call_id\":\"a\",\"name\":\"read\",\"arguments\":\"{}\"},{\"type\":\"function_call_output\",\"call_id\":\"a\",\"output\":\"result\"}]}");
        var responses=ResponsesRequestAdapter.Prepare(input,Program.TestKey,r=>OutboundPreparation.IsCompatible(r,settings));
        if(!responses.OutboundCompatible||!responses.Body.AsSpan().SequenceEqual(OutboundPreparation.Prepare(responses.Body,settings,_=>{},32<<20)))throw new Exception("Responses compatibility proof changed its body");checks++;
        input=Encoding.UTF8.GetBytes("{\"model\":\"m\",\"system\":\"Keep these rules.\",\"messages\":[{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"a\",\"name\":\"read\",\"input\":{}}]},{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"a\",\"content\":\"result\"}]}]}");
        var messages=MessagesRequestAdapter.Prepare(input,Program.TestKey,prompts:settings);
        if(!messages.OutboundCompatible||!messages.UpstreamBody.AsSpan().SequenceEqual(OutboundPreparation.Prepare(messages.UpstreamBody,settings,_=>{},32<<20)))throw new Exception("Messages compatibility proof changed its body");checks++;
        var random=new Random(11128);int proven=0;
        for(int attempt=0;attempt<1000;attempt++)
        {
            var list=new JsonArray(new JsonObject{["role"]="system",["content"]="Keep these rules."});
            for(int i=0,n=random.Next(1,8);i<n;i++)
            {
                string role=new[]{"user","assistant","tool","developer"}[random.Next(4)];
                var message=new JsonObject{["role"]=role,["content"]=random.Next(8)==0?"Claude Code":"normal text"};
                if(role=="tool")message["tool_call_id"]="id"+random.Next(3);
                if(role=="assistant"&&random.Next(2)==0)message["tool_calls"]=new JsonArray(new JsonObject{["id"]="id"+random.Next(3),["function"]=new JsonObject{["name"]="read",["arguments"]="{}"}});
                list.Add(message);
            }
            var candidate=new JsonObject{["model"]="m",["messages"]=list};
            bool compatible=OutboundPreparation.IsCompatible(candidate,settings);
            Verify(candidate,compatible);if(compatible)proven++;
        }
        if(proven<10)throw new Exception("Compatibility fuzzing did not exercise enough accepted histories");
        Console.WriteLine(JsonSerializer.Serialize(new{outboundFastPathChecks=checks}));
    }
}
