using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using KeySecurity;
using OpenAiProtocols;
using StreamingMigration;

namespace OpenAiChecks;

internal static class Program
{
    internal static readonly byte[] Key = KeyRules.ReasoningKey("synthetic-openai-key");
    internal static readonly JsonSerializerOptions Json = new(JsonSerializerDefaults.Web);
    sealed record Spec(string Name, string Op = "prepare", JsonElement Request = default, string Sse = "", bool Stream = false, string Protocol = "responses");
    static async Task Main(string[] args)
    {
        Console.InputEncoding = new UTF8Encoding(false); Console.OutputEncoding = new UTF8Encoding(false);
        if (args is ["--self-test"])
        {
            var stream = await StreamTests.Run(); var http = await HttpTests.Run();
            Console.WriteLine(JsonSerializer.Serialize(new { streamTests = stream.Count, httpTests = http.Count, stream, http }, Json)); return;
        }
        if (args is not ["--probe"]) throw new ArgumentException("Use --probe or --self-test");
        while (await Console.In.ReadLineAsync() is { } line)
        {
            var s = JsonSerializer.Deserialize<Spec>(line, Json) ?? throw new JsonException(); object result;
            try
            {
                byte[] bytes = s.Request.ValueKind == JsonValueKind.Undefined ? "{}"u8.ToArray() : JsonSerializer.SerializeToUtf8Bytes(s.Request);
                var request = s.Protocol == "chat" ? ChatRequestAdapter.Prepare(bytes) : ResponsesRequestAdapter.Prepare(bytes, Key);
                if (s.Op == "prepare")
                    result = new { status = 200, body = JsonSerializer.Deserialize<JsonElement>(request.Body) };
                else
                {
                    using var output = new MemoryStream(); var answer = await OpenAiPump.RunAsync(new MemoryStream(Encoding.UTF8.GetBytes(s.Sse)), output, request with { Stream = s.Stream }, Key);
                    result = answer.Outcome == StreamOutcome.Complete ? (object)new { status = 200, body = s.Stream ? JsonValue.Create(Encoding.UTF8.GetString(output.ToArray())) : JsonNode.Parse(answer.Body!) } :
                        new { status = 502, code = answer.Code };
                }
            }
            catch (OpenAiFailure e) { result = new { status = e.Status, error = e.Message }; }
            Console.WriteLine(JsonSerializer.Serialize(new { s.Name, result }, Json));
        }
    }
}
