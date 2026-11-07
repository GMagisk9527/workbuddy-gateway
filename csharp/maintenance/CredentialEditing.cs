using System.Collections.Frozen;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using AccountScheduling;

namespace AccountMaintenance;

public sealed record CredentialDocument(string Path,string Content,string Revision)
{public override string ToString()=>"CredentialDocument (redacted)";}

public sealed partial class CredentialManager
{
    static string DocumentRevision(ReadOnlySpan<byte> raw)=>Convert.ToHexStringLower(SHA256.HashData(raw));
    static ICredentialDocumentPersistence Editable(Cell cell)=>cell.Registration.Persistence as ICredentialDocumentPersistence??throw new CredentialFailure("credential_edit_unsupported");
    static string ReplacementStamp(Cell cell)
    {byte[] raw=CredentialCodec.Export(cell.Current.Credential);try{return cell.AffinityGeneration+":"+cell.BlockVersion+":"+DocumentRevision(raw);}finally{CryptographicOperations.ZeroMemory(raw);}}
    public string ReplacementStamp(AccountId id){var cell=Find(id);lock(cell.Barrier)return ReplacementStamp(cell);}
    public object[] CredentialViews=>cells.Select(pair=>
    {
        var c=Volatile.Read(ref pair.Value.Current).Credential;
        return (object)new{path=pair.Key.Value,exists=true,edition=c.Identity.Site==AccountSite.Cn?"cn":"intl",nickname=c.Identity.Nickname,uid=c.Identity.Uid,enterpriseId=c.Identity.EnterpriseId,domain=c.Domain,expiresAt=c.ExpiresAt,refreshExpiresAt=c.RefreshExpiresAt,lastRefreshTime=c.LastRefreshTime,accessTokenMasked=c.Access.Length==0?"":"****",refreshTokenMasked=c.Refresh.Length==0?"":"****"};
    }).ToArray();
    public async Task<CredentialDocument> ReadDocumentAsync(AccountId id,CancellationToken cancellation=default)
    {
        await registryGate.WaitAsync(cancellation);
        try
        {
            var cell=Find(id);using var permit=await cell.Gate.ReadAsync(cancellation);
            byte[] raw=Editable(cell).ReadDocument();
            try{if(raw.Length>256<<10)throw new CredentialFailure("credential_document_too_large");return new(id.Value,Encoding.UTF8.GetString(raw),DocumentRevision(raw));}
            finally{CryptographicOperations.ZeroMemory(raw);}
        }
        finally{registryGate.Release();}
    }
    public static AccountCredential ValidateEditedDocument(byte[] raw)
    {
        if(raw.Length is 0 or >262144)throw new CredentialFailure("credential_document_too_large");
        using var doc=JsonDocument.Parse(raw,new JsonDocumentOptions{MaxDepth=32});
        void Unique(JsonElement e)
        {
            if(e.ValueKind==JsonValueKind.Object){var seen=new HashSet<string>(StringComparer.OrdinalIgnoreCase);foreach(var p in e.EnumerateObject()){if(!seen.Add(p.Name))throw new CredentialFailure("invalid_credential_document");Unique(p.Value);}}
            else if(e.ValueKind==JsonValueKind.Array)foreach(var item in e.EnumerateArray())Unique(item);
        }
        Unique(doc.RootElement);
        if(doc.RootElement.ValueKind!=JsonValueKind.Object)throw new CredentialFailure("invalid_credential_document");
        foreach(var property in doc.RootElement.EnumerateObject())
            if(property.Name.Equals("edition",StringComparison.OrdinalIgnoreCase)&&property.Value.GetString() is not("cn" or "intl" or ""))throw new CredentialFailure("invalid_credential_site");
        var next=CredentialCodec.Decode(raw);
        if(string.IsNullOrWhiteSpace(next.Access)||string.IsNullOrWhiteSpace(next.Identity.Uid)||new[]{next.Access,next.Refresh}.Any(t=>t.Contains("****",StringComparison.Ordinal)||t.Contains("[REDACTED]",StringComparison.OrdinalIgnoreCase)))throw new CredentialFailure("invalid_credential_document");
        return next;
    }
    // Explicit admin replacement; never used by a scheduling toggle. The manifest
    // owner serializes changes and supplies the matching site control plane.
    public async Task<string> ReplaceDocumentAsync(AccountId id,byte[] raw,string revision,IAccountControlPlane control,string? loginStamp=null,CancellationToken cancellation=default)
    {
        var next=ValidateEditedDocument(raw);
        await registryGate.WaitAsync(cancellation);
        try
        {
            var cell=Find(id);using var permit=await cell.Gate.WriteAsync(cancellation);var persistence=Editable(cell);
            byte[] oldRaw=persistence.ReadDocument();
            try{if(revision.Length==0||revision!=DocumentRevision(oldRaw))throw new CredentialFailure("credential_revision_conflict");}
            finally{CryptographicOperations.ZeroMemory(oldRaw);}
            if(cells.Any(p=>p.Key!=id&&p.Value.Current.Credential.Identity.Site==next.Identity.Site&&p.Value.Current.Credential.Identity.Uid==next.Identity.Uid&&p.Value.Current.Credential.Identity.EnterpriseId==next.Identity.EnterpriseId))throw new CredentialFailure("credential_identity_duplicate");
            bool same=CredentialRules.SameIdentity(cell.Current.Credential,next);
            var registration=cell.Registration with{ModelEndpoint=SiteEndpoints.Model(next.Identity.Site),Control=control};
            cancellation.ThrowIfCancellationRequested();
            lock(cell.Barrier)
            {
                var disk=persistence.Load();
                if(loginStamp is not null&&(loginStamp!=ReplacementStamp(cell)||!SameCredential(disk.Credential,cell.Current.Credential)||disk.Disabled!=cell.Current.Disabled))throw new CredentialFailure("credential_revision_conflict");
                bool disabled=registration.Disabled||(loginStamp is null&&(disk.Disabled||cell.Current.Disabled||Volatile.Read(ref cell.Blocked)!=0));
                var saved=new PersistedCredential(next,disabled);var replacement=new Cell(registration,saved);
                if(same){replacement.Resources=cell.Resources;replacement.Packages=cell.Packages;}
                var updated=cells.ToDictionary();updated[id]=replacement;var published=updated.ToFrozenDictionary();
                try{persistence.SaveDocument(saved,raw);}
                catch{cell.Blocked=1;cell.Current=cell.Current with{Degraded=true};PublishAvailability(cell,cell.Current);throw;}
                cell.Retired=true;cell.BlockVersion++;cell.Blocked=1;
                cells=published;
                pool.ReplaceCredentialAccount(new(id,registration.ModelEndpoint,next.Identity.Site,Disabled:disabled),same,()=>PublishAvailability(replacement,replacement.Current));
            }
            return DocumentRevision(raw);
        }
        finally{registryGate.Release();}
    }
}
