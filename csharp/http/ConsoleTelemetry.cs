using System.Buffers;
using System.Diagnostics;
using System.Globalization;
using System.Text;
using System.Text.Json;
using System.Text.Json.Serialization;
using LedgerMigration;

namespace HttpMigration;

public sealed record ConsoleRow(string Account,string Model,long At,int Status,bool Complete,long? TotalTokens,long? InputTokens,long? OutputTokens,long? CachedTokens,double? Credit,double LatencyMs,double? TtftMs,string Protocol="",string Trace="",string Code="",string TtftMetric="");
public sealed record ConsoleAttempt(int Attempt,string Kind,int StatusCode,string BusinessCode,string Outcome,string FailureClass,double StartedMs,double? HeadersMs,double DurationMs);
public sealed record ConsoleAccount(string Id,string File,string Edition);
internal sealed record HistoryLogEntry(long Seq,ConsoleRow Row);

public sealed partial class ConsoleTelemetry
{
    const int Limit=20000,FileLimit=16<<20,SnapshotEvery=4096;
    // Lock order: requestLogDisk -> sync. Never hold sync during request-log I/O.
    readonly object sync=new(),disk=new(),requestLogDisk=new();readonly Queue<ConsoleRow> rows=new();readonly Queue<(long At,string Line)> logs=new();
    readonly List<HistoryLogEntry> historyPending=[];
    long logSeq,savedLogSeq,repairLength=-1;
    internal Func<string,Stream>? HistoryLogStreamFactory = null;
    internal Action<string>? BeforeRequestLogIo = null;
    readonly Queue<(long At,string Trace,object Data)> traces=new();volatile Dictionary<string,ConsoleAccount> accounts=new(StringComparer.Ordinal);
    public void RefreshAccounts(IEnumerable<ConsoleAccount> current)
    {lock(sync){var next=new Dictionary<string,ConsoleAccount>(accounts,StringComparer.Ordinal);foreach(var item in current){ValidateAccount(item);if(next.GetValueOrDefault(item.Id)!=item){next[item.Id]=item;revision++;}}accounts=next;}}
    readonly ConsoleSettings settings;long revision,saved=-1;int logBytes;public long StartedAt{get;}=DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
    readonly RequestLogStore? requestLogs;
    readonly bool logInitializationFailed;
    public bool LogWriteFailed=>logInitializationFailed||(requestLogs?.WriteFailed??false);
    public bool WriteFailed{get;private set;}public string Storage{get;private set;}="memory";
    [JsonSourceGenerationOptions(JsonSerializerDefaults.Web, DefaultIgnoreCondition = JsonIgnoreCondition.WhenWritingNull)]
    [JsonSerializable(typeof(ConsoleAccount))]
    [JsonSerializable(typeof(ConsoleAccount[]))]
    [JsonSerializable(typeof(ConsoleRow[]))]
    [JsonSerializable(typeof(ConsoleRow))]
    [JsonSerializable(typeof(HistoryLogEntry))]
    partial class HistoryJson : JsonSerializerContext { }
    public ConsoleTelemetry(ConsoleSettings settings,IEnumerable<ConsoleAccount> accounts,IReadOnlyDictionary<string,string>? legacyIdentities=null)
    {
        this.settings=settings;foreach(var a in accounts){ValidateAccount(a);this.accounts[a.Id]=a;}
        if(settings.DirectoryPath is not{} directory)return;
        try{requestLogs=new(directory,settings);}
        catch(Exception e)when(e is IOException or UnauthorizedAccessException or InvalidDataException){logInitializationFailed=true;}
        string path=Path.Combine(directory,"history.json"),logPath=Path.Combine(directory,"history.jsonl");
        try
        {
            long snapshotSeq=0;bool snapshotLoaded=false;
            if(File.Exists(path))
            {
                using var doc=JsonDocument.Parse(ConsoleSettings.Read(path,FileLimit));if(doc.RootElement.GetProperty("schema").GetInt32()!=1)throw new InvalidDataException();
                if(doc.RootElement.TryGetProperty("logSeq",out var savedSeq)&&savedSeq.TryGetInt64(out long saved))snapshotSeq=saved;
                var source=JsonSerializer.Deserialize(doc.RootElement.GetProperty("requests"), HistoryJson.Default.ConsoleRowArray)??[];if(source.Length>Limit)throw new InvalidDataException();foreach(var r in source)Validate(r);
                var historical=doc.RootElement.TryGetProperty("accounts",out var savedAccounts)?JsonSerializer.Deserialize(savedAccounts, HistoryJson.Default.ConsoleAccountArray)??[]:[];
                if(historical.Length>Limit)throw new InvalidDataException();var ids=new HashSet<string>(StringComparer.Ordinal);
                foreach(var a in historical){ValidateAccount(a);if(!ids.Add(a.Id))throw new InvalidDataException();}
                foreach(var a in historical)this.accounts.TryAdd(a.Id,a);foreach(var r in source)rows.Enqueue(r);Storage="file";snapshotLoaded=true;
            }
            else if(File.Exists(Path.Combine(directory,"history-seed.json")))ImportSeed(ConsoleSettings.Read(Path.Combine(directory,"history-seed.json"),64<<20),legacyIdentities??new Dictionary<string,string>());
            else if(!File.Exists(logPath))Storage="file";
            logSeq=savedLogSeq=snapshotSeq;
            if(File.Exists(logPath))LoadHistoryLog(logPath,snapshotSeq);
            else if(!snapshotLoaded&&Storage=="file")Storage="file";
        }
        catch(Exception e) when(e is IOException or InvalidDataException or UnauthorizedAccessException or JsonException or ArgumentException or InvalidOperationException or KeyNotFoundException){Storage="invalid";WriteFailed=true;}
    }
    static ReadOnlySpan<byte> TrimLine(ReadOnlySpan<byte> value)
    {
        int start=0,end=value.Length;
        while(start<end&&value[start]<=(byte)' ')start++;
        while(end>start&&value[end-1]<=(byte)' ')end--;
        return value[start..end];
    }
    void LoadHistoryLog(string path,long snapshotSeq)
    {
        byte[] raw=ConsoleSettings.Read(path,64<<20);
        long highest=snapshotSeq;int offset=0,lastGood=0;bool repair=false,addNewline=false;
        while(offset<raw.Length)
        {
            var span=raw.AsSpan(offset);int newline=span.IndexOf((byte)'\n');
            int lineEnd=newline<0?raw.Length:offset+newline;
            var line=TrimLine(raw.AsSpan(offset,lineEnd-offset));
            if(line.Length>0)
            {
                HistoryLogEntry? entry;
                try{entry=JsonSerializer.Deserialize(line,HistoryJson.Default.HistoryLogEntry);}
                catch(JsonException)
                {
                    if(newline<0){repair=true;break;} // torn append; drop it before the next write
                    throw new InvalidDataException("Invalid history log");
                }
                if(entry is not null)
                {
                    Validate(entry.Row);
                    if(entry.Seq>highest)
                    {
                        while(rows.Count>=Limit)rows.Dequeue();
                        rows.Enqueue(entry.Row);highest=entry.Seq;
                    }
                }
            }
            if(newline<0){lastGood=raw.Length;if(line.Length>0){repair=true;addNewline=true;}break;}
            offset=lineEnd+1;lastGood=offset;
        }
        if(repair)
        {
            try
            {
                using var log=new FileStream(path,FileMode.Open,FileAccess.Write,FileShare.Read);
                log.SetLength(lastGood);
                if(addNewline){log.Seek(0,SeekOrigin.End);log.WriteByte((byte)'\n');}
                log.Flush(true);
            }
            catch(IOException e){throw new InvalidDataException("History log repair failed",e);}
        }
        logSeq=highest;savedLogSeq=snapshotSeq;Storage="file";
    }
    static void Validate(ConsoleRow r)
    {
        if(r is null||r.Account is null||r.Account.Length>256||r.Model is null||r.Model.Length>256||r.Model.Any(char.IsControl)||r.Trace is null||r.Trace.Length>64||r.Trace.Any(char.IsControl)||r.Code is null||r.Code.Length>128||r.Code.Any(char.IsControl)||r.Protocol is not("" or "messages" or "chat_completions" or "responses")||r.At<=0||r.At>DateTimeOffset.UtcNow.ToUnixTimeMilliseconds()+300000||!double.IsFinite(r.LatencyMs)||r.LatencyMs<0||r.LatencyMs>86400000||r.Credit is{} c&&(!double.IsFinite(c)||c<0||c>1e12)||r.TtftMs is{} t&&(!double.IsFinite(t)||t<0||t>86400000)||BadTokens(r.TotalTokens)||BadTokens(r.InputTokens)||BadTokens(r.OutputTokens)||BadTokens(r.CachedTokens))throw new InvalidDataException("Invalid statistics record");
    }
    static bool BadTokens(long? n)=>n is <0 or >1000000000000;
    static void ValidateAccount(ConsoleAccount a)
    {if(a is null||string.IsNullOrEmpty(a.Id)||a.Id.Length>256||a.Id.Any(char.IsControl)||a.File is null||a.File.Length>1024||a.File.Any(char.IsControl)||a.Edition is not("" or "cn" or "intl"))throw new InvalidDataException("Invalid account summary");}
    void ImportSeed(byte[] raw,IReadOnlyDictionary<string,string> mapping)
    {
        using var doc=JsonDocument.Parse(raw);var root=doc.RootElement;if(root.GetProperty("schema").GetInt32()!=1)throw new InvalidDataException();var entries=root.GetProperty("requests");if(entries.ValueKind!=JsonValueKind.Array||entries.GetArrayLength()>Limit)throw new InvalidDataException();var staged=new List<ConsoleRow>();
        foreach(var item in entries.EnumerateArray())
        {
            var r=JsonSerializer.Deserialize(item, HistoryJson.Default.ConsoleRow)??throw new InvalidDataException();Validate(r);string account=mapping.GetValueOrDefault(r.Account)??"archived:"+r.Account[..Math.Min(48,r.Account.Length)];r=r with{Account=account};staged.Add(r);
        }
        foreach(var r in staged)rows.Enqueue(r);revision++;Storage="imported";
    }
    internal ConsoleTrace Begin(string protocol,string trace)=>new(this,protocol,trace);
    internal bool TrackCache=>settings.Logs.CacheDiagnostics;
    internal void Complete(ConsoleRow row,ConsoleAttempt[] attempts,string streamDetails="")
    {
        Validate(row);
        string line=$"{DateTimeOffset.FromUnixTimeMilliseconds(row.At):O} trace={row.Trace} protocol={row.Protocol} model={row.Model} status={row.Status} result={(row.Complete?"complete":"failed")} code={row.Code} durationMs={row.LatencyMs:F1}"+streamDetails;
        lock(requestLogDisk)
        {
            lock(sync)
            {
                while(rows.Count>=Limit)rows.Dequeue();rows.Enqueue(row);revision++;
                long seq=++logSeq;historyPending.Add(new(seq,row));
                if(historyPending.Count>8192)historyPending.RemoveAt(0);
                AddLog(row.At,line);
                while(traces.Count>=128)traces.Dequeue();traces.Enqueue((row.At,row.Trace,new Dictionary<string, object?> { ["trace"] = row.Trace, ["status"] = row.Status, ["attemptTrail"] = attempts, ["counters"] = new Dictionary<string, object?> { ["failed"] = row.Complete?0:1, ["canceled"] = row.Code=="cancelled"?1:0, ["incompleteStreams"] = row.Complete?0:1 }, ["attemptsDropped"] = 0 }));
            }
            BeforeRequestLogIo?.Invoke("append");requestLogs?.Append(line,DateTimeOffset.FromUnixTimeMilliseconds(row.At));
        }
    }
    void RemoveLog(){logBytes-=System.Text.Encoding.UTF8.GetByteCount(logs.Dequeue().Line);}
    void AddLog(long at,string line){int size=System.Text.Encoding.UTF8.GetByteCount(line);while(logs.Count>=1000||logBytes+size>1048576)RemoveLog();logs.Enqueue((at,line));logBytes+=size;}
    public void Audit(string code)
    {
        if(code.Length>80||code.Any(c=>!char.IsAsciiLetterOrDigit(c)&&c!='_'))return;
        lock(requestLogDisk)
        {
            var now=DateTimeOffset.UtcNow;string line=now.ToString("O")+" admin="+code;
            lock(sync)AddLog(now.ToUnixTimeMilliseconds(),line);
            BeforeRequestLogIo?.Invoke("append");requestLogs?.Append(line,now);
        }
    }
    public ConsoleRow[] Rows(){lock(sync)return rows.ToArray();}
    internal ConsoleRow[] RowsSince(long cutoff)
    {
        lock(sync) return rows.Where(row => row.At >= cutoff).ToArray();
    }
    public object? Trace(string id){lock(sync)return traces.LastOrDefault(t=>t.Trace==id&&t.At>=DateTimeOffset.UtcNow.AddMinutes(-5).ToUnixTimeMilliseconds()).Data;}
    public string[] Logs(int count)
    {
        count=Math.Clamp(count,1,1000);
        lock(requestLogDisk)
        {
            if(requestLogs is not null)
            {
                BeforeRequestLogIo?.Invoke("tail");var persisted=requestLogs.Tail(count);
                if(!requestLogs.WriteFailed)return persisted;
            }
            lock(sync)
            {
                long cutoff=LogCutoff(settings.RetentionDays,DateTimeOffset.UtcNow);
                return logs.Where(l=>l.At>=cutoff).TakeLast(count).Select(l=>l.Line).ToArray();
            }
        }
    }
    static long LogCutoff(int days,DateTimeOffset now)=>days==0?0:new DateTimeOffset(now.LocalDateTime.Date.AddDays(-(days-1))).ToUnixTimeMilliseconds();
    public int Cleanup(DateTimeOffset? now=null)
    {
        lock(requestLogDisk)
        {
            var time=now??DateTimeOffset.UtcNow;
            BeforeRequestLogIo?.Invoke("cleanup");requestLogs?.Cleanup(time);
            lock(sync)
            {
                int days=settings.RetentionDays;if(days==0)return 0;long cutoff=LogCutoff(days,time);
                // Completion order need not match timestamps; filter the entire bounded queue.
                var keep=logs.Where(l=>l.At>=cutoff).ToArray();int n=logs.Count-keep.Length;
                logs.Clear();logBytes=0;foreach(var entry in keep)AddLog(entry.At,entry.Line);return n;
            }
        }
    }
    internal bool NeedsSnapshot{get{lock(sync)return logSeq-savedLogSeq>=SnapshotEvery;}}
    public bool FlushHistory()
    {
        if(settings.DirectoryPath is not{} directory||Storage=="invalid")return false;
        lock(disk)
        {
            string path=Path.Combine(directory,"history.jsonl");
            try
            {
                ConsoleSettings.CheckLink(path);
                if(repairLength>=0)
                {
                    if(!TryTruncate(path,repairLength)){WriteFailed=true;return false;}
                    repairLength=-1;
                }
                List<HistoryLogEntry> batch;
                lock(sync){if(historyPending.Count==0)return true;batch=[..historyPending];historyPending.Clear();}
                long start=-1;
                try
                {
                    using var log=HistoryLogStreamFactory?.Invoke(path)??new FileStream(path,FileMode.OpenOrCreate,FileAccess.Write,FileShare.Read,65536,FileOptions.SequentialScan);
                    log.Seek(0,SeekOrigin.End);start=log.Length;
                    foreach(var entry in batch){byte[] line=JsonSerializer.SerializeToUtf8Bytes(entry,HistoryJson.Default.HistoryLogEntry);log.Write(line);log.WriteByte((byte)'\n');}
                    if(log is FileStream fileStream)fileStream.Flush(false);else log.Flush();
                    WriteFailed=false;return true;
                }
                catch(Exception e) when(e is IOException or UnauthorizedAccessException or JsonException)
                {
                    if(start>=0&&TryTruncate(path,start))repairLength=-1;else if(start>=0)repairLength=start;
                    lock(sync){historyPending.InsertRange(0,batch);while(historyPending.Count>8192)historyPending.RemoveAt(0);}
                    WriteFailed=true;return false;
                }
            }
            catch(Exception e) when(e is IOException or UnauthorizedAccessException)
            {WriteFailed=true;return false;}
        }
    }
    static bool TryTruncate(string path,long length)
    {
        try
        {
            using var file=new FileStream(path,FileMode.OpenOrCreate,FileAccess.Write,FileShare.Read);
            file.SetLength(length);file.Flush(true);return true;
        }
        catch(Exception e) when(e is IOException or UnauthorizedAccessException){return false;}
    }
    public bool Save()
    {
        if(settings.DirectoryPath is not{} directory||Storage=="invalid")return false;
        lock(disk)
        {
            if(!FlushHistory())return false;
            long version;ConsoleRow[] snapshot;ConsoleAccount[] summaries;long seq;
            lock(sync)
            {
                if(saved==revision&&savedLogSeq==logSeq)return true;
                version=revision;snapshot=rows.ToArray();seq=logSeq;
                var referenced=snapshot.Select(r=>r.Account).ToHashSet(StringComparer.Ordinal);
                summaries=accounts.Values.OrderByDescending(a=>referenced.Contains(a.Id)).ThenBy(a=>a.Id,StringComparer.Ordinal).Take(Limit).ToArray();
            }
            try
            {
                ConsoleSettings.Atomic(Path.Combine(directory,"history.json"),file=>WriteHistory(file,summaries,snapshot,seq),FileOptions.SequentialScan);
                string logPath=Path.Combine(directory,"history.jsonl");ConsoleSettings.CheckLink(logPath);
                using(var log=new FileStream(logPath,FileMode.Create,FileAccess.Write,FileShare.Read)){log.Flush(true);}
                lock(sync){saved=version;savedLogSeq=seq;WriteFailed=false;Storage="file";}return true;
            }
            catch(Exception e) when(e is IOException or InvalidDataException or UnauthorizedAccessException or JsonException){WriteFailed=true;return false;}
        }
    }
    void WriteHistory(FileStream file,ConsoleAccount[] summaries,ConsoleRow[] snapshot,long logSeq)
    {
        const int flushThreshold=32<<10;
        var buffer=new ArrayBufferWriter<byte>(2*flushThreshold);
        using var writer=new Utf8JsonWriter(buffer);
        void FlushIfNeeded(bool final=false)
        {
            if (writer.BytesCommitted+writer.BytesPending>FileLimit)
                throw new InvalidDataException("History exceeds its file budget");
            if (!final && writer.BytesPending+buffer.WrittenCount<flushThreshold) return;
            writer.Flush();
            file.Write(buffer.WrittenSpan);
            buffer.ResetWrittenCount();
        }

        writer.WriteStartObject();
        writer.WriteNumber("schema",1);
        writer.WriteNumber("startedAt",StartedAt);
        writer.WriteNumber("logSeq",logSeq);
        writer.WriteStartArray("accounts");
        foreach (var account in summaries)
        {
            JsonSerializer.Serialize(writer,account,HistoryJson.Default.ConsoleAccount);
            FlushIfNeeded();
        }
        writer.WriteEndArray();
        writer.WriteStartArray("requests");
        foreach (var row in snapshot)
        {
            JsonSerializer.Serialize(writer,row,HistoryJson.Default.ConsoleRow);
            FlushIfNeeded();
        }
        writer.WriteEndArray();
        writer.WriteEndObject();
        FlushIfNeeded(final:true);
    }
    public object Stats(IQueryCollection query)
    {
        string period=query["period"].ToString();if(period=="")period="day";if(period is not("hour" or "day" or "week" or "month"))throw new ArgumentException("统计粒度无效");
        var zone=TimeSpan.FromHours(8);var today=DateTimeOffset.UtcNow.ToOffset(zone).Date;
        DateTime Parse(string key,DateTime fallback){string text=query[key].ToString();return text==""?fallback:DateTime.TryParseExact(text,"yyyy-MM-dd",CultureInfo.InvariantCulture,DateTimeStyles.None,out var date)?date:throw new ArgumentException("日期无效");}
        var start=Parse("start",today.AddDays(period=="hour"?-1:-6));var end=Parse("end",today);if(end<start||(end-start).TotalDays>366)throw new ArgumentException("统计区间最多367天");
        long lo=new DateTimeOffset(start,zone).ToUnixTimeMilliseconds(),hi=new DateTimeOffset(end.AddDays(1),zone).ToUnixTimeMilliseconds();string account=query["account"].ToString(),site=query["site"].ToString();if(site is not("" or "cn" or "intl"))throw new ArgumentException("站点无效");
        var snapshot=Rows();var choices=accounts.Values.Concat(snapshot.Select(r=>r.Account).Distinct().Where(id=>!accounts.ContainsKey(id)).Take(1024).Select(id=>new ConsoleAccount(id,"旧版历史账号", ""))).ToArray();
        if(account!=""&&!choices.Any(a=>a.Id==account))throw new ArgumentException("账号不存在");var selected=snapshot.Where(r=>r.At>=lo&&r.At<hi&&(account==""||r.Account==account)&&(site==""||accounts.GetValueOrDefault(r.Account)?.Edition==site)).ToArray();
        DateTime Bucket(DateTime d)=>period switch{"hour"=>new(d.Year,d.Month,d.Day,d.Hour,0,0),"week"=>d.Date.AddDays(-((int)d.DayOfWeek+6)%7),"month"=>new(d.Year,d.Month,1),_=>d.Date};
        var grouped=selected.GroupBy(r=>Bucket(DateTimeOffset.FromUnixTimeMilliseconds(r.At).ToOffset(zone).DateTime)).ToDictionary(g=>g.Key,g=>g.ToArray());var buckets=new List<Dictionary<string,object?>>();var points=new List<ConsoleRow[]>();
        for(var t=Bucket(start);t<end.AddDays(1);t=period switch{"hour"=>t.AddHours(1),"week"=>t.AddDays(7),"month"=>t.AddMonths(1),_=>t.AddDays(1)})
        {if(buckets.Count>=2000)throw new ArgumentException("统计桶超过2000，请缩短小时区间");var value=grouped.GetValueOrDefault(t)??[];var aggregate=Aggregate(value);aggregate["start"]=new DateTimeOffset(t,zone).ToUnixTimeMilliseconds();aggregate["label"]=t.ToString(period=="hour"?"MM-dd HH:mm":"yyyy-MM-dd",CultureInfo.InvariantCulture);buckets.Add(aggregate);points.Add(value);}
        var models=selected.GroupBy(r=>r.Model).Select(g=>{var item=Aggregate(g.ToArray());item["model"]=g.Key;return item;}).OrderByDescending(r=>(long)r["tokens"]!).ThenByDescending(r=>(int)r["requests"]!).Take(1024).ToArray();
        var latency=selected.GroupBy(r=>r.Model).OrderByDescending(g=>g.Count()).Take(8).Select(g=>new Dictionary<string, object?> { ["model"] = g.Key, ["avgLatencyMs"] = points.Select(p=>Average(p.Where(r=>r.Model==g.Key).Select(r=>(double?)r.LatencyMs))).ToArray(), ["avgTtftMs"] = points.Select(p=>Average(p.Where(r=>r.Model==g.Key&&r.TtftMetric=="v2").Select(r=>r.TtftMs))).ToArray() }).ToArray();
        return new Dictionary<string, object?> { ["period"] = period, ["start"] = start.ToString("yyyy-MM-dd"), ["end"] = end.ToString("yyyy-MM-dd"), ["serverTime"] = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(), ["account"] = account, ["accounts"] = choices.Select(a=>new Dictionary<string, object?> { ["id"] = a.Id, ["file"] = a.File, ["edition"] = a.Edition }).ToArray(), ["summary"] = Aggregate(selected), ["buckets"] = buckets, ["models"] = models, ["modelLatency"] = latency, ["ttftMetric"] = "first_token_v2", ["storage"] = new Dictionary<string, object?> { ["mode"] = Storage, ["problem"] = WriteFailed?"统计保存失败，现有数据保留":"", ["maxRequests"] = Limit }, ["startedAt"] = StartedAt };
    }
    static double? Average(IEnumerable<double?> values) => values.Average();
    internal static Dictionary<string,object?> Aggregate(ConsoleRow[] rows)=>new()
    {
        ["requests"]=rows.Length,["success"]=rows.Count(r=>r.Complete),["failed"]=rows.Count(r=>!r.Complete),["tokens"]=rows.Sum(r=>r.TotalTokens??0),["inputTokens"]=rows.Sum(r=>r.InputTokens??0),["outputTokens"]=rows.Sum(r=>r.OutputTokens??0),["cachedTokens"]=rows.Sum(r=>r.CachedTokens??0),["credits"]=rows.Sum(r=>r.Credit??0),["tokenSamples"]=rows.Count(r=>r.TotalTokens.HasValue),["creditSamples"]=rows.Count(r=>r.Credit.HasValue),["ttftSamples"]=rows.Count(r=>r.TtftMetric=="v2"&&r.TtftMs.HasValue),["avgLatencyMs"]=rows.Length==0?null:rows.Average(r=>r.LatencyMs),["avgTtftMs"]=Average(rows.Where(r=>r.TtftMetric=="v2").Select(r=>r.TtftMs)),["ttftMetric"]="first_token_v2"
    };
    public object Performance(string? trace)
    {
        if(!string.IsNullOrEmpty(trace))return Trace(trace)??throw new KeyNotFoundException();var recent=RowsSince(DateTimeOffset.UtcNow.AddMinutes(-5).ToUnixTimeMilliseconds());
        object Phase(string name,string label,double[] values)=>new Dictionary<string, object?> { ["name"] = name, ["label"] = label, ["samples"] = values.Length, ["failed"] = 0, ["averageMs"] = values.Length==0?(double?)null:values.Average(), ["p95Ms"] = Percentile(values,.95), ["p99Ms"] = Percentile(values,.99) };
        var protocols=new[]{"all","messages","chat_completions","responses"}.Select(protocol=>{var data=recent.Where(r=>protocol=="all"||r.Protocol==protocol).ToArray();return new Dictionary<string, object?> { ["protocol"] = protocol, ["requests"] = data.Length, ["failed"] = data.Count(r=>!r.Complete), ["canceled"] = data.Count(r=>r.Code=="cancelled"), ["incompleteStreams"] = data.Count(r=>!r.Complete), ["phases"] = new[]{Phase("total","总耗时",data.Select(r=>r.LatencyMs).ToArray()),Phase("first_token","有效首Token",data.Where(r=>r.TtftMs.HasValue).Select(r=>r.TtftMs!.Value).ToArray())} };});
        return new Dictionary<string, object?> { ["asOf"] = DateTimeOffset.UtcNow.ToUnixTimeSeconds(), ["startedAt"] = StartedAt/1000, ["protocols"] = protocols.ToArray() };
    }
    static double? Percentile(double[] data,double fraction){if(data.Length==0)return null;Array.Sort(data);return data[Math.Min(data.Length-1,(int)Math.Ceiling(data.Length*fraction)-1)];}
}

internal sealed class ConsoleTrace(ConsoleTelemetry owner,string protocol,string trace)
{
    readonly long start=Stopwatch.GetTimestamp();readonly List<ConsoleAttempt> attempts=[];string account="",model="",code="";long? input,output,cached,total;double? credit,ttft;bool complete;
    long attemptStart;int attemptNumber;double? headers;
    bool trackStream;
    long? requestedOutputTokens;
    int? sentOutputTokens;
    DeadlineWriteStream? downstream;
    double? lastUpstreamReadMs;
    CacheDiagnostics? cacheDiagnostics;
    long? reportedCacheRead;
    internal void Model(string value)=>model=value;
    internal void Attempt(string id){account=id;attemptStart=Stopwatch.GetTimestamp();attemptNumber++;headers=null;lastUpstreamReadMs=null;}
    internal void TrackStream(DeadlineWriteStream? response, long? requested, int? sent)
    {
        trackStream=true;
        downstream=response;
        if(response is not null)response.TrackBytes=true;
        requestedOutputTokens=requested;
        sentOutputTokens=sent;
    }
    internal void UpstreamRead()=>lastUpstreamReadMs=Stopwatch.GetElapsedTime(attemptStart).TotalMilliseconds;
    internal void TrackCache(byte[] body,string caller,string? session,string account,string? credentialIdentity,string route)
    {
        cacheDiagnostics=null;
        reportedCacheRead=null;
        if(!owner.TrackCache || body.Length<CacheDiagnostics.MinimumBodyBytes)return;
        try{cacheDiagnostics=CacheDiagnostics.Read(body,caller,model,session,account,credentialIdentity,route);}
        catch(JsonException){/* Diagnostics do not participate in request validation. */}
    }
    internal void Headers(){headers=Stopwatch.GetElapsedTime(attemptStart).TotalMilliseconds;}
    internal void First(){ttft??=Stopwatch.GetElapsedTime(start).TotalMilliseconds;}
    internal void Result(StreamingMigration.StreamOutcome outcome,string failure){if(outcome!=StreamingMigration.StreamOutcome.Complete){complete=false;code=failure.Length<=128&&!failure.Any(char.IsControl)?failure:"stream_failed";}}
    internal void Observe(string? raw,string businessCode,int status,bool success)
    {
        code=businessCode.Length<=128&&!businessCode.Any(char.IsControl)?businessCode:"invalid_code";complete=success;
        if(attempts.Count<16)attempts.Add(new(attemptNumber,attemptNumber<=1?"initial":"retry",status,code,success?"complete":"failed",code,Stopwatch.GetElapsedTime(start,attemptStart==0?start:attemptStart).TotalMilliseconds,headers,attemptStart==0?0:Stopwatch.GetElapsedTime(attemptStart).TotalMilliseconds));
        try
        {
            if(raw is null||raw.Length>65536)return;
            if(cacheDiagnostics is not null&&success)reportedCacheRead=SessionAffinity.CachedTokens(raw);
            var observed=CreditRules.Observe(raw);
            if(observed.Kind is SettlementKind.Charged or SettlementKind.Zero){double sum=(credit??0)+observed.Credit;credit=double.IsFinite(sum)&&sum<=1e12?sum:null;}
            using var doc=JsonDocument.Parse(raw);var r=doc.RootElement;if(r.TryGetProperty("prompt_tokens",out _)&&r.TryGetProperty("completion_tokens",out _)){input=Math.Min((input??0)+Math.Min(observed.Input,500000000000),500000000000);output=Math.Min((output??0)+Math.Min(observed.Output,500000000000),500000000000);cached=Math.Min((cached??0)+Math.Min(observed.Cached,1000000000000),1000000000000);total=input+output;}
        }
        catch(Exception e) when(e is JsonException or ArgumentException or OverflowException or InvalidOperationException){/* Observability never changes settlement. */}
    }
    internal void End(int status)
    {
        if(model.Length==0||model.Length>256)return;
        bool succeeded=complete&&status<400;
        string streamDetails=trackStream&&!succeeded&&attemptStart!=0
            ?FormattableString.Invariant($" upstreamMs={Stopwatch.GetElapsedTime(attemptStart).TotalMilliseconds:F1} lastUpstreamReadMs={lastUpstreamReadMs:F1} downstreamBytes={downstream?.BytesWritten} requestedOutputTokens={requestedOutputTokens} sentOutputTokens={sentOutputTokens}")
            :"";
        if(succeeded && cacheDiagnostics is {} diagnostic)
            streamDetails+=FormattableString.Invariant($" cacheDiag=1 cacheSession={diagnostic.Session} cacheTools={diagnostic.Tools} cacheSystem={diagnostic.SystemPrompt} cacheHistory={diagnostic.History} cacheAccount={diagnostic.Account} cacheRoute={diagnostic.Route} cacheInputTokens={input} cacheReadTokens={reportedCacheRead} cacheTtftMs={ttft:F1}");
        try{owner.Complete(new(account,model,DateTimeOffset.UtcNow.ToUnixTimeMilliseconds(),status,succeeded,total,input,output,cached,credit,Stopwatch.GetElapsedTime(start).TotalMilliseconds,ttft,protocol,trace,status==499?"cancelled":code,"v2"),attempts.ToArray(),streamDetails);}
        catch(Exception e)when(e is IOException or InvalidDataException or ArgumentException or InvalidOperationException or OverflowException){owner.Audit("invalid_statistics_skipped");}
    }
}

internal sealed class ConsoleHistoryWorker(ConsoleTelemetry telemetry,ScheduledTaskJournal? tasks=null,ModelCatalog? catalog=null,CatalogCache? cache=null):BackgroundService,IHostedLifecycleService
{
    void Flush(){telemetry.FlushHistory();if(telemetry.NeedsSnapshot)telemetry.Save();tasks?.Save();if(cache is not null&&catalog?.NeedsSave==true)cache.TrySave(catalog);}
    void Save(){telemetry.Save();tasks?.Save();if(cache is not null&&catalog?.NeedsSave==true)cache.TrySave(catalog);}
    protected override async Task ExecuteAsync(CancellationToken token){using var timer=new PeriodicTimer(TimeSpan.FromSeconds(5));while(await timer.WaitForNextTickAsync(token))await Task.Run(Flush,CancellationToken.None);}
    public Task StartingAsync(CancellationToken token)=>Task.CompletedTask;public Task StartedAsync(CancellationToken token)=>Task.CompletedTask;public Task StoppingAsync(CancellationToken token)=>Task.CompletedTask;
    public Task StoppedAsync(CancellationToken token){Save();return Task.CompletedTask;}
}
