using System.Text.Json;

namespace HttpMigration;

public sealed class RequestFailure(string message, int status = 400, string type = "invalid_request_error") : Exception(message)
{ public int Status { get; } = status; public string ErrorType { get; } = type; }

internal static class JsonFields
{
    public static JsonElement Get(JsonElement e, string name) => e.ValueKind == JsonValueKind.Object && e.TryGetProperty(name, out var v) ? v : default;
    public static string Text(JsonElement e) => e.ValueKind == JsonValueKind.String ? e.GetString() ?? "" : "";
    public static string Text(JsonElement e, string name) => Text(Get(e, name));
    public static bool Present(JsonElement e) => e.ValueKind is not (JsonValueKind.Undefined or JsonValueKind.Null);
    public static IEnumerable<JsonElement> Items(JsonElement e) => e.ValueKind == JsonValueKind.Array ? e.EnumerateArray() : [];
}

// Preserve the Go converter's validation order and compatibility for instruction
// roles. Unsupported hosted capabilities fail before any upstream request.
internal static class RequestValidation
{
    static void Require(bool condition, string error) { if (!condition) throw new RequestFailure(error); }
    public static void Capabilities(JsonElement root)
    {
        var management = JsonFields.Get(root, "context_management");
        if (JsonFields.Present(management))
        {
            Require(management.ValueKind == JsonValueKind.Object, "context_management 必须是对象");
            Require(management.EnumerateObject().All(p => p.Name == "edits"), "当前 Chat 网关仅支持 context_management 中保留全部 thinking 的无操作配置");
            var edits = JsonFields.Get(management, "edits");
            if (JsonFields.Present(edits))
            {
                Require(edits.ValueKind == JsonValueKind.Array, "context_management.edits 必须是数组");
                foreach (var edit in edits.EnumerateArray())
                {
                    Require(edit.ValueKind == JsonValueKind.Object, "context_management.edits 必须包含编辑对象");
                    Require(JsonFields.Text(edit, "type") == "clear_thinking_20251015" && JsonFields.Text(edit, "keep") == "all", "当前 Chat 网关不支持 context_management 的历史裁剪；仅支持 clear_thinking_20251015 且 keep=all，请由客户端管理历史");
                    Require(edit.EnumerateObject().All(p => p.Name is "type" or "keep"), "当前 Chat 网关不支持此 context_management 编辑选项");
                }
            }
        }
        var servers = JsonFields.Get(root, "mcp_servers");
        if (JsonFields.Present(servers))
        {
            Require(servers.ValueKind == JsonValueKind.Array, "mcp_servers 必须是数组");
            Require(servers.GetArrayLength() == 0, "当前 Chat 网关不支持托管 mcp_servers；请由客户端执行工具");
        }
        var container = JsonFields.Get(root, "container");
        if (JsonFields.Present(container)) Require(container.ValueKind == JsonValueKind.String && JsonFields.Text(container) == "", "当前 Chat 网关不支持托管 container；请由客户端执行工具");
        var tier = JsonFields.Get(root, "service_tier");
        if (JsonFields.Present(tier))
        {
            Require(tier.ValueKind == JsonValueKind.String, "service_tier 必须是字符串");
            Require(JsonFields.Text(tier) is "" or "auto", "当前 Chat 网关无法保证 Anthropic service_tier；请使用 auto");
        }
    }
    public static void Input(JsonElement root)
    {
        var messages = JsonFields.Get(root, "messages");
        Require(messages.ValueKind == JsonValueKind.Array && messages.GetArrayLength() > 0, "messages 字段必须是非空数组");
        var system = JsonFields.Get(root, "system");
        if (JsonFields.Present(system)) Content(system, "system", "system");
        int i = 0;
        foreach (var message in messages.EnumerateArray())
        {
            string role = JsonFields.Text(message, "role");
            Require(message.ValueKind == JsonValueKind.Object && role.Length != 0, $"messages[{i}] 必须是包含 role 的消息对象");
            Require(role is "user" or "assistant" or "system" or "developer", $"messages[{i}].role 必须是 user、assistant、system 或 developer");
            Content(JsonFields.Get(message, "content"), role, $"messages[{i}].content"); i++;
        }
        var tools = JsonFields.Get(root, "tools");
        if (!JsonFields.Present(tools)) return;
        Require(tools.ValueKind == JsonValueKind.Array, "tools 必须是数组"); i = 0;
        foreach (var tool in tools.EnumerateArray())
        {
            Require(tool.ValueKind == JsonValueKind.Object, $"tools[{i}] 必须是工具对象");
            if (tool.TryGetProperty("function", out var fn))
                Require(fn.ValueKind == JsonValueKind.Object && JsonFields.Text(fn, "name") != "", $"tools[{i}].function 缺少有效 name");
            else
            {
                Require(JsonFields.Text(tool, "type") is "" or "custom", $"tools[{i}] 的工具类型无法转为 Chat 自定义函数；当前不支持 Anthropic 托管或预定义工具");
                Require(JsonFields.Text(tool, "name") != "", $"tools[{i}] 缺少有效 name");
                if (tool.TryGetProperty("strict", out var strict)) Require(strict.ValueKind is JsonValueKind.True or JsonValueKind.False, $"tools[{i}].strict 必须是布尔值");
            }
            i++;
        }
    }
    static bool Image(JsonElement block)
    {
        var source = JsonFields.Get(block, "source");
        return JsonFields.Text(source, "type") switch
        {
            "url" => JsonFields.Text(source, "url") != "",
            "base64" => JsonFields.Text(source, "media_type") != "" && JsonFields.Text(source, "data") != "",
            _ => false
        };
    }
    static void Content(JsonElement content, string role, string path)
    {
        if (content.ValueKind == JsonValueKind.String) return;
        Require(content.ValueKind == JsonValueKind.Array && (role == "system" || content.GetArrayLength() > 0), $"{path} 必须是文本或非空内容块数组");
        int i = 0;
        foreach (var block in content.EnumerateArray())
        {
            Require(block.ValueKind == JsonValueKind.Object, $"{path}[{i}] 必须是内容块对象");
            string at = $"{path}[{i++}]", type = JsonFields.Text(block, "type"); bool valid = false;
            switch (type)
            {
                case "text": valid = JsonFields.Get(block, "text").ValueKind == JsonValueKind.String; break;
                case "image": valid = role is "user" or "tool" && Image(block); break;
                case "thinking": case "redacted_thinking": valid = role == "assistant"; break;
                case "tool_use":
                    if (role == "assistant" && JsonFields.Text(block, "id") != "" && JsonFields.Text(block, "name") != "")
                    {
                        Require(JsonFields.Get(block, "input").ValueKind == JsonValueKind.Object, $"{at}.input 必须是 JSON 对象"); valid = true;
                    }
                    break;
                case "tool_result":
                    if (role == "user" && JsonFields.Text(block, "tool_use_id") != "")
                    {
                        if (block.TryGetProperty("is_error", out var flag)) Require(flag.ValueKind is JsonValueKind.True or JsonValueKind.False, $"{at}.is_error 必须是布尔值");
                        var nested = JsonFields.Get(block, "content");
                        if (JsonFields.Present(nested) && !(nested.ValueKind == JsonValueKind.Array && nested.GetArrayLength() == 0)) Content(nested, "tool", at + ".content");
                        valid = true;
                    }
                    break;
            }
            Require(valid, $"{at} 的内容块类型或字段无法转换到 Chat；支持文本、图片及客户端工具调用/结果，不支持文档、搜索结果或托管工具块");
        }
    }
    public static void Output(JsonElement root)
    {
        var output = JsonFields.Get(root, "output_config");
        if (!JsonFields.Present(output)) return;
        Require(output.ValueKind == JsonValueKind.Object, "output_config 必须是对象");
        Require(!JsonFields.Present(JsonFields.Get(output, "format")), "当前 WorkBuddy Chat 上游不支持保证 JSON Schema 约束的 output_config.format；请移除此选项或使用原生支持结构化输出的上游");
    }
}
