namespace KeySecurity;

public interface IKeyFiles : IDisposable
{
    byte[]? Read(string name);
    void WriteAtomic(string name, ReadOnlySpan<byte> bytes);
    void Delete(string name);
}

// One owner for the directory. Temp files share the target filesystem and are
// flushed before replace. Directory fsync/power-loss behavior still needs Linux validation.
public sealed class KeyFiles : IKeyFiles
{
    const int MaxDocument = 8 << 20;
    readonly string root;
    readonly FileStream owner;
    public KeyFiles(string directory)
    {
        root = Path.GetFullPath(directory); Directory.CreateDirectory(root);
        owner = new FileStream(Path.Combine(root, ".keystore.lock"), FileMode.OpenOrCreate, FileAccess.ReadWrite, FileShare.None);
    }
    string PathOf(string name)
    {
        if (name is not ("apikeys.json" or "keyauth.json" or "keystore.pending")) throw new ArgumentException("Unknown key document");
        return Path.Combine(root, name);
    }
    public byte[]? Read(string name)
    {
        try
        {
            using var file = new FileStream(PathOf(name), FileMode.Open, FileAccess.Read, FileShare.Read);
            if (file.Length > MaxDocument) throw new InvalidDataException("Key document exceeds limit");
            using var buffer = new MemoryStream(); byte[] chunk = new byte[8192];
            while (true) { int n = file.Read(chunk); if (n == 0) return buffer.ToArray(); if (n > MaxDocument - buffer.Length) throw new InvalidDataException("Key document exceeds limit"); buffer.Write(chunk, 0, n); }
        }
        catch (FileNotFoundException) { return null; }
    }
    public void WriteAtomic(string name, ReadOnlySpan<byte> bytes)
    {
        if (bytes.Length > MaxDocument) throw new InvalidDataException("Key document exceeds limit");
        string path = PathOf(name), temporary = path + "." + Guid.NewGuid().ToString("N") + ".tmp";
        try
        {
            var options = new FileStreamOptions { Mode = FileMode.CreateNew, Access = FileAccess.Write, Share = FileShare.None, Options = FileOptions.WriteThrough };
            if (!OperatingSystem.IsWindows()) options.UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite;
            using (var file = new FileStream(temporary, options)) { file.Write(bytes); file.Flush(flushToDisk: true); }
            File.Move(temporary, path, overwrite: true);
        }
        finally { if (File.Exists(temporary)) File.Delete(temporary); }
    }
    public void Delete(string name) => File.Delete(PathOf(name));
    public void Dispose() => owner.Dispose();
}
