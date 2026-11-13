namespace LedgerMigration;

public interface ILedgerFiles : IDisposable
{
    byte[]? Read(string name);
    void Replace(string name, ReadOnlySpan<byte> bytes);
    int Append(ReadOnlySpan<byte> bytes);
    void Sync();
}

public sealed class LedgerFiles : ILedgerFiles
{
    const long Limit = 64L << 20;
    readonly string root;
    readonly FileStream owner;
    FileStream? wal;
    public LedgerFiles(string directory)
    {
        root = Path.GetFullPath(directory); Directory.CreateDirectory(root);
        owner = new FileStream(Path.Combine(root, ".ledger.lock"), FileMode.OpenOrCreate, FileAccess.ReadWrite, FileShare.None);
    }
    string PathOf(string name) => name is CreditLedger.WalName or CreditLedger.StateName or CreditLedger.SnapshotName
        ? Path.Combine(root, name) : throw new ArgumentException("Unknown ledger file");
    public byte[]? Read(string name)
    {
        try
        {
            using var source = new FileStream(PathOf(name), FileMode.Open, FileAccess.Read, FileShare.ReadWrite);
            using var output = new MemoryStream(); byte[] buffer = new byte[8192];
            while (true) { int n = source.Read(buffer); if (n == 0) return output.ToArray(); if (n > Limit - output.Length) throw new InvalidDataException("Ledger file exceeds limit"); output.Write(buffer, 0, n); }
        }
        catch (FileNotFoundException) { return null; }
    }
    public void Replace(string name, ReadOnlySpan<byte> bytes)
    {
        if (bytes.Length > Limit) throw new InvalidDataException("Ledger file exceeds limit");
        if (name == CreditLedger.WalName) { wal?.Dispose(); wal = null; }
        string path = PathOf(name), temp = path + "." + Guid.NewGuid().ToString("N") + ".tmp";
        try
        {
            // Durability comes from Flush(true); WriteThrough would make the
            // same write synchronous twice (O_SYNC on Unix) for no extra safety.
            var options = new FileStreamOptions { Mode = FileMode.CreateNew, Access = FileAccess.Write, Share = FileShare.None };
            if (!OperatingSystem.IsWindows()) options.UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite;
            using (var file = new FileStream(temp, options)) { file.Write(bytes); file.Flush(true); }
            File.Move(temp, path, true);
        }
        finally { if (File.Exists(temp)) File.Delete(temp); }
    }
    public int Append(ReadOnlySpan<byte> bytes)
    {
        if (wal is null)
        {
            // Buffered write; Sync() below still fsyncs before any state is applied.
            var options = new FileStreamOptions { Mode = FileMode.OpenOrCreate, Access = FileAccess.Write, Share = FileShare.Read };
            if (!OperatingSystem.IsWindows()) options.UnixCreateMode = UnixFileMode.UserRead | UnixFileMode.UserWrite;
            wal = new FileStream(PathOf(CreditLedger.WalName), options); wal.Seek(0, SeekOrigin.End);
        }
        if (bytes.Length > Limit - wal.Length) throw new IOException("WAL exceeds limit");
        wal.Write(bytes); return bytes.Length;
    }
    public void Sync() => wal?.Flush(true);
    public void Dispose() { wal?.Dispose(); owner.Dispose(); }
}
