using System.Text.Json;
using AccountLogin;
using AccountMaintenance;
using AccountScheduling;

namespace HttpMigration;

public sealed partial class StandaloneAccounts
{
    public string SuggestedLoginFile(IEnumerable<string>? existingNames=null)
    {
        var used=Credentials.Status.Select(s=>s.Id.Value).Concat(existingNames??[]).ToHashSet(StringComparer.OrdinalIgnoreCase);
        for(int i=1;i<=10000;i++){string name=$"workbuddy{i}.json";if(!used.Contains(name))return name;}
        throw new LoginFailure("login_capacity_full");
    }
    public ILoginDestination LoginDestination(string label,IEnumerable<string>? existingNames=null,string? existingId=null)
    {
        if(!CanReload||!LoginRules.ValidFile(label)||label.Trim().Length>255)throw new LoginFailure("invalid_login_target");
        label=label.Trim();
        existingId??=Credentials.Status.FirstOrDefault(s=>s.Id.Value.Equals(label,StringComparison.OrdinalIgnoreCase))?.Id.Value;
        if(existingId is null&&(existingNames??[]).Contains(label,StringComparer.OrdinalIgnoreCase))throw new LoginFailure("login_destination_exists");
        return new Destination(this,label,existingId,existingId is null?null:Credentials.ReplacementStamp(new(existingId)));
    }
    sealed class Destination(StandaloneAccounts owner,string label,string? existingId,string? stamp):ILoginDestination
    {
        public string Label=>label;
        public string Key=>owner.manifest+"|"+(existingId??label);
        public void Commit(AccountCredential credential)=>owner.AddLoginAsync(label,credential,existingId,stamp).GetAwaiter().GetResult();
    }
    async Task AddLoginAsync(string label,AccountCredential credential,string? existingId,string? stamp)
    {
        using var deadline=new CancellationTokenSource(TimeSpan.FromSeconds(30));
        await changes.WaitAsync(deadline.Token);
        string? stagedDirectory=null;bool committed=false;
        try
        {
            var current=ReadManifest(manifest!);
            if(existingId is not null)
            {
                var entry=EditableEntry(existingId);var document=await Credentials.ReadDocumentAsync(entry.Id,deadline.Token);
                byte[] raw=CredentialCodec.Export(credential);
                try{await Credentials.ReplaceDocumentAsync(entry.Id,raw,document.Revision,ControlFor(credential.Identity.Site),stamp,deadline.Token);}
                finally{System.Security.Cryptography.CryptographicOperations.ZeroMemory(raw);}
                observedFiles=null;NotifyChanged();return;
            }
            if(current.Count>=1024||current.Any(e=>e.Id.Value.Equals(label,StringComparison.OrdinalIgnoreCase)))throw new LoginFailure("login_destination_exists");
            if(Credentials.ContainsIdentity(credential))throw new LoginFailure("login_identity_already_loaded");
            string root=Path.GetDirectoryName(manifest!)!;
            string directory=Path.Combine(root,"login-"+Guid.NewGuid().ToString("N"));
            stagedDirectory=directory;
            using(var created=CredentialFiles.Create(directory,credential)){}
            var next=current.Append(new Entry(new(label),directory,false)).ToArray();
            var bytes=EncodeManifest(root, next);
            var before=ConsoleSettings.Read(manifest!,1<<20);
            await Credentials.ReconcileAsync(Registrations(next),()=>
            {
                if(!ConsoleSettings.Read(manifest!,1<<20).AsSpan().SequenceEqual(before))throw new ConsoleConflict();
                ConsoleSettings.Atomic(manifest!,bytes);
                committed=true;
            },deadline.Token);
            entries=next;observedFiles=null;NotifyChanged();
        }
        finally
        {
            if(!committed&&stagedDirectory is{} abandoned)
            {
                if(stores.Remove(abandoned,out var store)){owned.Remove(store);store.Dispose();}
                // Only files created in this random, uncommitted directory; never
                // delete an existing account or recurse into unexpected content.
                try
                {
                    RejectLink(abandoned);
                    foreach(string file in new[]{"credential.json","state.json",".pending",".credential.lock",".csharp-credential-preview"})
                    {string path=Path.Combine(abandoned,file);RejectLink(path);File.Delete(path);}
                    if(Directory.Exists(abandoned)&&!Directory.EnumerateFileSystemEntries(abandoned).Any())Directory.Delete(abandoned);
                }
                catch(Exception e)when(e is IOException or UnauthorizedAccessException or ArgumentException){}
            }
            changes.Release();
        }
    }
}

public sealed class ConsoleLoginService: IHostedService,IAsyncDisposable
{
    readonly LoginCoordinator coordinator;
    readonly StandaloneAccounts owner;
    readonly IReadOnlyDictionary<string,string> labels;
    public ConsoleLoginService(StandaloneAccounts owner,IReadOnlyDictionary<string,string> labels,Func<AccountSite,ILoginFlow>? factory=null,LoginOptions? options=null)
    {this.owner=owner;this.labels=labels;coordinator=new(factory??(site=>LoopbackLoginFlow.ForWorkBuddy(site)),options);}
    public string SuggestedFile=>owner.SuggestedLoginFile(labels.Values);
    public LoginSnapshot? Find(string id)=>coordinator.Find(id);
    public bool Cancel(string id)=>coordinator.Cancel(id);
    public async Task<LoginSnapshot> Start(string file,AccountSite site,CancellationToken token)
    {
        var matches=labels.Where(p=>p.Value.Equals(file.Trim(),StringComparison.OrdinalIgnoreCase)&&owner.Credentials.Status.Any(s=>s.Id.Value==p.Key)).Select(p=>p.Key).ToArray();
        if(matches.Length>1)throw new LoginFailure("login_destination_ambiguous");
        return (await coordinator.StartAsync(site,owner.LoginDestination(file,labels.Values,matches.SingleOrDefault()),token)).Snapshot;
    }
    public Task StartAsync(CancellationToken token)=>Task.CompletedTask;
    public Task StopAsync(CancellationToken token)=>coordinator.DisposeAsync().AsTask();
    public ValueTask DisposeAsync()=>coordinator.DisposeAsync();
}
