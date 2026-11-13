using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using OpenAiProtocols;
using StreamingMigration;
using static OpenAiChecks.StreamTests;

namespace OpenAiChecks;

internal static class ToolArgumentTests
{
    internal static readonly IReadOnlyDictionary<string, string> Expected = new Dictionary<string, string>
    {
        ["write_file"] = JsonSerializer.Serialize(new { path = "中文.txt", content = "line 1\n中文 😀 \\\"quoted\\\"" }),
        ["read_file"] = JsonSerializer.Serialize(new { path = "other.txt", offset = 7 })
    };
    internal static JsonObject Fragment(int index, string arguments, string id = "", string name = "")
    {
        var function = new JsonObject { ["arguments"] = arguments };
        if (name != "") function["name"] = name;
        var fragment = new JsonObject { ["index"] = index, ["function"] = function };
        if (id != "") fragment["id"] = id;
        return fragment;
    }
    internal static string Frame(params JsonObject[] fragments) => "data: " + new JsonObject
    {
        ["choices"] = new JsonArray(new JsonObject { ["delta"] = new JsonObject { ["tool_calls"] = new JsonArray(fragments.Select(f => (JsonNode?)f).ToArray()) } })
    }.ToJsonString() + "\n\n";
    internal static string Interleaved()
    {
        string a = Expected["write_file"], b = Expected["read_file"];
        int first = a.Length / 4, second = a.Length / 2, third = 3 * a.Length / 4, split = b.Length / 2;
        return Frame(Fragment(0, a[..first]), Fragment(1, b[..split], "call_read", "read_file")) +
            Frame(Fragment(0, a[first..second], "call_write"), Fragment(1, b[split..])) +
            Frame(Fragment(0, a[second..third], name: "write_file"), Fragment(0, a[third..])) + Finish;
    }
    internal static void Verify(string path, bool stream, string output)
    {
        var actual = new Dictionary<string, string>();
        var ids = new HashSet<string>();
        if (path.EndsWith("responses", StringComparison.Ordinal))
        {
            JsonNode completed = stream ? Events(output).Single(e => OpenWire.Text(e["type"]) == "response.completed")["response"]! : JsonNode.Parse(output)!;
            foreach (var call in completed["output"]!.AsArray().OfType<JsonObject>().Where(c => OpenWire.Text(c["type"]) == "function_call"))
            {
                string name = OpenWire.Text(call["name"]);
                actual.Add(name, OpenWire.Text(call["arguments"])); ids.Add(OpenWire.Text(call["call_id"]));
                if (stream)
                {
                    var events = Events(output);
                    string id = OpenWire.Text(call["id"]);
                    string joined = string.Concat(events.Where(e => OpenWire.Text(e["type"]) == "response.function_call_arguments.delta" && OpenWire.Text(e["item_id"]) == id).Select(e => OpenWire.Text(e["delta"])));
                    Check(joined == actual[name], "late identity and repeated same-event fragments are emitted exactly once");
                    Check(events.Single(e => OpenWire.Text(e["type"]) == "response.function_call_arguments.done" && OpenWire.Text(e["item_id"]) == id)["arguments"]!.GetValue<string>() == joined, "done arguments match deltas");
                }
            }
        }
        else if (path.EndsWith("messages", StringComparison.Ordinal))
        {
            if (stream)
            {
                var events = Events(output);
                foreach (var start in events.Where(e => OpenWire.Text(e["type"]) == "content_block_start" && OpenWire.Text(e["content_block"]?["type"]) == "tool_use"))
                {
                    long index = OpenWire.Int(start["index"]);
                    string arguments = string.Concat(events.Where(e => OpenWire.Text(e["type"]) == "content_block_delta" && OpenWire.Int(e["index"]) == index).Select(e => OpenWire.Text(e["delta"]?["partial_json"])));
                    actual.Add(OpenWire.Text(start["content_block"]?["name"]), arguments); ids.Add(OpenWire.Text(start["content_block"]?["id"]));
                }
                Check(events.Count(e => OpenWire.Text(e["type"]) == "message_stop") == 1, "Messages terminates once");
            }
            else foreach (var call in JsonNode.Parse(output)!["content"]!.AsArray().OfType<JsonObject>().Where(c => OpenWire.Text(c["type"]) == "tool_use"))
            { actual.Add(OpenWire.Text(call["name"]), call["input"]!.ToJsonString()); ids.Add(OpenWire.Text(call["id"])); }
        }
        else if (stream)
        {
            var calls = Events(output).SelectMany(e => OpenWire.Items(e["choices"])).OfType<JsonObject>()
                .SelectMany(c => OpenWire.Items(c["delta"]?["tool_calls"])).OfType<JsonObject>().GroupBy(t => OpenWire.Int(t["index"]));
            foreach (var call in calls)
            {
                string name = call.Select(t => OpenWire.Text(t["function"]?["name"])).First(n => n != "");
                actual.Add(name, string.Concat(call.Select(t => OpenWire.Text(t["function"]?["arguments"]))));
                ids.Add(call.Select(t => OpenWire.Text(t["id"])).First(id => id != ""));
            }
        }
        else foreach (var call in JsonNode.Parse(output)!["choices"]![0]!["message"]!["tool_calls"]!.AsArray().OfType<JsonObject>())
        { actual.Add(OpenWire.Text(call["function"]?["name"]), OpenWire.Text(call["function"]?["arguments"])); ids.Add(OpenWire.Text(call["id"])); }
        Check(actual.Count == Expected.Count && ids.SetEquals(["call_write", "call_read"]), "parallel tool identities are preserved");
        foreach (var expected in Expected)
            Check(actual.TryGetValue(expected.Key, out string? arguments) && JsonNode.DeepEquals(JsonNode.Parse(arguments), JsonNode.Parse(expected.Value)), "tool JSON is unchanged: " + expected.Key);
    }
    internal static async Task<List<string>> Run()
    {
        foreach (bool responses in new[] { false, true }) foreach (bool stream in new[] { false, true })
        {
            var (result, output) = await Pump(Interleaved(), Request(responses, stream));
            Check(result.Outcome == StreamOutcome.Complete, "interleaved tool stream completes");
            Verify(responses ? "/v1/responses" : "/v1/chat/completions", stream, output);
        }
        const string customRequest = """{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"custom","name":"run"}]}]}""";
        const string input = "line 1\n中文 😀";
        string customArguments = JsonSerializer.Serialize(new { input });
        int split = customArguments.Length / 3;
        string customSse = Frame(Fragment(0, customArguments[..split])) + Frame(Fragment(0, customArguments[split..(2 * split)], "call_custom", "ns__run")) + Frame(Fragment(0, customArguments[(2 * split)..])) + Finish;
        foreach (bool stream in new[] { false, true })
        {
            var (result, output) = await Pump(customSse, Request(stream: stream, json: customRequest));
            Check(result.Outcome == StreamOutcome.Complete, "fragmented custom call completes");
            JsonNode response = stream ? Events(output).Single(e => OpenWire.Text(e["type"]) == "response.completed")["response"]! : JsonNode.Parse(output)!;
            var call = response["output"]![0]!;
            Check(OpenWire.Text(call["namespace"]) == "ns" && OpenWire.Text(call["name"]) == "run" && OpenWire.Text(call["call_id"]) == "call_custom" && OpenWire.Text(call["input"]) == input, "custom call retains namespace, identity and input");
            if (stream) Check(string.Concat(Events(output).Where(e => OpenWire.Text(e["type"]) == "response.custom_tool_call_input.delta").Select(e => OpenWire.Text(e["delta"]))) == input, "custom input is emitted once");
        }
        foreach (bool responses in new[] { false, true }) foreach (bool stream in new[] { false, true })
        {
            var (result, output) = await Pump(Frame(Fragment(0, new string('x', 8192), "call_large", "write_file")) + Finish, Request(responses, stream), 4096);
            Check(result.Code == "output_budget_exceeded" && !output.Contains("[DONE]", StringComparison.Ordinal) && !output.Contains("response.completed", StringComparison.Ordinal), "tool output budget still prevents false success");
            using var cancelled = new CancellationTokenSource(); using var downstream = new MemoryStream();
            int callbacks = 0;
            result = await OpenAiPump.RunAsync(new CancelAfterFrame(Frame(Fragment(0, "{", "call_cancel", "write_file")), cancelled), downstream, Request(responses, stream), Program.Key,
                cancellation: cancelled.Token, beforeTerminal: _ => callbacks++);
            output = Encoding.UTF8.GetString(downstream.ToArray());
            Check(result.Outcome == StreamOutcome.Cancelled && callbacks == 1 && !output.Contains("[DONE]", StringComparison.Ordinal) && !output.Contains("response.completed", StringComparison.Ordinal), "cancellation after a tool fragment remains terminal");
        }
        // A 512 KiB argument split into small chunks previously allocated over
        // 1 GiB by materializing all preceding chunks on every Feed call.
        byte[] fragment = Encoding.UTF8.GetBytes(Frame(Fragment(0, new string('x', 256)))[6..^2]);
        byte[] start = Encoding.UTF8.GetBytes(Frame(Fragment(0, "{\"content\":\"", "call_large", "write_file"))[6..^2]);
        byte[] end = Encoding.UTF8.GetBytes(Frame(Fragment(0, "\"}"))[6..^2]);
        var warm = new ChatState("m", 64L << 20); warm.Feed(start); warm.Feed(fragment); warm.Feed(end);
        long before = GC.GetAllocatedBytesForCurrentThread();
        var state = new ChatState("m", 64L << 20); state.Feed(start);
        for (int i = 0; i < 2048; i++) state.Feed(fragment);
        state.Feed(end);
        long allocated = GC.GetAllocatedBytesForCurrentThread() - before;
        Check(allocated < 64L << 20, "fragmented 512 KiB tool arguments must stay below 64 MiB cumulative allocation");
        using var parsed = JsonDocument.Parse(state.Calls.Single().Arguments.ToString());
        Check(parsed.RootElement.GetProperty("content").GetString() == new string('x', 512 << 10), "allocation optimization retains every tool argument byte");
        return ["interleaved late-identity tools preserve arguments in Chat/Responses and both modes", "fragmented custom tools preserve namespace and input", "tool budgets and cancellation retain failure semantics", $"512KiB fragmented tool allocation: {allocated} bytes (<64MiB)"];
    }
    sealed class CancelAfterFrame(string frame, CancellationTokenSource cancellation) : MemoryStream(Encoding.UTF8.GetBytes(frame))
    {
        public override ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default)
        {
            if (Position == Length) { cancellation.Cancel(); return ValueTask.FromCanceled<int>(cancellationToken); }
            return base.ReadAsync(buffer, cancellationToken);
        }
    }
}
