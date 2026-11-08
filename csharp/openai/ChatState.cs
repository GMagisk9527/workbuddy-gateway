using System.Text;
using System.Text.Json.Nodes;
using StreamingMigration;
using static OpenAiProtocols.OpenWire;

namespace OpenAiProtocols;

public sealed class ChatState(string model, long limit,IRetainedOutputBudget? budget=null)
{
    public sealed class Tool
    {
        public int Index; public string Id = "", Name = "", Type = "function"; public StringBuilder Arguments = new();
    }
    readonly Dictionary<int, Tool> tools = [];
    public sealed record ToolChange(int Choice, int Index, string Id, string Name, string Arguments, string Accumulated);
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
    public (JsonObject Raw, ChatChunk Chunk) Feed(string payload)
    {
        Changes.Clear();
        var raw = payload.Trim() == "null" ? new JsonObject() : Parse(Encoding.UTF8.GetBytes(payload));
        if (raw["usage"] is JsonObject evidence) { Reserve(evidence.ToJsonString()); Usage = (JsonObject)evidence.DeepClone(); }
        var parsed = ChunkReader.Parse(payload); ReceivedOutput |= parsed.HasOutput;
        if (Text(raw["id"]) != "") Id = Text(raw["id"]);
        if (Text(raw["model"]) != "") Model = Text(raw["model"]);
        if (raw["created"] is not null) Created = Int(raw["created"]);
        int choiceIndex = -1;
        foreach (var choice in Items(raw["choices"]).OfType<JsonObject>())
        {
            choiceIndex++;
            if (Text(choice["finish_reason"]) != "") FinishReason = Text(choice["finish_reason"]);
            if (choice["delta"] is not JsonObject d) continue;
            if (Text(d["role"]) != "") Role = Text(d["role"]);
            foreach (var (name, target) in new[] { ("content", Content), ("reasoning_content", Reasoning), ("refusal", Refusal) })
            { string text = Text(d[name]); Reserve(text); target.Append(text); }
            foreach (var fragment in Items(d["tool_calls"]).OfType<JsonObject>())
            {
                int index = (int)Int(fragment["index"]);
                if (!tools.TryGetValue(index, out var tool))
                {
                    if (512 > limit - retained) throw new ConversionFailure("output_budget_exceeded");
                    budget?.Add(512);retained += 512; tool = new() { Index = index }; tools.Add(index, tool); Calls.Add(tool);
                }
                string id = Text(fragment["id"]), type = Text(fragment["type"]);
                if (id != "") { Reserve(id); tool.Id = id; }
                if (type != "") tool.Type = type;
                if (fragment["function"] is JsonObject fn)
                {
                    string name = Text(fn["name"]), arguments = Text(fn["arguments"]);
                    if (name != "") { Reserve(name); tool.Name = name; }
                    Reserve(arguments); tool.Arguments.Append(arguments);
                    if ((tool.Id == "" || tool.Name == "") && tool.Arguments.Length > 1 << 20) throw new ConversionFailure("pending_tool_limit");
                }
                Changes.Add(new(choiceIndex, index, tool.Id, tool.Name, fragment["function"] is JsonObject function ? Text(function["arguments"]) : "", tool.Arguments.ToString()));
            }
        }
        return (raw, parsed);
    }
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
