using System.Text;
using System.Text.Json;
using AccountLogin;
using AccountMaintenance;
using AccountScheduling;

namespace LoginChecks;

internal static class Program
{
    static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web);
    sealed record Spec(string Name, string Op, string File = "", string Site = "cn", string Status = "pending", int RemainingMs = 0, JsonElement Token = default, JsonElement Account = default);
    static async Task Main(string[] args)
    {
        Console.InputEncoding = new UTF8Encoding(false); Console.OutputEncoding = new UTF8Encoding(false);
        if (args is ["--self-test", var root])
        {
            Directory.CreateDirectory(root); var coordinator = await CoordinatorTests.Run(root); var http = await HttpLoginTests.Run();
            Console.WriteLine(JsonSerializer.Serialize(new { coordinatorTests = coordinator.Count, httpTests = http.Count, coordinator, http }, Json)); return;
        }
        if (args is not ["--probe"]) throw new ArgumentException("Use --probe or --self-test <scratch>");
        while (await Console.In.ReadLineAsync() is { } line)
        {
            var s = JsonSerializer.Deserialize<Spec>(line, Json) ?? throw new JsonException(); object result;
            var site = s.Site == "intl" ? AccountSite.Intl : AccountSite.Cn;
            if (s.Op == "file") result = LoginRules.ValidFile(s.File);
            else if (s.Op == "platform") result = LoginRules.Platform(site);
            else if (s.Op == "remaining")
            {
                var status = Enum.Parse<LoginStatus>(s.Status, true); var now = DateTimeOffset.FromUnixTimeSeconds(1800000000);
                result = LoginRules.Remaining(status, now.AddMilliseconds(s.RemainingMs), now);
            }
            else result = JsonSerializer.Deserialize<JsonElement>(CredentialCodec.Export(LoginRules.Candidate(s.Token, s.Account, site, 1800000000)));
            Console.WriteLine(JsonSerializer.Serialize(new { s.Name, result }, Json));
        }
    }
}
