using System.Collections.Immutable;
using System.Text.Json;

namespace TypedMigration;

public enum RoleKind { User, Assistant, System, Developer, Other }
public sealed record MessageRole(RoleKind Kind, string Original)
{
    public static MessageRole Parse(string text) => new(text switch {
        "user" => RoleKind.User, "assistant" => RoleKind.Assistant,
        "system" => RoleKind.System, "developer" => RoleKind.Developer, _ => RoleKind.Other
    }, text);
    // Unnamed numeric enum values cannot come from Parse. Keep CS8509 for new
    // named roles so adding one requires an explicit wire mapping.
#pragma warning disable CS8524
    public string WireName => Kind switch {
        RoleKind.User => "user", RoleKind.Assistant => "assistant",
        RoleKind.System => "system", RoleKind.Developer => "developer", RoleKind.Other => Original
    };
#pragma warning restore CS8524
}
public sealed record ToolCallId
{
    public string Value { get; }
    public ToolCallId(string value) { ArgumentNullException.ThrowIfNull(value); Value = value; }
}
public sealed record ToolName
{
    public string Value { get; }
    public ToolName(string value) { ArgumentNullException.ThrowIfNull(value); Value = value; }
}
public abstract record Content;
public sealed record StringContent(string Text) : Content;
// Used only during a request document's lifetime, before byte serialization.
internal sealed record BorrowedTextContent(JsonElement Text) : Content;
public sealed record BlocksContent(ImmutableArray<Block> Blocks) : Content;
public sealed record EmptyContent : Content;
public sealed record InvalidContent : Content;

public interface IBlockVisitor
{
    void Visit(TextBlock block); void Visit(ImageBlock block);
    void Visit(ToolUseBlock block); void Visit(ToolResultBlock block);
    void Visit(ThinkingBlock block); void Visit(RedactedBlock block); void Visit(UnknownBlock block);
}
public abstract record Block { public abstract void Accept(IBlockVisitor visitor); }
public sealed record TextBlock(string Text) : Block { public override void Accept(IBlockVisitor v) => v.Visit(this); }
public sealed record ImageBlock(ImageSource? Source) : Block { public override void Accept(IBlockVisitor v) => v.Visit(this); }
public sealed record ToolUseBlock(ToolCallId Id, ToolName Name, JsonElement? Input) : Block { public override void Accept(IBlockVisitor v) => v.Visit(this); }
public sealed record ToolResultBlock(ToolCallId Id, Content Content, bool IsError) : Block { public override void Accept(IBlockVisitor v) => v.Visit(this); }
public sealed record ThinkingBlock(string? Text) : Block { public override void Accept(IBlockVisitor v) => v.Visit(this); }
public sealed record RedactedBlock : Block { public override void Accept(IBlockVisitor v) => v.Visit(this); }
public sealed record UnknownBlock : Block { public override void Accept(IBlockVisitor v) => v.Visit(this); }

public abstract record ImageSource;
public sealed record UrlImage(string Url) : ImageSource;
public sealed record Base64Image(string MediaType, string Data) : ImageSource;
public sealed record InputMessage(MessageRole Role, Content Content);
public abstract record InputTool;
public sealed record PassthroughTool(JsonElement Shape) : InputTool;
public sealed record FunctionTool(ToolName Name, string? Description, JsonElement? Schema, bool? Strict) : InputTool;
public sealed record InputRequest(string? SystemText, ImmutableArray<InputMessage> Messages,
    bool HasTools, ImmutableArray<InputTool> Tools, string? ToolChoice);

public abstract record ChatPart;
public sealed record ChatText(string Text) : ChatPart;
public sealed record ChatImage(string Url) : ChatPart;
public sealed record ChatToolCall(ToolCallId Id, ToolName Name, string Arguments);
public abstract record ChatMessage;
public sealed record TextMessage(MessageRole Role, string Text) : ChatMessage;
public sealed record UserPartsMessage(ImmutableArray<ChatPart> Parts) : ChatMessage;
public sealed record ToolMessage(ToolCallId Id, string Text) : ChatMessage;
internal sealed record BorrowedToolMessage(ToolCallId Id, JsonElement Text) : ChatMessage;
public sealed record AssistantMessage(string Text, ImmutableArray<string> Thoughts, ImmutableArray<ChatToolCall> Calls) : ChatMessage;
public sealed record ChatRequest(string? SystemText, ImmutableArray<ChatMessage> Messages,
    bool HasTools, ImmutableArray<InputTool> Tools, string? ToolChoice);
