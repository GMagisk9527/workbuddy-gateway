using System.Text;
using System.Text.Json;
namespace HttpMigration;
internal static class OutboundTests
{
    internal static void Run()
    {
        int checks=0;void Check(bool b,string s){if(!b)throw new InvalidOperationException(s);checks++;}
        byte[] Encode(string s)=>Encoding.UTF8.GetBytes(s);
        var settings=new SystemPromptOptions();
        byte[] unchanged=Encode("{\"model\":\"hy3\",\"messages\":[{\"role\":\"system\",\"content\":\"Claude Code DeepSeek Harness `x\\ny`\"},{\"role\":\"user\",\"content\":\"原文\"}]}");
        Check(ReferenceEquals(unchanged,OutboundPreparation.Prepare(unchanged,settings,_=>throw new Exception("Unexpected reservation"),32<<20)),"Unchanged input uses original bytes and preserves identities/code");
        byte[] large=JsonSerializer.SerializeToUtf8Bytes(new{model="hy3",messages=new[]{new{role="user",content=new string('x',1<<20)}}});
        _=OutboundPreparation.Prepare(large,settings,_=>{},32<<20);
        long allocated=GC.GetAllocatedBytesForCurrentThread();int reserved=0;byte[] output=OutboundPreparation.Prepare(large,settings,n=>reserved=n,32<<20);allocated=GC.GetAllocatedBytesForCurrentThread()-allocated;
        using(var doc=JsonDocument.Parse(output))Check(doc.RootElement.GetProperty("messages")[1].GetProperty("content").GetString()!.Length==1<<20&&reserved==output.Length,"Large user content retained and complete output reserved");
        Check(allocated<6L*large.Length,"Structural pass does not build a decoded full content tree");
        int attempts=0;bool denied=false;
        try{OutboundPreparation.Prepare(large,settings,_=>{attempts++;throw new RequestFailure("synthetic budget",503);},32<<20);}catch(RequestFailure e){denied=e.Status==503;}
        Check(denied&&attempts==1,"Memory refusal propagates before output allocation");
        bool bounded=false;try{OutboundPreparation.Prepare(large,settings,_=>throw new Exception("Reserve after size rejection"),large.Length);}catch(RequestFailure e){bounded=e.Status==413;}
        Check(bounded,"System injection expansion is size checked before reservation");
        bool invalid=false;try{new SystemPromptOptions(Force:new string('x',65537)).Validate();}catch(ArgumentException){invalid=true;}
        Check(invalid,"Prompt configuration bounded");
        byte[] blocks=Encode("{\"model\":\"hy3\",\"messages\":[{\"role\":\"system\",\"content\":[{\"type\":\"text\"}]}]}");
        byte[] first=OutboundPreparation.Prepare(blocks,new(Force:"force"),_=>{},4096);
        Check(ReferenceEquals(first,OutboundPreparation.Prepare(first,new(Force:"force"),_=>throw new Exception("Duplicated force"),4096)),"Missing text field is safe; force is idempotent");
        byte[] body=Encode("{\"model\":\"deepseek-model\",\"reasoning_effort\":\"high\",\"messages\":[{\"role\":\"assistant\",\"content\":\"answer\",\"reasoning_content\":\"actual trace\"}]}");
        using(var doc=JsonDocument.Parse(OutboundPreparation.Prepare(body,settings,_=>{},4096)))
        {var m=doc.RootElement.GetProperty("messages")[1];Check(m.GetProperty("reasoning_content").GetString()=="actual trace"&&m.GetProperty("reasoning").GetString()=="actual trace"&&m.GetProperty("content").GetString()=="answer","History replay preserves actual trace and answer");}
        Console.WriteLine(JsonSerializer.Serialize(new{outboundChecks=checks,largeInputBytes=large.Length,allocatedBytes=allocated,realModelCalls=0}));
    }
}
