using System.Security.Cryptography;
using System.Text.Json;
using AccountMaintenance;
namespace HttpMigration;
public sealed record ResourceCacheStatus(string LoadedFrom,bool WriteFailed,long Saves);
public sealed class ResourceSnapshotCache:IDisposable
{
    const int Limit=(4<<20)+2048;const string Marker=".csharp-resource-cache";
    readonly string root;readonly FileStream owner;readonly object sync=new();bool disposed;
    ResourceCacheStatus status=new("none",false,0);public ResourceCacheStatus Status=>Volatile.Read(ref status);
    ResourceSnapshotCache(string directory)
    {
        root=Path.GetFullPath(directory);Directory.CreateDirectory(root);string marker=Path.Combine(root,Marker);
        if(!File.Exists(marker)){if(Directory.EnumerateFileSystemEntries(root).Any())throw new ArgumentException("Resource cache directory is not owned");using var file=new FileStream(marker,FileMode.CreateNew,FileAccess.Write,FileShare.None);file.Write("resource-cache-v1"u8);file.Flush(true);}
        using(var file=new FileStream(marker,FileMode.Open,FileAccess.Read,FileShare.Read)){if(file.Length!=17)throw new ArgumentException("Invalid resource cache marker");byte[] data=new byte[17];file.ReadExactly(data);if(!data.AsSpan().SequenceEqual("resource-cache-v1"u8))throw new ArgumentException("Invalid resource cache marker");}
        owner=new(Path.Combine(root,".resources.lock"),FileMode.OpenOrCreate,FileAccess.ReadWrite,FileShare.None);
    }
    public static ResourceSnapshotCache Open(string directory)=>new(directory);
    sealed record Data(byte[] Envelope,byte[] Snapshot);
    Data? Read(CredentialManager manager,string name)
    {
        using var file=new FileStream(Path.Combine(root,name),FileMode.Open,FileAccess.Read,FileShare.Read);if(file.Length is <1 or >Limit)throw new ArgumentException("Resource cache bounds");
        byte[] bytes=new byte[(int)file.Length];file.ReadExactly(bytes);if(file.ReadByte()!=-1)throw new ArgumentException("Resource cache bounds");
        using var doc=JsonDocument.Parse(bytes,new JsonDocumentOptions{MaxDepth=12});var envelope=doc.RootElement;
        if(envelope.GetProperty("version").GetInt32()!=1)throw new ArgumentException("Invalid resource cache version");
        byte[] snapshot=System.Text.Encoding.UTF8.GetBytes(envelope.GetProperty("snapshot").GetRawText());string hash=envelope.GetProperty("sha256").GetString()??"";
        if(hash.Length!=64||!CryptographicOperations.FixedTimeEquals(Convert.FromHexString(hash),SHA256.HashData(snapshot)))throw new ArgumentException("Invalid resource cache digest");
        manager.ValidateResourceSnapshot(snapshot);return new(bytes,snapshot);
    }
    static bool Recoverable(Exception e)=>e is IOException or UnauthorizedAccessException or ArgumentException or JsonException or KeyNotFoundException or InvalidOperationException or FormatException or OverflowException;
    public bool TryLoad(CredentialManager manager)
    {
        lock(sync)
        {
            ObjectDisposedException.ThrowIf(disposed,this);bool invalid=false;
            foreach(string name in new[]{"resources.json","resources.backup.json"})
            {
                try{var data=Read(manager,name)!;manager.ImportResources(data.Snapshot);Volatile.Write(ref status,Status with{LoadedFrom=name=="resources.json"?"primary":"backup"});return true;}
                catch(FileNotFoundException){ }catch(DirectoryNotFoundException){ }
                catch(Exception e) when(Recoverable(e)){invalid=true;}
            }
            Volatile.Write(ref status,Status with{LoadedFrom=invalid?"invalid":"none"});return false;
        }
    }
    void Atomic(string name,byte[] bytes)
    {
        string target=Path.Combine(root,name),temp=target+".tmp";
        try
        {
            if(File.Exists(temp))File.Delete(temp);
            using(var file=new FileStream(temp,FileMode.CreateNew,FileAccess.Write,FileShare.None,4096,FileOptions.WriteThrough)){file.Write(bytes);file.Flush(true);}File.Move(temp,target,true);
        }
        finally{try{if(File.Exists(temp))File.Delete(temp);}catch(Exception e) when(e is IOException or UnauthorizedAccessException){ }}
    }
    public bool TrySave(CredentialManager manager)
    {
        lock(sync)
        {
            ObjectDisposedException.ThrowIf(disposed,this);
            try
            {
                byte[] snapshot=manager.ExportResources();manager.ValidateResourceSnapshot(snapshot);using var buffer=new MemoryStream();
                using(var writer=new Utf8JsonWriter(buffer)){writer.WriteStartObject();writer.WriteNumber("version",1);writer.WriteString("sha256",Convert.ToHexStringLower(SHA256.HashData(snapshot)));writer.WritePropertyName("snapshot");writer.WriteRawValue(snapshot);writer.WriteEndObject();}
                byte[] next=buffer.ToArray();if(next.Length>Limit)throw new ArgumentException("Resource cache bounds");Data? old=null,backup=null;
                try{old=Read(manager,"resources.json");}catch(Exception e) when(Recoverable(e)){ }
                if(old is null)try{backup=Read(manager,"resources.backup.json");}catch(Exception e) when(Recoverable(e)){ }
                if(old is not null)Atomic("resources.backup.json",old.Envelope);else if(backup is null)Atomic("resources.backup.json",next);
                Atomic("resources.json",next);Volatile.Write(ref status,Status with{WriteFailed=false,Saves=Status.Saves+1});return true;
            }
            catch(Exception e) when(Recoverable(e)){Volatile.Write(ref status,Status with{WriteFailed=true});return false;}
        }
    }
    public void Dispose(){lock(sync){if(disposed)return;disposed=true;owner.Dispose();}}
}
