using System.Collections.Immutable;
using System.Text.Json;

namespace TypedMigration;

// No JSON field-name lookups or mutable JSON DOM in the mapping core.
public static class MessageMapper
{
    public static string Arguments(JsonElement? input) => input is { } value ? GoJson.Format(value) : "{}";
    public static string ImageUrl(ImageSource? source) => source switch {
        UrlImage image => image.Url,
        Base64Image image => "data:" + image.MediaType + ";base64," + image.Data,
        null => "", _ => throw new NotSupportedException("image source")
    };
    static string ContentText(Content content) => content switch {
        StringContent text => text.Text,
        BorrowedTextContent text => text.Text.GetString()!,
        BlocksContent list => string.Concat(list.Blocks.OfType<TextBlock>().Select(t => t.Text)),
        _ => ""
    };
    public static ChatRequest Map(InputRequest request)
    {
        var messages = ImmutableArray.CreateBuilder<ChatMessage>();
        foreach (var message in request.Messages) {
            switch (message.Content) {
                case StringContent text: messages.Add(new TextMessage(message.Role, text.Text)); break;
                case EmptyContent: messages.Add(new TextMessage(message.Role, "")); break;
                case BlocksContent content:
                    switch (message.Role.Kind) {
                        case RoleKind.User:
                            var user = new UserCollector(); foreach (var block in content.Blocks) block.Accept(user);
                            messages.AddRange(user.Tools);
                            if (user.Parts.Count == 1 && user.Parts[0] is ChatText only) messages.Add(new TextMessage(message.Role, only.Text));
                            else if (user.Parts.Count != 0) messages.Add(new UserPartsMessage(user.Parts.ToImmutable()));
                            break;
                        case RoleKind.Assistant:
                            var assistant = new AssistantCollector(); foreach (var block in content.Blocks) block.Accept(assistant);
                            messages.Add(new AssistantMessage(string.Concat(assistant.Text), assistant.Thoughts.ToImmutable(), assistant.Calls.ToImmutable()));
                            break;
                        default:
                            var instructions = new InstructionCollector(); foreach (var block in content.Blocks) block.Accept(instructions);
                            if (instructions.Text.Count != 0) messages.Add(new TextMessage(message.Role, string.Concat(instructions.Text)));
                            break;
                    }
                    break;
            }
        }
        return new(request.SystemText, messages.ToImmutable(), request.HasTools, request.Tools, request.ToolChoice);
    }
    sealed class UserCollector : IBlockVisitor
    {
        public ImmutableArray<ChatPart>.Builder Parts { get; } = ImmutableArray.CreateBuilder<ChatPart>();
        public List<ChatMessage> Tools { get; } = [];
        public void Visit(TextBlock block) => Parts.Add(new ChatText(block.Text));
        public void Visit(ImageBlock block) { if (ImageUrl(block.Source) is { Length: > 0 } url) Parts.Add(new ChatImage(url)); }
        public void Visit(ToolResultBlock block)
        {
            if(!block.IsError&&block.Content is BorrowedTextContent raw){Tools.Add(new BorrowedToolMessage(block.Id,raw.Text));return;}
            string text = ContentText(block.Content); bool hasImage = false;
            if (block.Content is BlocksContent list)
                foreach (var image in list.Blocks.OfType<ImageBlock>())
                    if (ImageUrl(image.Source) is { Length: > 0 } url) { hasImage = true; Parts.Add(new ChatImage(url)); }
            if (text == "" && hasImage) text = "[图片]";
            if (block.IsError) text = text == "" ? "[Tool execution error]" : "[Tool execution error]\n" + text;
            Tools.Add(new ToolMessage(block.Id, text));
        }
        public void Visit(ToolUseBlock block) { }
        public void Visit(ThinkingBlock block) { }
        public void Visit(RedactedBlock block) { }
        public void Visit(UnknownBlock block) { }
    }
    sealed class AssistantCollector : IBlockVisitor
    {
        public List<string> Text { get; } = [];
        public ImmutableArray<string>.Builder Thoughts { get; } = ImmutableArray.CreateBuilder<string>();
        public ImmutableArray<ChatToolCall>.Builder Calls { get; } = ImmutableArray.CreateBuilder<ChatToolCall>();
        public void Visit(TextBlock block) { if (block.Text != "") Text.Add(block.Text); }
        public void Visit(ThinkingBlock block) { if (block.Text is { } text) Thoughts.Add(text); }
        public void Visit(ToolUseBlock block) => Calls.Add(new(block.Id, block.Name, Arguments(block.Input)));
        public void Visit(ImageBlock block) { }
        public void Visit(ToolResultBlock block) { }
        public void Visit(RedactedBlock block) { }
        public void Visit(UnknownBlock block) { }
    }
    sealed class InstructionCollector : IBlockVisitor
    {
        public List<string> Text { get; } = [];
        public void Visit(TextBlock block) { if (block.Text != "") Text.Add(block.Text); }
        public void Visit(ImageBlock block) { }
        public void Visit(ToolUseBlock block) { }
        public void Visit(ToolResultBlock block) { }
        public void Visit(ThinkingBlock block) { }
        public void Visit(RedactedBlock block) { }
        public void Visit(UnknownBlock block) { }
    }
}
