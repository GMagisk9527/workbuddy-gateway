using KeySecurity;
using LedgerMigration;

static class StorageChecks
{
    internal static void Run()
    {
        var keyFiles=new MemoryKeys();var ledgerFiles=new MemoryLedger();string id,secret;
        // Initialise the ledger before creating a key. Existing keys with an
        // absent ledger are deliberately blocked as possible accounting loss.
        using(var fresh=new CreditLedger(ledgerFiles))
            if(!fresh.Health.Healthy)throw new Exception("Native fresh ledger failed");
        using(var store=new KeyStore(keyFiles))
        {
            var issued=store.Create(new("synthetic native probe",QuotaTotal:100));id=issued.Item.Id;secret=issued.TakeSecret();
        }
        using(var ledger=new CreditLedger(ledgerFiles,[id]))
        {
            var attempt=ledger.Begin(id);
            if(ledger.Complete(attempt,"{\"credit\":2.5,\"prompt_tokens\":20,\"completion_tokens\":3,\"cached_tokens\":7}",200)!=SettlementKind.Charged)
                throw new Exception("Native settlement failed");
            if(ledger.Complete(attempt,"{\"credit\":999}",200)!=SettlementKind.Duplicate)
                throw new Exception("Native duplicate settlement");
            ledger.Compact();
        }
        using(var store=new KeyStore(keyFiles))
        using(var ledger=new CreditLedger(ledgerFiles,[id]))
        {
            store.AttachAccounting(ledger);
            using var lease=store.Acquire(secret);
            if(lease.Identity?.Id!=id || ledger.GetUsage(id) is not {Total:2.5,Requests:1,Input:20,Output:3,Cached:7} || !ledger.Health.Healthy)
                throw new Exception("Native durable reload failed");
            ledger.CancelUncharged(ledger.Begin(id));
        }
    }
    sealed class MemoryKeys:IKeyFiles
    {
        readonly Dictionary<string,byte[]> data=[];
        public byte[]? Read(string name)=>data.GetValueOrDefault(name)?.ToArray();
        public void WriteAtomic(string name,ReadOnlySpan<byte> bytes)=>data[name]=bytes.ToArray();
        public void Delete(string name)=>data.Remove(name);
        public void Dispose(){}
    }
    sealed class MemoryLedger:ILedgerFiles
    {
        readonly Dictionary<string,byte[]> data=[];
        public byte[]? Read(string name)=>data.GetValueOrDefault(name)?.ToArray();
        public void Replace(string name,ReadOnlySpan<byte> bytes)=>data[name]=bytes.ToArray();
        public int Append(ReadOnlySpan<byte> bytes)
        {data[CreditLedger.WalName]=[..data.GetValueOrDefault(CreditLedger.WalName)??[],..bytes];return bytes.Length;}
        public void Sync(){}
        public void Dispose(){}
    }
}
