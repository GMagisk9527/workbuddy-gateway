using System.Linq;
using System.Text;
using System.Text.Json;
using System.Collections.Immutable;
using TypedMigration;

namespace HttpMigration;

/// <summary>
/// Prompt identity replacement, aligned with the Go gateway's identity.go /
/// dshguicontext.go behaviour. Only system/developer/assistant text is touched;
/// user, tool, reasoning and tool arguments are never rewritten, and matches
/// that overlap Markdown code are skipped.
/// </summary>
public sealed record IdentityOptions(bool? Enabled=null,string Target="",IReadOnlyDictionary<string,string>? Replacements=null)
{
    public static IdentityOptions Default{get;}=new();
    // Keep configuration out of logs, matching SystemPromptOptions.
    public override string ToString()=>"IdentityOptions (redacted)";
}

internal sealed record IdentityRule(string Old,string New,bool Wildcard,string Prefix,string Suffix)
{
    internal int FixedLength=>Wildcard?Old.Length-3:Old.Length;
    internal string Replace(string text,out bool matched)
    {
        matched=false;
        if(!Wildcard)
        {
            int first=text.IndexOf(Old,StringComparison.Ordinal);
            if(first<0)return text;
            matched=true;
            return string.Concat(text.AsSpan(0,first),New,text.AsSpan(first+Old.Length));
        }
        if(!text.StartsWith(Prefix,StringComparison.Ordinal))return text;
        ReadOnlySpan<char> remainder=text.AsSpan(Prefix.Length);
        int limit=4*IdentitySettings.WildcardMaxRunes+Suffix.Length;
        if(remainder.Length>limit)remainder=remainder[..limit];
        int newline=remainder.IndexOfAny('\r','\n');
        if(newline>=0)remainder=remainder[..newline];
        int end=remainder.IndexOf(Suffix,StringComparison.Ordinal);
        if(end<0||IdentitySettings.RuneCount(remainder[..end])>IdentitySettings.WildcardMaxRunes)return text;
        matched=true;
        if(New==Old)return text; // mapping a default to itself disables the rule
        return string.Concat(New,text.AsSpan(Prefix.Length+end+Suffix.Length));
    }
}

internal sealed class IdentitySettings
{
    internal const int WildcardMaxRunes=256;
    const string WildcardToken="(.*)";
    const string DshPrefix="You are an AI agent powered by DeepSeek Harness";
    const string NeutralPrefix="You are an AI agent powered by a large language model";
    const string NeutralAgent="You are an AI coding assistant.";
    const string GuiSentence="You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names.";
    const string GuiNeutral="You are interacting with the user through a Web GUI whose URL the runtime context names.";
    // Go only rewrites the full opening: the sentence plus its "this page" clause.
    const string GuiClause=" When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI.";
    const string GuiOpening=GuiSentence+GuiClause;
    static readonly string[] ClaudeCodePrefixes=
    [
        "You are Claude Code, Anthropic's official CLI tool for Claude.",
        "You are Claude Code, Anthropic's official CLI for Claude.",
        "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
        "You are a Claude agent, built on Anthropic's Claude Agent SDK.",
        "You are Claude Code, an AI assistant that orchestrates software engineering tasks across multiple workers.",
        "You are Claude Code.",
    ];
    static readonly string[] DefaultTerms=["Claude Code","Claude","DeepSeek Harness","Codex","ZCode"];

    internal readonly bool Enabled;internal readonly string Prefix;internal readonly IdentityRule[] Rules;internal readonly bool Custom;
    IdentitySettings(bool enabled,string prefix,IdentityRule[] rules,bool custom){Enabled=enabled;Prefix=prefix;Rules=rules;Custom=custom;}

    internal static IdentitySettings Default{get;}=new(true,NeutralPrefix,[],false);

    internal static IdentitySettings Build(IdentityOptions? options)
    {
        if(options is null||(options.Enabled is null&&string.IsNullOrWhiteSpace(options.Target)&&(options.Replacements is null||options.Replacements.Count==0)))
            return Default;
        bool enabled=options.Enabled??true;
        string target=string.IsNullOrWhiteSpace(options.Target)?"WorkBuddy":options.Target.Trim();
        string prefixTarget=target;var rules=new List<IdentityRule>();
        bool custom=options.Replacements is{Count:>0};
        if(custom)
        {
            foreach(var pair in options.Replacements!)
            {
                if(Compile(pair.Key,pair.Value,out var rule))rules.Add(rule);
            }
            if(options.Replacements!.TryGetValue("DeepSeek Harness",out var replacement)&&!string.IsNullOrWhiteSpace(replacement))
                prefixTarget=replacement;
        }
        else
        {
            foreach(var term in DefaultTerms)rules.Add(new(term,target,false,"",""));
            rules.Add(new("workspace root","workspace base",false,"",""));
            rules.Add(new("are paths the user","are paths that the user",false,"",""));
        }
        // Longest fixed text first; exact literals win ties, then ordinal order.
        rules.Sort((a,b)=>a.FixedLength!=b.FixedLength?b.FixedLength.CompareTo(a.FixedLength):
            a.Wildcard!=b.Wildcard?(a.Wildcard?1:0)-(b.Wildcard?1:0):
            string.CompareOrdinal(a.Old,b.Old));
        return new(enabled,"You are "+prefixTarget,[..rules],custom);
    }

    static bool Compile(string old,string value,out IdentityRule rule)
    {
        rule=new IdentityRule(old,value,false,"","");
        if(string.IsNullOrWhiteSpace(old))return false;
        int count=0;
        for(int i=old.IndexOf(WildcardToken,StringComparison.Ordinal);i>=0;i=old.IndexOf(WildcardToken,i+WildcardToken.Length,StringComparison.Ordinal))count++;
        if(count==0)return true;
        if(count>1)return false;
        int at=old.IndexOf(WildcardToken,StringComparison.Ordinal);
        string prefix=old[..at],suffix=old[(at+WildcardToken.Length)..];
        if(string.IsNullOrWhiteSpace(prefix)||string.IsNullOrWhiteSpace(suffix))return false;
        if(old.Contains('\r')||old.Contains('\n'))return false;
        rule=new IdentityRule(old,value,true,prefix,suffix);
        return true;
    }

    internal bool DshNameRulePresent=>Rules.Any(r=>r.Old=="DeepSeek Harness");

    /// <summary>Returns (old,new) when a leading identity prefix should be replaced.</summary>
    internal (string Old,string New) PrefixReplacement(string text)
    {
        if(Rules.Length==0&&!Custom)
        {
            foreach(var candidate in ClaudeCodePrefixes)
            {
                if(!text.StartsWith(candidate,StringComparison.Ordinal))continue;
                if(text.Length==candidate.Length||text[candidate.Length] is ' ' or '\t' or '\r' or '\n')
                    return (candidate,NeutralAgent);
            }
        }
        if((!Custom||DshNameRulePresent)&&text.Length>=DshPrefix.Length&&
            string.Equals(text[..DshPrefix.Length],DshPrefix,StringComparison.OrdinalIgnoreCase))
            return (DshPrefix,Prefix);
        return ("","");
    }

    internal static int RuneCount(ReadOnlySpan<char> text)
    {
        int count=0;
        while(!text.IsEmpty){count++;var status=System.Text.Rune.DecodeFromUtf16(text,out _,out int used);if(status!=System.Buffers.OperationStatus.Done||used<=0)break;text=text[used..];}
        return count;
    }
    internal bool MightMatch(string text,bool allowWildcard=true)
    {
        if(!Enabled||text.Length==0)return false;
        if(PrefixReplacement(text).Old.Length>0||(!Custom&&text.Contains(GuiOpening,StringComparison.Ordinal)))return true;
        return Rules.Any(r=>r.Wildcard?allowWildcard&&text.StartsWith(r.Prefix,StringComparison.Ordinal):text.Contains(r.Old,StringComparison.Ordinal));
    }
    internal string Rewrite(string text,IReadOnlyList<(int Start,int End)> code,int offset,bool allowPrefix,bool allowWildcard=true)
    {
        if(!MightMatch(text,allowWildcard))return text;
        (string oldPrefix,string newPrefix)=allowPrefix?PrefixReplacement(text):("","");
        string? wildcardRewritten=null;int consumed=0;
        if(allowPrefix&&allowWildcard)
        {
            foreach(var rule in Rules)
            {
                if(!rule.Wildcard)continue;
                var candidate=rule.Replace(text,out bool matched);
                if(matched){wildcardRewritten=candidate;consumed=text.Length-candidate.Length+rule.New.Length;
                    if(rule.New==rule.Old)return text;break;}
            }
            if(wildcardRewritten is not null&&OverlapsCode(code,offset,consumed))wildcardRewritten=null;
        }
        var builder=new StringBuilder(text.Length+16);
        int pos=0;
        if(wildcardRewritten is not null)
        {
            builder.Append(wildcardRewritten.AsSpan(0,wildcardRewritten.Length-(text.Length-consumed)));
            pos=consumed;oldPrefix="";
        }
        if(oldPrefix.Length>0&&!OverlapsCode(code,offset,oldPrefix.Length))
        {
            builder.Append(newPrefix);pos=Math.Max(pos,oldPrefix.Length);
        }
        // Replace outside code spans only, preserving every code byte.
        int cursor=pos;
        foreach(var (start,end) in code)
        {
            int sliceStart=Math.Max(cursor,start-offset),sliceEnd=Math.Min(text.Length,end-offset);
            if(sliceEnd<=sliceStart)continue;
            builder.Append(ReplaceLiterals(text[cursor..sliceStart]));
            builder.Append(text.AsSpan(sliceStart,sliceEnd-sliceStart));
            cursor=sliceEnd;
        }
        builder.Append(ReplaceLiterals(text[cursor..]));
        string result=builder.ToString();
        return result==text?text:result;
    }

    static bool OverlapsCode(IReadOnlyList<(int Start,int End)> code,int offset,int length)
    {
        foreach(var (start,end) in code)
            if(start<offset+length&&end>offset)return true;
        return false;
    }

    string ReplaceLiterals(string text)
    {
        // Single left-to-right pass like Go's strings.Replacer: output is never
        // rescanned. GUI and literal replacements share the original code offsets.
        StringBuilder? builder=null;int copied=0;
        for(int i=0;i<text.Length;)
        {
            int match=text.Length;string old="",replacement="";
            int gui=Custom?-1:text.IndexOf(GuiOpening,i,StringComparison.Ordinal);
            if(gui>=0){match=gui;old=GuiOpening;replacement=GuiNeutral+GuiClause;}
            foreach(var rule in Rules)
            {
                if(rule.Wildcard)continue;
                int at=text.IndexOf(rule.Old,i,StringComparison.Ordinal);
                if(at>=0&&at<match){match=at;old=rule.Old;replacement=rule.New;}
            }
            if(old.Length==0)break;
            builder??=new StringBuilder(text.Length);
            builder.Append(text.AsSpan(copied,match-copied)).Append(replacement);
            copied=i=match+old.Length;
        }
        if(builder is null)return text;
        builder.Append(text.AsSpan(copied));
        return builder.ToString();
    }

    // ---- Markdown code ranges (fenced blocks, indented blocks, inline spans) ----

    internal static List<(int Start,int End)> CodeRanges(string text)
    {
        var spanEnds=CodeSpanEnds(text);
        var ranges=new List<(int Start,int End)>();
        void Add(int start,int end)
        {
            if(ranges.Count>0&&start<=ranges[^1].End)ranges[^1]=(ranges[^1].Start,Math.Max(end,ranges[^1].End));
            else ranges.Add((start,end));
        }
        char fence='\0';int fenceLength=0,inlineEnd=0;
        for(int start=0;start<text.Length;)
        {
            int end=text.IndexOf('\n',start);
            if(end<0)end=text.Length;
            string line=text[start..end].TrimEnd('\r');
            int next=Math.Min(end+1,text.Length);
            bool isFence=false;
            if(inlineEnd<=start)
            {
                var (marker,length,info)=FenceDelimiter(line);
                if(marker!=0)
                {
                    if(fence==0&&(marker!='`'||!info.Contains('`'))){fence=marker;fenceLength=length;isFence=true;}
                    else if(fence==marker&&length>=fenceLength&&info.Trim(' ','\t').Length==0){fence='\0';fenceLength=0;isFence=true;}
                }
            }
            int indent=line.Length-line.TrimStart(' ').Length;
            bool indented=indent>=4||line.StartsWith('\t');
            if(isFence||fence!=0||(indented&&inlineEnd<=start))Add(start,next);
            else
            {
                for(int pos=Math.Max(start,inlineEnd);pos<end;)
                {
                    if(text[pos]=='\\'&&pos+1<end&&(text[pos+1]=='\\'||text[pos+1]=='`')){pos+=2;continue;}
                    if(text[pos]!='`'){pos++;continue;}
                    int n=1;while(pos+n<end&&text[pos+n]=='`')n++;
                    if(spanEnds.TryGetValue(pos,out int close)){Add(pos,close);inlineEnd=pos=close;}
                    else pos+=n;
                }
            }
            start=next;
        }
        return ranges;
    }

    static (char Marker,int Length,string Info) FenceDelimiter(string line)
    {
        string trimmed=line.TrimStart(' ');
        if(line.Length-trimmed.Length>3||trimmed.Length<3||(trimmed[0]!='`'&&trimmed[0]!='~'))return ('\0',0,"");
        int n=1;while(n<trimmed.Length&&trimmed[n]==trimmed[0])n++;
        return n<3?('\0',0,""):(trimmed[0],n,trimmed[n..]);
    }

    static Dictionary<int,int> CodeSpanEnds(string text)
    {
        var runs=new List<(int Start,int Length,int Paragraph)>();
        int paragraph=0;
        for(int start=0;start<text.Length;)
        {
            int end=text.IndexOf('\n',start);
            if(end<0)end=text.Length;
            string line=text[start..end].TrimEnd('\r');
            var (marker,_,info)=FenceDelimiter(line);
            bool fenceLine=marker!=0&&(marker!='`'||!info.Contains('`'));
            if(line.Trim(' ','\t').Length==0||fenceLine)paragraph++;
            if(fenceLine){start=end+1;continue;}
            for(int pos=start;pos<end;)
            {
                if(text[pos]!='`'){pos++;continue;}
                int n=1;while(pos+n<end&&text[pos+n]=='`')n++;
                runs.Add((pos,n,paragraph));pos+=n;
            }
            start=end+1;
        }
        var ends=new Dictionary<int,int>();var next=new Dictionary<int,int>();
        int current=paragraph;
        for(int i=runs.Count-1;i>=0;i--)
        {
            var run=runs[i];
            if(run.Paragraph!=current){next.Clear();current=run.Paragraph;}
            if(next.TryGetValue(run.Length,out int close))ends[run.Start]=close+run.Length;
            next[run.Length]=run.Start;
        }
        return ends;
    }
}

/// <summary>Applies identity replacement to a Chat-shaped messages array.</summary>
internal static class IdentityRewrite
{
    // Anthropic text blocks have independent opening prefixes. Apply the text
    // policy before the typed mapper concatenates them; retain all non-text blocks.
    internal static InputRequest BeforeMap(InputRequest input,JsonElement root,IdentityOptions? options)
    {
        if(!IdentitySettings.Build(options).Enabled)return input;
        string? system=input.SystemText;
        if(root.TryGetProperty("system",out var rawSystem)&&rawSystem.ValueKind is JsonValueKind.String or JsonValueKind.Array)
        {
            var message=OutboundPreparation.V.Object(("role",OutboundPreparation.V.Text("system")),("content",OutboundPreparation.V.From(rawSystem)));
            if(Apply([message],options))
            {
                var content=message.Get("content")!;
                system=content.Is(JsonValueKind.String)?content.Str():string.Join("\n",content.Arr.Where(p=>p.S("type")=="text"&&p.S("text")!="").Select(p=>p.S("text")));
            }
        }
        var messages=input.Messages.Select(message=>
        {
            if(message.Role.Kind is not (RoleKind.System or RoleKind.Developer or RoleKind.Assistant))return message;
            OutboundPreparation.V? content=message.Content switch
            {
                TypedMigration.StringContent text=>OutboundPreparation.V.Text(text.Text),
                BlocksContent blocks=>OutboundPreparation.V.Array(blocks.Blocks.OfType<TextBlock>().Select(t=>OutboundPreparation.V.Object(("type",OutboundPreparation.V.Text("text")),("text",OutboundPreparation.V.Text(t.Text))))),
                _=>null
            };
            if(content is null)return message;
            var wrapper=OutboundPreparation.V.Object(("role",OutboundPreparation.V.Text(message.Role.WireName)),("content",content));
            if(!Apply([wrapper],options))return message;
            content=wrapper.Get("content")!;
            if(message.Content is TypedMigration.StringContent)return message with{Content=new TypedMigration.StringContent(content.Str())};
            int i=0;return message with{Content=new BlocksContent(((BlocksContent)message.Content).Blocks.Select(b=>b is TextBlock?new TextBlock(content.Arr[i++].S("text")):b).ToImmutableArray())};
        }).ToImmutableArray();
        return input with{SystemText=system,Messages=messages};
    }
    internal static bool Apply(List<OutboundPreparation.V> messages,IdentityOptions? options,bool allowWildcard=true)
    {
        var settings=IdentitySettings.Build(options);
        if(!settings.Enabled)return false;
        bool changed=false;
        foreach(var message in messages)
        {
            var role=message.Role;
            if(role is not ("system" or "developer" or "assistant"))continue;
            var parts=TextParts(message);
            if(parts.Count==0||!parts.Any(p=>settings.MightMatch(p.Text,allowWildcard)))continue;
            string joined=string.Join("\n",parts.Select(p=>p.Text));
            var code=IdentitySettings.CodeRanges(joined);
            foreach(var (container,key,text,offset) in parts)
            {
                string next=settings.Rewrite(text,code,offset,true,allowWildcard);
                if(next==text)continue;
                container.Set(key,OutboundPreparation.V.Text(next));
                changed=true;
            }
        }
        return changed;
    }

    /// <summary>Rewrite a standalone string, preserving its code context (used for force dedup).</summary>
    internal static string Transform(string text,IdentityOptions? options)
    {
        if(string.IsNullOrEmpty(text))return text;
        var settings=IdentitySettings.Build(options);
        if(!settings.Enabled)return text;
        if(!settings.MightMatch(text,false))return text;
        return settings.Rewrite(text,IdentitySettings.CodeRanges(text),0,true,false);
    }
    static List<(OutboundPreparation.V Container,string Key,string Text,int Offset)> TextParts(OutboundPreparation.V message)
    {
        var parts=new List<(OutboundPreparation.V,string,string,int)>();
        var content=message.Get("content");
        if(content is null)return parts;
        if(content.Is(JsonValueKind.String))
        {
            var text=content.Str();
            if(text.Length>0)parts.Add((message,"content",text,0));
            return parts;
        }
        if(!content.Is(JsonValueKind.Array))return parts;
        int offset=0;
        foreach(var block in content.Arr)
        {
            if(block.S("type")!="text")continue;
            var text=block.S("text");
            if(text.Length==0)continue;
            if(parts.Count>0)offset+=1;
            parts.Add((block,"text",text,offset));
            offset+=text.Length;
        }
        return parts;
    }
}
