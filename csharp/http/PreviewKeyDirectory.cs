using KeySecurity;

namespace HttpMigration;

public static class PreviewKeyDirectory
{
    // The test host uses its own directory; importing an existing Go key table is
    // a separate migration operation and must not happen by launching this CLI.
    public static KeyStore Open(string directory,Credential? legacy=null)
    {
        string full = Path.GetFullPath(directory), marker = Path.Combine(full, ".csharp-preview");
        if (Directory.Exists(full) && !File.Exists(marker) && Directory.EnumerateFileSystemEntries(full).Any())
            throw new ArgumentException("Preview requires a new empty directory or a directory already owned by this preview");
        Directory.CreateDirectory(full);
        if (!File.Exists(marker)) File.WriteAllText(marker, "C# local key preview v1\n");
        return KeyStore.Open(full,legacy:legacy);
    }
}
