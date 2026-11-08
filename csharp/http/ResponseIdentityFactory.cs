using System.Text;
using System.Text.Json;
using System.Text.RegularExpressions;
using StreamingMigration;

namespace HttpMigration;

internal static partial class ResponseIdentityFactory
{
    [GeneratedRegex(@"(?:^|[.!?。！？;；]\s*)(?:you are an AI agent powered by\s+|you are\s+|you're\s+|you’re\s+|i am\s+|i'm\s+|i’m\s+|we are\s+|你是\s*|您是\s*|我是\s*)",RegexOptions.IgnoreCase|RegexOptions.CultureInvariant,100)]
    private static partial Regex Declaration();
    internal static ResponseIdentityPlan? Create(JsonElement request,IdentityOptions? options)
    {
        if(options?.ResponseRestore!=true||options.Enabled==false)return null;
        string target=string.IsNullOrWhiteSpace(options.Target)?"WorkBuddy":options.Target.Trim();
        if(Encoding.UTF8.GetByteCount(target)>4096)return null;
        var settings=IdentitySettings.Build(options);
        var names=(settings.Rules.Length==0?IdentitySettings.Build(new(Target:target)).Rules:settings.Rules)
            .Where(r=>!r.Wildcard&&r.New.Trim()==target).Select(r=>r.Old).Distinct().OrderByDescending(n=>n.Length).ToArray();
        var found=new HashSet<string>(StringComparer.Ordinal);
        foreach(string text in InstructionTexts(request))
        {
            string fence="";
            foreach(string raw in text.Split('\n'))
            {
                string line=raw.Trim();
                if(fence.Length>0){if(line.StartsWith(fence,StringComparison.Ordinal))fence="";continue;}
                if(line.StartsWith("```",StringComparison.Ordinal)||line.StartsWith("~~~",StringComparison.Ordinal)){fence=line[..3];continue;}
                if(line.StartsWith('>')||line.StartsWith('\"')||line.StartsWith('\'')||line.StartsWith('`'))continue;
                foreach(Match match in Declaration().Matches(line))
                {
                    string declaration=line[(match.Index+match.Length)..].TrimStart('`','*','_');
                    string? name=names.FirstOrDefault(n=>declaration.StartsWith(n,StringComparison.Ordinal)&&(declaration.Length==n.Length||!ResponseIdentityPlanBoundary(declaration[n.Length])));
                    if(name is null)continue;
                    string tail=declaration[name.Length..].TrimStart('`','*','_',',','，',' ','\t').ToLowerInvariant();
                    if(new[]{"or ","and ","/","或","、"}.Any(c=>tail.StartsWith(c,StringComparison.Ordinal)))return null;
                    found.Add(name);if(found.Count>1)return null;
                }
            }
        }
        return found.Count==1?new([new(target,found.Single())]):null;
    }
    static bool ResponseIdentityPlanBoundary(char c)=>char.IsAsciiLetterOrDigit(c)||c=='_';
    static IEnumerable<string> InstructionTexts(JsonElement root)
    {
        foreach(string field in new[]{"instructions","system"})
            if(root.TryGetProperty(field,out var value))foreach(string text in Content(value))yield return text;
        foreach(string field in new[]{"messages","input"})
            if(root.TryGetProperty(field,out var list)&&list.ValueKind==JsonValueKind.Array)
                foreach(var item in list.EnumerateArray())
                    if(item.ValueKind==JsonValueKind.Object&&item.TryGetProperty("role",out var role)&&role.ValueKind==JsonValueKind.String&&role.GetString()?.Trim().ToLowerInvariant() is "system" or "developer"&&item.TryGetProperty("content",out var value))
                        foreach(string text in Content(value))yield return text;
    }
    static IEnumerable<string> Content(JsonElement value)
    {
        if(value.ValueKind==JsonValueKind.String){yield return value.GetString()??"";yield break;}
        if(value.ValueKind!=JsonValueKind.Array)yield break;
        foreach(var block in value.EnumerateArray())
            if(block.ValueKind==JsonValueKind.Object&&block.TryGetProperty("type",out var type)&&type.ValueKind==JsonValueKind.String&&type.GetString() is "text" or "input_text"&&block.TryGetProperty("text",out var text)&&text.ValueKind==JsonValueKind.String)yield return text.GetString()??"";
    }
}
