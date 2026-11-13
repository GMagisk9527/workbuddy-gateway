using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using OpenAiProtocols;
using StreamingMigration;
using static OpenAiChecks.StreamTests;

namespace OpenAiChecks;

internal static class Utf8ResponseTests
{
    internal static async Task<List<string>> Run()
    {
        const string first = """{"id":"chat-fixed","created":123,"model":"m","extra":{"nested":[1,"keep"]},"choices":[null,{"delta":{"role":"assistant","reasoning_content":"思考 😀","content":"discard","content":"中文 ","tool_calls":[null,{"index":2,"id":"call_a","type":"function","function":{"name":"run","arguments":"{"}}]}}]}""";
        const string next = """{"choices":[{"delta":{"content":"code: x < 2\n","refusal":"no","tool_calls":[{"index":2,"function":{"arguments":"\"x\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"credit":2,"credit":3,"custom":{"source":"measured"},"prompt_tokens":20,"completion_tokens":3}}""";
        var rawState = new ChatState("m", 1 << 20); var typedState = new ChatState("m", 1 << 20);
        foreach (string payload in new[] { first, "null", "{\"choices\":[{\"delta\":false}]}", next })
        {
            byte[] bytes = Encoding.UTF8.GetBytes(payload);
            var (raw, chunk) = rawState.Feed(bytes); var typed = typedState.FeedResponses(bytes);
            Check(JsonSerializer.Serialize(chunk) == JsonSerializer.Serialize(typed), "typed chunks are identical");
            if (payload == first) Check(raw["extra"]!["nested"]![1]!.GetValue<string>() == "keep", "Chat keeps unknown fields");
            // Reuse the caller's entire input buffer after Feed returns.
            bytes.AsSpan().Fill((byte)'!');
        }
        Check(JsonNode.DeepEquals(rawState.Completion(), typedState.Completion()), "raw and typed states agree after borrowed input reuse");
        Check(typedState.Content.ToString() == "中文 code: x < 2\n" && typedState.Reasoning.ToString() == "思考 😀" && typedState.Refusal.ToString() == "no", "content is unchanged and duplicate keys use last value");
        Check(typedState.Id == "chat-fixed" && typedState.Created == 123 && typedState.Calls.Single() is { Index: 2, Id: "call_a", Name: "run", Type: "function" } && typedState.Calls[0].Arguments.ToString() == "{\"x\":1}", "metadata and tool association survive");
        Check(typedState.Usage!["credit"]!.GetValue<int>() == 3 && typedState.Usage["custom"]!["source"]!.GetValue<string>() == "measured", "owned raw usage remains exact");

        var restore = new ResponseIdentityPlan([new("GatewayAgent", "OriginalAgent")]);
        foreach (bool stream in new[] { false, true })
        {
            var request = Request(stream: stream) with { ResponseRestore = restore };
            const string sse = "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Gateway\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Agent thinks\",\"content\":\"GatewayAgent says hi\"}}]}\n\n";
            var (result, output) = await Pump(sse + Finish, request);
            JsonNode complete = stream ? Events(output).Last(e => OpenWire.Text(e["type"]) == "response.completed")["response"]! : JsonNode.Parse(output)!;
            Check(result.Outcome == StreamOutcome.Complete && complete["output"]![0]!["summary"]![0]!["text"]!.GetValue<string>() == "OriginalAgent thinks" && complete["output"]![1]!["content"]![0]!["text"]!.GetValue<string>() == "OriginalAgent says hi", "identity restoration fallback keeps split prefixes and channel switching");
        }
        const string unknown = "data: {\"vendor_extension\":{\"nested\":[1,\"keep\"]},\"choices\":[{\"delta\":{\"content\":\"hello\",\"vendor_delta\":{\"keep\":true}}}]}\n\n";
        var (chatResult, chatOutput) = await Pump(unknown + Finish, Request(responses: false));
        var chatEvent = Events(chatOutput).First();
        Check(chatResult.Outcome == StreamOutcome.Complete && chatEvent["vendor_extension"]!["nested"]![1]!.GetValue<string>() == "keep" && chatEvent["choices"]![0]!["delta"]!["vendor_delta"]!["keep"]!.GetValue<bool>(), "Chat unknown-field forwarding stays intact");

        byte[] fragment = Encoding.UTF8.GetBytes("{\"choices\":[{\"delta\":{\"content\":\"" + new string('x', 256) + "\"}}]}");
        new ChatState("m", 1 << 20).FeedResponses(fragment);
        long before = GC.GetAllocatedBytesForCurrentThread(); var state = new ChatState("m", 8 << 20);
        for (int i = 0; i < 2048; i++) state.FeedResponses(fragment);
        long allocated = GC.GetAllocatedBytesForCurrentThread() - before;
        Check(state.Content.Length == 512 << 10 && allocated < 8 << 20, "typed 512 KiB Responses state stays below 8 MiB allocation");
        return ["UTF-8 Responses preserves metadata, duplicate keys, tool IDs and owned usage", "Responses identity restoration retains split prefixes in both modes", "Chat keeps unknown vendor fields", $"typed Responses allocation: {allocated} bytes (<8MiB)"];
    }
}
