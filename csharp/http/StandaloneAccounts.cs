using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;

namespace HttpMigration;

// Startup ownership for independent accounts. Dispose only after the host drains.
public sealed partial class StandaloneAccounts : IDisposable
{
    sealed record Entry(AccountId Id,string Directory,bool Disabled);
    readonly List<IDisposable> owned;
    readonly Dictionary<string,CredentialFiles> stores=new(OperatingSystem.IsWindows()?StringComparer.OrdinalIgnoreCase:StringComparer.Ordinal);
    readonly Dictionary<AccountSite,IAccountControlPlane> controls=new();
    readonly SemaphoreSlim changes=new(1,1);
    IReadOnlyList<Entry> entries=[];
    string? manifest;
    Func<AccountSite,IAccountControlPlane>? factory;
    int disposed;
    public AccountPool Pool { get; }
    public CredentialManager Credentials { get; }
    public Uri Upstream { get; }
    StandaloneAccounts(List<IDisposable> owned,AccountPool pool,CredentialManager credentials,Uri upstream)
    {this.owned=owned;Pool=pool;Credentials=credentials;Upstream=upstream;}
    public static StandaloneAccounts OpenSingle(string directory)=>Open([new(new("standalone-account"),Path.GetFullPath(directory),false)],null);
    public static StandaloneAccounts OpenManifest(string manifest,Func<AccountSite,IAccountControlPlane>? controlFactory=null)
    {
        var runtime=Open(ReadManifest(manifest),controlFactory);runtime.manifest=Path.GetFullPath(manifest);return runtime;
    }
    static IReadOnlyList<Entry> ReadManifest(string manifest)
    {
        string full=Path.GetFullPath(manifest),root=Path.GetDirectoryName(full)!;
        RejectLink(full);RejectLink(root);
        using var file=new FileStream(full,FileMode.Open,FileAccess.Read,FileShare.Read);
        if(file.Length is <1 or >1048576)throw new ArgumentException("Account manifest exceeds bounds");
        byte[] bytes=new byte[(int)file.Length];file.ReadExactly(bytes);
        using var doc=JsonDocument.Parse(bytes,new JsonDocumentOptions{MaxDepth=8});var r=doc.RootElement;
        if(r.ValueKind!=JsonValueKind.Object||r.EnumerateObject().Any(p=>p.Name is not ("version" or "accounts"))||r.EnumerateObject().Select(p=>p.Name).Distinct().Count()!=r.EnumerateObject().Count()||r.GetProperty("version").GetInt32()!=1)throw new ArgumentException("Invalid account manifest");
        var accounts=r.GetProperty("accounts");
        if(accounts.ValueKind!=JsonValueKind.Array||accounts.GetArrayLength()>1024)throw new ArgumentException("Account manifest count exceeds bounds");
        var entries=new List<Entry>();var ids=new HashSet<AccountId>();var paths=new HashSet<string>(OperatingSystem.IsWindows()?StringComparer.OrdinalIgnoreCase:StringComparer.Ordinal);
        foreach(var account in accounts.EnumerateArray())
        {
            if(account.ValueKind!=JsonValueKind.Object||account.EnumerateObject().Any(p=>p.Name is not ("id" or "directory" or "disabled"))||account.EnumerateObject().Select(p=>p.Name).Distinct().Count()!=account.EnumerateObject().Count())throw new ArgumentException("Invalid account manifest entry");
            var id=new AccountId(account.GetProperty("id").GetString()!);string relative=account.GetProperty("directory").GetString()??"";
            string[] parts=relative.Replace('\\','/').Split('/');
            if(Path.IsPathRooted(relative)||parts.Any(p=>p is "" or "." or ".." ||p.Contains(':')))throw new ArgumentException("Account directory must be a relative descendant");
            string directory=root;foreach(string part in parts){directory=Path.Combine(directory,part);RejectLink(directory);}
            directory=Path.GetFullPath(directory);
            if(!ids.Add(id)||!paths.Add(directory))throw new ArgumentException("Duplicate account id or directory");
            bool disabled=false;
            if(account.TryGetProperty("disabled",out var flag)){if(flag.ValueKind is not (JsonValueKind.True or JsonValueKind.False))throw new ArgumentException("Invalid account disabled flag");disabled=flag.GetBoolean();}
            foreach(string name in new[]{".csharp-credential-preview","credential.json","state.json",".credential.lock"})RejectLink(Path.Combine(directory,name));
            entries.Add(new(id,directory,disabled));
        }
        return entries;
    }
    static void RejectLink(string path)
    {
        // Missing entries are reported by normal store validation later.
        if((File.Exists(path)||Directory.Exists(path))&&(File.GetAttributes(path)&FileAttributes.ReparsePoint)!=0)throw new ArgumentException("Account paths cannot contain symbolic links");
    }
    static StandaloneAccounts Open(IReadOnlyList<Entry> entries,Func<AccountSite,IAccountControlPlane>? factory)
    {
        var owned=new List<IDisposable>();
        try
        {
            long total=0;var loaded=new List<(Entry Entry,CredentialFiles Files,PersistedCredential Saved)>();var identities=new HashSet<(AccountSite,string,string)>();
            foreach(var entry in entries)
            {
                total=checked(total+new FileInfo(Path.Combine(entry.Directory,"credential.json")).Length+new FileInfo(Path.Combine(entry.Directory,"state.json")).Length);
                if(total>16<<20)throw new ArgumentException("Account credential set exceeds bounds");
                var files=CredentialFiles.Open(entry.Directory);owned.Add(files);var saved=files.Load();var identity=saved.Credential.Identity;
                if(identity.Uid.Length==0||!identities.Add((identity.Site,identity.Uid,identity.EnterpriseId)))throw new ArgumentException("Missing or duplicate account identity");
                loaded.Add((entry,files,saved));
            }
            var controls=new Dictionary<AccountSite,IAccountControlPlane>();
            foreach(var site in loaded.Select(a=>a.Saved.Credential.Identity.Site).Distinct())
            {
                var control=factory is null?new WorkBuddyControlPlane(site):factory(site);
                if(control is null)throw new ArgumentException("Account control plane is required");
                controls.Add(site,control);if(control is IDisposable disposable&&!owned.Any(o=>ReferenceEquals(o,disposable)))owned.Add(disposable);
            }
            var snapshots=loaded.Select(a=>new AccountSnapshot(a.Entry.Id,SiteEndpoints.Model(a.Saved.Credential.Identity.Site),a.Saved.Credential.Identity.Site,Disabled:a.Entry.Disabled)).ToArray();
            var pool=new AccountPool(snapshots);
            var manager=new CredentialManager(pool,loaded.Select(a=>new CredentialRegistration(a.Entry.Id,SiteEndpoints.Model(a.Saved.Credential.Identity.Site),a.Files,controls[a.Saved.Credential.Identity.Site],a.Entry.Disabled)));
            var runtime=new StandaloneAccounts(owned,pool,manager,snapshots.FirstOrDefault()?.Upstream??SiteEndpoints.Model(AccountSite.Cn)){entries=entries,factory=factory};
            foreach(var item in loaded)runtime.stores.Add(item.Entry.Directory,item.Files);
            foreach(var item in controls)runtime.controls.Add(item.Key,item.Value);
            return runtime;
        }
        catch{Release(owned);throw;}
    }
    static void Release(List<IDisposable> resources)
    {for(int i=resources.Count-1;i>=0;i--){try{resources[i].Dispose();}catch{ /* Continue releasing the other account locks. */ }}}
    public void Dispose(){if(Interlocked.Exchange(ref disposed,1)==0)Release(owned);}
}
