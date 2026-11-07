using System.Collections.Immutable;
using System.Text.Json;

namespace TypedMigration;

// This is a compatibility adapter for the production helpers, not a replacement
// for the full gateway request validator. Unknown envelope fields stay harmless.
public static class WireReader
{
    static JsonElement Property(JsonElement e, string name) => e.ValueKind == JsonValueKind.Object && e.TryGetProperty(name, out var value) ? value : default;
    static string Text(JsonElement e) => e.ValueKind == JsonValueKind.String ? e.GetString()! : "";
    static string Text(JsonElement e, string name) => Text(Property(e, name));
    static IEnumerable<JsonElement> Items(JsonElement e) => e.ValueKind == JsonValueKind.Array ? e.EnumerateArray() : [];
    // JsonElements are confined to truly open schemas/arguments; clone them so
    // domain objects remain valid after the input document has been disposed.
    static JsonElement? Owned(JsonElement e) => e.ValueKind is JsonValueKind.Null or JsonValueKind.Undefined ? null : e.Clone();
    static ImageSource? Image(JsonElement element)
    {
        var source = Property(element, "source");
        switch (Text(source, "type")) {
            case "url": return new UrlImage(Text(source, "url"));
            case "base64":
                var media = Text(source, "media_type"); var data = Text(source, "data");
                return media != "" && data != "" ? new Base64Image(media, data) : null;
            default: return null;
        }
    }
    static Content ReadContent(JsonElement e,bool borrowToolResults=false) => e.ValueKind switch {
        JsonValueKind.String => new StringContent(Text(e)),
        JsonValueKind.Array => new BlocksContent(Items(e).Select(b=>ReadBlock(b,borrowToolResults)).ToImmutableArray()),
        JsonValueKind.Null or JsonValueKind.Undefined => new EmptyContent(),
        _ => new InvalidContent()
    };
    static Content ToolContent(JsonElement block,bool borrow)
    {
        var content=Property(block,"content");
        return borrow&&Property(block,"is_error").ValueKind!=JsonValueKind.True&&GoJson.TryRawString(content,out _,out _)
            ?new BorrowedTextContent(content):ReadContent(content);
    }
    static Block ReadBlock(JsonElement e,bool borrowToolResults) => Text(e, "type") switch {
        "text" => new TextBlock(Text(e, "text")),
        "image" => new ImageBlock(Image(e)),
        "tool_use" => new ToolUseBlock(new(Text(e, "id")), new(Text(e, "name")), Owned(Property(e, "input"))),
        "tool_result" => new ToolResultBlock(new(Text(e, "tool_use_id")), ToolContent(e,borrowToolResults), Property(e, "is_error").ValueKind == JsonValueKind.True),
        "thinking" => new ThinkingBlock(Property(e, "thinking").ValueKind == JsonValueKind.String ? Text(e, "thinking") : null),
        "redacted_thinking" => new RedactedBlock(),
        _ => new UnknownBlock()
    };
    public static InputRequest Read(ReadOnlyMemory<byte> input)
    {
        using var document = JsonDocument.Parse(input, new JsonDocumentOptions { MaxDepth = 256 });
        return Read(document.RootElement);
    }
    public static InputRequest Read(JsonElement root)=>Read(root,false);
    // The caller must keep root's JsonDocument alive through WireWriter encoding.
    // The normal Read overloads continue to produce independently owned models.
    public static InputRequest ReadBorrowed(JsonElement root)=>Read(root,true);
    static InputRequest Read(JsonElement root,bool borrowToolResults)
    {
        if (root.ValueKind != JsonValueKind.Object) throw new JsonException("object required");
        var system = Property(root, "system");
        var systemText = system.ValueKind == JsonValueKind.String ? Text(system) : string.Join("\n", Items(system).Where(b => Text(b, "type") == "text" && Text(b, "text") != "").Select(b => Text(b, "text")));
        var messages = Items(Property(root, "messages")).Where(m => m.ValueKind == JsonValueKind.Object)
            .Select(m => new InputMessage(MessageRole.Parse(Text(m, "role")), ReadContent(Property(m, "content"),borrowToolResults))).ToImmutableArray();
        var tools = ImmutableArray.CreateBuilder<InputTool>();
        foreach (var tool in Items(Property(root, "tools"))) {
            if (tool.ValueKind != JsonValueKind.Object) continue;
            if (tool.TryGetProperty("function", out _)) { tools.Add(new PassthroughTool(tool.Clone())); continue; }
            var description = Text(tool, "description"); var strict = Property(tool, "strict");
            tools.Add(new FunctionTool(new(Text(tool, "name")), description == "" ? null : description,
                Owned(Property(tool, "input_schema")), strict.ValueKind is JsonValueKind.True or JsonValueKind.False ? strict.GetBoolean() : null));
        }
        var choice = Property(root, "tool_choice"); string? selection = null;
        if (choice.ValueKind == JsonValueKind.String) selection = Text(choice);
        if (choice.ValueKind == JsonValueKind.Object) selection = Text(choice, "name") != "" ? "required" : Text(choice, "type").ToLowerInvariant() switch { "none" => "none", "any" or "required" => "required", _ => "auto" };
        return new(systemText == "" ? null : systemText, messages, Property(root, "tools").ValueKind == JsonValueKind.Array, tools.ToImmutable(), selection);
    }
}
