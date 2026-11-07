using AccountMaintenance;

namespace HttpMigration;

public sealed class CalendarMaintenanceOptions
{
    public string Directory{get;}
    public bool CheckinEnabled{get;}
    public IReadOnlyList<int> KeepaliveHours{get;}
    public CalendarMaintenanceOptions(string directory,bool checkinEnabled=true,IEnumerable<int>? keepaliveHours=null)
    {Directory=Path.GetFullPath(directory);CheckinEnabled=checkinEnabled;int[] hours=(keepaliveHours??[22]).Distinct().Order().ToArray();if(hours.Length>24||hours.Any(h=>h is <0 or >23))throw new ArgumentException("Keepalive hours must be 0..23");KeepaliveHours=Array.AsReadOnly(hours);}
    public static CalendarMaintenanceOptions FromEnvironment(string directory)
    {
        bool checkin=Environment.GetEnvironmentVariable("WORKBUDDY_DAILY_CHECKIN") switch{null or "" or "1" or "true"=>true,"0" or "false"=>false,_=>throw new ArgumentException("Invalid daily check-in switch")};
        var raw=Environment.GetEnvironmentVariable("WORKBUDDY_KEEPALIVE_HOURS")??"22";
        return new(directory,checkin,raw.Split(',',StringSplitOptions.RemoveEmptyEntries|StringSplitOptions.TrimEntries).Select(s=>int.Parse(s,System.Globalization.CultureInfo.InvariantCulture)));
    }
}

public sealed class KeepaliveWorker:BackgroundService
{
    readonly CredentialManager credentials;readonly CalendarMaintenanceOptions options;readonly TimeProvider time;readonly TimeZoneInfo zone;readonly Func<TimeSpan,CancellationToken,Task> delay;
    readonly SemaphoreSlim run=new(1,1);
    public ScheduledTaskState TaskState{get;}
    public ScheduledTaskState CatchupState{get;}
    public KeepaliveWorker(CredentialManager credentials,CalendarMaintenanceOptions options,TimeProvider? time=null,TimeZoneInfo? zone=null,Func<TimeSpan,CancellationToken,Task>? delay=null)
    {this.credentials=credentials;this.options=options;this.time=time??TimeProvider.System;this.zone=zone??TimeZoneInfo.Local;this.delay=delay??((span,ct)=>Task.Delay(span,this.time,ct));TaskState=new(this.time);CatchupState=new(this.time);}
    public static DateTimeOffset? Next(DateTimeOffset now,IEnumerable<int> hours,TimeZoneInfo zone)
    {
        var local=TimeZoneInfo.ConvertTime(now,zone);DateTimeOffset? next=null;
        foreach(int hour in hours)
        {
            DateTime date=DateTime.SpecifyKind(local.Date.AddHours(hour),DateTimeKind.Unspecified);
            if(zone.IsInvalidTime(date))date=date.AddHours(1);
            var candidate=new DateTimeOffset(date,zone.GetUtcOffset(date));
            if(candidate<=now){date=date.AddDays(1);if(zone.IsInvalidTime(date))date=date.AddHours(1);candidate=new(date,zone.GetUtcOffset(date));}
            if(next is null||candidate<next)next=candidate;
        }
        return next;
    }
    public async Task RunOnceAsync(bool overdueOnly,CancellationToken cancellation=default)
    {
        await run.WaitAsync(cancellation);var tracker=overdueOnly?CatchupState:TaskState;string trace=tracker.Begin();int attempted=0,ok=0,failed=0;
        try
        {
            foreach(var id in credentials.KeepaliveCandidates(overdueOnly))
            {
                cancellation.ThrowIfCancellationRequested();if(attempted>0)await delay(TimeSpan.FromSeconds(1),cancellation);attempted++;
                try{if(await credentials.KeepaliveAsync(id,overdueOnly,cancellation))ok++;}
                catch(Exception e)when(e is CredentialFailure or HttpRequestException or IOException){failed++;}
            }
            tracker.Finish(trace,attempted,ok,failed,TimeSpan.Zero,outcome:attempted>0&&ok==0&&failed==0?"skipped":null);
            tracker.Schedule(overdueOnly?0:Next(time.GetUtcNow(),options.KeepaliveHours,zone)?.ToUnixTimeSeconds()??0);
        }
        catch(OperationCanceledException){tracker.Finish(trace,attempted,ok,failed,TimeSpan.Zero,outcome:"cancelled");throw;}
        catch{tracker.Finish(trace,attempted,ok,failed,TimeSpan.Zero,outcome:"failed");throw;}
        finally{run.Release();}
    }
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        if(options.KeepaliveHours.Count==0)return;
        CatchupState.Schedule(time.GetUtcNow().AddSeconds(90).ToUnixTimeSeconds());
        await Task.WhenAll(Daily(stoppingToken),Catchup(stoppingToken));
    }
    async Task Catchup(CancellationToken ct){await delay(TimeSpan.FromSeconds(90),ct);await RunOnceAsync(true,ct);}
    async Task Daily(CancellationToken ct)
    {
        while(!ct.IsCancellationRequested&&Next(time.GetUtcNow(),options.KeepaliveHours,zone) is{} next)
        {
            TaskState.Schedule(next.ToUnixTimeSeconds());
            while(time.GetUtcNow()<next){var remaining=next-time.GetUtcNow();if(remaining<=TimeSpan.Zero)break;await delay(remaining<TimeSpan.FromMinutes(1)?remaining:TimeSpan.FromMinutes(1),ct);}
            await RunOnceAsync(false,ct);
        }
    }
}

public sealed class DailyCheckinWorker(DailyCheckinScheduler scheduler,CredentialManager credentials,TimeProvider? clock=null):BackgroundService
{
    readonly TimeProvider time=clock??TimeProvider.System;readonly SemaphoreSlim run=new(1,1);
    public ScheduledTaskState TaskState{get;}=new(clock);
    public async Task RunOnceAsync(bool startup=false,CancellationToken cancellation=default)
    {
        if(!await run.WaitAsync(0,cancellation))return;
        try
        {
            cancellation.ThrowIfCancellationRequested();var jobs=scheduler.Claim(credentials.CheckinTargets,startup);TaskState.Schedule(scheduler.NextAt);
            if(jobs.Count==0)return;
            string trace=TaskState.Begin();int good=0,bad=0;
            foreach(var job in jobs)
            {
                bool success=false;
                try
                {
                    using var deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);var remaining=DateTimeOffset.FromUnixTimeMilliseconds(job.End)-time.GetUtcNow();
                    if(remaining<=TimeSpan.Zero)throw new OperationCanceledException();deadline.CancelAfter(remaining<TimeSpan.FromSeconds(30)?remaining:TimeSpan.FromSeconds(30));
                    _=await credentials.CheckinAsync(job.Id,job.Key,deadline.Token);success=true;good++;
                }
                catch(Exception e)when(e is CredentialFailure or HttpRequestException or IOException or OperationCanceledException){bad++;}
                finally{scheduler.Complete(job,success);}
                if(success)try{await credentials.FetchResourcesAsync(job.Id,cancellation);}catch(Exception e)when(e is CredentialFailure or HttpRequestException or IOException or OperationCanceledException){/* Check-in result remains independent of resource refresh. */}
            }
            TaskState.Finish(trace,jobs.Count,good,bad,TimeSpan.Zero,cacheFailed:scheduler.Blocked,outcome:cancellation.IsCancellationRequested?"cancelled":null);TaskState.Schedule(cancellation.IsCancellationRequested?0:scheduler.NextAt);
        }
        finally{run.Release();}
    }
    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {await RunOnceAsync(true,stoppingToken);using var timer=new PeriodicTimer(TimeSpan.FromSeconds(1),time);while(await timer.WaitForNextTickAsync(stoppingToken))await RunOnceAsync(false,stoppingToken);}
}
