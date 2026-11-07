using System.Text.Json.Nodes;
using static OpenAiProtocols.OpenWire;

namespace OpenAiProtocols;

public static class ChatRequestAdapter
{
    public static OpenAiRequest Prepare(ReadOnlyMemory<byte> bytes)=>Prepare(bytes,null);
    public static OpenAiRequest Prepare(ReadOnlyMemory<byte> bytes,Func<JsonObject,bool>? compatibility)
    {
        using var document=ParseRequest(bytes);var root = document.Root; string model = Model(root);
        Require(root["messages"] is JsonArray { Count: > 0 }, "messages 字段缺失或为空");
        foreach (var raw in (JsonArray)root["messages"]!)
        {
            Require(raw is JsonObject, "messages 必须包含消息对象");
            var message = (JsonObject)raw!; string role = Text(message["role"]);
            Require(role is "user" or "assistant" or "system" or "developer" or "tool" or "function", "messages.role 无效");
            Require(message["content"] is null or JsonArray || message["content"] is JsonValue v && IsString(v), "messages.content 必须是字符串或内容块数组");
        }
        bool stream = Bool(root["stream"]); root["model"] = model; root["stream"] = true;
        bool compatible=compatibility?.Invoke(root)??false;
        bool explicitModel=root["model"] is JsonValue modelValue&&modelValue.TryGetValue<string>(out var rawModel)&&rawModel==model;
        if(stream&&explicitModel&&!document.HasDuplicates&&OpenWire.IsMinified(document.Raw.Span))
            return new(model,true,document.Raw.ToArray(),ClientProtocol.Chat,new Dictionary<string,ResponseTool>(),new()){OutboundCompatible=compatible};
        return new(model, stream, Encode(root, RequestLimit,optimizeLarge:bytes.Length>=32768), ClientProtocol.Chat, new Dictionary<string, ResponseTool>(), new()){OutboundCompatible=compatible};
    }
    // Normalize empty protocol noise while retaining extension fields and exact argument fragments.
    public static JsonObject Clean(JsonObject chunk)
    {
        foreach (var choice in Items(chunk["choices"]).OfType<JsonObject>())
        {
            if (choice["finish_reason"] is JsonValue reason && reason.TryGetValue<string>(out var s) && s == "") choice["finish_reason"] = null;
            if (choice["delta"] is not JsonObject delta) continue;
            foreach (var call in Items(delta["tool_calls"]).OfType<JsonObject>())
            {
                foreach (string name in new[] { "id", "type" }) if (call[name] is JsonValue v && v.TryGetValue<string>(out var value) && value == "") call.Remove(name);
                if (call["function"] is JsonObject fn && fn["name"] is JsonValue f && f.TryGetValue<string>(out var n) && n == "") fn.Remove("name");
            }
            foreach (var field in delta.ToArray())
            {
                bool empty = field.Value is null || field.Value is JsonArray { Count: 0 } || field.Value is JsonObject { Count: 0 } ||
                    field.Value is JsonValue value && value.TryGetValue<string>(out var text) && text == "";
                if (field.Key == "function_call" && field.Value is JsonObject legacy && string.IsNullOrWhiteSpace(Text(legacy["name"])) && string.IsNullOrWhiteSpace(Text(legacy["arguments"]))) empty = true;
                if (empty) delta.Remove(field.Key);
            }
        }
        return chunk;
    }
}
