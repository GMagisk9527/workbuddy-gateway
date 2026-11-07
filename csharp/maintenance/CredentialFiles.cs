using System.Security.Cryptography;
using System.Text.Json;

namespace AccountMaintenance;

public sealed record PersistedCredential(AccountCredential Credential, bool Disabled = false);
public interface ICredentialPersistence : IDisposable
{
    PersistedCredential Load();
    void Save(PersistedCredential state);
}
public interface ICredentialDocumentPersistence:ICredentialPersistence
{
    byte[] ReadDocument();
    void SaveDocument(PersistedCredential state,ReadOnlySpan<byte> document);
}

// Explicit preview ownership, one writer, Go-compatible credential JSON plus preview-only state.
public sealed class CredentialFiles : ICredentialDocumentPersistence
{
    const string Marker = ".csharp-credential-preview";
    readonly string root;
    readonly FileStream owner;
    bool failed;
    CredentialFiles(string root)
    {
        this.root = root;
        if (!File.Exists(Path.Combine(root, Marker))) throw new CredentialFailure("credential_directory_not_owned");
        owner = new(Path.Combine(root, ".credential.lock"), FileMode.OpenOrCreate, FileAccess.ReadWrite, FileShare.None);
    }
    public static CredentialFiles Create(string directory, AccountCredential initial)
    {
        string root = Path.GetFullPath(directory); Directory.CreateDirectory(root);
        if (Directory.EnumerateFileSystemEntries(root).Any()) throw new CredentialFailure("credential_directory_not_empty");
        using (var marker = new FileStream(Path.Combine(root, Marker), FileMode.CreateNew, FileAccess.Write, FileShare.None)) marker.Flush(true);
        var files = new CredentialFiles(root);
        try { files.Save(new(initial)); return files; } catch { files.Dispose(); throw; }
    }
    public static CredentialFiles Open(string directory)
    {
        var files = new CredentialFiles(Path.GetFullPath(directory));
        try { _ = files.Load(); return files; } catch { files.Dispose(); throw; }
    }
    byte[] Read(string name)
    {
        CheckPath(name);
        using var file = new FileStream(Path.Combine(root, name), FileMode.Open, FileAccess.Read, FileShare.Read);
        if (file.Length > 1 << 20) throw new CredentialFailure("credential_document_too_large");
        using var buffer = new MemoryStream(); byte[] block = new byte[8192];
        while (true) { int n = file.Read(block); if (n == 0) return buffer.ToArray(); if (buffer.Length + n > 1 << 20) throw new CredentialFailure("credential_document_too_large"); buffer.Write(block, 0, n); }
    }
    void CheckPath(string name)
    {
        for(var directory=new DirectoryInfo(root);directory is not null;directory=directory.Parent)
            if((directory.Attributes&FileAttributes.ReparsePoint)!=0)throw new CredentialFailure("credential_path_link");
        string path=Path.Combine(root,name);
        if(File.Exists(path)&&(File.GetAttributes(path)&FileAttributes.ReparsePoint)!=0)throw new CredentialFailure("credential_path_link");
    }
    public byte[] ReadDocument(){_=Load();return Read("credential.json");}
    public PersistedCredential Load()
    {
        if (failed || File.Exists(Path.Combine(root, ".pending"))) throw new CredentialFailure("credential_store_incomplete");
        try
        {
            using var doc = JsonDocument.Parse(Read("state.json"));
            if (doc.RootElement.GetProperty("version").GetInt32() != 1) throw new CredentialFailure("invalid_credential_state");
            return new(CredentialCodec.Decode(Read("credential.json")), doc.RootElement.GetProperty("disabled").GetBoolean());
        }
        catch (JsonException) { throw new CredentialFailure("invalid_credential_state"); }
        catch (KeyNotFoundException) { throw new CredentialFailure("invalid_credential_state"); }
        catch (InvalidOperationException) { throw new CredentialFailure("invalid_credential_state"); }
    }
    void Atomic(string name, ReadOnlySpan<byte> data)
    {
        CheckPath(name);
        string temp = Path.Combine(root, ".write-" + Guid.NewGuid().ToString("N"));
        try
        {
            using (var file = new FileStream(temp, FileMode.CreateNew, FileAccess.Write, FileShare.None, 4096, FileOptions.WriteThrough)) { file.Write(data); file.Flush(true); }
            File.Move(temp, Path.Combine(root, name), true);
        }
        finally { if (File.Exists(temp)) File.Delete(temp); }
    }
    public void Save(PersistedCredential state)
    {
        byte[] encoded = CredentialCodec.Export(state.Credential);
        try{SaveDocument(state,encoded);}finally{CryptographicOperations.ZeroMemory(encoded);}
    }
    public void SaveDocument(PersistedCredential state,ReadOnlySpan<byte> document)
    {
        if (failed || File.Exists(Path.Combine(root, ".pending"))) throw new CredentialFailure("credential_store_incomplete");
        var decoded=CredentialCodec.Decode(document);
        byte[] actual=CredentialCodec.Export(decoded),expected=CredentialCodec.Export(state.Credential);
        try{if(!actual.AsSpan().SequenceEqual(expected))throw new CredentialFailure("credential_document_mismatch");}
        finally{CryptographicOperations.ZeroMemory(actual);CryptographicOperations.ZeroMemory(expected);}
        try
        {
            Atomic(".pending", "1"u8); Atomic("credential.json", document);
            Atomic("state.json", JsonSerializer.SerializeToUtf8Bytes(new { version = 1, disabled = state.Disabled }));
            File.Delete(Path.Combine(root, ".pending"));
        }
        catch { failed = true; throw new CredentialFailure("credential_persist_failed"); }
    }
    public void Dispose() => owner.Dispose();
}
