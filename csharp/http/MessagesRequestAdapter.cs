using System.Globalization;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Encodings.Web;
using System.Security.Cryptography;
using StreamingMigration;
using TypedMigration;

namespace HttpMigration;

public sealed record PreparedMessages(string Model, bool Stream, bool Thinking, bool Hidden, byte[] UpstreamBody)
{public bool OutboundCompatible{get;init;}}

public static class MessagesRequestAdapter
{
    public const int DefaultLimit = 32 << 20;
    static readonly JsonDocumentOptions Document = new() { MaxDepth = 256 };
    static readonly JsonSerializerOptions EncodingOptions = new() { Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping, MaxDepth = 256 };
    static readonly GatewayJson NodeJson = new(EncodingOptions);
    static string Type(JsonElement root) => JsonFields.Text(JsonFields.Get(root, "thinking"), "type").Trim().ToLowerInvariant();
    public static PreparedMessages Prepare(ReadOnlyMemory<byte> bytes, byte[] reasoningKey, int limit = DefaultLimit, SystemPromptOptions? prompts = null,Action<JsonElement>? inspectInput=null,Func<JsonObject,bool>? compatibility=null,Func<JsonObject,byte[]?>? prepareBody=null)
    {
        if (bytes.Length > limit) throw new RequestFailure("请求体超过大小限制", 413, "request_too_large");
        try
        {
            using var document = JsonDocument.Parse(bytes, Document);
            var root = document.RootElement;
            if (root.ValueKind == JsonValueKind.Null) throw new RequestFailure("请求体必须是 JSON 对象");
            if (root.ValueKind != JsonValueKind.Object) throw new JsonException();
            inspectInput?.Invoke(root);
            // Go decodes known generation scalars to float64 before validation.
            foreach (var scalar in new[] { JsonFields.Get(root, "max_tokens"), JsonFields.Get(root, "temperature"), JsonFields.Get(root, "top_p"), JsonFields.Get(root, "top_k"), JsonFields.Get(JsonFields.Get(root, "thinking"), "budget_tokens") })
                if (scalar.ValueKind == JsonValueKind.Number && (!scalar.TryGetDouble(out double number) || !double.IsFinite(number))) throw new JsonException();
            var value = JsonFields.Get(root, "model");
            string model = value.ValueKind == JsonValueKind.Undefined || value.ValueKind == JsonValueKind.String && JsonFields.Text(value) == "" ? "hy4-preview" : JsonFields.Text(value);
            if (model.Length == 0 || Encoding.UTF8.GetByteCount(model) > 256 || model.EnumerateRunes().Any(r => Rune.IsWhiteSpace(r) || Rune.GetUnicodeCategory(r) is UnicodeCategory.Control or UnicodeCategory.Format))
                throw new RequestFailure("model 必须是 1～256 字节的有效 UTF-8 字符串，不能包含空白或控制字符");
            bool thinking = Type(root) is not ("" or "disabled");
            string display = JsonFields.Text(JsonFields.Get(root, "thinking"), "display");
            var displayValue = JsonFields.Get(JsonFields.Get(root, "thinking"), "display");
            if (JsonFields.Present(displayValue) && (displayValue.ValueKind != JsonValueKind.String || display is not ("" or "summarized" or "omitted" or "updates")))
                throw new RequestFailure("thinking.display 必须是 summarized、omitted 或 updates");
            bool hidden = thinking && display is "omitted" or "updates";
            JsonObject? body = null;
            bool references = false, signatures = false;
            foreach (var message in JsonFields.Items(JsonFields.Get(root, "messages")))
                foreach (var block in JsonFields.Items(JsonFields.Get(message, "content")))
                {
                    if (JsonFields.Text(block, "type") == "thinking" && JsonFields.Text(block, "signature").StartsWith("wbg-r1.", StringComparison.Ordinal)) signatures = true;
                    if (JsonFields.Text(block, "type") == "tool_result")
                        foreach (var part in JsonFields.Items(JsonFields.Get(block, "content")))
                            if (JsonFields.Text(part, "type") == "tool_reference") references = true;
                }
            if (signatures || references)
            {
                body = CopyObject(root);
                RestoreReasoning(body, reasoningKey, model);
                root = JsonSerializer.SerializeToElement(body, NodeJson.JsonObject);
            }
            RequestValidation.Capabilities(root);
            if (references)
            {
                NormalizeReferences(body!, limit);
                root = JsonSerializer.SerializeToElement(body, NodeJson.JsonObject);
            }
            RequestValidation.Input(root); RequestValidation.Output(root);

            var typed=WireReader.ReadBorrowed(root);
            if(prompts is not null)typed=IdentityRewrite.BeforeMap(typed,root,prompts.Identity);
            var mapped = MessageMapper.Map(typed);
            var messages = mapped.Messages;
            if (mapped.SystemText is { } system) messages = messages.Insert(0, new TextMessage(MessageRole.Parse("system"), system));
            if (messages.IsEmpty) throw new RequestFailure("messages 字段缺失或为空");
            var raw = WireWriter.ToNode(mapped with { SystemText = null, Messages = messages });
            raw["model"] = model;
            if (raw["tools"] is JsonArray tools && tools.Count == 0) raw.Remove("tools");
            foreach (string name in new[] { "temperature", "top_p", "top_k" })
                if (JsonFields.Get(root, name).ValueKind == JsonValueKind.Number)
                {
                    double number = Number(JsonFields.Get(root, name)); raw[name] = number;
                }
            var max = JsonFields.Get(root, "max_tokens");
            if (max.ValueKind == JsonValueKind.Number && Number(max) > 0) raw["max_tokens"] = unchecked((long)Number(max));
            if (JsonFields.Get(root, "stop_sequences") is { ValueKind: JsonValueKind.Array } stop && stop.GetArrayLength() > 0) raw["stop"] = JsonNode.Parse(stop.GetRawText());
            var choice = JsonFields.Get(root, "tool_choice");
            if (choice.ValueKind == JsonValueKind.Object)
            {
                var disabled = JsonFields.Get(choice, "disable_parallel_tool_use");
                if (disabled.ValueKind is JsonValueKind.True or JsonValueKind.False) raw["parallel_tool_calls"] = !disabled.GetBoolean();
                string selected = JsonFields.Text(choice, "name");
                if (selected.Length > 0)
                {
                    var selectedTools = (raw["tools"] as JsonArray ?? []).Where(t => t is JsonObject o && o["function"] is JsonObject f && f["name"] is JsonValue n && n.TryGetValue<string>(out string? name) && name == selected).Select(t => t?.DeepClone()).ToArray();
                    if (selectedTools.Length == 0) throw new RequestFailure("tool_choice 指定的工具未在 tools 中定义");
                    raw["tools"] = new JsonArray(selectedTools);
                }
            }
            var th = JsonFields.Get(root, "thinking");
            string kind = Type(root);
            if (kind == "disabled") raw["reasoning_effort"] = "off";
            if (kind is "enabled" or "adaptive")
            {
                string effort = JsonFields.Text(th, "effort").Trim();
                double budget = JsonFields.Get(th, "budget_tokens").ValueKind == JsonValueKind.Number ? Number(JsonFields.Get(th, "budget_tokens")) : 0;
                raw["reasoning_effort"] = effort.Length != 0 ? effort : budget > 0 ? budget switch { >= 65536 => "max", >= 16384 => "high", >= 4096 => "medium", _ => "low" } : "high";
            }
            string outputEffort = JsonFields.Text(JsonFields.Get(root, "output_config"), "effort").Trim();
            if (kind != "disabled" && outputEffort.Length > 0) raw["reasoning_effort"] = outputEffort;
            // Both local response modes consume an upstream SSE stream.
            raw["stream"] = true;
            byte[]? prepared=prepareBody?.Invoke(raw);
            bool compatible=prepareBody is not null||(compatibility?.Invoke(raw)??(prompts is not null&&OutboundPreparation.IsCompatible(raw,prompts)));
            if(prepared is not null)return new(model,JsonFields.Get(root,"stream").ValueKind==JsonValueKind.True,thinking,hidden,prepared){OutboundCompatible=compatible};
            var rawSizes=new Dictionary<JsonValue,int>();
            int size=Size(raw, limit,rawSizes); // Check the complete escaped size before allocating UTF-8 output.
            byte[] encoded = GoJson.FormatUtf8(raw,size,rawSizes);
            return new(model, JsonFields.Get(root, "stream").ValueKind == JsonValueKind.True, thinking, hidden, encoded){OutboundCompatible=compatible};
        }
        catch (JsonException) { throw new RequestFailure("无效的 JSON 请求体"); }
        catch (InvalidOperationException) { throw new RequestFailure("无效的 JSON 字段或 Unicode 文本"); }
    }
    static double Number(JsonElement e)
    {
        if (!e.TryGetDouble(out double n) || !double.IsFinite(n)) throw new RequestFailure("数值超出支持范围");
        return n;
    }
    // JsonObject's lazy materialization throws on duplicate properties. Go's
    // decoder uses the final value, including inside messages and schemas.
    // Scalar nodes borrow the input document only within Prepare; no node escapes.
    static JsonObject CopyObject(JsonElement value)
    {
        var result = new JsonObject(); foreach (var p in value.EnumerateObject()) result[p.Name] = CopyNode(p.Value); return result;
    }
    static JsonNode? CopyNode(JsonElement value) => value.ValueKind switch
    {
        JsonValueKind.Object => CopyObject(value),
        JsonValueKind.Array => new JsonArray(value.EnumerateArray().Select(CopyNode).ToArray()),
        JsonValueKind.Null => null,
        _ => JsonValue.Create(value)
    };
    static void RestoreReasoning(JsonObject root, byte[] key, string model)
    {
        var codec = new ReasoningCodec(key, model);
        if (root["messages"] is not JsonArray messages) return;
        foreach (var message in messages.OfType<JsonObject>())
            if (message["content"] is JsonArray content)
                foreach (var block in content.OfType<JsonObject>())
                    if (block["type"] is JsonValue type && type.TryGetValue<string>(out string? kind) && kind == "thinking" && block["signature"] is JsonValue signature && signature.TryGetValue<string>(out string? token) && token.StartsWith("wbg-r1.", StringComparison.Ordinal))
                    {
                        try { block["thinking"] = codec.Open(token); }
                        catch (Exception e) when (e is CryptographicException or FormatException) { throw new RequestFailure("无法恢复网关推理状态：签名无效，请使用原 API Key 和模型回放原始历史"); }
                    }
    }
    static string Canonical(JsonNode? node) => GoJson.Format(node);
    static string Text(JsonNode? node) => node is JsonValue value && value.TryGetValue<string>(out string? text) ? text : "";
    // Count Go's UTF-8/HTML escaping without allocating the expanded string.
    static int Size(JsonNode? node, int limit,Dictionary<JsonValue,int>? rawSizes=null)
    {
        int count = 0;
        void Add(int n) { if (n > limit - count) throw new RequestFailure("转换后的请求超过大小限制", 413, "request_too_large"); count += n; }
        void Quote(string text)
        {
            Add(GoJson.QuotedUtf8Size(text));
        }
        void Walk(JsonNode? value)
        {
            switch (value)
            {
                case null: Add(4); break;
                case JsonObject obj:
                    Add(2); int i = 0; foreach (var p in obj) { if (i++ > 0) Add(1); Quote(p.Key); Add(1); Walk(p.Value); }
                    break;
                case JsonArray list:
                    Add(2); for (int n = 0; n < list.Count; n++) { if (n > 0) Add(1); Walk(list[n]); }
                    break;
                case JsonValue scalar:
                    if(GoJson.TryRawString(scalar,out _,out int size)){Add(size);if(rawSizes is not null)rawSizes[scalar]=size;}
                    else if (scalar.TryGetValue<string>(out string? text)) Quote(text);
                    else Add(scalar.ToJsonString(EncodingOptions).Length);
                    break;
            }
        }
        Walk(node); return count;
    }
    static JsonObject? Function(JsonObject tool)
    {
        if (tool.ContainsKey("function")) return tool["function"] as JsonObject;
        var fn = new JsonObject { ["name"] = Text(tool["name"]) };
        if (tool["description"] is JsonValue d && d.TryGetValue<string>(out string? description) && description.Length > 0) fn["description"] = description;
        if (tool["input_schema"] is { } schema) fn["parameters"] = schema.DeepClone();
        if (tool["strict"] is JsonValue strict && strict.TryGetValue<bool>(out bool flag)) fn["strict"] = flag;
        return fn;
    }
    static void NormalizeReferences(JsonObject root, int limit)
    {
        Dictionary<string, List<JsonObject>>? definitions = null;
        var replacements = new List<(JsonArray Parent, int Index, JsonObject Value)>();
        var emitted = new HashSet<string>(); int total = -1;
        var messages = root["messages"] as JsonArray ?? [];
        for (int i = 0; i < messages.Count; i++)
        {
            if (messages[i] is not JsonObject message || Text(message["role"]) != "user" || message["content"] is not JsonArray blocks) continue;
            for (int b = 0; b < blocks.Count; b++)
            {
                if (blocks[b] is not JsonObject result || Text(result["type"]) != "tool_result" || result["content"] is not JsonArray content) continue;
                for (int k = 0; k < content.Count; k++)
                {
                    if (content[k] is not JsonObject reference || Text(reference["type"]) != "tool_reference") continue;
                    string path = $"messages[{i}].content[{b}].content[{k}]", name = Text(reference["tool_name"]);
                    if (name.Length == 0) throw new RequestFailure(path + ".tool_name 必须是非空字符串");
                    if (definitions is null)
                    {
                        definitions = [];
                        foreach (var tool in (root["tools"] as JsonArray ?? []).OfType<JsonObject>())
                        {
                            var fn = Function(tool); string toolName = fn is null ? "" : Text(fn["name"]);
                            if (toolName.Length > 0 && fn is not null) { if (!definitions.ContainsKey(toolName)) definitions[toolName] = []; definitions[toolName].Add(fn); }
                        }
                    }
                    if (!definitions.TryGetValue(name, out var found) || found.Count != 1) throw new RequestFailure(path + ".tool_name 必须引用 tools 中唯一声明的工具");
                    if (total < 0) total = Size(root, limit);
                    bool first = emitted.Add(name);
                    if (first) Size(found[0], limit);
                    string text = first ? "\n[Referenced tool definition]\n" + Canonical(found[0]) + "\n" : "\n[Referenced tool]\n" + name + "\n";
                    var replacement = new JsonObject { ["type"] = "text", ["text"] = text };
                    int previous = Size(reference, limit), next = Size(replacement, limit);
                    if (next > limit - (total - previous)) throw new RequestFailure("工具引用展开超过请求大小限制", 413, "request_too_large");
                    total += next - previous; replacements.Add((content, k, replacement));
                }
            }
        }
        foreach (var r in replacements) r.Parent[r.Index] = r.Value;
    }
}
