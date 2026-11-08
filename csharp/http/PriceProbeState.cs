using System.Text.Json;
using AccountScheduling;

namespace HttpMigration;

internal static class PriceProbeState
{
    internal static List<ModelPriceProbeWorker.Probe> Read(string path,DateTimeOffset now)
    {
        if(!File.Exists(path))return [];
        var probes=JsonSerializer.Deserialize(ConsoleSettings.Read(path,1<<20), StorageJson.Default.ListProbe)??throw new InvalidDataException();
        if(probes.Count>1000||probes.Any(p=>p is null||p.Model is null||p.Site is not("cn" or "intl")||p.Model.Length is 0 or >256||p.Model!=KeySecurity.KeyRules.Model(p.Model)||p.Model.Any(char.IsControl)||p.LastAt<0||p.LastAt>now.AddMinutes(5).ToUnixTimeSeconds()||!Enum.IsDefined(p.Cost))||probes.Select(p=>(p.Site,p.Model)).Distinct().Count()!=probes.Count)throw new InvalidDataException();
        return probes;
    }
    // Reading old observations must not instantiate or enable a model caller.
    internal static bool Restore(string directory,ModelCatalog catalog)
    {
        try
        {
            foreach(var p in Read(Path.Combine(directory,"price-probes.json"),DateTimeOffset.UtcNow))
                catalog.ObserveProbeCost(p.Site=="cn"?AccountSite.Cn:AccountSite.Intl,p.Model,p.Cost,p.LastAt);
            return true;
        }
        catch(Exception e)when(e is IOException or UnauthorizedAccessException or JsonException or ArgumentException){return false;}
    }
}
