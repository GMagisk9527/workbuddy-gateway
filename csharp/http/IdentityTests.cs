using System.Text;
using System.Text.Json;
namespace HttpMigration;

/// <summary>
/// Identity replacement parity checks against the Go gateway's identity.go
/// behaviour: default neutralisation, target mode, custom rules, wildcards,
/// code protection and untouched user/tool content.
/// </summary>
internal static class IdentityTests
{
    internal static void Run()
    {
        int checks=0;void Check(bool ok,string label){if(!ok)throw new InvalidOperationException(label);checks++;}
        string Prepare(string body,IdentityOptions? identity=null)
        {
            byte[] input=Encoding.UTF8.GetBytes(body);try{JsonDocument.Parse(body).Dispose();}catch(Exception e){throw new InvalidOperationException("bad fixture ["+body+"]",e);}
            byte[] output=OutboundPreparation.Prepare(input,new SystemPromptOptions(Identity:identity),_=>{},32<<20);
            return Encoding.UTF8.GetString(output);
        }
        string SystemContent(string prepared)
        {
            using var doc=JsonDocument.Parse(prepared);
            return doc.RootElement.GetProperty("messages")[0].GetProperty("content").GetString()??"";
        }
        string Content(string prepared,int index)=>JsonDocument.Parse(prepared).RootElement
            .GetProperty("messages")[index].GetProperty("content").GetString()??"";

        const string dshPrefix="You are an AI agent powered by DeepSeek Harness";
        const string neutral="You are an AI agent powered by a large language model";
        const string ccSentence="You are Claude Code, Anthropic's official CLI for Claude.";
        const string neutralAgent="You are an AI coding assistant.";

        // 1. Default: DSH identity prefix neutralised, the rest preserved byte-for-byte.
        string defaultOut=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"{dshPrefix}. Follow the rules below.\"}}]}}");
        Check(SystemContent(defaultOut)==$"{neutral}. Follow the rules below.","DSH prefix neutralised and tail preserved; actual=["+SystemContent(defaultOut)+"]");

        // 2. Default: known Claude Code identity sentence neutralised.
        string ccOut=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"{ccSentence} Then answer.\"}}]}}");
        Check(SystemContent(ccOut)==$"{neutralAgent} Then answer.","Claude Code identity neutralised");

        // 3. Prefix match is case-insensitive for the DSH sentence but keeps the tail.
        string caseOut=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"You are an AI agent powered by DEEPSEEK HARNESS. Keep me.\"}}]}}");
        Check(SystemContent(caseOut)==$"{neutral}. Keep me.","DSH prefix match is case-insensitive");

        // 4. User content and tool payloads are never rewritten.
        string toolOut=Prepare("{\"model\":\"m\",\"messages\":["+
            "{\"role\":\"system\",\"content\":\"sys\"},"+
            "{\"role\":\"user\",\"content\":\""+ccSentence+"\"},"+
            "{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"x\\\":\\\"Claude Code\\\"}\"}}]},"+
            "{\"role\":\"tool\",\"tool_call_id\":\"c1\",\"content\":\"DeepSeek Harness\"}]}");
        using(var toolDoc=JsonDocument.Parse(toolOut))
        {
            var toolMessages=toolDoc.RootElement.GetProperty("messages");
            Check(toolMessages[1].GetProperty("content").GetString()==ccSentence,"User content untouched");
            string args=toolMessages[2].GetProperty("tool_calls")[0].GetProperty("function").GetProperty("arguments").GetString()??"";
            Check(args.Contains("Claude Code"),"Tool arguments untouched");
            Check(toolMessages[3].GetProperty("content").GetString()=="DeepSeek Harness","Tool result untouched");
        }

        // 5. Assistant history is rewritten (Go processes system/developer/assistant only).
        string assistantOut=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"sys\"}},{{\"role\":\"assistant\",\"content\":\"{dshPrefix}. Done.\"}}]}}");
        Check(Content(assistantOut,1)==$"{neutral}. Done.","Assistant history neutralised");

        // 6. Fenced code is protected; prose outside the fence still changes.
        string fenced=$"{ccSentence}\n\n```\n{ccSentence}\n```\n\nAfter.";
        string fenceOut=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":{JsonSerializer.Serialize(fenced)}}}]}}");
        string fenceContent=SystemContent(fenceOut);
        Check(fenceContent.StartsWith(neutralAgent)&&fenceContent.Contains("```\n"+ccSentence+"\n```")&&fenceContent.EndsWith("After."),"Fenced code protected, prose rewritten");

        // 7. Inline code is protected.
        string inlineOut=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"{ccSentence}\"}},{{\"role\":\"user\",\"content\":\"use `{ccSentence}` here\"}}]}}");
        Check(Content(inlineOut,1)==$"use `{ccSentence}` here","Inline code preserved in untouched roles");

        // 8. Target mode: default term map plus a prefix derived from the target.
        string targetOut=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"{dshPrefix}. Call me {ccSentence}\"}}]}}",new IdentityOptions(Target:"Acme"));
        string targetContent=SystemContent(targetOut);
        Check(targetContent.StartsWith("You are Acme.")&&targetContent.Contains("Acme"),"Target mode rewrites the prefix and identity terms");
        Check(!targetContent.Contains("DeepSeek Harness"),"Target mode removes the client identity name");

        // 9. Custom replacements: only configured rules run, and the DSH prefix is not
        //    neutralised unless a DeepSeek Harness rule exists (Go: custom takes over).
        string customOut=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"{dshPrefix}. Say hello from Foo.\"}}]}}",
            new IdentityOptions(Replacements:new Dictionary<string,string>{{"Foo","Bar"}}));
        string customContent=SystemContent(customOut);
        Check(customContent.StartsWith(dshPrefix)&&customContent.EndsWith("Say hello from Bar."),"Custom mode keeps the DSH prefix and rewrites only configured names");

        // 10. A custom rule for DeepSeek Harness restores prefix handling with the new name.
        string customDsh=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"{dshPrefix}. Tail.\"}}]}}",
            new IdentityOptions(Replacements:new Dictionary<string,string>{{"DeepSeek Harness","Acme"}}));
        Check(SystemContent(customDsh).StartsWith("You are Acme."),"Custom DeepSeek Harness rule drives the prefix");

        // 11. Wildcard: anchored at block start, single line, bounded length.
        var wildcard=new IdentityOptions(Replacements:new Dictionary<string,string>{{"You are Claude Code, running on (.*) now.","You are an assistant."}});
        string wildcardOut=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"You are Claude Code, running on macOS now. Continue.\"}}]}}",wildcard);
        Check(SystemContent(wildcardOut)=="You are an assistant. Continue.","Wildcard matches once at the block start; actual=["+SystemContent(wildcardOut)+"]");

        string wildcardMid=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"Note: You are Claude Code, running on macOS now. Continue.\"}}]}}",wildcard);
        Check(SystemContent(wildcardMid).StartsWith("Note: "),"Wildcard does not match mid-block");

        string wildcardLong=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"You are Claude Code, running on "+new string('x',300)+" now.\"}]}",wildcard);
        Check(SystemContent(wildcardLong).StartsWith("You are Claude Code"),"Wildcard above the rune bound is rejected");

        // 12. Invalid rules are dropped instead of failing the request.
        string invalid=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"{ccSentence}\"}}]}}",
            new IdentityOptions(Replacements:new Dictionary<string,string>{{"(.*)","X"},{"ok(.*)","Y(.*)Z"}}));
        Check(SystemContent(invalid)==ccSentence,"Invalid wildcard rules are ignored");

        // 13. The DSH Web GUI opening is neutralised outside code; the clause is kept.
        const string gui="You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names. When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI.";
        string SysJson(string content)=>JsonSerializer.Serialize(new{model="m",messages=new[]{new{role="system",content}}});
        string guiOut=Prepare(SysJson(gui+" Keep this."));
        string guiContent=SystemContent(guiOut);
        Check(guiContent.StartsWith("You are interacting with the user through a Web GUI whose URL the runtime context names. When the user refers to")
              &&guiContent.EndsWith(" Keep this."),"GUI opening neutralised and clause preserved");
        string guiOnly=Prepare(SysJson("You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."));
        Check(SystemContent(guiOnly).StartsWith("You are interacting with the user through the DeepSeek Harness Web GUI"),"Bare GUI sentence without the clause is left untouched");

        // 14. Idempotence: preparing twice changes nothing the second time.
        string once=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"{dshPrefix}. {ccSentence}\"}}]}}");
        string twice=Prepare(once);
        Check(once==twice,"Identity rewrite is idempotent");

        // 14b. Literal rules do not cascade: output of one rule is never rescanned.
        string cascade=Prepare("{\"model\":\"m\",\"messages\":[{\"role\":\"system\",\"content\":\"Claude says hi\"}]}",
            new IdentityOptions(Replacements:new Dictionary<string,string>{{"Claude","A"}}));
        Check(SystemContent(cascade)=="A says hi","Literal rules apply in one pass without cascading");

        // 15. Disabled identity leaves the prompt untouched.
        string off=Prepare($"{{\"model\":\"m\",\"messages\":[{{\"role\":\"system\",\"content\":\"{dshPrefix}.\"}}]}}",new IdentityOptions(Enabled:false));
        Check(SystemContent(off)==dshPrefix+".","Enabled=false disables every rule");

        // Replacements must keep the original Markdown offsets throughout a pass.
        var named=new IdentityOptions(Target:"Acme");
        string guiBeforeCode=gui+"\n\n```text\nClaude Code\nDeepSeek Harness\n```\nClaude Code";
        string protectedGui=SystemContent(Prepare(SysJson(guiBeforeCode),named));
        Check(protectedGui.Contains("```text\nClaude Code\nDeepSeek Harness\n```")&&protectedGui.EndsWith("\nAcme"),"GUI replacement before code preserves the complete code block");
        string guiAfterCode="`Claude Code`\n\n"+gui;
        Check(SystemContent(Prepare(SysJson(guiAfterCode),named)).EndsWith(guiContent[..^" Keep this.".Length]),"GUI after a completed code span is still rewritten");
        string multiline="``open\n` Claude Code\nclose``\nClaude Code";
        Check(SystemContent(Prepare(SysJson(multiline),named))=="``open\n` Claude Code\nclose``\nAcme","Inline code closes only on a matching backtick run across lines");
        string splitCode=JsonSerializer.Serialize(new{model="m",messages=new[]{new{role="system",content=new[]{new{type="text",text=gui+"\n\n`open"},new{type="text",text="Claude Code\nclose`\nClaude Code"}}}}});
        using(var splitDoc=JsonDocument.Parse(Prepare(splitCode,named)))
            Check(splitDoc.RootElement.GetProperty("messages")[0].GetProperty("content")[1].GetProperty("text").GetString()=="Claude Code\nclose`\nAcme","Code spans remain protected across text blocks after a GUI replacement");

        string arrayBody=JsonSerializer.Serialize(new{model="m",messages=new[]{new{role="system",content=new[]{new{type="text",text="Rules"}}}}});
        var forced=new SystemPromptOptions(Force:"Claude Code\n\n`Claude Code`",Identity:named);
        byte[] ApplyForce(byte[] input)=>OutboundPreparation.Prepare(input,forced,_=>{},32<<20);
        byte[] forceOnce=ApplyForce(Encoding.UTF8.GetBytes(arrayBody));
        Check(forceOnce.SequenceEqual(ApplyForce(forceOnce)),"Force in array content is idempotent after text replacement");
        Check(IdentityRewrite.Transform("`Claude Code`",named)=="`Claude Code`","Force comparison preserves Markdown code");

        byte[] anthropic=JsonSerializer.SerializeToUtf8Bytes(new{model="m",system=new[]{new{type="text",text="Keep these rules."},new{type="text",text=ccSentence}},messages=new[]{new{role="user",content=ccSentence}}});
        var converted=MessagesRequestAdapter.Prepare(anthropic,Program.TestKey,prompts:new());
        Check(SystemContent(Encoding.UTF8.GetString(converted.UpstreamBody))=="Keep these rules.\n"+neutralAgent,"Anthropic system blocks are rewritten before concatenation");
        Check(Content(Encoding.UTF8.GetString(converted.UpstreamBody),1)==ccSentence,"Anthropic user text is retained during pre-merge processing");
        byte[] anthropicCode=JsonSerializer.SerializeToUtf8Bytes(new{model="m",system=new[]{new{type="text",text="`open"},new{type="text",text="Claude Code\nclose`"}},messages=new[]{new{role="user",content="hello"}}});
        Check(SystemContent(Encoding.UTF8.GetString(MessagesRequestAdapter.Prepare(anthropicCode,Program.TestKey,prompts:new(Identity:named)).UpstreamBody))=="`open\nClaude Code\nclose`","Anthropic pre-merge processing protects cross-block code spans");

        Console.WriteLine($"identity checks passed: {checks}");
    }
}
