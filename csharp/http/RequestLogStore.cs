using System.Text;
using System.Text.Json;
using System.Text.RegularExpressions;

namespace HttpMigration;

internal sealed record LogOptions(long MaxFileBytes=8L<<20,long MaxTotalBytes=64L<<20,int MaxFiles=16,int SampleFirst=20,int SampleEvery=100,bool Hy4CacheDiagnostics=true)
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
        bool cacheDiagnostics=logs.TryGetProperty("hy4CacheDiagnostics",out var value)?value.GetBoolean():defaults.Hy4CacheDiagnostics;
        var next=new LogOptions(Value("maxFileBytes",defaults.MaxFileBytes),Value("maxTotalBytes",defaults.MaxTotalBytes),checked((int)Value("maxFiles",defaults.MaxFiles)),checked((int)Value("sampleFirst",defaults.SampleFirst)),checked((int)Value("sampleEvery",defaults.SampleEvery)),cacheDiagnostics);next.Validate();return next;
    }
}
// This store accepts only already-redacted gateway metadata. It never receives
// request bodies, credentials, arbitrary client events or upstream error text.
internal sealed class RequestLogStore
{
    static readonly Regex Name=new("^requests-[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9]{6}\\.log$",RegexOptions.CultureInvariant);
    readonly object sync=new();readonly string directory;readonly ConsoleSettings settings;
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
    static bool Expired(FileInfo file,int days,DateTimeOffset now)
    {return days>0&&DateTime.TryParseExact(file.Name.AsSpan(9,10),"yyyy-MM-dd",System.Globalization.CultureInfo.InvariantCulture,System.Globalization.DateTimeStyles.None,out var day)&&day<now.LocalDateTime.Date.AddDays(-(days-1));}
    internal void Append(string line,DateTimeOffset now)
    {
        lock(sync)
        {
            try
            {
                var limits=settings.Logs;byte[] bytes=Encoding.UTF8.GetBytes(line+"\n");if(bytes.LongLength>limits.MaxFileBytes){Rejected++;return;}
                var files=Files().ToList();string prefix="requests-"+now.LocalDateTime.ToString("yyyy-MM-dd",System.Globalization.CultureInfo.InvariantCulture)+"-";
                var current=files.LastOrDefault(f=>f.Name.StartsWith(prefix,StringComparison.Ordinal));
                bool create=current is null||current.Length+bytes.Length>limits.MaxFileBytes;
                int index=current is null?0:int.Parse(current.Name.AsSpan(20,6),System.Globalization.CultureInfo.InvariantCulture)+(create?1:0);
                if(index>999999){Rejected++;return;}
                string path=Path.Combine(directory,prefix+index.ToString("D6",System.Globalization.CultureInfo.InvariantCulture)+".log");long total=files.Sum(f=>f.Length);
                foreach(var file in files.ToArray())
                {
                    if(file.FullName==path)continue;
                    if(!Expired(file,settings.RetentionDays,now)&&total+bytes.Length<=limits.MaxTotalBytes&&files.Count+(create?1:0)<=limits.MaxFiles)continue;
                    ConsoleSettings.CheckLink(file.FullName);File.Delete(file.FullName);total-=file.Length;files.Remove(file);
                }
                if(total+bytes.Length>limits.MaxTotalBytes||files.Count+(create?1:0)>limits.MaxFiles){Rejected++;return;}
                ConsoleSettings.CheckLink(path);using(var stream=new FileStream(path,FileMode.Append,FileAccess.Write,FileShare.Read)){stream.Write(bytes);}
                WriteFailed=false;
            }
            catch(Exception e)when(e is IOException or UnauthorizedAccessException or InvalidDataException){WriteFailed=true;}
        }
    }
    internal int Cleanup(DateTimeOffset now)
    {
        lock(sync)
        {
            try
            {
                var files=Files();var limits=settings.Logs;long total=files.Sum(f=>f.Length);int count=files.Length,removed=0;
                foreach(var file in files)
                {
                    if(!Expired(file,settings.RetentionDays,now)&&total<=limits.MaxTotalBytes&&count<=limits.MaxFiles)continue;
                    ConsoleSettings.CheckLink(file.FullName);File.Delete(file.FullName);total-=file.Length;count--;removed++;
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
                    if(Expired(file,settings.RetentionDays,DateTimeOffset.Now)||remaining<=0)continue;
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
