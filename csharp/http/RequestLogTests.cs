using System.Text.Json;

namespace HttpMigration;

internal static class RequestLogTests
{
    internal static int Run(string root)
    {
        int checks=0;void Check(bool ok,string why){if(!ok)throw new InvalidOperationException(why);checks++;}
        Directory.CreateDirectory(root);using var settings=new ConsoleSettings(new());
        void Configure(int days,long file,long total,int count)=>settings.Update(JsonSerializer.Serialize(new{logs=new{retentionDays=days,maxFileBytes=file,maxTotalBytes=total,maxFiles=count,sampleFirst=2,sampleEvery=3}}),settings.View().Revision);
        Configure(7,100,200,2);var store=new RequestLogStore(root,settings);var now=DateTimeOffset.Now;
        for(int i=0;i<10;i++)store.Append(i+new string('x',59),now);
        string dir=Path.Combine(root,"request-logs");var files=new DirectoryInfo(dir).GetFiles("*.log");
        Check(files.Length==2&&files.All(f=>f.Length<=100)&&files.Sum(f=>f.Length)<=200,"Log rotation enforces per-file, total, and count budgets");
        Check(store.Tail(10).Select(l=>l[0]).SequenceEqual(new[]{'8','9'}),"Newest log entries remain ordered across rolled files");
        var restored=new RequestLogStore(root,settings);Check(restored.Tail(10).SequenceEqual(store.Tail(10)),"Request logs survive a new store instance");
        store.Append(new string('z',101),now);Check(store.Rejected==1&&store.Tail(10).Length==2,"Oversized log line rejected without corrupting retained logs");
        settings.SetRetention(0);Check(settings.Logs==new LogOptions(100,200,2,2,3),"Saving retention preserves rotation and sampling settings");
        Configure(0,1000,5000,10);store.Append("old",now.AddDays(-30));Check(store.Tail(20).Contains("old"),"Explicit retention zero keeps old logs");
        settings.SetRetention(7);Check(store.Cleanup(now)==1&&!store.Tail(20).Contains("old"),"Seven-day cleanup deletes only expired logs");
        string unrelated=Path.Combine(dir,"credential.json");File.WriteAllText(unrelated,"unrelated synthetic data");
        Configure(7,100,100,1);store.Cleanup(now);Check(File.ReadAllText(unrelated)=="unrelated synthetic data"&&new DirectoryInfo(dir).GetFiles("*.log").Length<=1,"Cleanup preserves unrelated files while enforcing hot-shrunk budgets");
        string foreign=Path.Combine(root,"foreign");Directory.CreateDirectory(Path.Combine(foreign,"request-logs"));File.WriteAllText(Path.Combine(foreign,"request-logs","keep"),"keep");
        try{_=new RequestLogStore(foreign,settings);throw new InvalidOperationException("Unowned directory accepted");}catch(InvalidDataException){checks++;}
        string telemetryRoot=Path.Combine(root,"telemetry");
        using(var persisted=new ConsoleSettings(new(),telemetryRoot))
        {
            var telemetry=new ConsoleTelemetry(persisted,[new("synthetic","original.json","intl")]);telemetry.Complete(new("synthetic","model",now.ToUnixTimeMilliseconds(),200,true,1,1,0,0,1,2,1,"responses","trace",""),[]);
            Check(telemetry.Save()&&telemetry.Logs(10).Single().Contains("protocol=responses"),"Redacted request metadata persists alongside independent statistics");
        }
        using(var persisted=new ConsoleSettings(new(),telemetryRoot))
        {
            var telemetry=new ConsoleTelemetry(persisted,[]);Check(telemetry.Logs(10).Length==1&&telemetry.Rows().Length==1,"Statistics and log history both restore after restart");
            using(var stats=JsonDocument.Parse(JsonSerializer.Serialize(telemetry.Stats(new Microsoft.AspNetCore.Http.QueryCollection(new Dictionary<string,Microsoft.Extensions.Primitives.StringValues>{{"site","intl"}})))))
            {Check(stats.RootElement.GetProperty("accounts")[0].GetProperty("file").GetString()=="original.json"&&stats.RootElement.GetProperty("summary").GetProperty("requests").GetInt32()==1,"Deleted account label and site survive restart for historical filtering");}
            telemetry.RefreshAccounts([new("synthetic","renamed.json","cn")]);Check(telemetry.Save(),"Account-only changes persist without a new request");
            telemetry.Cleanup(now.AddDays(8));Check(telemetry.Logs(10).Length==0&&telemetry.Rows().Length==1,"Expiration never deletes statistics history");
        }
        using(var persisted=new ConsoleSettings(new(),telemetryRoot))
        {
            var telemetry=new ConsoleTelemetry(persisted,[]);using var saved=JsonDocument.Parse(File.ReadAllText(Path.Combine(telemetryRoot,"history.json")));
            Check(saved.RootElement.GetProperty("accounts")[0].GetProperty("file").GetString()=="renamed.json","Renamed account summary persisted");
        }
        string blockedRoot=Path.Combine(root,"blocked-logs");
        using(var persisted=new ConsoleSettings(new(),blockedRoot))
        {
            Directory.CreateDirectory(Path.Combine(blockedRoot,"request-logs"));string keep=Path.Combine(blockedRoot,"request-logs","foreign");File.WriteAllText(keep,"keep");
            var telemetry=new ConsoleTelemetry(persisted,[]);telemetry.Audit("synthetic_audit");
            Check(telemetry.LogWriteFailed&&telemetry.Logs(10).Single().Contains("synthetic_audit")&&telemetry.Save()&&File.ReadAllText(keep)=="keep","Unavailable/unowned log storage preserves service, memory logs and statistics without overwriting foreign data");
        }
        return checks+RequestLogConcurrencyTests.Run(Path.Combine(root,"concurrency"));
    }
}
