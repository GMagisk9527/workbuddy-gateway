using System.Security.Cryptography;
using System.Text;
using System.Text.Json;

namespace HttpMigration;

public sealed record CatalogCacheStatus(string LoadedFrom,bool WriteFailed,long Saves);

// An expendable capability cache. It never owns credentials, quotas, or the Go cache.
public sealed class CatalogCache : IDisposable
{
    const string Marker=".csharp-catalog-cache";
    const string Ownership="WorkBuddy C# model catalog cache v1\n";
    const int MaxSnapshot=8<<20, MaxEnvelope=MaxSnapshot+4096;
    readonly string root;
    readonly FileStream owner;
    readonly object sync=new();
    bool disposed;
    CatalogCacheStatus status=new("none",false,0);
    public CatalogCacheStatus Status=>Volatile.Read(ref status);
    CatalogCache(string directory)
    {
        root=Path.GetFullPath(directory);Directory.CreateDirectory(root);
        string marker=Path.Combine(root,Marker);
        if(!File.Exists(marker))
        {
            if(Directory.EnumerateFileSystemEntries(root).Any())throw new ArgumentException("Catalog cache requires a new empty or owned directory");
            using var file=new FileStream(marker,FileMode.CreateNew,FileAccess.Write,FileShare.None);
            file.Write(Encoding.ASCII.GetBytes(Ownership));file.Flush(true);
        }
        using(var file=new FileStream(marker,FileMode.Open,FileAccess.Read,FileShare.Read))
        {
            if(file.Length!=Ownership.Length)throw new ArgumentException("Invalid catalog cache ownership marker");
            byte[] bytes=new byte[Ownership.Length];file.ReadExactly(bytes);
            if(!bytes.AsSpan().SequenceEqual(Encoding.ASCII.GetBytes(Ownership)))throw new ArgumentException("Invalid catalog cache ownership marker");
        }
        owner=new(Path.Combine(root,".cache.lock"),FileMode.OpenOrCreate,FileAccess.ReadWrite,FileShare.None);
    }
    public static CatalogCache Open(string directory)=>new(directory);
    sealed record ReadResult(byte[]? Snapshot=null,byte[]? Envelope=null,bool Invalid=false,bool Unavailable=false);
    ReadResult Read(string name)
    {
        try
        {
            using var file=new FileStream(Path.Combine(root,name),FileMode.Open,FileAccess.Read,FileShare.Read);
            if(file.Length is <1 or >MaxEnvelope)return new(Invalid:true);
            byte[] bytes=new byte[(int)file.Length];file.ReadExactly(bytes);
            if(file.ReadByte()!=-1)return new(Invalid:true);
            using var doc=JsonDocument.Parse(bytes,new JsonDocumentOptions{MaxDepth=36});var r=doc.RootElement;
            if(r.GetProperty("version").GetInt32()!=1)return new(Invalid:true);
            string hash=r.GetProperty("sha256").GetString()??"";
            if(hash.Length!=64)return new(Invalid:true);
            byte[] snapshot=Encoding.UTF8.GetBytes(r.GetProperty("snapshot").GetRawText());
            if(snapshot.Length>MaxSnapshot||!CryptographicOperations.FixedTimeEquals(Convert.FromHexString(hash),SHA256.HashData(snapshot)))return new(Invalid:true);
            var validate=new ModelCatalog();validate.Load(snapshot);
            return new(snapshot,bytes);
        }
        catch(FileNotFoundException){return new();}
        catch(DirectoryNotFoundException){return new();}
        catch(Exception e) when(e is IOException or UnauthorizedAccessException){return new(Unavailable:true);}
        catch(Exception e) when(e is JsonException or ArgumentException or KeyNotFoundException or InvalidOperationException or FormatException or OverflowException){return new(Invalid:true);}
    }
    public bool TryLoad(ModelCatalog catalog)
    {
        lock(sync)
        {
            ObjectDisposedException.ThrowIf(disposed,this);
            var primary=Read("catalog.json");var backup=primary.Snapshot is null?Read("catalog.backup.json"):new ReadResult();
            var found=primary.Snapshot??backup.Snapshot;
            string source=primary.Snapshot is not null?"primary":backup.Snapshot is not null?"backup":primary.Unavailable||backup.Unavailable?"unavailable":primary.Invalid||backup.Invalid?"invalid":"none";
            if(found is not null)catalog.Load(found);
            Volatile.Write(ref status,Status with{LoadedFrom=source});return found is not null;
        }
    }
    static byte[] Envelope(byte[] snapshot)
    {
        if(snapshot.Length>MaxSnapshot)throw new ArgumentException("Catalog snapshot exceeds bounds");
        using var buffer=new MemoryStream();
        using(var writer=new Utf8JsonWriter(buffer))
        {
            writer.WriteStartObject();writer.WriteNumber("version",1);
            writer.WriteString("sha256",Convert.ToHexStringLower(SHA256.HashData(snapshot)));
            writer.WritePropertyName("snapshot");writer.WriteRawValue(snapshot);writer.WriteEndObject();
        }
        return buffer.ToArray();
    }
    void Atomic(string name,byte[] bytes)
    {
        string target=Path.Combine(root,name),temporary=target+".tmp";
        try
        {
            // Fixed temporary names bound crash debris; never accumulate orphan files.
            if(File.Exists(temporary))File.Delete(temporary);
            using(var file=new FileStream(temporary,FileMode.CreateNew,FileAccess.Write,FileShare.None,4096,FileOptions.WriteThrough))
            {file.Write(bytes);file.Flush(true);}
            File.Move(temporary,target,true);
        }
        finally
        {
            try{if(File.Exists(temporary))File.Delete(temporary);}
            catch(Exception e) when(e is IOException or UnauthorizedAccessException){ }
        }
    }
    public bool TrySave(ModelCatalog catalog)
    {
        lock(sync)
        {
            ObjectDisposedException.ThrowIf(disposed,this);
            try
            {
                byte[] next=Envelope(catalog.Export());var current=Read("catalog.json");
                // A corrupt primary must never overwrite the last valid backup.
                if(current.Envelope is not null)Atomic("catalog.backup.json",current.Envelope);
                else if(Read("catalog.backup.json").Snapshot is null)Atomic("catalog.backup.json",next);
                Atomic("catalog.json",next);
                Volatile.Write(ref status,Status with{WriteFailed=false,Saves=Status.Saves+1});return true;
            }
            catch(Exception e) when(e is IOException or UnauthorizedAccessException or ArgumentException or JsonException)
            {Volatile.Write(ref status,Status with{WriteFailed=true});return false;}
        }
    }
    public void Dispose()
    {
        lock(sync){if(disposed)return;disposed=true;owner.Dispose();}
    }
}
