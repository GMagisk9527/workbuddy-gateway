using System.Globalization;
using System.Text;
using System.Text.Json;
using System.Text.RegularExpressions;

namespace HttpMigration;

internal sealed record LogOptions(long MaxFileBytes=8L<<20,long MaxTotalBytes=64L<<20,int MaxFiles=16,int SampleFirst=20,int SampleEvery=100,bool CacheDiagnostics=true)
{
    internal void Validate(){if(MaxFileBytes is <1 or >1L<<30||MaxTotalBytes<MaxFileBytes||MaxTotalBytes>1L<<40||MaxFiles is <1 or >10000||SampleFirst is <1 or >100000||SampleEvery is <1 or >100000)throw new ArgumentException("Invalid log budget");}
}
public sealed partial class ConsoleSettings
{
    LogOptions logOptions=new();internal LogOptions Logs=>Volatile.Read(ref logOptions);
    static LogOptions ReadLogOptions(JsonElement root)
    {
        var defaults=new LogOptions();if(!root.TryGetProperty("logs",out var logs))return defaults;
        long Value(string name,long fallback){if(!logs.TryGetProperty(name,out var n))return fallback;long v=n.GetInt64();return v==0?fallback:v;}
        bool cacheDiagnostics=defaults.CacheDiagnostics;
        if(logs.TryGetProperty("hy4CacheDiagnostics",out var legacy))cacheDiagnostics=legacy.GetBoolean();
        if(logs.TryGetProperty("cacheDiagnostics",out var value))cacheDiagnostics=value.GetBoolean();
        var next=new LogOptions(Value("maxFileBytes",defaults.MaxFileBytes),Value("maxTotalBytes",defaults.MaxTotalBytes),checked((int)Value("maxFiles",defaults.MaxFiles)),checked((int)Value("sampleFirst",defaults.SampleFirst)),checked((int)Value("sampleEvery",defaults.SampleEvery)),cacheDiagnostics);next.Validate();return next;
    }
}
// This store accepts only already-redacted gateway metadata. It never receives
// request bodies, credentials, arbitrary client events or upstream error text.
internal sealed class RequestLogStore
{
    // Rotation is decided from cached sizes; directory enumeration only happens
    // when the store is (re)loaded or explicitly cleaned up. A single append is
    // one unbuffered write, not an enumerate/stat/open/close cycle.
    sealed record LogFile(string Name,long Length,DateTime Date);
    static readonly Regex Name=new("^requests-[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9]{6}\\.log$",RegexOptions.CultureInvariant);
    readonly object sync=new();readonly string directory;readonly ConsoleSettings settings;
    readonly List<LogFile> files=[];
    FileStream? current;string? currentName;long currentLength;DateTime currentDate;
    long total;bool loaded;
    internal bool WriteFailed{get;private set;}internal long Rejected{get;private set;}
    internal RequestLogStore(string parent,ConsoleSettings settings)
    {
        this.settings=settings;directory=Path.Combine(parent,"request-logs");ConsoleSettings.CheckLink(directory);Directory.CreateDirectory(directory);
        string marker=Path.Combine(directory,".csharp-request-logs");ConsoleSettings.CheckLink(marker);
        if(!File.Exists(marker)){if(Directory.EnumerateFileSystemEntries(directory).Any())throw new InvalidDataException("Unowned log directory");File.WriteAllText(marker,"request-logs-v1");}
        if(File.ReadAllText(marker)!="request-logs-v1")throw new InvalidDataException("Invalid log ownership");
    }
    FileInfo[] Files()
    {
        ConsoleSettings.CheckLink(directory);
        return new DirectoryInfo(directory).EnumerateFiles().Where(f=>Name.IsMatch(f.Name)&&(f.Attributes&FileAttributes.ReparsePoint)==0).OrderBy(f=>f.Name,StringComparer.Ordinal).ToArray();
    }
    static DateTime DateOf(string name)=>DateTime.TryParseExact(name.AsSpan(9,10),"yyyy-MM-dd",CultureInfo.InvariantCulture,DateTimeStyles.None,out var date)?date:DateTime.MinValue;
    static int IndexOf(string name)=>int.Parse(name.AsSpan(20,6),CultureInfo.InvariantCulture);
    void Reload()
    {
        files.Clear();total=0;
        foreach(var file in Files()){var date=DateOf(file.Name);files.Add(new(file.Name,file.Length,date));total+=file.Length;}
        loaded=true;
    }
    void CloseCurrent(){current?.Dispose();current=null;currentName=null;currentLength=0;currentDate=default;}
    void Replace(string name,long length,DateTime date)
    {int at=files.FindIndex(f=>f.Name==name);if(at>=0)files[at]=files[at] with{Length=length,Date=date};}
    void OpenExisting(LogFile file)
    {
        CloseCurrent();
        string path=Path.Combine(directory,file.Name);ConsoleSettings.CheckLink(path);
        current=new FileStream(path,FileMode.Append,FileAccess.Write,FileShare.Read,1);
        currentName=file.Name;currentLength=current.Length;currentDate=file.Date;
        if(currentLength!=file.Length){total+=currentLength-file.Length;Replace(file.Name,currentLength,file.Date);}
    }
    void Create(string name,DateTime date)
    {
        CloseCurrent();
        string path=Path.Combine(directory,name);ConsoleSettings.CheckLink(path);
        current=new FileStream(path,FileMode.Append,FileAccess.Write,FileShare.Read,1);
        currentName=name;currentLength=0;currentDate=date;files.Add(new(name,0,date));
    }
    void Trim(DateTimeOffset now,LogOptions limits,string? keep,ref long projectedTotal,ref int projectedCount)
    {
        int days=settings.RetentionDays;
        bool over=projectedTotal>limits.MaxTotalBytes||projectedCount>limits.MaxFiles;
        DateTime cutoff=now.LocalDateTime.Date.AddDays(-(Math.Max(1,days)-1));
        bool expired=days>0&&files.Any(f=>f.Name!=keep&&f.Date<cutoff);
        if(!over&&!expired)return;
        foreach(var file in files.OrderBy(f=>f.Date).ThenBy(f=>f.Name,StringComparer.Ordinal).ToArray())
        {
            if(file.Name==keep)continue;
            bool stale=days>0&&file.Date<cutoff;
            bool tooLarge=projectedTotal>limits.MaxTotalBytes||projectedCount>limits.MaxFiles;
            if(!stale&&!tooLarge)continue;
            string path=Path.Combine(directory,file.Name);
            try{ConsoleSettings.CheckLink(path);File.Delete(path);}
            catch(FileNotFoundException){}
            catch(DirectoryNotFoundException){}
            files.Remove(file);total-=file.Length;projectedTotal-=file.Length;projectedCount--;
        }
    }
    internal void Append(string line,DateTimeOffset now)
    {
        lock(sync)
        {
            try
            {
                var limits=settings.Logs;byte[] bytes=Encoding.UTF8.GetBytes(line+"\n");
                if(bytes.LongLength>limits.MaxFileBytes){Rejected++;return;}
                if(!loaded)Reload();
                DateTime date=now.LocalDateTime.Date;LogFile? existing=null;bool create=false;
                if(current is not null&&currentDate==date&&currentLength+bytes.Length<=limits.MaxFileBytes){/* append to current */ }
                else if(current is not null&&currentDate==date){create=true;}
                else
                {
                    foreach(var file in files)
                    {
                        if(file.Date!=date)continue;
                        if(existing is null||IndexOf(file.Name)>IndexOf(existing.Name))existing=file;
                    }
                    if(existing is null||existing.Length+bytes.Length>limits.MaxFileBytes)create=true;
                }
                string? keepName=create?null:existing?.Name??currentName;
                long projectedTotal=total+bytes.Length;int projectedCount=files.Count+(create?1:0);
                // Close the old handle before trimming so a date change or a
                // rotation cannot leave a deleted file open on Windows.
                if(create||current is not null&&currentDate!=date)CloseCurrent();
                Trim(now,limits,keepName,ref projectedTotal,ref projectedCount);
                if(projectedTotal>limits.MaxTotalBytes||projectedCount>limits.MaxFiles){Rejected++;return;}
                if(create)
                {
                    // Never reuse a lower index: file-name order is the log order,
                    // even after trimming removed the oldest rolled files.
                    int index=0;
                    foreach(var file in files)
                        if(file.Date==date)index=Math.Max(index,IndexOf(file.Name)+1);
                    if(existing is not null)index=Math.Max(index,IndexOf(existing.Name)+1);
                    while(files.Any(f=>f.Date==date&&IndexOf(f.Name)==index))index++;
                    if(index>999999){Rejected++;return;}
                    string prefix="requests-"+now.LocalDateTime.ToString("yyyy-MM-dd",CultureInfo.InvariantCulture)+"-";
                    Create(prefix+index.ToString("D6",CultureInfo.InvariantCulture)+".log",date);
                }
                else if(existing is not null&&currentName!=existing.Name)OpenExisting(existing);
                if(current is null){WriteFailed=true;return;}
                current.Write(bytes);currentLength+=bytes.Length;total+=bytes.Length;
                int at=files.FindIndex(f=>f.Name==currentName);
                if(at>=0)files[at]=files[at] with{Length=currentLength};
                else files.Add(new(currentName!,currentLength,currentDate));
                WriteFailed=false;
            }
            catch(Exception e)when(e is IOException or UnauthorizedAccessException or InvalidDataException)
            {CloseCurrent();loaded=false;WriteFailed=true;}
        }
    }
    internal int Cleanup(DateTimeOffset now)
    {
        lock(sync)
        {
            CloseCurrent();loaded=false;
            try
            {
                var entries=Files();var limits=settings.Logs;long bytes=entries.Sum(f=>f.Length);int count=entries.Length,removed=0;
                int days=settings.RetentionDays;
                DateTime cutoff=now.LocalDateTime.Date.AddDays(-(Math.Max(1,days)-1));
                foreach(var file in entries)
                {
                    bool expired=days>0&&DateOf(file.Name)<cutoff;
                    bool over=bytes>limits.MaxTotalBytes||count>limits.MaxFiles;
                    if(!expired&&!over)continue;
                    ConsoleSettings.CheckLink(file.FullName);File.Delete(file.FullName);bytes-=file.Length;count--;removed++;
                }
                WriteFailed=false;return removed;
            }
            catch(Exception e)when(e is IOException or UnauthorizedAccessException or InvalidDataException){WriteFailed=true;return 0;}
        }
    }
    internal string[] Tail(int count)
    {
        lock(sync)
        {
            var lines=new Queue<string>();long remaining=1<<20;
            try
            {
                foreach(var file in Files().Reverse())
                {
                    if(settings.RetentionDays>0&&DateOf(file.Name)<DateTime.Now.Date.AddDays(-(settings.RetentionDays-1)))continue;
                    if(remaining<=0)continue;
                    ConsoleSettings.CheckLink(file.FullName);using var stream=new FileStream(file.FullName,FileMode.Open,FileAccess.Read,FileShare.ReadWrite);
                    long take=Math.Min(stream.Length,remaining);stream.Seek(-take,SeekOrigin.End);using var reader=new StreamReader(stream,Encoding.UTF8);
                    if(take<stream.Length)reader.ReadLine();var chunk=new Queue<string>();string? line;
                    while((line=reader.ReadLine()) is not null){if(chunk.Count>=count)chunk.Dequeue();chunk.Enqueue(line);}remaining-=take;
                    var merged=chunk.Concat(lines).TakeLast(count).ToArray();lines=new(merged);if(lines.Count>=count)break;
                }
            }
            catch(Exception e)when(e is IOException or UnauthorizedAccessException){WriteFailed=true;}
            return lines.ToArray();
        }
    }
}
