using System.Text.Encodings.Web;
using System.Text.Json;
using System.Text.Json.Nodes;

namespace TypedMigration;

public static class WireWriter
{
    public static JsonObject ToNode(ChatRequest request)
    {
        JsonObject Message(ChatMessage m) => m switch
        {
            TextMessage t => new() { ["role"] = t.Role.WireName, ["content"] = t.Text },
            ToolMessage t => new() { ["role"] = "tool", ["tool_call_id"] = t.Id.Value, ["content"] = t.Text },
            BorrowedToolMessage t => new() { ["role"] = "tool", ["tool_call_id"] = t.Id.Value, ["content"] = JsonValue.Create(t.Text) },
            UserPartsMessage u => new() { ["role"] = "user", ["content"] = new JsonArray(u.Parts.Select(p => (JsonNode)(p switch
            {
                ChatText t => new JsonObject { ["type"] = "text", ["text"] = t.Text },
                ChatImage i => new JsonObject { ["type"] = "image_url", ["image_url"] = new JsonObject { ["url"] = i.Url } },
                _ => throw new NotSupportedException("part")
            })).ToArray()) },
            AssistantMessage a => Assistant(a),
            _ => throw new NotSupportedException("message")
        };
        JsonObject Assistant(AssistantMessage a)
        {
            var node = new JsonObject { ["role"] = "assistant", ["content"] = a.Text };
            if (!a.Thoughts.IsEmpty) node["reasoning_content"] = string.Join("\n\n", a.Thoughts);
            if (!a.Calls.IsEmpty) node["tool_calls"] = new JsonArray(a.Calls.Select(c => (JsonNode)new JsonObject { ["id"] = c.Id.Value, ["type"] = "function", ["function"] = new JsonObject { ["name"] = c.Name.Value, ["arguments"] = c.Arguments } }).ToArray());
            return node;
        }
        JsonNode Tool(InputTool t)
        {
            if (t is PassthroughTool raw) return JsonNode.Parse(GoJson.Format(raw.Shape))!;
            if (t is not FunctionTool f) throw new NotSupportedException("tool");
            var function = new JsonObject { ["name"] = f.Name.Value };
            if (f.Description is not null) function["description"] = f.Description;
            if (f.Schema is { } s) function["parameters"] = JsonNode.Parse(GoJson.Format(s));
            if (f.Strict is { } strict) function["strict"] = strict;
            return new JsonObject { ["type"] = "function", ["function"] = function };
        }
        var result = new JsonObject { ["messages"] = request.Messages.IsEmpty ? null : new JsonArray(request.Messages.Select(m => (JsonNode)Message(m)).ToArray()) };
        if (request.SystemText is { } system) result["system"] = system;
        if (request.HasTools) result["tools"] = new JsonArray(request.Tools.Select(Tool).ToArray());
        if (request.ToolChoice is { } choice) result["tool_choice"] = choice;
        return result;
    }
    public static byte[] Write(ChatRequest request)
    {
        using var stream = new MemoryStream();
        using (var writer = new Utf8JsonWriter(stream, new JsonWriterOptions { Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping, MaxDepth = 512 })) {
            writer.WriteStartObject();
            if (request.SystemText is { } system) writer.WriteString("system", system);
            writer.WritePropertyName("messages");
            if (request.Messages.IsEmpty) writer.WriteNullValue();
            else { writer.WriteStartArray(); foreach (var message in request.Messages) WriteMessage(writer, message); writer.WriteEndArray(); }
            if (request.HasTools) {
                writer.WriteStartArray("tools");
                foreach (var tool in request.Tools) {
                    switch (tool) {
                        case PassthroughTool raw: raw.Shape.WriteTo(writer); break;
                        case FunctionTool function:
                            writer.WriteStartObject(); writer.WriteString("type", "function"); writer.WriteStartObject("function");
                            writer.WriteString("name", function.Name.Value);
                            if (function.Description is { } description) writer.WriteString("description", description);
                            if (function.Schema is { } schema) { writer.WritePropertyName("parameters"); schema.WriteTo(writer); }
                            if (function.Strict is { } strict) writer.WriteBoolean("strict", strict);
                            writer.WriteEndObject(); writer.WriteEndObject(); break;
                        default: throw new NotSupportedException("tool");
                    }
                }
                writer.WriteEndArray();
            }
            if (request.ToolChoice is { } choice) writer.WriteString("tool_choice", choice);
            writer.WriteEndObject();
        }
        return stream.ToArray();
    }
    static void WriteMessage(Utf8JsonWriter writer, ChatMessage message)
    {
        writer.WriteStartObject();
        switch (message) {
            case TextMessage text: writer.WriteString("role", text.Role.WireName); writer.WriteString("content", text.Text); break;
            case ToolMessage tool:
                writer.WriteString("role", "tool"); writer.WriteString("tool_call_id", tool.Id.Value); writer.WriteString("content", tool.Text); break;
            case BorrowedToolMessage tool:
                writer.WriteString("role","tool");writer.WriteString("tool_call_id",tool.Id.Value);writer.WritePropertyName("content");tool.Text.WriteTo(writer);break;
            case UserPartsMessage user:
                writer.WriteString("role", "user"); writer.WriteStartArray("content");
                foreach (var part in user.Parts) {
                    writer.WriteStartObject();
                    switch (part) {
                        case ChatText text: writer.WriteString("type", "text"); writer.WriteString("text", text.Text); break;
                        case ChatImage image:
                            writer.WriteString("type", "image_url"); writer.WriteStartObject("image_url"); writer.WriteString("url", image.Url); writer.WriteEndObject(); break;
                        default: throw new NotSupportedException("part");
                    }
                    writer.WriteEndObject();
                }
                writer.WriteEndArray(); break;
            case AssistantMessage assistant:
                writer.WriteString("role", "assistant"); writer.WriteString("content", assistant.Text);
                if (!assistant.Thoughts.IsEmpty) writer.WriteString("reasoning_content", string.Join("\n\n", assistant.Thoughts));
                if (!assistant.Calls.IsEmpty) {
                    writer.WriteStartArray("tool_calls");
                    foreach (var call in assistant.Calls) {
                        writer.WriteStartObject(); writer.WriteString("id", call.Id.Value); writer.WriteString("type", "function");
                        writer.WriteStartObject("function"); writer.WriteString("name", call.Name.Value); writer.WriteString("arguments", call.Arguments);
                        writer.WriteEndObject(); writer.WriteEndObject();
                    }
                    writer.WriteEndArray();
                }
                break;
            default: throw new NotSupportedException("message");
        }
        writer.WriteEndObject();
    }
}
