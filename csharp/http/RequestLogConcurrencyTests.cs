namespace HttpMigration;

internal static class RequestLogConcurrencyTests
{
    internal static int Run(string root)
    {
        using var settings=new ConsoleSettings(new(),root);
        var telemetry=new ConsoleTelemetry(settings,[new("synthetic","synthetic.json","intl")]);
        ConsoleRow Row(string trace)=>new("synthetic","model",DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),200,true,1,1,0,0,1,2,1,"responses",trace,"");
        telemetry.Complete(Row("initial"),[]);
        int checks=0;
        void Check(bool value,string name){if(!value)throw new InvalidOperationException(name);checks++;}
        // Stall the exact I/O boundary, with the same lock ownership as real
        // slow storage. Queries and history persistence must remain independent.
        foreach(var (operation,action) in new (string,Action)[]{
            ("append",()=>telemetry.Complete(Row("blocked"),[])),
            ("append",()=>telemetry.Audit("blocked_audit")),
            ("tail",()=>telemetry.Logs(10)),
            ("cleanup",()=>telemetry.Cleanup())})
        {
            using var entered=new ManualResetEventSlim();using var release=new ManualResetEventSlim();
            telemetry.BeforeRequestLogIo=kind=>{if(kind==operation){entered.Set();if(!release.Wait(TimeSpan.FromSeconds(10)))throw new TimeoutException("Log I/O test timed out");}};
            var io=Task.Factory.StartNew(action,CancellationToken.None,TaskCreationOptions.LongRunning,TaskScheduler.Default);
            try
            {
                Check(entered.Wait(TimeSpan.FromSeconds(5)),"Reached log I/O boundary");
                Task.Run(()=>
                {
                    Check(telemetry.Rows().Length>=1&&telemetry.RowsSince(0).Length>=1,"Slow log I/O does not hold statistics lock");
                    Check(telemetry.Trace("initial") is not null,"Trace lookup is independent of log storage");
                    telemetry.RefreshAccounts([new("synthetic","renamed.json","intl")]);
                    Check(telemetry.FlushHistory(),"History journal flush progresses during slow request-log I/O");
                }).WaitAsync(TimeSpan.FromSeconds(3)).GetAwaiter().GetResult();
                Check(!io.IsCompleted,"Query completion does not require releasing log I/O");
            }
            finally {release.Set();io.WaitAsync(TimeSpan.FromSeconds(5)).GetAwaiter().GetResult();telemetry.BeforeRequestLogIo=null;}
        }
        Parallel.For(0,32,i=>{telemetry.Complete(Row("parallel_"+i),[]);telemetry.Audit("parallel_audit");});
        var persisted=telemetry.Logs(1000);
        var actual=persisted.Where(l=>l.Contains(" protocol=responses ",StringComparison.Ordinal))
            .Select(l=>l.Split(" trace=",StringSplitOptions.None)[1].Split(' ')[0]);
        Check(actual.SequenceEqual(telemetry.Rows().Select(r=>r.Trace)),"Concurrent completion memory and disk order agree");
        Check(persisted.Count(l=>l.Contains(" admin=parallel_audit",StringComparison.Ordinal))==32,"Concurrent audit entries are preserved");
        Check(telemetry.Save(),"Statistics persist after concurrent log operations");
        return checks;
    }
}
