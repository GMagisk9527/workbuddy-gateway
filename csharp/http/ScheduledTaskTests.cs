using System.Text.Json;
namespace HttpMigration;

internal static class ScheduledTaskTests
{
    sealed class Clock:TimeProvider
    {public DateTimeOffset Now=DateTimeOffset.Parse("2026-11-06T00:00:00Z");public override DateTimeOffset GetUtcNow()=>Now;}
    internal static void Run()
    {
        int count=0;void Check(bool ok,string label){if(!ok)throw new InvalidOperationException(label);count++;}
        var clock=new Clock();var state=new ScheduledTaskState(clock);var interval=TimeSpan.FromSeconds(60);
        Check(state.Snapshot.Status=="unknown"&&state.Snapshot.Success is null&&state.Snapshot.LastRunAt==0,"No invented history");
        foreach(var test in new[]{(0,0,0,false,"skipped"),(3,3,0,false,"success"),(3,0,3,false,"failed"),(3,2,1,false,"partial"),(3,3,0,true,"partial")})
        {
            string trace=state.Begin();var live=state.Snapshot;
            Check(live.Status=="running"&&live.Success is null&&live.LastFinishedAt==0&&live.NextRunAt==0,"Live run does not expose stale completion");
            clock.Now=clock.Now.AddSeconds(5);state.Finish(trace,test.Item1,test.Item2,test.Item3,interval,test.Item4);var done=state.Snapshot;
            Check(done.Status==test.Item5,"Outcome classification "+test.Item5);
            Check(done.LastRunAt==live.LastRunAt&&done.LastFinishedAt==clock.Now.ToUnixTimeSeconds()&&done.NextRunAt==clock.Now.Add(interval).ToUnixTimeSeconds(),"Real timestamps and estimated next");
            Check(done.Success==(test.Item5=="success"?true:test.Item5 is "failed" or "partial"?false:(bool?)null),"Tri-state success");
            state.Finish(trace,1,1,0,interval);Check(state.Snapshot==done,"Duplicate completion ignored");
        }
        string old=state.Begin(),current=state.Begin();state.Finish(old,1,1,0,interval);Check(state.Snapshot.TraceId==current&&state.Snapshot.Status=="running","Stale completion cannot clobber newer run");
        state.Finish(current,1,0,0,interval,outcome:"cancelled");Check(state.Snapshot.Status=="cancelled"&&state.Snapshot.Success is null&&state.Snapshot.NextRunAt==0,"Cancelled is not success or scheduled");
        Check(new ScheduledTaskState(clock).Snapshot.Runs==0,"Process restart has no foreign run history");
        var services=new ServiceCollection().BuildServiceProvider();using(services)
        {
            var options=new LocalGatewayOptions(new("http://127.0.0.1:1"),new byte[32]);
            var doc=JsonSerializer.SerializeToElement(ScheduledTasksView.Build(services,options));
            var rows=doc.GetProperty("tasks").EnumerateArray().ToArray();
            Check(rows.Length==11&&rows.Select(r=>r.GetProperty("id").GetString()).Distinct().Count()==11,"Go task identities listed exactly once");
            Check(rows.All(r=>!r.GetProperty("enabled").GetBoolean()&&r.GetProperty("nextRunAt").GetInt64()==0&&r.GetProperty("success").ValueKind==JsonValueKind.Null),"Disabled tasks have no invented schedule/result");
            Check(rows.Single(r=>r.GetProperty("id").GetString()=="daily_checkin").GetProperty("supported").GetBoolean(),"Implemented check-in can be disabled");
        }
        Console.WriteLine(JsonSerializer.Serialize(new{scheduledTaskChecks=count,realModelCalls=0}));
    }
}
