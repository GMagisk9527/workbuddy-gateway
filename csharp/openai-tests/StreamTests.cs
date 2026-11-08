using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using KeySecurity;
using OpenAiProtocols;
using StreamingMigration;

namespace OpenAiChecks;

internal static class StreamTests
{
    internal const string Text = "data: {\"id\":\"chatcmpl-fixed\",\"created\":100,\"model\":\"m\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n";
    internal const string Finish = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":2,\"prompt_tokens\":20,\"completion_tokens\":3,\"cached_tokens\":5,\"completion_tokens_details\":{\"reasoning_tokens\":0},\"completion_thinking_tokens\":4}}\n\n";
    internal static void Check(bool value, string name) { if (!value) throw new InvalidOperationException(name); }
    internal static OpenAiRequest Request(bool responses = true, bool stream = true, string? json = null)
    {
        byte[] body = Encoding.UTF8.GetBytes(json ?? (responses ? "{\"model\":\"m\",\"input\":\"hi\"}" : "{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"));
        var result = responses ? ResponsesRequestAdapter.Prepare(body, Program.Key) : ChatRequestAdapter.Prepare(body); return result with { Stream = stream };
    }
    internal static async Task<(OpenAiResult Result, string Output)> Pump(string sse, OpenAiRequest request, long limit = 1 << 20, Action<OpenAiResult>? terminal = null)
    {
        using var output = new MemoryStream(); var result = await OpenAiPump.RunAsync(new MemoryStream(Encoding.UTF8.GetBytes(sse)), output, request, Program.Key, limit, beforeTerminal: terminal);
        return (result, request.Stream ? Encoding.UTF8.GetString(output.ToArray()) : result.Body is null ? "" : Encoding.UTF8.GetString(result.Body));
    }
    internal static List<JsonObject> Events(string value) => value.Split('\n').Where(s => s.StartsWith("data: {", StringComparison.Ordinal)).Select(s => (JsonObject)JsonNode.Parse(s[6..])!).ToList();
    public static async Task<List<string>> Run()
    {
        var tests = new List<string>(); async Task Test(string name, Func<Task> action) { await action(); tests.Add(name); }
        await Test("both protocols and response modes complete with measured raw usage", async () =>
        {
            foreach (bool responses in new[] { false, true }) foreach (bool stream in new[] { false, true })
            {
                int callbacks = 0; var (result, output) = await Pump(Text + Finish, Request(responses, stream), terminal: r => { callbacks++; Check(r.RawUsageJson!.Contains("\"credit\":2", StringComparison.Ordinal), "actual usage"); });
                Check(result.Outcome == StreamOutcome.Complete && callbacks == 1 && output.Contains("hello", StringComparison.Ordinal), "complete");
                if (stream) Check(output.EndsWith("data: [DONE]\n\n", StringComparison.Ordinal), "final marker");
            }
        });
        await Test("length and content-filter preserve incomplete Responses semantics", async () =>
        {
            foreach (string reason in new[] { "length", "content_filter" }) foreach (bool stream in new[] { false, true })
            {
                var (result, output) = await Pump(Text + Finish.Replace("\"stop\"", "\"" + reason + "\"", StringComparison.Ordinal), Request(stream: stream));
                Check(result.Outcome == StreamOutcome.Complete && (stream ? output.Contains("response.incomplete", StringComparison.Ordinal) && !output.Contains("response.completed", StringComparison.Ordinal) : JsonNode.Parse(output)!["status"]!.GetValue<string>() == "incomplete"), "incomplete reason");
            }
        });
        await Test("bad tail after finish cannot emit success or DONE", async () =>
        {
            foreach (bool responses in new[] { false, true })
            {
                var (result, output) = await Pump(Text + Finish + "data: {bad}\n\n", Request(responses));
                Check(result.Outcome == StreamOutcome.Failed && !output.Contains("[DONE]", StringComparison.Ordinal) && !output.Contains("response.completed", StringComparison.Ordinal) && !output.Contains("\"finish_reason\":\"stop\"", StringComparison.Ordinal), "no false terminal");
            }
        });
        await Test("EOF or DONE without a finish remains failure", async () =>
        {
            foreach (string suffix in new[] { "", "data: [DONE]\n\n" })
            {
                var (result, output) = await Pump(Text + suffix, Request()); Check(result.Outcome == StreamOutcome.Failed && output.Contains("response.failed", StringComparison.Ordinal) && !output.Contains("[DONE]", StringComparison.Ordinal), "missing finish");
            }
        });
        await Test("a backend error frame after finish cannot be turned into success", async () =>
        {
            foreach (bool responses in new[] { false, true })
            {
                var (result, output) = await Pump(Text + Finish + "data: {\"error\":{\"code\":\"stream_interrupted\",\"message\":\"synthetic\"}}\n\n", Request(responses));
                Check(result.Outcome == StreamOutcome.Failed && !output.Contains("[DONE]", StringComparison.Ordinal), "backend error is terminal");
            }
        });
        await Test("tool arguments arriving before identity are emitted once", async () =>
        {
            string sse = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"function\":{\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\n" +
                "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_a\",\"function\":{\"name\":\"run\",\"arguments\":\"1}\"}},{\"index\":1,\"function\":{\"arguments\":\" \"}}]}}]}\n\n" + Finish;
            var (result, output) = await Pump(sse, Request()); var events = Events(output);
            string args = string.Concat(events.Where(e => OpenWire.Text(e["type"]) == "response.function_call_arguments.delta").Select(e => OpenWire.Text(e["delta"])));
            Check(result.Outcome == StreamOutcome.Complete && args == "{\"a\":1} ", "no repeated fragments");
        });
        await Test("ready identities assign contiguous output indices without holes", async () =>
        {
            string sse = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"b\",\"function\":{\"name\":\"second\",\"arguments\":\"{}\"}},{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"first\"}}]}}]}\n\n" + Finish;
            var (_, output) = await Pump(sse, Request()); var added = Events(output).Where(e => OpenWire.Text(e["type"]) == "response.output_item.added").ToArray();
            Check(added.Length == 2 && OpenWire.Text(added[0]["item"]?["name"]) == "second" && added[0]["output_index"]!.GetValue<int>() == 0 && added[1]["output_index"]!.GetValue<int>() == 1, "ready order");
        });
        await Test("missing tool identity never yields completed or item.done", async () =>
        {
            string sse = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n" + Finish;
            var (result, output) = await Pump(sse, Request()); Check(result.Code == "incomplete_tool_identity" && !output.Contains("response.output_item.done", StringComparison.Ordinal) && !output.Contains("[DONE]", StringComparison.Ordinal), "invalid identity");
        });
        await Test("custom namespace calls restore input and identity in both modes", async () =>
        {
            string request = "{\"model\":\"m\",\"input\":\"hi\",\"tools\":[{\"type\":\"namespace\",\"name\":\"ns\",\"tools\":[{\"type\":\"custom\",\"name\":\"run\"}]}]}";
            string sse = "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"function\":{\"name\":\"ns__run\",\"arguments\":\"{\\\"input\\\":\\\"a\\\\nb\\\"}\"}}]}}]}\n\n" + Finish;
            foreach (bool stream in new[] { false, true })
            {
                var (result, output) = await Pump(sse, Request(stream: stream, json: request)); Check(result.Outcome == StreamOutcome.Complete, "custom");
                JsonNode call = stream ? Events(output).Last(e => OpenWire.Text(e["type"]) == "response.completed")["response"]!["output"]![0]! : JsonNode.Parse(output)!["output"]![0]!;
                Check(OpenWire.Text(call["type"]) == "custom_tool_call" && OpenWire.Text(call["namespace"]) == "ns" && OpenWire.Text(call["name"]) == "run" && OpenWire.Text(call["input"]) == "a\nb", "restored custom");
            }
        });
        await Test("encrypted reasoning roundtrips with credential model and protocol binding", async () =>
        {
            var request = Request(stream: false, json: "{\"model\":\"m\",\"input\":\"hi\",\"include\":[\"reasoning.encrypted_content\"]}");
            var (_, output) = await Pump("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"private thought\"}}]}\n\n" + Text + Finish, request);
            var reasoning = JsonNode.Parse(output)!["output"]![0]!.DeepClone(); string cipher = OpenWire.Text(reasoning["encrypted_content"]);
            Check(new ReasoningCodec(Program.Key, "m", "responses").Open(cipher) == "private thought", "opaque decode");
            reasoning["summary"] = new JsonArray();
            byte[] replay = OpenWire.Encode(new JsonObject { ["model"] = "m", ["input"] = new JsonArray(reasoning, new JsonObject { ["role"] = "assistant", ["content"] = "answer" }) }, 1 << 20);
            var restored = ResponsesRequestAdapter.Prepare(replay, Program.Key); Check(Encoding.UTF8.GetString(restored.Body).Contains("private thought", StringComparison.Ordinal), "history restore");
            try { ResponsesRequestAdapter.Prepare(replay, KeyRules.ReasoningKey("other-key")); throw new InvalidOperationException("wrong credential"); } catch (OpenAiFailure) { }
        });
        await Test("explicit business rejection preserves already measured usage", async () =>
        {
            var (result, output) = await Pump(Text + Finish + "data: {\"code\":11128}\n\n", Request());
            Check(result.Code == "11128" && result.RawUsageJson!.Contains("\"credit\":2", StringComparison.Ordinal) && !output.Contains("[DONE]", StringComparison.Ordinal), "usage on rejection");
        });
        await Test("callback failure prevents final success frames", async () =>
        {
            using var output = new MemoryStream();
            try { await OpenAiPump.RunAsync(new MemoryStream(Encoding.UTF8.GetBytes(Text + Finish)), output, Request(), Program.Key, beforeTerminal: _ => throw new KeyFailure(503, "ledger_failure", "synthetic")); throw new InvalidOperationException("callback succeeded"); }
            catch (KeyFailure) { Check(!Encoding.UTF8.GetString(output.ToArray()).Contains("response.completed", StringComparison.Ordinal), "settlement barrier"); }
        });
        await Test("output budget rejects long deltas without success", async () =>
        {
            string huge = "data: " + JsonSerializer.Serialize(new { choices = new[] { new { delta = new { content = new string('x', 8192) } } } }) + "\n\n" + Finish;
            var (result, output) = await Pump(huge, Request(), 2048); Check(result.Code == "output_budget_exceeded" && !output.Contains("[DONE]", StringComparison.Ordinal), "bounded output");
        });
        await Test("reasoning token detail zero wins over legacy fallback", async () =>
        {
            var (_, output) = await Pump(Text + Finish, Request(stream: false));
            Check(JsonNode.Parse(output)!["usage"]!["output_tokens_details"]!["reasoning_tokens"]!.GetValue<int>() == 0, "explicit zero");
        });
        await Test("parallel tool outputs retain images and exact JSON content", () =>
        {
            var request = ResponsesRequestAdapter.Prepare(Encoding.UTF8.GetBytes("{\"model\":\"m\",\"input\":[{\"type\":\"function_call_output\",\"call_id\":\"a\",\"output\":[{\"type\":\"input_text\",\"text\":\"ok\"},{\"type\":\"input_image\",\"image_url\":\"data:image/png;base64,AA==\"}]}]}"), Program.Key);
            var body = JsonNode.Parse(request.Body)!; Check(body["messages"]!.AsArray().Count == 2 && OpenWire.Text(body["messages"]![0]!["content"]) == "ok" && OpenWire.Text(body["messages"]![1]!["role"]) == "user", "image promotion"); return Task.CompletedTask;
        });
        return tests;
    }
}
