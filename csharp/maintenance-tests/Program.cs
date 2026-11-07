using System.Text.Json;
using AccountMaintenance;

namespace MaintenanceChecks;

internal static class Program
{
    static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web);
    sealed record Spec(string Name, string Op, string Body = "", int Status = 0, string Token = "", JsonElement Credential = default, JsonElement Reply = default);
    static async Task Main(string[] args)
    {
        Console.InputEncoding = new System.Text.UTF8Encoding(false); Console.OutputEncoding = new System.Text.UTF8Encoding(false);
        if (args is ["--self-test", var directory])
        {
            Directory.CreateDirectory(directory); var manager = await ManagerTests.Run(directory); var http = await HttpMaintenanceTests.Run();
            Console.WriteLine(JsonSerializer.Serialize(new { managerTests = manager.Count, httpTests = http.Count, manager, http }, Json)); return;
        }
        if (args is not ["--probe"]) throw new ArgumentException("Use --probe or --self-test <directory>");
        while (await Console.In.ReadLineAsync() is { } line)
        {
            var s = JsonSerializer.Deserialize<Spec>(line, Json) ?? throw new JsonException(); object result;
            if (s.Op == "classify") result = CredentialRules.Classify(s.Body) == RefreshFailureKind.SessionDead ? "session_dead" : "transient";
            else if (s.Op == "auth_failure") result = CredentialRules.AuthenticationRejected(s.Status, s.Body);
            else if (s.Op == "jwt") result = CredentialRules.JwtExpiry(s.Token);
            else
            {
                var c = CredentialCodec.Decode(JsonSerializer.SerializeToUtf8Bytes(s.Credential));
                if (s.Op == "merge") c = CredentialRules.Merge(c, JsonSerializer.SerializeToUtf8Bytes(s.Reply), 1800000000);
                result = JsonSerializer.Deserialize<JsonElement>(CredentialCodec.Export(c));
            }
            Console.WriteLine(JsonSerializer.Serialize(new { s.Name, result }, Json));
        }
    }
}
