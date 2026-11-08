using System.Security.Cryptography;
using System.Text;
using AccountMaintenance;
using AccountScheduling;

namespace HttpMigration;

public sealed partial class StandaloneAccounts
{
    IAccountControlPlane ControlFor(AccountSite site)
    {
        if(controls.TryGetValue(site,out var control))return control;
        control=factory is null?new WorkBuddyControlPlane(site):factory(site);
        if(control is null)throw new ArgumentException("Account control plane is required");
        controls.Add(site,control);if(control is IDisposable disposable&&!owned.Contains(disposable))owned.Add(disposable);
        return control;
    }
    Entry EditableEntry(string id)
    {
        if(manifest is null)throw new CredentialFailure("credential_edit_unsupported");
        var current=ReadManifest(manifest);var entry=current.SingleOrDefault(e=>e.Id.Value==id)??throw new KeyNotFoundException();
        if(!entries.Any(e=>e==entry))throw new CredentialFailure("credential_revision_conflict");return entry;
    }
    public async Task<CredentialDocument> ReadCredentialAsync(string id,CancellationToken cancellation=default)
    {
        await changes.WaitAsync(cancellation);
        try{return await Credentials.ReadDocumentAsync(EditableEntry(id).Id,cancellation);}
        finally{changes.Release();}
    }
    public async Task<string> EditCredentialAsync(string id,string content,string revision,CancellationToken cancellation=default)
    {
        byte[] raw=Encoding.UTF8.GetBytes(content);
        try
        {
            var candidate=CredentialManager.ValidateEditedDocument(raw);
            await changes.WaitAsync(cancellation);
            try
            {
                var entry=EditableEntry(id);
                string updated=await Credentials.ReplaceDocumentAsync(entry.Id,raw,revision,ControlFor(candidate.Identity.Site),cancellation:cancellation);
                observedFiles=null;NotifyChanged();return updated;
            }
            finally{changes.Release();}
        }
        finally{CryptographicOperations.ZeroMemory(raw);}
    }
    void NotifyChanged()
    {
        // Subscribers are refresh hints; a subscriber racing host shutdown must
        // not turn an already-durable account commit into an apparent failure.
        var callbacks=Changed;if(callbacks is null)return;
        foreach(Action callback in callbacks.GetInvocationList())
            try{callback();}catch(ObjectDisposedException){}
    }
}
