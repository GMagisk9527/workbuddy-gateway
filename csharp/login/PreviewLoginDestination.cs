using AccountMaintenance;

namespace AccountLogin;

// Explicit per-account preview directory. No production scanning or silent adoption.
public sealed class PreviewLoginDestination : ILoginDestination
{
    readonly bool replace;
    public string Key { get; }
    public string Label { get; }
    public PreviewLoginDestination(string directory, string label = "workbuddy.json", bool replaceExisting = false)
    {
        if (!LoginRules.ValidFile(label)) throw new LoginFailure("invalid_credential_name");
        Key = Path.GetFullPath(directory); Label = label.Trim(); replace = replaceExisting;
        if (File.Exists(Key)) throw new LoginFailure("login_destination_exists");
        if (Directory.Exists(Key) && Directory.EnumerateFileSystemEntries(Key).Any() && !replace) throw new LoginFailure("login_destination_exists");
        if (replace && !File.Exists(Path.Combine(Key, ".csharp-credential-preview"))) throw new LoginFailure("login_destination_not_owned");
    }
    public void Commit(AccountCredential credential)
    {
        if (!replace) { using var created = CredentialFiles.Create(Key, credential); return; }
        using var store = CredentialFiles.Open(Key);
        if (!CredentialRules.SameIdentity(store.Load().Credential, credential)) throw new LoginFailure("login_identity_changed");
        store.Save(new(credential)); // Successful same-account re-login clears persisted credential disable.
    }
}
