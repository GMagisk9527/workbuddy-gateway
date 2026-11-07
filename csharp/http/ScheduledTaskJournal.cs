using System.Text.Json;

namespace HttpMigration;

public sealed class ScheduledTaskJournal
{
    public sealed record Entry(string Id,ScheduledTaskSnapshot Run);
    readonly object sync=new(),disk=new();readonly Queue<Entry> entries=[];
    readonly string? path;long revision,saved;bool invalid;
    public bool WriteFailed{get;private set;}
    public ScheduledTaskJournal(string? directory)
    {
        if(directory is null)return;path=Path.Combine(directory,"task-history.json");
        try
        {
            if(!File.Exists(path))return;
            var rows=JsonSerializer.Deserialize<Entry[]>(ConsoleSettings.Read(path,512<<10))??throw new InvalidDataException();
            if(rows.Length>256||rows.Any(e=>e is null||e.Id is null||e.Run is null||e.Id.Length>40||e.Id.Any(c=>!char.IsAsciiLetter(c)&&c!='_')||e.Run.TraceId is null||e.Run.TraceId.Length>64||e.Run.TraceId.Any(c=>!char.IsAsciiHexDigit(c))||e.Run.Status is not("success" or "failed" or "partial" or "skipped" or "cancelled")))throw new InvalidDataException();
            foreach(var entry in rows)entries.Enqueue(entry);
        }
        catch(Exception e)when(e is IOException or UnauthorizedAccessException or InvalidDataException or JsonException){invalid=true;WriteFailed=true;}
    }
    public void Attach(string id,ScheduledTaskState state)=>state.Completed+=run=>Record(id,run);
    void Record(string id,ScheduledTaskSnapshot run)
    {
        lock(sync)
        {
            var last=entries.LastOrDefault(e=>e.Id==id);
            // Match Go's summarized high-frequency task logging.
            if(id is "config_reload" or "status_snapshot"&&last is not null&&last.Run.Status==run.Status&&run.LastFinishedAt-last.Run.LastFinishedAt<60)return;
            while(entries.Count>=256)entries.Dequeue();entries.Enqueue(new(id,run));revision++;
        }
    }
    public string[] Lines(int count,int retentionDays)
    {lock(sync){long cutoff=retentionDays==0?0:new DateTimeOffset(DateTime.Now.Date.AddDays(-(retentionDays-1))).ToUnixTimeSeconds();return entries.Where(e=>e.Run.LastFinishedAt>=cutoff).TakeLast(Math.Clamp(count,1,256)).Select(e=>JsonSerializer.Serialize(e)).ToArray();}}
    public int Cleanup(int retentionDays,DateTimeOffset now)
    {
        if(retentionDays==0)return 0;lock(sync)
        {
            long cutoff=new DateTimeOffset(now.LocalDateTime.Date.AddDays(-(retentionDays-1))).ToUnixTimeSeconds();
            var keep=entries.Where(e=>e.Run.LastFinishedAt>=cutoff).ToArray();int removed=entries.Count-keep.Length;
            if(removed>0){entries.Clear();foreach(var e in keep)entries.Enqueue(e);revision++;}return removed;
        }
    }
    public bool Save()
    {
        if(path is null)return true;if(invalid)return false;
        lock(disk)
        {
            Entry[] rows;long version;lock(sync){if(revision==saved)return true;rows=entries.ToArray();version=revision;}
            try{ConsoleSettings.Atomic(path,JsonSerializer.SerializeToUtf8Bytes(rows));lock(sync){saved=version;WriteFailed=false;}return true;}
            catch(Exception e)when(e is IOException or UnauthorizedAccessException){WriteFailed=true;return false;}
        }
    }
}
