using System.Diagnostics;
using System.Globalization;

namespace HttpMigration;

internal sealed record MemoryAdmissionOptions(bool Enabled=true,long MaxBodyBytes=0,int High=85,int Low=75)
{
    internal void Validate(){if(MaxBodyBytes<0||High is <1 or >100||Low is <1 or >100||Low>=High)throw new ArgumentException("Invalid memory admission settings");}
}
internal readonly record struct MemoryUsage(long Used,long Limit)
{
    internal long Threshold(int percent)=>Limit/100*percent+(Limit%100*percent+99)/100;
}
internal sealed record MemorySample(MemoryUsage Runtime,MemoryUsage[] Cgroups)
{
    internal static MemorySample Empty{get;}=new(default,[]);
    internal (bool High,bool Low) Watermarks(int high,int low)
    {
        bool atHigh=false,atLow=true;
        void Check(MemoryUsage scope){if(scope.Limit<=0)return;atHigh|=scope.Used>=scope.Threshold(high);atLow&=scope.Used<=scope.Threshold(low);}
        Check(Runtime);foreach(var scope in Cgroups)Check(scope);return(atHigh,atLow);
    }
    internal long BodyLimit(MemoryAdmissionOptions options)
    {
        if(!options.Enabled)return long.MaxValue;if(options.MaxBodyBytes>0)return options.MaxBodyBytes;
        long limit=64L<<20;if(Runtime.Limit>0)limit=Math.Min(limit,Runtime.Limit/4);
        foreach(var scope in Cgroups)if(scope.Limit>0)limit=Math.Min(limit,scope.Limit/8);
        return Math.Max(1,limit);
    }
}
// Native counters differ from Go: managed live bytes are bounded by the GC's
// effective hard limit, while cgroup counters also cover native/other processes.
// See learn.microsoft.com/dotnet/api/system.gcmemoryinfo.totalavailablememorybytes.
internal sealed class SystemMemorySampler
{
    readonly object sync=new();readonly (string Used,string Limit)[] files;
    long? last;MemorySample cached=MemorySample.Empty;
    internal SystemMemorySampler()
    {files=OperatingSystem.IsLinux()?CgroupPaths("/sys/fs/cgroup",Read("/proc/self/cgroup")):[];}
    static string Read(string path)
    {try{return File.ReadAllText(path);}catch(Exception e)when(e is IOException or UnauthorizedAccessException){return "";}}
    internal static (string Used,string Limit)[] CgroupPaths(string root,string proc)
    {
        var paths=new List<(string,string)>();var seen=new HashSet<string>(StringComparer.Ordinal);
        void Add(string basis,string relative,string used,string limit)
        {
            basis=Path.GetFullPath(basis);string dir=Path.GetFullPath(Path.Combine(basis,relative.TrimStart('/')));
            if(dir!=basis&&!dir.StartsWith(basis+Path.DirectorySeparatorChar,StringComparison.Ordinal))return;
            while(true){string path=Path.Combine(dir,used);if(seen.Add(path))paths.Add((path,Path.Combine(dir,limit)));if(dir==basis)break;dir=Path.GetDirectoryName(dir)!;}
        }
        foreach(string line in proc.Split('\n'))
        {
            var fields=line.Split(':',3);if(fields.Length!=3)continue;
            if(fields[0]=="0"&&fields[1]=="")Add(root,fields[2],"memory.current","memory.max");
            if(fields[1].Split(',').Contains("memory")){Add(Path.Combine(root,"memory"),fields[2],"memory.usage_in_bytes","memory.limit_in_bytes");Add(root,fields[2],"memory.usage_in_bytes","memory.limit_in_bytes");}
        }
        Add(root,"","memory.current","memory.max");Add(Path.Combine(root,"memory"),"","memory.usage_in_bytes","memory.limit_in_bytes");Add(root,"","memory.usage_in_bytes","memory.limit_in_bytes");return paths.ToArray();
    }
    internal MemorySample Snapshot()
    {
        lock(sync)
        {
            if(last is{} at&&Stopwatch.GetElapsedTime(at)<TimeSpan.FromMilliseconds(250))return cached;
            var groups=new List<MemoryUsage>();
            foreach(var file in files)
                if(long.TryParse(Read(file.Limit).Trim(),NumberStyles.None,CultureInfo.InvariantCulture,out long limit)&&limit>0&&limit<1L<<60&&long.TryParse(Read(file.Used).Trim(),NumberStyles.None,CultureInfo.InvariantCulture,out long used))groups.Add(new(used,limit));
            var info=GC.GetGCMemoryInfo();cached=new(new(GC.GetTotalMemory(false),info.TotalAvailableMemoryBytes),groups.ToArray());last=Stopwatch.GetTimestamp();return cached;
        }
    }
}
