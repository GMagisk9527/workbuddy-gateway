using System.Security.Cryptography;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;

namespace HttpMigration;

public sealed record CheckinPlan(long FirstAt,long RetryAt,long StartupAt=0,int Attempts=0,string Status="pending",long LastAt=0,string Message="",bool RetryEnd=false);
public sealed record CheckinDocument(int Schema,string Date,Dictionary<string,CheckinPlan> Plans,long LastStartupAt=0);
public sealed record CheckinJob(string Key,AccountId Id,string Date,int Attempt,long End,string Trace);
public sealed record CheckinView(string Date,string Status,int Attempts=0,long NextAt=0,long LastAt=0,string Message="");

// Go-compatible schema/identity and CST windows; write an attempt before any RPC.
public sealed class DailyCheckinScheduler:IDisposable
{
    readonly object sync=new();readonly string directory;readonly FileStream owner;readonly TimeProvider time;readonly Func<int,int> random;readonly Action<byte[]> save;
    CheckinDocument doc=new(1,"",[]);HashSet<string> active=[];
    public bool Blocked{get;private set;}
    static readonly JsonSerializerOptions Json=new(JsonSerializerDefaults.Web);
    static readonly TimeSpan Zone=TimeSpan.FromHours(8);
    public DailyCheckinScheduler(string directory,TimeProvider? time=null,Func<int,int>? random=null,Action<byte[]>? save=null)
    {
        this.directory=Path.GetFullPath(directory);this.time=time??TimeProvider.System;this.random=random??RandomNumberGenerator.GetInt32;
        Directory.CreateDirectory(this.directory);ConsoleSettings.CheckLink(this.directory);var marker=Path.Combine(this.directory,".csharp-calendar");ConsoleSettings.CheckLink(marker);
        if(!File.Exists(marker)){if(Directory.EnumerateFileSystemEntries(this.directory).Any())throw new InvalidDataException("Calendar directory is not owned");File.WriteAllText(marker,"calendar-v1");}
        if(File.ReadAllText(marker)!="calendar-v1")throw new InvalidDataException("Invalid calendar ownership");
        string file=Path.Combine(this.directory,"checkin-state.json"),lockFile=Path.Combine(this.directory,".calendar.lock");ConsoleSettings.CheckLink(lockFile);owner=new(lockFile,FileMode.OpenOrCreate,FileAccess.ReadWrite,FileShare.None);
        this.save=save??(bytes=>ConsoleSettings.Atomic(file,bytes));
        try{if(File.Exists(file)){var loaded=JsonSerializer.Deserialize<CheckinDocument>(ConsoleSettings.Read(file,8<<20),Json)??throw new InvalidDataException();Validate(loaded);doc=loaded;}}
        catch(Exception e)when(e is IOException or UnauthorizedAccessException or InvalidDataException or JsonException or ArgumentException or InvalidOperationException){Blocked=true;}
    }
    static (long Start,long Retry,long End,long DayEnd) Windows(DateTimeOffset at)
    {var d=at.ToOffset(Zone).Date;var start=new DateTimeOffset(d.AddHours(8),Zone);return(start.ToUnixTimeMilliseconds(),start.AddMinutes(90).ToUnixTimeMilliseconds(),start.AddHours(2).ToUnixTimeMilliseconds(),start.AddHours(16).ToUnixTimeMilliseconds());}
    static string Day(DateTimeOffset at)=>at.ToOffset(Zone).ToString("yyyy-MM-dd",System.Globalization.CultureInfo.InvariantCulture);
    static void Validate(CheckinDocument d)
    {
        if(d.Schema!=1||d.Plans is null||d.Plans.Count>1024||!DateTimeOffset.TryParseExact(d.Date+"+08:00","yyyy-MM-ddzzz",System.Globalization.CultureInfo.InvariantCulture,System.Globalization.DateTimeStyles.None,out var date))throw new InvalidDataException();
        var w=Windows(date);if(d.LastStartupAt!=0&&(d.LastStartupAt<w.Start||d.LastStartupAt>=w.DayEnd))throw new InvalidDataException();
        foreach(var (key,p) in d.Plans)
            if(key.Length!=64||key.Any(c=>!char.IsAsciiHexDigit(c))||p is null||p.Status is not("pending" or "running" or "success" or "failed" or "missed")||p.Attempts is <0 or >2||p.FirstAt<w.Start||p.FirstAt>=w.Retry||p.RetryAt<w.Retry||p.RetryAt>=w.End||p.StartupAt!=0&&(p.StartupAt<w.Start||p.StartupAt>=w.DayEnd)||p.LastAt<0||p.LastAt>=w.DayEnd||p.Status=="success"&&p.Attempts==0||p.Message is null||p.Message.Length>400)throw new InvalidDataException();
    }
    bool Save()
    {
        if(Blocked)return false;
        try{byte[] bytes=JsonSerializer.SerializeToUtf8Bytes(doc,Json);if(bytes.Length>8<<20)throw new InvalidDataException();save(bytes);return true;}
        catch(Exception e)when(e is IOException or UnauthorizedAccessException or InvalidDataException){Blocked=true;return false;}
    }
    long Due(CheckinPlan p)=>Math.Max(p.StartupAt,doc.LastStartupAt+60000);
    public IReadOnlyList<CheckinJob> Claim(IEnumerable<CheckinTarget> targets,bool startup=false)
    {
        lock(sync)
        {
            if(Blocked)return [];var now=time.GetUtcNow();long ms=now.ToUnixTimeMilliseconds();string day=Day(now);var w=Windows(now);
            if(string.CompareOrdinal(doc.Date,day)>0){Blocked=true;return [];}
            bool dirty=false;if(doc.Date!=day){doc=new(1,day,[]);dirty=true;}
            if(startup)foreach(var (key,p) in doc.Plans.ToArray())if(p.Status=="running")
            {doc.Plans[key]=p with{Status="failed",RetryEnd=p.Attempts==2,Message="上次尝试结果未知，不补发原请求"};if(p.StartupAt!=0&&ms>=w.Start)doc=doc with{LastStartupAt=Math.Max(doc.LastStartupAt,ms)};dirty=true;}
            var eligible=targets.Where(t=>t.Supported&&t.Eligible).DistinctBy(t=>t.Key).Take(1025).ToArray();if(eligible.Length>1024){Blocked=true;return [];}
            active=eligible.Select(t=>t.Key).ToHashSet(StringComparer.Ordinal);
            foreach(var t in eligible)
            {
                if(!doc.Plans.TryGetValue(t.Key,out var p))
                {
                    if(doc.Plans.Count>=1024){Blocked=true;return [];}
                    int first=random(5400),retry=random(1800);if(first is <0 or >=5400||retry is <0 or >=1800){Blocked=true;return [];}
                    p=new(w.Start+first*1000L,w.Retry+retry*1000L);dirty=true;
                    if(p.FirstAt<=ms&&!(startup&&ms>=w.Start))p=p with{Status="missed",Message="随机时间已过；账号事件不触发补签"};
                }
                var old=p;
                if(p.Status=="failed"&&p.Attempts==1&&!p.RetryEnd&&(ms>=w.End||startup&&p.RetryAt<=ms||p.StartupAt!=0&&Math.Max(p.RetryAt,Due(p))>=w.End))p=p with{RetryEnd=true,Message="失败重试窗口已过，当天不再重试"};
                if(p.Status=="pending"&&p.StartupAt==0&&ms>=w.Retry&&!startup)p=p with{Status="missed",Message="首次窗口已过，不补签"};
                if(p.Status=="pending"&&p.StartupAt!=0&&Due(p)>=w.DayEnd)p=p with{Status="missed",Message="队列超出今天，不跨日补签"};
                doc.Plans[t.Key]=p;dirty|=old!=p;
            }
            if(startup&&ms>=w.Start)
            {
                long slot=Math.Max(ms,doc.LastStartupAt+60000);
                foreach(var t in eligible.Where(t=>doc.Plans[t.Key].Status=="pending"&&doc.Plans[t.Key].Attempts==0).OrderBy(t=>doc.Plans[t.Key].StartupAt==0?long.MaxValue:doc.Plans[t.Key].StartupAt))
                {var p=doc.Plans[t.Key];long at=Math.Max(slot,p.StartupAt);doc.Plans[t.Key]=at>=w.DayEnd?p with{Status="missed",Message="启动补签队列超出今天"}:p with{StartupAt=at,Message="启动补签排队；前一请求完成后间隔一分钟"};dirty=true;slot=at+60000;}
            }
            bool busy=doc.Plans.Values.Any(p=>p.StartupAt!=0&&p.Status=="running");var jobs=new List<CheckinJob>();
            foreach(var t in eligible)
            {
                var p=doc.Plans[t.Key];if(p.Status is "success" or "missed" or "running"||p.StartupAt!=0&&busy)continue;
                int attempt=0;long end=w.Retry;
                if(p.Status=="pending"&&p.Attempts==0&&p.StartupAt!=0&&ms>=w.Start&&ms<w.DayEnd&&Due(p)<=ms){attempt=1;end=w.DayEnd;}
                else if(!startup&&p.Status=="pending"&&p.Attempts==0&&p.StartupAt==0&&ms>=w.Start&&ms<w.Retry&&p.FirstAt<=ms)attempt=1;
                else if(!startup&&p.Status=="failed"&&p.Attempts==1&&!p.RetryEnd&&ms>=w.Retry&&ms<w.End&&p.RetryAt<=ms&&(p.StartupAt==0||Due(p)<=ms)){attempt=2;end=w.End;}
                if(attempt==0)continue;
                doc.Plans[t.Key]=p with{Attempts=attempt,Status="running",LastAt=ms,Message="正在按北京时间计划签到"};dirty=true;
                if(p.StartupAt!=0){doc=doc with{LastStartupAt=ms};busy=true;}
                jobs.Add(new(t.Key,t.Id,day,attempt,end,Guid.NewGuid().ToString("N")));if(jobs.Count>=4)break;
            }
            return dirty&&!Save()?[]:jobs;
        }
    }
    public void Complete(CheckinJob job,bool success)
    {
        lock(sync)
        {
            if(Blocked||doc.Date!=job.Date||!doc.Plans.TryGetValue(job.Key,out var p)||p.Status!="running"||p.Attempts!=job.Attempt)return;
            var now=time.GetUtcNow();if(p.StartupAt!=0&&Day(now)==job.Date)doc=doc with{LastStartupAt=Math.Max(doc.LastStartupAt,now.ToUnixTimeMilliseconds())};
            doc.Plans[job.Key]=p with{Status=success?"success":"failed",RetryEnd=!success&&(job.Attempt==2||now.ToUnixTimeMilliseconds()>=Windows(now).End),Message=success?"北京时间今天已签到":"签到请求未成功；只在当天计划窗口重试"};Save();
        }
    }
    public long NextAt
    {
        get{lock(sync){if(Blocked)return 0;long now=time.GetUtcNow().ToUnixTimeMilliseconds();var w=Windows(time.GetUtcNow());var due=doc.Plans.Where(k=>active.Contains(k.Key)).Select(k=>k.Value).Select(p=>p.Status=="pending"&&p.Attempts==0?(p.StartupAt==0?p.FirstAt:Due(p)):p.Status=="failed"&&p.Attempts==1&&!p.RetryEnd?Math.Max(p.RetryAt,p.StartupAt==0?0:Due(p)):0).Where(at=>at>now&&at<w.DayEnd).DefaultIfEmpty(msNext(w.Start,now)).Min();return due/1000;}}
    }
    static long msNext(long start,long now)=>start>now?start:start+86400000;
    public CheckinView View(CheckinTarget target)
    {
        lock(sync)
        {
            var now=time.GetUtcNow();string day=Day(now);if(!target.Supported)return new(day,"unsupported",Message:"国际站或企业账号不参与个人签到");if(Blocked)return new(day,"unknown",Message:"状态文件不可用，已暂停以防重复请求");
            if(doc.Date==day&&doc.Plans.TryGetValue(target.Key,out var p)){long next=p.Status=="pending"&&p.Attempts==0?(p.StartupAt==0?p.FirstAt:Due(p)):p.Status=="failed"&&p.Attempts==1&&!p.RetryEnd?Math.Max(p.RetryAt,p.StartupAt==0?0:Due(p)):0;return new(day,p.Status,p.Attempts,next/1000,p.LastAt/1000,p.Message);}
            return new(day,now.ToUnixTimeMilliseconds()>=Windows(now).Retry?"missed":"pending",Message:target.Eligible?"等待今日计划":"账号不可用，不发送签到");
        }
    }
    public void Dispose()=>owner.Dispose();
}
