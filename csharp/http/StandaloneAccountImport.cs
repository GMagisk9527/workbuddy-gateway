using System.Text.Json;
using AccountMaintenance;

namespace HttpMigration;

internal static class StandaloneAccountImport
{
    internal static void Run(string source,string destination)
    {
        using var input=new FileStream(source,FileMode.Open,FileAccess.Read,FileShare.Read);
        if(input.Length is <1 or >1048576) throw new CredentialFailure("credential_document_too_large");
        byte[] bytes=new byte[checked((int)input.Length)]; input.ReadExactly(bytes);
        try
        {
            var credential=CredentialCodec.Decode(bytes);
            using var json=JsonDocument.Parse(bytes);
            if(credential.Identity.Uid.Length==0 || string.IsNullOrEmpty(json.RootElement.GetProperty("auth").GetProperty("accessToken").GetString())) throw new CredentialFailure("credential_identity_missing");
            using var owned=CredentialFiles.Create(destination,credential);
            Console.WriteLine("{\"imported\":true,\"sourceModified\":false}");
        }
        finally { System.Security.Cryptography.CryptographicOperations.ZeroMemory(bytes); }
    }
}
