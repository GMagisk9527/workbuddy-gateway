using System.Collections.Frozen;
using System.Text.Json.Nodes;
using static OpenAiProtocols.OpenWire;

namespace OpenAiProtocols;

internal static class ResponsesTools
{
    public static string Flatten(string ns, string name) => string.IsNullOrWhiteSpace(ns) ? name : ns + "__" + name;
    public static (JsonArray Tools, IReadOnlyDictionary<string, ResponseTool> Map) Convert(JsonNode? source)
    {
        var tools = new JsonArray(); var map = new Dictionary<string, ResponseTool>(StringComparer.Ordinal);
        void Add(string ns, JsonObject raw, string kind)
        {
            string name = Text(raw["name"]).Trim(), description = Text(raw["description"]); var parameters = raw["parameters"]; var strict = raw["strict"];
            if (kind == "custom")
            {
                if (name == "") name = "custom_tool";
                if (raw["format"] is not null) description += (description == "" ? "" : "\n") + "Input format: " + Canonical(raw["format"]);
                parameters = new JsonObject { ["type"] = "object", ["properties"] = new JsonObject { ["input"] = new JsonObject { ["type"] = "string", ["description"] = "Raw input for " + name + "." } }, ["required"] = new JsonArray("input"), ["additionalProperties"] = false };
                strict = null;
            }
            if (name == "") return;
            string upstream = Flatten(ns, name);
            Require(map.TryAdd(upstream, new(name, ns, kind == "custom" ? ResponseToolKind.Custom : kind == "web_search" ? ResponseToolKind.WebSearch : ns == "" ? ResponseToolKind.Function : ResponseToolKind.Namespace)),
                "tools 中的工具名称在转换后重名；请为函数、自定义工具及命名空间子工具使用互不冲突的名称");
            var fn = new JsonObject { ["name"] = upstream };
            if (description != "") fn["description"] = description;
            if (parameters is not null) fn["parameters"] = Copy(parameters);
            if (strict is not null) fn["strict"] = Copy(strict);
            tools.Add(new JsonObject { ["type"] = "function", ["function"] = fn });
        }
        foreach (var tool in Items(source).OfType<JsonObject>())
        {
            string kind = Text(tool["type"]);
            if (kind is "" or "function") Add("", Text(tool["name"]) == "" && tool["function"] is JsonObject fn ? fn : tool, "function");
            else if (kind == "custom") Add("", tool, kind);
            else if (kind == "namespace")
            {
                string ns = Text(tool["name"]).Trim(); if (ns == "") continue;
                foreach (var child in Items(tool["tools"]).OfType<JsonObject>()) Add(ns, child, Text(child["type"]) == "custom" ? "custom" : "function");
            }
            else if (kind == "web_search") Add("", new()
            {
                ["name"] = "web_search",
                ["description"] = "Search the web and return results. Provide a concise query.",
                ["parameters"] = new JsonObject { ["type"] = "object", ["properties"] = new JsonObject { ["query"] = new JsonObject { ["type"] = "string", ["description"] = "Search query." } }, ["required"] = new JsonArray("query") }
            }, kind);
        }
        return (tools, map.ToFrozenDictionary());
    }
    public static JsonObject Call(string callId, string upstream, string arguments, IReadOnlyDictionary<string, ResponseTool> map)
    {
        map.TryGetValue(upstream, out var meta); bool custom = meta?.Kind == ResponseToolKind.Custom;
        var result = new JsonObject { ["id"] = (custom ? "ctc_" : "fc_") + Guid.NewGuid().ToString("N"), ["type"] = custom ? "custom_tool_call" : "function_call", ["status"] = "completed", ["call_id"] = callId, ["name"] = custom ? meta!.Name : upstream };
        if (meta?.Namespace is { Length: > 0 } ns) { result["name"] = meta.Name; result["namespace"] = ns; }
        if (custom)
        {
            string input = arguments;
            try { var obj = JsonNode.Parse(arguments); if (obj is JsonObject o && o["input"] is JsonValue v && v.TryGetValue<string>(out var s)) input = s; } catch (System.Text.Json.JsonException) { }
            result["input"] = input;
        }
        else result["arguments"] = arguments;
        return result;
    }
}
