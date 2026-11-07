using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using System.Security.Cryptography;
using StreamingMigration;
using static OpenAiProtocols.OpenWire;

namespace OpenAiProtocols;

public static class ResponsesRequestAdapter
{
    public static OpenAiRequest Prepare(ReadOnlyMemory<byte> bytes, byte[] reasoningKey)=>Prepare(bytes,reasoningKey,null);
    public static OpenAiRequest Prepare(ReadOnlyMemory<byte> bytes, byte[] reasoningKey,Func<JsonObject,bool>? compatibility,Action<JsonElement>? inspectInput=null)
    {
        using var document=ParseRequest(bytes);var root = document.Root; string model = Model(root);
        inspectInput?.Invoke(document.Original);
        bool encrypted = Items(root["include"]).Any(x => Text(x) == "reasoning.encrypted_content");
        foreach (var item in Items(root["input"]).OfType<JsonObject>())
        {
            if (Text(item["type"]) != "reasoning" || !Text(item["encrypted_content"]).StartsWith("wbg-r1.", StringComparison.Ordinal)) continue;
            try { item["content"] = new JsonArray(new JsonObject { ["type"] = "reasoning_text", ["text"] = new ReasoningCodec(reasoningKey, model, "responses").Open(Text(item["encrypted_content"])) }); }
            catch (Exception e) when (e is CryptographicException or FormatException) { throw new OpenAiFailure("invalid_request", "无法恢复网关推理状态：签名无效，请使用原 API Key 和模型回放原始历史"); }
        }
        Validate(root);
        var messages = new JsonArray();
        if (!string.IsNullOrWhiteSpace(Text(root["instructions"]))) messages.Add((JsonNode?)new JsonObject { ["role"] = "system", ["content"] = Text(root["instructions"]) });
        if (root["input"] is JsonValue input && IsString(input))
        { if (HasText(input)) messages.Add((JsonNode?)new JsonObject { ["role"] = "user", ["content"] = BorrowScalar(input) }); }
        else
        {
            JsonObject? assistant = null; string reasoning = "";
            void Flush() { if (assistant is not null) messages.Add((JsonNode?)assistant); assistant = null; reasoning = ""; }
            foreach (var raw in Items(root["input"]))
            {
                if (raw is JsonValue value && IsString(value)) { Flush(); messages.Add((JsonNode?)new JsonObject { ["role"] = "user", ["content"] = BorrowScalar(value) }); continue; }
                if (raw is not JsonObject item) continue;
                string type = Text(item["type"]);
                if (type == "reasoning")
                {
                    if (assistant is not null) Flush();
                    string thought = ReasoningText(item); if (thought != "") reasoning += (reasoning == "" ? "" : "\n\n") + thought;
                    continue;
                }
                if (type == "web_search_call") continue;
                foreach (var message in ConvertItem(item))
                {
                    if (Text(message["role"]) == "assistant")
                    {
                        if (assistant is null)
                        {
                            assistant = message;
                            if (reasoning != "" && !assistant.ContainsKey("reasoning_content")) assistant["reasoning_content"] = reasoning;
                        }
                        else
                        {
                            bool Payload(JsonNode? n) => n is JsonArray { Count: > 0 } || !string.IsNullOrWhiteSpace(Text(n));
                            if (Payload(message["content"]))
                            {
                                if (!Payload(assistant["content"])) assistant["content"] = Copy(message["content"]);
                                else
                                {
                                    var parts = Parts(assistant["content"]); foreach (var part in Parts(message["content"])) parts.Add(Copy(part)); assistant["content"] = parts;
                                }
                            }
                            if (message["tool_calls"] is JsonArray calls)
                            {
                                if (assistant["tool_calls"] is not JsonArray) assistant["tool_calls"] = new JsonArray();
                                foreach (var call in calls) ((JsonArray)assistant["tool_calls"]!).Add(Copy(call));
                            }
                        }
                    }
                    else { Flush(); messages.Add((JsonNode?)message); }
                }
            }
            Flush();
        }
        Require(messages.Count > 0, "input 为空：Responses 请求必须提供 input 或 instructions");
        var chat = new JsonObject { ["model"] = model, ["messages"] = messages, ["stream"] = true };
        foreach (string name in new[] { "temperature", "top_p", "parallel_tool_calls", "service_tier", "prompt_cache_key", "prompt_cache_retention", "safety_identifier", "user" })
            if (root[name] is not null) chat[name] = Copy(root[name]);
        if (root["max_output_tokens"] is not null) chat["max_tokens"] = Copy(root["max_output_tokens"]);
        if (root["text"] is JsonObject output)
        {
            if (output["verbosity"] is not null) chat["verbosity"] = Copy(output["verbosity"]);
            if (output["format"] is JsonObject format) chat["response_format"] = Copy(format);
        }
        if (root["reasoning"] is JsonObject reason)
        {
            string effort = Text(reason["effort"]); if (effort is not ("" or "none")) chat["reasoning_effort"] = effort;
            if (Text(reason["summary"]) != "") chat["reasoning_summary"] = Text(reason["summary"]);
        }
        var (tools, map) = ResponsesTools.Convert(root["tools"]); if (tools.Count > 0) chat["tools"] = tools;
        var choice = root["tool_choice"];
        if (Text(choice) is "auto" or "none" or "required") chat["tool_choice"] = Copy(choice);
        else if (choice is JsonObject selected && Text(selected["type"]) is "function" or "custom")
        {
            string name = Text(selected["name"]); if (name == "" && selected["function"] is JsonObject fn) name = Text(fn["name"]);
            name = ResponsesTools.Flatten(Text(selected["namespace"]), name);
            if (name != "")
            {
                var matching = tools.OfType<JsonObject>().Where(t => Text(t["function"]?["name"]) == name).Select(t => t.DeepClone()).ToArray();
                Require(matching.Length > 0, "tool_choice 指定的工具未在 tools 中定义"); chat["tool_choice"] = "required"; chat["tools"] = new JsonArray(matching);
            }
        }
        bool compatible=compatibility?.Invoke(chat)??false;
        return new(model, Bool(root["stream"]), Encode(chat, RequestLimit,optimizeLarge:bytes.Length>=32768), ClientProtocol.Responses, map, (JsonObject?)Copy(root["metadata"]) ?? new(), encrypted){OutboundCompatible=compatible};
    }
    static JsonArray Parts(JsonNode? content) => content is JsonArray arr ? (JsonArray)arr.DeepClone() : Text(content) != "" ? new JsonArray(new JsonObject { ["type"] = "text", ["text"] = Text(content) }) : [];
    public static string ReasoningText(JsonObject item)
    {
        string Join(JsonNode? parts) => string.Join("\n\n", Items(parts).OfType<JsonObject>().Select(p => Text(p["text"])).Where(t => t != ""));
        string text = Join(item["content"]); return text == "" ? Join(item["summary"]) : text;
    }
    static JsonNode Content(JsonNode? raw)
    {
        if (raw is null) return JsonValue.Create("")!;
        if (raw is JsonValue scalar) return BorrowScalar(scalar);
        var parts = new JsonArray();
        foreach (var part in Items(raw))
        {
            if (part is JsonValue value && IsString(value)) { parts.Add((JsonNode?)new JsonObject { ["type"] = "text", ["text"] = BorrowScalar(value) }); continue; }
            if (part is not JsonObject p) continue;
            string type = Text(p["type"]);
            if (type is "input_text" or "output_text" or "text" or "refusal")
            {
                string field = type == "refusal" ? "refusal" : "text";
                if (p[field] is JsonValue text && IsString(text)) parts.Add((JsonNode?)new JsonObject { ["type"] = "text", ["text"] = BorrowScalar(text) });
            }
            else if (type == "input_image" && Text(p["image_url"]) != "")
            {
                var image = new JsonObject { ["url"] = Text(p["image_url"]) }; if (p.ContainsKey("detail")) image["detail"] = Copy(p["detail"]);
                parts.Add((JsonNode?)new JsonObject { ["type"] = "image_url", ["image_url"] = image });
            }
        }
        return parts.Count == 0 ? JsonValue.Create("")! : parts;
    }
    static string Stringify(JsonNode? value) => value is null ? "" : value is JsonValue v && v.TryGetValue<string>(out var s) ? s : Canonical(value);
    static IEnumerable<JsonObject> ConvertItem(JsonObject item)
    {
        string type = Text(item["type"]);
        if (type is "function_call" or "custom_tool_call")
        {
            string args = type == "function_call" ? Text(item["arguments"]) : Canonical(new JsonObject { ["input"] = Text(item["input"]) });
            yield return new() { ["role"] = "assistant", ["content"] = null, ["tool_calls"] = new JsonArray(new JsonObject { ["id"] = Text(item["call_id"]), ["type"] = "function", ["function"] = new JsonObject { ["name"] = ResponsesTools.Flatten(Text(item["namespace"]), Text(item["name"])), ["arguments"] = args } }) };
            yield break;
        }
        if (type is "function_call_output" or "custom_tool_call_output")
        {
            if(item["output"] is JsonValue scalar&&scalar.GetValueKind()==JsonValueKind.String)
            {
                yield return new(){["role"]="tool",["tool_call_id"]=Text(item["call_id"]),["content"]=scalar.TryGetValue<JsonElement>(out var raw)?JsonValue.Create(raw):scalar.DeepClone()};
                yield break;
            }
            var output = item["output"]; var texts = new List<string>(); var images = new JsonArray();
            bool typed = Items(output).OfType<JsonObject>().Any(p => Text(p["type"]) is "input_text" or "output_text" or "text" or "input_image" or "image_url");
            if (!typed) texts.Add(Stringify(output));
            else foreach (var part in Items(output))
            {
                if (part is JsonObject p)
                {
                    string kind = Text(p["type"]);
                    if (kind is "input_text" or "output_text" or "text" && p["text"] is JsonValue v && v.TryGetValue<string>(out var s)) { texts.Add(s); continue; }
                    if (kind is "input_image" or "image_url")
                    {
                        string url = p["image_url"] is JsonObject nested ? Text(nested["url"]) : Text(p["image_url"]);
                        string detail = p["image_url"] is JsonObject n ? Text(n["detail"]) : Text(p["detail"]);
                        if (url != "") { var image = new JsonObject { ["url"] = url }; if (detail != "") image["detail"] = detail; images.Add((JsonNode?)new JsonObject { ["type"] = "image_url", ["image_url"] = image }); continue; }
                    }
                }
                texts.Add(Stringify(part));
            }
            string text = string.Join('\n', texts); if (text == "" && images.Count > 0) text = "[图片]";
            yield return new() { ["role"] = "tool", ["tool_call_id"] = Text(item["call_id"]), ["content"] = text };
            if (images.Count > 0) yield return new() { ["role"] = "user", ["content"] = images }; yield break;
        }
        var message = new JsonObject { ["role"] = Text(item["role"]), ["content"] = Content(item["content"]) };
        if (Text(item["role"]) == "assistant" && item["reasoning_content"] is JsonValue reasoning && reasoning.TryGetValue<string>(out _)) message["reasoning_content"] = Copy(reasoning);
        yield return message;
    }
    static void Validate(JsonObject r)
    {
        foreach (string key in new[] { "previous_response_id", "conversation" })
            Require(r[key] is null || r[key] is JsonValue v && v.TryGetValue<string>(out var s) && s == "", key + " 不受无状态网关支持：请在 input 中回放完整消息及 function_call/function_call_output 历史");
        Require(!Bool(r["store"]), "store=true 不受无状态网关支持：请使用 store=false 并由客户端回放历史");
        if (r["background"] is not null) { Require(r["background"] is JsonValue b && b.TryGetValue<bool>(out _), "background 必须是布尔值"); Require(!Bool(r["background"]), "background=true 不受当前同步 Chat 网关支持；请使用 background=false"); }
        if (r["prompt"] is not null) { Require(r["prompt"] is JsonObject, "prompt 必须是对象"); Require(((JsonObject)r["prompt"]!).Count == 0, "当前 Chat 网关不支持 prompt 模板；请在 instructions/input 中提供展开后的内容"); }
        if (r["truncation"] is not null) { Require(r["truncation"] is JsonValue t && t.TryGetValue<string>(out _), "truncation 必须是字符串"); Require(Text(r["truncation"]) is "" or "disabled", "当前 Chat 网关不支持自动 truncation；请使用 disabled 并由客户端管理历史长度"); }
        if (r["include"] is not null) { Require(r["include"] is JsonArray, "include 必须是字符串数组"); foreach (var include in Items(r["include"])) { Require(include is JsonValue v && v.TryGetValue<string>(out _), "include 必须是字符串数组"); Require(Text(include) == "reasoning.encrypted_content", "当前 Chat 网关的 include 仅支持 reasoning.encrypted_content"); } }
        if (r["text"] is not null)
        {
            Require(r["text"] is JsonObject, "text 必须是对象");
            if (r["text"]!["format"] is { } format)
            {
                Require(format is JsonObject, "text.format 必须是对象");
                if (format["type"] is { } kind) { Require(kind is JsonValue v && v.TryGetValue<string>(out _), "text.format.type 必须是字符串"); Require(Text(kind) is "" or "text", "当前 WorkBuddy Chat 上游不支持保证结构化输出约束的 text.format；请使用 type=text 或原生支持结构化输出的上游"); }
            }
        }
        if (r["metadata"] is { } metadata)
        {
            Require(metadata is JsonObject, "metadata 必须是字符串键值对象"); Require(((JsonObject)metadata).Count <= 16, "metadata 最多允许 16 个键值对");
            foreach (var p in (JsonObject)metadata) Require(p.Key.EnumerateRunes().Count() <= 64 && p.Value is JsonValue v && v.TryGetValue<string>(out var s) && s.EnumerateRunes().Count() <= 512, "metadata 的键必须不超过 64 个字符，值必须是不超过 512 个字符的字符串");
        }
        Require(r["input"] is null or JsonArray || r["input"] is JsonValue value && IsString(value), "input 必须是字符串或消息数组");
        int index = -1;
        foreach (var raw in Items(r["input"]))
        {
            index++; if (raw is JsonValue v && IsString(v)) continue;
            Require(raw is JsonObject, $"input[{index}] 必须是消息对象"); var item = (JsonObject)raw!; string type = Text(item["type"]), prefix = $"input[{index}]: ";
            if (type is "" or "message")
            {
                Require(Text(item["role"]) is "system" or "developer" or "user" or "assistant", prefix + "message.role 必须是 system/developer/user/assistant");
                Require(item["content"] is null or JsonArray || item["content"] is JsonValue text && IsString(text), prefix + "message.content 必须是字符串或内容块数组");
                foreach (var part in Items(item["content"]))
                {
                    if (part is JsonValue partValue && IsString(partValue)) continue;
                    Require(part is JsonObject, prefix + "消息内容块结构非法"); var p = (JsonObject)part!; string kind = Text(p["type"]);
                    Require(kind is "input_text" or "output_text" or "text" or "refusal" or "input_image", prefix + "Chat 上游不支持此消息内容块类型");
                    if (kind == "input_image") Require(Text(p["image_url"]) != "", prefix + "Chat 上游要求 input_image.image_url，不支持仅含 file_id 的图片");
                }
            }
            else if (type == "reasoning") Require(item["encrypted_content"] is null || ReasoningText(item) != "", prefix + "Chat 上游无法回放仅含 encrypted_content 的 reasoning 项");
            else if (type is "function_call" or "custom_tool_call")
            {
                Require(Text(item["call_id"]) != "" && Text(item["name"]) != "", prefix + type + " 必须包含 call_id 和 name");
                string field = type == "function_call" ? "arguments" : "input";
                Require(item[field] is JsonValue s && s.TryGetValue<string>(out _), prefix + (type == "function_call" ? "function_call.arguments 必须是 JSON 字符串" : "custom_tool_call.input 必须是字符串"));
            }
            else if (type is "function_call_output" or "custom_tool_call_output") Require(Text(item["call_id"]) != "", prefix + type + " 必须包含 call_id");
            else Require(type == "web_search_call", prefix + "当前 Chat 上游不支持此 Responses 项类型；请回放完整文本/函数调用历史");
        }
    }
}
