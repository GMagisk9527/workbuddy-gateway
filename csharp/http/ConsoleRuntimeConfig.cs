using System.Collections.Frozen;
using System.Text.Json;
namespace HttpMigration;

public sealed partial class ConsoleSettings
{
    SystemPromptOptions promptBaseline=new();AffinityOptions affinityBaseline=new();SessionAffinity? affinity;
    SystemPromptOptions prompts=new();FrozenDictionary<string,double> tokenMultipliers=FrozenDictionary<string,double>.Empty;
    AccountMaintenance.CredentialManager? credentials;
    public void AttachCredits(AccountMaintenance.CredentialManager manager){lock(sync){credentials=manager;ApplyRuntime();}}
    public SystemPromptOptions Prompts=>Volatile.Read(ref prompts);
    public double TokenMultiplier(string model)=>Volatile.Read(ref tokenMultipliers).GetValueOrDefault(KeySecurity.KeyRules.Model(model),1);
    public void AttachRuntime(SystemPromptOptions initial,AffinityOptions initialAffinity,SessionAffinity store)
    {lock(sync){promptBaseline=initial;affinityBaseline=initialAffinity;affinity=store;ApplyRuntime();}}
    static SystemPromptOptions ReadPrompts(JsonElement root,SystemPromptOptions fallback)
    {
        var next=fallback;
        if(root.TryGetProperty("systemPrompt",out var p))
        {Fields(p,"fallback","force");next=next with{Fallback=p.TryGetProperty("fallback",out var f)?f.GetString()!:fallback.Fallback,Force=p.TryGetProperty("force",out var force)?force.GetString()!:fallback.Force};}
        if(root.TryGetProperty("identity",out var identity))
        {
            Fields(identity,"enabled","target","replacements");var old=fallback.Identity??new();
            IReadOnlyDictionary<string,string>? replacements=old.Replacements;
            if(identity.TryGetProperty("replacements",out var r))
            {
                if(r.ValueKind!=JsonValueKind.Object||r.EnumerateObject().Count()>256)throw new ArgumentException("Invalid identity replacements");
                replacements=r.EnumerateObject().ToFrozenDictionary(p=>p.Name,p=>p.Value.GetString()??throw new ArgumentException("Invalid replacement"));
                if(replacements.Any(p=>p.Key.Length is 0 or >4096||p.Value.Length>4096))throw new ArgumentException("Invalid replacement bounds");
            }
            next=next with{Identity=new(identity.TryGetProperty("enabled",out var enabled)?enabled.GetBoolean():old.Enabled,identity.TryGetProperty("target",out var target)?target.GetString()!:old.Target,replacements)};
            if(next.Identity.Target is null||next.Identity.Target.Length>4096)throw new ArgumentException("Invalid target");
        }
        next.Validate();return next;
    }
    static AffinityOptions ReadAffinity(JsonElement root,AffinityOptions fallback)
    {
        if(!root.TryGetProperty("affinity",out var a))return fallback;
        Fields(a,"enabled","mode","ttlSeconds","capacity","prefixHeadBytes");
        int Number(string key,int value)=>a.TryGetProperty(key,out var n)?n.GetInt32():value;
        var mode=a.TryGetProperty("mode",out var m)?m.GetString() switch{"hybrid"=>AffinityMode.Hybrid,"prefix"=>AffinityMode.Prefix,"session"=>AffinityMode.Session,_=>throw new ArgumentException("Invalid affinity mode")}:fallback.Mode;
        var next=new AffinityOptions(a.TryGetProperty("enabled",out var e)?e.GetBoolean():fallback.Enabled,mode,Number("capacity",fallback.Capacity),Number("ttlSeconds",fallback.TtlSeconds),Number("prefixHeadBytes",fallback.PrefixBytes));next.Validate();return next;
    }
    static FrozenDictionary<string,double> ReadMultipliers(JsonElement root)
    {
        if(!root.TryGetProperty("tokenEstimation",out var estimation))return FrozenDictionary<string,double>.Empty;
        Fields(estimation,"modelMultipliers");if(!estimation.TryGetProperty("modelMultipliers",out var m))return FrozenDictionary<string,double>.Empty;
        if(m.ValueKind!=JsonValueKind.Object||m.EnumerateObject().Count()>128)throw new ArgumentException("Invalid estimation map");
        var values=new Dictionary<string,double>();foreach(var p in m.EnumerateObject())
        {string id=KeySecurity.KeyRules.Model(p.Name);double value=p.Value.GetDouble();if(id.Length is 0 or >256||id.Any(char.IsWhiteSpace)||id.Any(char.IsControl)||!double.IsFinite(value)||value<0.25||value>4||!values.TryAdd(id,value))throw new ArgumentException("Invalid token multiplier");}
        return values.ToFrozenDictionary();
    }
    static void ValidateRuntime(JsonElement root)
    {
        _=ReadPrompts(root,new());_=ReadAffinity(root,new());_=ReadMultipliers(root);_=ReadAdmission(root,new());_=ReadMemoryAdmission(root,new());_=ReadUpstream(root,UpstreamTimeouts.Default);_=ReadService(root,ServiceTimeouts.Default);_=ReadOutput(root,new());_=ReadRetries(root,new());_=ReadLogOptions(root);
        if(root.TryGetProperty("credits",out var credits))
        {Fields(credits,"fetch","expiringFirst","windowDays");if(credits.TryGetProperty("fetch",out var f))_=f.GetBoolean();if(credits.TryGetProperty("expiringFirst",out var e))_=e.GetBoolean();if(credits.TryGetProperty("windowDays",out var w)&&(w.GetInt32()<1||w.GetInt32()>3650))throw new ArgumentException("Invalid credit window");}
    }
    void ApplyRuntime()
    {
        using var doc=JsonDocument.Parse(document);var root=doc.RootElement;
        admission?.Configure(ReadAdmission(root,admissionBaseline));
        memoryAdmission?.Configure(ReadMemoryAdmission(root,memoryBaseline));
        outputAdmission?.Configure(ReadOutput(root,outputBaseline));
        retryAdmission?.Configure(ReadRetries(root,retryBaseline));
        Volatile.Write(ref logOptions,ReadLogOptions(root));
        Volatile.Write(ref upstream,ReadUpstream(root,upstreamBaseline));
        var nextService=ReadService(root,serviceBaseline);Volatile.Write(ref service,nextService);if(hostOptions is not null)hostOptions.ShutdownTimeout=nextService.Grace+nextService.Cleanup;
        Volatile.Write(ref prompts,ReadPrompts(root,promptBaseline));Volatile.Write(ref tokenMultipliers,ReadMultipliers(root));affinity?.Configure(ReadAffinity(root,affinityBaseline));
        if(credentials is not null)credentials.FetchCreditPackages=!root.TryGetProperty("credits",out var credits)||!credits.TryGetProperty("fetch",out var fetch)||fetch.GetBoolean();
    }
}
