using System.Text.Json;
using AccountScheduling;

namespace HttpMigration;

public sealed class ConsoleRuntime:IDisposable
{
    public ConsoleSettings Settings{get;}public ConsoleTelemetry Telemetry{get;}
    public ScheduledTaskJournal TaskJournal{get;}
    public DailyCheckinScheduler? Checkins{get;}
    public IReadOnlyDictionary<string,string> Labels{get;}
    readonly StandaloneAccounts? accountOwner;
    void RefreshAccounts()
    {if(accountOwner is{} owner){Telemetry.RefreshAccounts(owner.Credentials.ManagementStatus.Select(a=>new ConsoleAccount(a.Id,Labels.GetValueOrDefault(a.Id)??a.Id,a.Site==AccountSite.Cn?"cn":"intl")));Settings.RefreshAccountPolicies();}}
    public ConsoleRuntime(LocalGatewayOptions options)
    {
        var labels=new Dictionary<string,string>(StringComparer.Ordinal);var aliases=new Dictionary<string,string>(StringComparer.Ordinal);
        if(options.ConsoleDirectory is{} directory)
        {
            string? parent=Path.GetDirectoryName(Path.GetDirectoryName(Path.GetFullPath(directory)));string map=Path.Combine(parent??"","migration-map.json");
            if(File.Exists(map))
            {
                using var doc=JsonDocument.Parse(ConsoleSettings.Read(map,1<<20));var rows=doc.RootElement.GetProperty("accounts");if(rows.GetArrayLength()>1024)throw new InvalidDataException("Console identity map budget");
                foreach(var row in rows.EnumerateArray())
                {
                    string id=row.GetProperty("id").GetString()!,source=row.GetProperty("source").GetString()!;if(id.Length>256||source.Length>1024)throw new InvalidDataException();
                    labels[id]=Path.GetFileName(source);string original=row.TryGetProperty("originalPath",out var p)?p.GetString()!:source;
                    if(options.Credentials!.Status.Any(s=>s.Id.Value==id))aliases[options.Credentials.HistoricalIdentity(new(id),original)]=id;
                }
            }
        }
        Settings=new(options.Catalog!,options.ConsoleDirectory);
        if(options.ConsoleDirectory is{} priceDirectory)PriceProbeState.Restore(priceDirectory,options.Catalog!);
        if(options.Accounts is{} pool)Settings.AttachAccountPolicies(pool,labels);
        TaskJournal=new(options.ConsoleDirectory);
        try{Labels=labels;if(options.Calendar is{CheckinEnabled:true} calendar)Checkins=new(calendar.Directory);Telemetry=new(Settings,options.Credentials!.ManagementStatus.Select(a=>new ConsoleAccount(a.Id,labels.GetValueOrDefault(a.Id)??a.Id,a.Site==AccountSite.Cn?"cn":"intl")),aliases);}
        catch{Checkins?.Dispose();Settings.Dispose();throw;}
        accountOwner=options.AccountOwner;if(accountOwner is not null)accountOwner.Changed+=RefreshAccounts;
    }
    public void Dispose(){if(accountOwner is not null)accountOwner.Changed-=RefreshAccounts;Checkins?.Dispose();Settings.Dispose();}
}
