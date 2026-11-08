using System.Collections.Immutable;
using System.Text;

namespace StreamingMigration;

public sealed record ResponseIdentityRule(string Target, string Original);

// Only the current request owns this plan. Substitutions are a single pass over
// upstream text, never tools or metadata, and generated text is never rescanned.
public sealed class ResponseIdentityPlan
{
    public ImmutableArray<ResponseIdentityRule> Rules { get; }
    public int HoldLength { get; }
    public bool Active => !Rules.IsEmpty;
    public ResponseIdentityPlan(IEnumerable<ResponseIdentityRule> rules)
    {
        Rules=rules.OrderByDescending(r=>r.Target.Length).ThenBy(r=>r.Target,StringComparer.Ordinal).ToImmutableArray();
        if(Rules.Length>256||Rules.Any(r=>r.Target.Length==0||Encoding.UTF8.GetByteCount(r.Target)>4096||Encoding.UTF8.GetByteCount(r.Original)>16384))throw new ArgumentException("Invalid response restoration bounds");
        HoldLength=Rules.IsEmpty?0:Rules.Max(r=>r.Target.Length);
    }
    public override string ToString()=>"ResponseIdentityPlan (redacted)";
    public ResponseIdentityRestorer CreateRestorer()=>new(this);
    internal static bool Identifier(char c)=>char.IsAsciiLetterOrDigit(c)||c=='_';
    public string Restore(string text)=>!Active?text:Process(text,text.Length,new(),true).Text;

    internal (string Text,int Consumed) Process(string text,int limit,AddressContext address,bool final)
    {
        if(limit<=0)return ("",0);
        StringBuilder? output=null;int cursor=0,scan=0;
        while(scan<text.Length)
        {
            int start=text.Length,end=0;string replacement="";
            foreach(var rule in Rules)
            {
                for(int from=scan;from<text.Length;)
                {
                    int at=text.IndexOf(rule.Target,from,StringComparison.Ordinal);if(at<0)break;
                    int next=at+rule.Target.Length;from=at+1;
                    if(at>0&&Identifier(text[at-1])||next<text.Length&&Identifier(text[next]))continue;
                    if(at<start||at==start&&next>end){start=at;end=next;replacement=rule.Original;}break;
                }
            }
            if(start>=limit)break;
            if(end>limit||!final&&end==text.Length)
            {
                if(output is null)return(text[..start],start);
                output.Append(text.AsSpan(cursor,start-cursor));return(output.ToString(),start);
            }
            address.Advance(text.AsSpan(scan,start-scan));bool protect=address.Protects(text,end);
            address.Advance(text.AsSpan(start,end-start));scan=end;
            if(protect)continue;
            output??=new StringBuilder(limit);output.Append(text.AsSpan(cursor,start-cursor)).Append(replacement);cursor=end;
        }
        if(output is null)return(text[..limit],limit);
        output.Append(text.AsSpan(cursor,limit-cursor));return(output.ToString(),limit);
    }
    internal struct AddressContext
    {
        bool path,dot;char previous,quote;
        internal void Advance(ReadOnlySpan<char> value)
        {
            foreach(char c in value)
            {
                if(c is '\"' or '\'' or '`'&&(quote==c||quote==0&&!Identifier(previous)))
                {if(quote==0)quote=c;else this=new();previous='\0';dot=false;continue;}
                if(char.IsWhiteSpace(c)||"<>，。！？；".Contains(c))
                {if(quote==0||c is '\n' or '\r')this=new();else{previous='\0';dot=false;}continue;}
                if(dot&&Identifier(c))path=true;
                if(c is '/' or '\\' or ':' or '@')path=true;
                dot=c=='.'&&Identifier(previous);previous=c<128?c:'\0';
            }
        }
        internal bool Protects(string text,int end)
        {
            if(path||previous is '.' or '-')return true;
            if(end==text.Length)return false;
            return text[end] is '/' or '\\' or ':' or '@' or '-'||text[end]=='.'&&end+1<text.Length&&(Identifier(text[end+1])||text[end+1]=='-');
        }
    }
    public ChatChunk RestoreChunk(ChatChunk chunk,Dictionary<(int Choice,string Channel),ResponseIdentityRestorer> restorers)
    {
        return chunk with{Choices=chunk.Choices.Select(choice=>
        {
            string Rewrite(string channel,string value,bool finish)
            {
                var key=(choice.Index,channel);if(value.Length==0&&!restorers.ContainsKey(key))return "";
                if(!restorers.TryGetValue(key,out var restorer)){if(restorers.Count>=512)throw new ConversionFailure("output_budget_exceeded");restorers[key]=restorer=CreateRestorer();}
                string text=restorer.Write(value);return finish?text+restorer.Flush():text;
            }
            bool terminal=choice.Finish.Length>0||!choice.Tools.IsEmpty;
            return choice with{Text=Rewrite("content",choice.Text,terminal||choice.Reasoning.Length>0),Reasoning=Rewrite("reasoning_content",choice.Reasoning,terminal||choice.Text.Length>0||choice.Refusal.Length>0)};
        }).ToImmutableArray()};
    }
}

public sealed class ResponseIdentityRestorer(ResponseIdentityPlan plan)
{
    string carry="";ResponseIdentityPlan.AddressContext address;
    public int PendingLength=>carry.Length;
    public string Write(string delta)
    {
        if(!plan.Active)return delta;
        string pending=carry+delta;int cut=pending.Length;
        for(int start=Math.Max(0,pending.Length-plan.HoldLength-1);start<pending.Length;start++)
        {
            var suffix=pending.AsSpan(start);
            foreach(var rule in plan.Rules)
            {
                bool dotted=suffix.Length==rule.Target.Length+1&&suffix[^1]=='.'&&suffix.StartsWith(rule.Target,StringComparison.Ordinal);
                if(rule.Target.AsSpan().StartsWith(suffix,StringComparison.Ordinal)||dotted)cut=Math.Min(cut,Math.Max(0,start-1));
            }
        }
        if(cut==pending.Length&&cut>0)cut--;
        if(cut>0&&cut<pending.Length&&char.IsLowSurrogate(pending[cut])&&char.IsHighSurrogate(pending[cut-1]))cut--;
        var result=plan.Process(pending,cut,address,false);address.Advance(pending.AsSpan(0,result.Consumed));carry=pending[result.Consumed..];return result.Text;
    }
    public string Flush()
    {
        var result=plan.Process(carry,carry.Length,address,true);carry="";address=new();return result.Text;
    }
    public override string ToString()=>"ResponseIdentityRestorer (redacted)";
}
