using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using StreamingMigration;
using static OpenAiProtocols.OpenWire;

namespace OpenAiProtocols;

public sealed class ChatState(string model, long limit,IRetainedOutputBudget? budget=null,ResponseIdentityPlan? responseRestore=null)
{
    public sealed class Tool
    {
        public int Index; public string Id = "", Name = "", Type = "function"; public StringBuilder Arguments = new();
    }
    readonly Dictionary<int, Tool> tools = [];
    readonly Dictionary<(int Choice, string Channel), ResponseIdentityRestorer> restorers = [];
    public sealed record ToolChange(int Choice, int Index, string Id, string Name, string Arguments, int AccumulatedLength);
    public List<ToolChange> Changes { get; } = [];
    public List<Tool> Calls { get; } = [];
    public StringBuilder Content { get; } = new();
    public StringBuilder Reasoning { get; } = new();
    public StringBuilder Refusal { get; } = new();
    public string Model { get; private set; } = model;
    public string Id { get; private set; } = "";
    public string Role { get; private set; } = "assistant";
    public long Created { get; private set; }
    public string FinishReason { get; private set; } = "";
    public JsonObject? Usage { get; private set; }
    public bool ReceivedOutput { get; private set; }
    long retained;
    void Reserve(string value)
    {
        int bytes = Encoding.UTF8.GetByteCount(value); if (bytes > limit - retained) throw new ConversionFailure("output_budget_exceeded");budget?.Add(bytes); retained += bytes;
    }
    public (JsonObject Raw, ChatChunk Chunk) Feed(string payload)=>Feed(Encoding.UTF8.GetBytes(payload));
    public (JsonObject Raw, ChatChunk Chunk) Feed(ReadOnlyMemory<byte> payload)
    {
        var (raw, chunk) = FeedCore(payload, materializeRaw: true);
        return (raw!, chunk);
    }
    // Responses uses the typed chunk and accumulated state, not Chat's unknown
    // fields. Only materialize the full DOM when identity restoration needs it.
    public ChatChunk FeedResponses(ReadOnlyMemory<byte> payload) => FeedCore(payload, materializeRaw: false).Chunk;
    (JsonObject? Raw, ChatChunk Chunk) FeedCore(ReadOnlyMemory<byte> payload, bool materializeRaw)
    {
        Changes.Clear();
        JsonDocument document;
        try { document=JsonDocument.Parse(payload,new JsonDocumentOptions{MaxDepth=10000}); }
        catch(JsonException){ throw new OpenAiFailure("invalid_json","无效的 JSON 请求体"); }
        using(document)
        {
        var root=document.RootElement;
        if(root.ValueKind is not (JsonValueKind.Object or JsonValueKind.Null)) throw new OpenAiFailure("invalid_request","请求体必须是 JSON 对象");
        var parsed = ChunkReader.Parse(root); ReceivedOutput |= parsed.HasOutput;
        var usage = Field(root, "usage");
        if (usage.ValueKind == JsonValueKind.Object)
        {
            var evidence = (JsonObject)ToNode(usage)!;
            Reserve(evidence.ToJsonString()); Usage = evidence;
        }
        JsonObject? raw = materializeRaw || responseRestore is { Active: true }
            ? root.ValueKind == JsonValueKind.Null ? new JsonObject() : (JsonObject)ToNode(root)! : null;
        if (responseRestore is { Active: true })
        {
            ResponseIdentityWire.Apply(raw!, responseRestore, restorers);
            using var restored = JsonDocument.Parse(raw!.ToJsonString(), new JsonDocumentOptions { MaxDepth = 10000 });
            parsed = ChunkReader.Parse(restored.RootElement);
            Accumulate(restored.RootElement);
        }
        else Accumulate(root);
        return (raw, parsed);
        }
    }
    static JsonElement Field(JsonElement value, string name) => value.ValueKind == JsonValueKind.Object && value.TryGetProperty(name, out var field) ? field : default;
    static string String(JsonElement value) => value.ValueKind == JsonValueKind.String ? value.GetString() ?? "" : "";
    static long Integer(JsonElement value) => value.ValueKind == JsonValueKind.Number && value.TryGetInt64(out long n) ? n : 0;
    static IEnumerable<JsonElement> Elements(JsonElement value) => value.ValueKind == JsonValueKind.Array ? value.EnumerateArray() : [];
    void Append(JsonElement delta, string name, StringBuilder target)
    { string text = String(Field(delta, name)); Reserve(text); target.Append(text); }
    void Accumulate(JsonElement root)
    {
        string id = String(Field(root, "id")), model = String(Field(root, "model"));
        if (id != "") Id = id;
        if (model != "") Model = model;
        var created = Field(root, "created");
        if (created.ValueKind is not (JsonValueKind.Null or JsonValueKind.Undefined)) Created = Integer(created);
        int choiceIndex = -1;
        foreach (var choice in Elements(Field(root, "choices")))
        {
            if (choice.ValueKind != JsonValueKind.Object) continue;
            choiceIndex++;
            string finish = String(Field(choice, "finish_reason"));
            if (finish != "") FinishReason = finish;
            var d = Field(choice, "delta");
            if (d.ValueKind != JsonValueKind.Object) continue;
            string role = String(Field(d, "role"));
            if (role != "") Role = role;
            Append(d, "content", Content); Append(d, "reasoning_content", Reasoning); Append(d, "refusal", Refusal);
            foreach (var fragment in Elements(Field(d, "tool_calls")))
            {
                if (fragment.ValueKind != JsonValueKind.Object) continue;
                int index = (int)Integer(Field(fragment, "index"));
                if (!tools.TryGetValue(index, out var tool))
                {
                    if (512 > limit - retained) throw new ConversionFailure("output_budget_exceeded");
                    budget?.Add(512);retained += 512; tool = new() { Index = index }; tools.Add(index, tool); Calls.Add(tool);
                }
                string toolId = String(Field(fragment, "id")), type = String(Field(fragment, "type"));
                if (toolId != "") { Reserve(toolId); tool.Id = toolId; }
                if (type != "") tool.Type = type;
                var fn = Field(fragment, "function");
                string arguments = "";
                if (fn.ValueKind == JsonValueKind.Object)
                {
                    string name = String(Field(fn, "name")); arguments = String(Field(fn, "arguments"));
                    if (name != "") { Reserve(name); tool.Name = name; }
                    Reserve(arguments); tool.Arguments.Append(arguments);
                    if ((tool.Id == "" || tool.Name == "") && tool.Arguments.Length > 1 << 20) throw new ConversionFailure("pending_tool_limit");
                }
                // Only the first Responses delta needs the accumulated prefix.
                // Capture its boundary, including when this tool appears again
                // in the same chunk, without copying every preceding fragment.
                Changes.Add(new(choiceIndex, index, tool.Id, tool.Name, arguments, tool.Arguments.Length));
            }
        }
    }
    internal string ArgumentsThrough(ToolChange change) => tools[change.Index].Arguments.ToString(0, change.AccumulatedLength);
    public void Validate()
    {
        if (FinishReason == "") throw new ConversionFailure("stream_closed_without_finish");
        if (Calls.Any(c => c.Id == "" || c.Name == "")) throw new ConversionFailure("incomplete_tool_identity");
    }
    public JsonObject Completion()
    {
        Validate();
        var message = new JsonObject { ["role"] = Role, ["content"] = Content.ToString() };
        if (Refusal.Length > 0) message["refusal"] = Refusal.ToString();
        if (Reasoning.Length > 0) message["reasoning_content"] = Reasoning.ToString();
        if (Calls.Count > 0) message["tool_calls"] = new JsonArray(Calls.Select(t => (JsonNode)new JsonObject
        {
            ["id"] = t.Id,
            ["type"] = t.Type,
            ["index"] = t.Index,
            ["function"] = new JsonObject { ["name"] = t.Name, ["arguments"] = t.Arguments.ToString() }
        }).ToArray());
        var result = new JsonObject
        {
            ["id"] = Id == "" ? "chatcmpl-" + Guid.NewGuid().ToString("N") : Id,
            ["object"] = "chat.completion",
            ["created"] = Created == 0 ? DateTimeOffset.UtcNow.ToUnixTimeSeconds() : Created,
            ["model"] = Model,
            ["choices"] = new JsonArray(new JsonObject { ["index"] = 0, ["message"] = message, ["finish_reason"] = FinishReason })
        };
        if (Usage is not null) result["usage"] = Usage.DeepClone(); return result;
    }
}
