using System.Text;
using System.Text.Json;
using TypedMigration;

namespace StreamingMigration;

// Evolving this protocol requires every writer to implement the new variant.
public interface IEventWriter<T>
{
    T Write(MessageStart e); T Write(TextStart e); T Write(ThinkingStart e); T Write(ToolStart e);
    T Write(TextDelta e); T Write(ThinkingDelta e); T Write(SignatureDelta e); T Write(ToolDelta e);
    T Write(BlockStop e); T Write(MessageDelta e); T Write(MessageStop e); T Write(StreamError e);
}
public abstract record ProtocolEvent
{
    public virtual int? BlockIndex => null;
    public abstract T Accept<T>(IEventWriter<T> writer);
}
public abstract record BlockEvent(int Index) : ProtocolEvent { public override int? BlockIndex => Index; }
public sealed record MessageStart(string Id, string Model) : ProtocolEvent { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
public sealed record TextStart(int Index) : BlockEvent(Index) { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
public sealed record ThinkingStart(int Index) : BlockEvent(Index) { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
public sealed record ToolStart(int Index, ToolCallId Id, ToolName Name) : BlockEvent(Index) { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
public sealed record TextDelta(int Index, string Text) : BlockEvent(Index) { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
public sealed record ThinkingDelta(int Index, string Text) : BlockEvent(Index) { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
public sealed record SignatureDelta(int Index, string Signature) : BlockEvent(Index) { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
public sealed record ToolDelta(int Index, string Arguments) : BlockEvent(Index) { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
public sealed record BlockStop(int Index) : BlockEvent(Index) { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
public sealed record Usage(long Input, long Output, long Cached, long Created);
public sealed record MessageDelta(string StopReason, Usage Usage) : ProtocolEvent { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
public sealed record MessageStop : ProtocolEvent { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
public sealed record StreamError(string Code) : ProtocolEvent { public override T Accept<T>(IEventWriter<T> w) => w.Write(this); }
