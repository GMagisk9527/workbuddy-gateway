using System.Text.Json;

namespace HttpMigration;

public sealed partial class ConsoleSettings
{
    RequestAdmission? admission;AdmissionOptions admissionBaseline=new();PreparationAdmission? memoryAdmission;MemoryAdmissionOptions memoryBaseline=new();
    internal void AttachAdmission(RequestAdmission gate,PreparationAdmission? memory=null)
    {lock(sync){admission=gate;admissionBaseline=gate.Options;memoryAdmission=memory;memoryBaseline=memory?.Options??new();ApplyRuntime();}}
    static void AdmissionFields(JsonElement a)=>Fields(a,"maxActive","maxPreparing","maxWaiting","waitTimeoutMs","fairQueue","maxWaitingPerKey","memoryProtection","maxBodyBytes","memoryHighWatermarkPercent","memoryLowWatermarkPercent");
    static AdmissionOptions ReadAdmission(JsonElement root,AdmissionOptions fallback)
    {
        if(!root.TryGetProperty("admission",out var a))return fallback;
        AdmissionFields(a);
        int Number(string name,int initial)=>a.TryGetProperty(name,out var n)?n.GetInt32():initial;
        int active=Number("maxActive",fallback.MaxActive),wait=Number("waitTimeoutMs",fallback.WaitTimeoutMs);
        var result=new AdmissionOptions(active==0?2:active,Number("maxPreparing",fallback.MaxPreparing),Number("maxWaiting",fallback.MaxWaiting),wait==0?15000:wait,
            a.TryGetProperty("fairQueue",out var fair)?fair.GetBoolean():fallback.FairQueue,Number("maxWaitingPerKey",fallback.MaxWaitingPerKey));
        result.Validate();return result;
    }
    static MemoryAdmissionOptions ReadMemoryAdmission(JsonElement root,MemoryAdmissionOptions fallback)
    {
        if(!root.TryGetProperty("admission",out var a))return fallback;AdmissionFields(a);
        int Number(string name,int initial)=>a.TryGetProperty(name,out var n)?n.GetInt32():initial;
        int high=Number("memoryHighWatermarkPercent",fallback.High),low=Number("memoryLowWatermarkPercent",fallback.Low);
        var result=new MemoryAdmissionOptions(a.TryGetProperty("memoryProtection",out var enabled)?enabled.GetBoolean():fallback.Enabled,
            a.TryGetProperty("maxBodyBytes",out var bytes)?bytes.GetInt64():fallback.MaxBodyBytes,high==0?85:high,low==0?75:low);
        result.Validate();return result;
    }
}
