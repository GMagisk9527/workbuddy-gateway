using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;

namespace HttpMigration;

public sealed record RuntimeCacheStatus(string LoadedFrom,bool WriteFailed,long Saves,long SavedVersion,int RestoredAccounts);

// An owned, expendable scheduling checkpoint. The credit WAL remains its own owner.
public sealed class RuntimeSnapshotCache:IDisposable
{
    const string Marker=".csharp-model-runtime",Ownership="model-runtime-v1\n";
    const int EnvelopeLimit=AccountPool.RuntimeSnapshotLimit+2048;
    readonly string root;readonly FileStream owner;readonly object sync=new();bool disposed;
    AccountPool? savedPool;CredentialManager? savedCredentials;
    RuntimeCacheStatus status=new("none",false,0,-1,0);public RuntimeCacheStatus Status=>Volatile.Read(ref status);
    RuntimeSnapshotCache(string directory)
    {
        root=Path.GetFullPath(directory);Directory.CreateDirectory(root);NoLink(root);string marker=Path.Combine(root,Marker);NoLink(marker);
        if(!File.Exists(marker))
        {
            if(Directory.EnumerateFileSystemEntries(root).Any())throw new ArgumentException("Runtime cache directory is not owned");
            using var file=new FileStream(marker,FileMode.CreateNew,FileAccess.Write,FileShare.None);file.Write(Encoding.ASCII.GetBytes(Ownership));file.Flush(true);
        }
        using(var file=new FileStream(marker,FileMode.Open,FileAccess.Read,FileShare.Read))
        {
            if(file.Length!=Ownership.Length)throw new ArgumentException("Invalid runtime cache marker");byte[] bytes=new byte[Ownership.Length];file.ReadExactly(bytes);if(!bytes.AsSpan().SequenceEqual(Encoding.ASCII.GetBytes(Ownership)))throw new ArgumentException("Invalid runtime cache marker");
        }
        NoLink(Path.Combine(root,".runtime.lock"));owner=new(Path.Combine(root,".runtime.lock"),FileMode.OpenOrCreate,FileAccess.ReadWrite,FileShare.None);
    }
    public static RuntimeSnapshotCache Open(string directory)=>new(directory);
    static void NoLink(string path)
    {try{if((File.GetAttributes(path)&FileAttributes.ReparsePoint)!=0)throw new ArgumentException("Runtime cache links are not supported");}catch(FileNotFoundException){}catch(DirectoryNotFoundException){}}
    sealed record Data(byte[] Envelope,byte[] Snapshot);
    static bool Recoverable(Exception e)=>e is IOException or UnauthorizedAccessException or ArgumentException or JsonException or KeyNotFoundException or InvalidOperationException or FormatException or OverflowException;
    Data Read(AccountPool pool,string name)
    {
        string path=Path.Combine(root,name);NoLink(root);NoLink(path);
        using var file=new FileStream(path,FileMode.Open,FileAccess.Read,FileShare.Read);if(file.Length is <1 or >EnvelopeLimit)throw new ArgumentException("Runtime cache bounds");
        byte[] bytes=new byte[(int)file.Length];file.ReadExactly(bytes);if(file.ReadByte()!=-1)throw new ArgumentException("Runtime cache bounds");
        using var doc=JsonDocument.Parse(bytes,new JsonDocumentOptions{MaxDepth=14});var r=doc.RootElement;
        if(r.ValueKind!=JsonValueKind.Object||r.EnumerateObject().Count()!=3||r.EnumerateObject().Select(p=>p.Name).Distinct(StringComparer.Ordinal).Count()!=3||r.GetProperty("version").GetInt32()!=1)throw new ArgumentException("Invalid runtime envelope");
        string hash=r.GetProperty("sha256").GetString()??"";byte[] snapshot=Encoding.UTF8.GetBytes(r.GetProperty("snapshot").GetRawText());
        if(hash.Length!=64||!CryptographicOperations.FixedTimeEquals(Convert.FromHexString(hash),SHA256.HashData(snapshot)))throw new ArgumentException("Invalid runtime digest");
        pool.ValidateRuntimeSnapshot(snapshot);return new(bytes,snapshot);
    }
    public bool TryLoad(AccountPool pool,CredentialManager credentials)
    {
        lock(sync)
        {
            ObjectDisposedException.ThrowIf(disposed,this);bool invalid=false;
            foreach(string name in new[]{"runtime.json","runtime.backup.json"})
            {
                try{var data=Read(pool,name);int count=pool.ImportRuntime(data.Snapshot,credentials.RuntimeIdentities);Volatile.Write(ref status,Status with{LoadedFrom=name=="runtime.json"?"primary":"backup",RestoredAccounts=count});return true;}
                catch(FileNotFoundException){}catch(DirectoryNotFoundException){}catch(Exception e) when(Recoverable(e)){invalid=true;}
            }
            Volatile.Write(ref status,Status with{LoadedFrom=invalid?"invalid":"none",RestoredAccounts=0});return false;
        }
    }
    void Atomic(string name,byte[] bytes)
    {
        string target=Path.Combine(root,name),temp=target+".tmp";NoLink(root);NoLink(target);NoLink(temp);
        try
        {
            if(File.Exists(temp))File.Delete(temp);
            using(var file=new FileStream(temp,FileMode.CreateNew,FileAccess.Write,FileShare.None,4096,FileOptions.WriteThrough)){file.Write(bytes);file.Flush(true);}File.Move(temp,target,true);
        }
        finally{try{NoLink(temp);if(File.Exists(temp))File.Delete(temp);}catch(Exception e) when(Recoverable(e)){}}
    }
    public bool TrySave(AccountPool pool,CredentialManager credentials)
    {
        lock(sync)
        {
            ObjectDisposedException.ThrowIf(disposed,this);
            if(ReferenceEquals(savedPool,pool)&&ReferenceEquals(savedCredentials,credentials)&&!Status.WriteFailed&&Status.SavedVersion==pool.RuntimeVersion)return true;
            try
            {
                var export=pool.ExportRuntime(credentials.RuntimeIdentities);pool.ValidateRuntimeSnapshot(export.Snapshot);
                using var buffer=new MemoryStream();using(var w=new Utf8JsonWriter(buffer)){w.WriteStartObject();w.WriteNumber("version",1);w.WriteString("sha256",Convert.ToHexStringLower(SHA256.HashData(export.Snapshot)));w.WritePropertyName("snapshot");w.WriteRawValue(export.Snapshot);w.WriteEndObject();}
                byte[] next=buffer.ToArray();if(next.Length>EnvelopeLimit)throw new ArgumentException("Runtime cache bounds");Data? old=null,backup=null;
                try{old=Read(pool,"runtime.json");}catch(Exception e) when(Recoverable(e)){}
                if(old is null)try{backup=Read(pool,"runtime.backup.json");}catch(Exception e) when(Recoverable(e)){}
                if(old is not null)Atomic("runtime.backup.json",old.Envelope);else if(backup is null)Atomic("runtime.backup.json",next);
                Atomic("runtime.json",next);savedPool=pool;savedCredentials=credentials;Volatile.Write(ref status,Status with{WriteFailed=false,Saves=Status.Saves+1,SavedVersion=export.Version});return true;
            }
            catch(Exception e) when(Recoverable(e)){Volatile.Write(ref status,Status with{WriteFailed=true});return false;}
        }
    }
    public void Dispose(){lock(sync){if(disposed)return;disposed=true;owner.Dispose();}}
}
