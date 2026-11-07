using System.Security.Cryptography;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;

namespace HttpMigration;

public sealed partial class StandaloneAccounts
{
    string? observedFiles;
    public bool CanReload=>manifest is not null;
    public event Action? Changed;
    string Fingerprint(IReadOnlyList<Entry> list)
    {
        using var hash=IncrementalHash.CreateHash(HashAlgorithmName.SHA256);long total=0;
        void Add(string path)
        {var data=ConsoleSettings.Read(path,1<<20);total+=data.Length;if(total>16<<20)throw new InvalidDataException("Account set budget");hash.AppendData(data);CryptographicOperations.ZeroMemory(data);}
        Add(manifest!);
        foreach(var entry in list)
        {RejectLink(entry.Directory);if(File.Exists(Path.Combine(entry.Directory,".pending")))throw new CredentialFailure("credential_store_incomplete");Add(Path.Combine(entry.Directory,"credential.json"));Add(Path.Combine(entry.Directory,"state.json"));}
        return Convert.ToHexStringLower(hash.GetHashAndReset());
    }
    List<CredentialRegistration> Registrations(IReadOnlyList<Entry> next)
    {
        var result=new List<CredentialRegistration>();
        foreach(var entry in next)
        {
            if(entries.Any(e=>e.Id==entry.Id&&e.Directory!=entry.Directory))throw new ArgumentException("Account id cannot change directory");
            if(!stores.TryGetValue(entry.Directory,out var store))
            {
                if(stores.Count>=4096)throw new ArgumentException("Account ownership limit; restart after draining retired accounts");
                store=CredentialFiles.Open(entry.Directory);stores.Add(entry.Directory,store);owned.Add(store);
            }
            var saved=store.Load();var site=saved.Credential.Identity.Site;
            if(!controls.TryGetValue(site,out var control))
            {control=factory is null?new WorkBuddyControlPlane(site):factory(site);controls.Add(site,control);if(control is IDisposable disposable&&!owned.Contains(disposable))owned.Add(disposable);}
            result.Add(new(entry.Id,SiteEndpoints.Model(site),store,control,entry.Disabled));
        }
        return result;
    }
    public async Task<bool> ReloadAsync(CancellationToken cancellation=default)
    {
        if(manifest is null)return false;
        await changes.WaitAsync(cancellation);
        try
        {
            var next=ReadManifest(manifest);var stamp=Fingerprint(next);if(stamp==observedFiles)return false;
            var registrations=Registrations(next);
            bool changed=await Credentials.ReconcileAsync(registrations,cancellation:cancellation);
            entries=next;observedFiles=stamp;if(changed)NotifyChanged();return changed;
        }
        finally{changes.Release();}
    }
    public async Task DeleteAsync(string id,CancellationToken cancellation=default)
    {
        if(manifest is null)throw new ArgumentException("Account deletion requires a manifest");
        await changes.WaitAsync(cancellation);
        try
        {
            var current=ReadManifest(manifest);
            var entry=current.SingleOrDefault(e=>e.Id.Value==id)??throw new KeyNotFoundException();
            var remaining=current.Where(e=>e.Id!=entry.Id).ToArray();
            string root=Path.GetDirectoryName(manifest)!;
            var bytes=EncodeManifest(root, remaining);
            var before=ConsoleSettings.Read(manifest,1<<20);
            await Credentials.ReconcileAsync(Registrations(remaining),()=>
            {
                if(!ConsoleSettings.Read(manifest,1<<20).AsSpan().SequenceEqual(before))throw new ConsoleConflict();
                foreach(var name in new[]{"credential.json","state.json"})ConsoleSettings.CheckLink(Path.Combine(entry.Directory,name));
                ConsoleSettings.Atomic(manifest,bytes);
            },cancellation);
            entries=remaining;observedFiles=null;NotifyChanged();
            // The manifest commit is authoritative. Even if unlink fails, the
            // account remains removed across restart, without restoring old data.
            foreach(var name in new[]{"credential.json","state.json",".pending"})
            {string path=Path.Combine(entry.Directory,name);ConsoleSettings.CheckLink(path);File.Delete(path);}
        }
        finally{changes.Release();}
    }
}

public sealed class CredentialReloadWorker(StandaloneAccounts accounts,TimeSpan interval):BackgroundService
{
    public ScheduledTaskState TaskState{get;}=new();
    public TimeSpan Interval{get;}=interval>=TimeSpan.FromSeconds(1)&&interval<=TimeSpan.FromHours(1)?interval:throw new ArgumentException("Invalid account reload interval");
    public async Task RunOnceAsync(CancellationToken token=default)
    {
        string trace=TaskState.Begin();
        try
        {
            using var deadline=CancellationTokenSource.CreateLinkedTokenSource(token);deadline.CancelAfter(TimeSpan.FromSeconds(1));
            bool changed=await accounts.ReloadAsync(deadline.Token);TaskState.Finish(trace,1,1,0,Interval,outcome:changed?"success":"skipped");
        }
        catch(OperationCanceledException)when(!token.IsCancellationRequested){TaskState.Finish(trace,1,0,0,Interval,outcome:"skipped");}
        catch(OperationCanceledException){TaskState.Finish(trace,1,0,0,Interval,outcome:"cancelled");throw;}
        catch(Exception e)when(e is IOException or UnauthorizedAccessException or JsonException or CredentialFailure or ArgumentException or InvalidOperationException or KeyNotFoundException or OverflowException)
        {TaskState.Finish(trace,1,0,1,Interval);}
    }
    protected override async Task ExecuteAsync(CancellationToken token)
    {while(!token.IsCancellationRequested){await RunOnceAsync(token);await Task.Delay(Interval,token);}}
}
