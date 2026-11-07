using System.Globalization;
using System.Text.Json.Nodes;
using StreamingMigration;
using static OpenAiProtocols.OpenWire;

namespace OpenAiProtocols;

public static class ResponsesOutput
{
    public static string Status(string finish) => finish is "length" or "content_filter" ? "incomplete" : "completed";
    public static JsonObject Envelope(string id, string model, long created, JsonObject metadata) => new()
    {
        ["id"] = id,
        ["object"] = "response",
        ["created_at"] = created,
        ["completed_at"] = null,
        ["status"] = "in_progress",
        ["error"] = null,
        ["incomplete_details"] = null,
        ["instructions"] = null,
        ["max_output_tokens"] = null,
        ["model"] = model,
        ["output"] = new JsonArray(),
        ["parallel_tool_calls"] = true,
        ["previous_response_id"] = null,
        ["reasoning"] = new JsonObject { ["effort"] = null, ["summary"] = null },
        ["store"] = false,
        ["temperature"] = 1.0,
        ["text"] = new JsonObject { ["format"] = new JsonObject { ["type"] = "text" } },
        ["tool_choice"] = "auto",
        ["tools"] = new JsonArray(),
        ["top_p"] = 1.0,
        ["truncation"] = "disabled",
        ["usage"] = null,
        ["metadata"] = metadata.DeepClone()
    };
    public static JsonObject Usage(JsonObject raw)
    {
        long Number(JsonNode? n)
        {
            if (n is JsonValue v && v.TryGetValue<double>(out var d) && double.IsFinite(d) && d > long.MinValue && d < long.MaxValue) return (long)d;
            return Int(n);
        }
        long? Flexible(JsonNode? n)
        {
            string s = n is null ? "" : n is JsonValue v && v.TryGetValue<string>(out var t) ? t : n.ToJsonString();
            return double.TryParse(s, NumberStyles.Float, CultureInfo.InvariantCulture, out var d) && double.IsFinite(d) && d >= 0 && d < long.MaxValue ? (long)d : null;
        }
        long cached = 0;
        foreach (var value in new[] { raw["prompt_cache_hit_tokens"], raw["cache_read_input_tokens"], raw["cached_tokens"], (raw["prompt_tokens_details"] as JsonObject)?["cached_tokens"], (raw["input_tokens_details"] as JsonObject)?["cached_tokens"] })
            if (Flexible(value) is long c) { cached = c; if (c > 0) break; }
        long input = Number(raw["prompt_tokens"]), output = Number(raw["completion_tokens"]), total = Number(raw["total_tokens"]);
        long? Thought(JsonNode? n)
        {
            if (n is not JsonValue value || value.TryGetValue<string>(out _)) return null;
            string number = n.ToJsonString();
            return double.TryParse(number, NumberStyles.Float, CultureInfo.InvariantCulture, out var d) && double.IsFinite(d) && d >= 0 && d < long.MaxValue && Math.Truncate(d) == d ? (long)d : null;
        }
        long thought = Thought((raw["completion_tokens_details"] as JsonObject)?["reasoning_tokens"]) ?? Thought(raw["completion_thinking_tokens"]) ?? 0;
        return new()
        {
            ["input_tokens"] = input,
            ["input_tokens_details"] = new JsonObject { ["cached_tokens"] = cached },
            ["output_tokens"] = output,
            ["output_tokens_details"] = new JsonObject { ["reasoning_tokens"] = thought },
            ["total_tokens"] = total == 0 ? input + output : total
        };
    }
    public static JsonObject Reasoning(string id, string status, string text, ReasoningCodec? codec)
    {
        var item = new JsonObject { ["id"] = id, ["type"] = "reasoning", ["status"] = status, ["summary"] = new JsonArray(new JsonObject { ["type"] = "summary_text", ["text"] = text }) };
        if (codec is not null && text != "") item["encrypted_content"] = codec.Seal(text); return item;
    }
    public static JsonObject Message(string id, string status, string text, string refusal, bool includeEmpty = true)
    {
        var parts = new JsonArray();
        if (text != "" || refusal == "" && includeEmpty) parts.Add((JsonNode?)new JsonObject { ["type"] = "output_text", ["text"] = text, ["annotations"] = new JsonArray() });
        if (refusal != "") parts.Add((JsonNode?)new JsonObject { ["type"] = "refusal", ["refusal"] = refusal });
        return new() { ["id"] = id, ["type"] = "message", ["status"] = status, ["role"] = "assistant", ["content"] = parts };
    }
    public static JsonObject Complete(ChatState state, OpenAiRequest request, byte[] key)
    {
        state.Validate(); string status = Status(state.FinishReason); long now = DateTimeOffset.UtcNow.ToUnixTimeSeconds();
        var response = Envelope(state.Id == "" ? "resp_" + Guid.NewGuid().ToString("N") : "resp_" + state.Id.Replace("chatcmpl-", "", StringComparison.Ordinal), request.Model, state.Created > 0 && state.Created <= now ? state.Created : now, request.Metadata);
        var output = new JsonArray(); var codec = request.EncryptedReasoning ? new ReasoningCodec(key, request.Model, "responses") : null;
        if (state.Reasoning.Length > 0) output.Add((JsonNode?)Reasoning("rs_" + Guid.NewGuid().ToString("N"), status, state.Reasoning.ToString(), codec));
        if (state.Content.Length > 0 || state.Refusal.Length > 0 || state.Calls.Count == 0) output.Add((JsonNode?)Message("msg_" + Guid.NewGuid().ToString("N"), status, state.Content.ToString(), state.Refusal.ToString()));
        foreach (var tool in state.Calls) { var call = ResponsesTools.Call(tool.Id, tool.Name, tool.Arguments.ToString(), request.Tools); call["status"] = status; output.Add((JsonNode?)call); }
        response["status"] = status; response["output"] = output; if (status == "completed") response["completed_at"] = now;
        else response["incomplete_details"] = new JsonObject { ["reason"] = state.FinishReason == "length" ? "max_output_tokens" : "content_filter" };
        if (state.Usage is not null) response["usage"] = Usage(state.Usage); return response;
    }
}
