using System.Text;
using System.Text.Json;
using KeySecurity;

namespace SecurityChecks;

internal static class Program
{
    sealed record Probe(string Name, string Op, KeyOptions? Options = null, string Hex = "", string? Token = null,
        string StoredSecret = Fixtures.Secret, bool Enabled = true, long ExpiresAt = 0, int MaxConcurrent = 1, string[]? Models = null, string Model = "", int Held = 0, JsonElement Document = default);
    static async Task Main(string[] args)
    {
        Console.InputEncoding = new UTF8Encoding(false); Console.OutputEncoding = new UTF8Encoding(false);
        if (args is ["--self-test", var directory])
        {
            Directory.CreateDirectory(directory); var store = await StoreTests.Run(Path.GetFullPath(directory)); var http = await HttpSecurityTests.Run();
            Console.WriteLine(JsonSerializer.Serialize(new { storeTests = store.Count, httpTests = http.Count, store, http }, Fixtures.Json)); return;
        }
        if (args is not ["--probe"]) throw new ArgumentException("Use --probe or --self-test <scratch directory>");
        while (await Console.In.ReadLineAsync() is { } line)
        {
            var spec = JsonSerializer.Deserialize<Probe>(line, Fixtures.Json) ?? throw new JsonException(); object output;
            switch (spec.Op)
            {
                case "normalize": output = new { spec.Name, result = KeyRules.Normalize(spec.Options ?? new()) }; break;
                case "base32": output = new { spec.Name, result = KeyRules.Base32(Convert.FromHexString(spec.Hex)) }; break;
                case "allows": output = new { spec.Name, result = KeyRules.Allows(spec.Models ?? [], spec.Model) }; break;
                case "import":
                    var backend = new MemoryBackend(); backend.Data["apikeys.json"] = JsonSerializer.SerializeToUtf8Bytes(spec.Document);
                    using (var imported = new KeyStore(backend.Open())) output = new { spec.Name, result = imported.Authenticate(spec.Token ?? spec.StoredSecret) is not null };
                    break;
                default:
                    using (var store = new KeyStore(Fixtures.Store(spec.StoredSecret, spec.Enabled, spec.ExpiresAt, spec.MaxConcurrent, spec.Models).Open()))
                    {
                        string token = spec.Token ?? spec.StoredSecret;
                        if (spec.Op == "authenticate") output = new { spec.Name, result = store.Authenticate(token) is not null };
                        else
                        {
                            var held = new List<KeyLease>();
                            for (int i = 0; i < spec.Held; i++) { try { held.Add(store.Acquire(spec.StoredSecret)); } catch (KeyFailure) { break; } }
                            string code = Fixtures.Failure(() => { using var lease = store.Acquire(token, spec.Model); });
                            foreach (var lease in held) lease.Dispose(); output = new { spec.Name, result = code };
                        }
                    }
                    break;
            }
            Console.WriteLine(JsonSerializer.Serialize(output, Fixtures.Json));
        }
    }
}
