using System.Text;
using System.Text.Encodings.Web;
using System.Text.Json;
using StreamingMigration;

namespace HttpMigration;

public sealed record AggregateResult(bool Complete, string Code, byte[]? Body, Usage Usage, string? RawUsageJson, bool ReceivedOutput = false);

// Aggregation consumes typed protocol events and only retains the non-streaming
// response. JSON tool arguments are written raw after the state validates them.
public sealed class MessageAggregate(long limit,IRetainedOutputBudget? budget=null) : IEventWriter<int>
{
    abstract class Slot;
    sealed class TextSlot(bool thinking) : Slot { public bool Thinking = thinking; public StringBuilder Text = new(); public string Signature = ""; }
    sealed class ToolSlot(string id, string name) : Slot { public string Id = id, Name = name; public StringBuilder Arguments = new(); }
    readonly SortedDictionary<int, Slot> slots = [];
    string id = "", stop = "";
    Usage usage = new(0, 0, 0, 0);
    long retained;
    bool complete;
    void Reserve(string value) { long n = Encoding.UTF8.GetByteCount(value); if (n > limit - retained) throw new ConversionFailure("output_budget_exceeded");budget?.Add(n); retained += n; }
    void Add(int index, Slot slot) { if (512 > limit - retained) throw new ConversionFailure("output_budget_exceeded");budget?.Add(512); retained += 512; slots.Add(index, slot); }
    public int Write(MessageStart e) { id = e.Id; return 0; }
    public int Write(TextStart e) { Add(e.Index, new TextSlot(false)); return 0; }
    public int Write(ThinkingStart e) { Add(e.Index, new TextSlot(true)); return 0; }
    public int Write(ToolStart e) { Reserve(e.Id.Value); Reserve(e.Name.Value); Add(e.Index, new ToolSlot(e.Id.Value, e.Name.Value)); return 0; }
    public int Write(TextDelta e) { Reserve(e.Text); ((TextSlot)slots[e.Index]).Text.Append(e.Text); return 0; }
    public int Write(ThinkingDelta e) { Reserve(e.Text); ((TextSlot)slots[e.Index]).Text.Append(e.Text); return 0; }
    public int Write(SignatureDelta e) { Reserve(e.Signature); ((TextSlot)slots[e.Index]).Signature = e.Signature; return 0; }
    public int Write(ToolDelta e) { Reserve(e.Arguments); ((ToolSlot)slots[e.Index]).Arguments.Append(e.Arguments); return 0; }
    public int Write(BlockStop e) => 0;
    public int Write(MessageDelta e) { stop = e.StopReason; usage = e.Usage; return 0; }
    public int Write(MessageStop e) { complete = true; return 0; }
    public int Write(StreamError e) => throw new ConversionFailure(e.Code);
    public byte[] Encode(string model)
    {
        if (!complete) throw new ConversionFailure("missing_finish");
        using var buffer = new BoundedMemory(limit,budget);
        using (var w = new Utf8JsonWriter(buffer, new JsonWriterOptions { Encoder = JavaScriptEncoder.UnsafeRelaxedJsonEscaping, MaxDepth = 10000 }))
        {
            w.WriteStartObject(); w.WriteString("id", id); w.WriteString("type", "message"); w.WriteString("role", "assistant"); w.WriteString("model", model);
            w.WriteStartArray("content");
            foreach (var slot in slots.Values)
            {
                w.WriteStartObject();
                switch (slot)
                {
                    case TextSlot t:
                        w.WriteString("type", t.Thinking ? "thinking" : "text"); w.WriteString(t.Thinking ? "thinking" : "text", t.Text.ToString());
                        if (t.Thinking) w.WriteString("signature", t.Signature);
                        break;
                    case ToolSlot t:
                        w.WriteString("type", "tool_use"); w.WriteString("id", t.Id); w.WriteString("name", t.Name); w.WritePropertyName("input");
                        w.WriteRawValue(t.Arguments.Length == 0 ? "{}" : t.Arguments.ToString()); break;
                }
                w.WriteEndObject();
            }
            w.WriteEndArray(); w.WriteString("stop_reason", stop); w.WriteNull("stop_sequence");
            w.WriteStartObject("usage"); w.WriteNumber("input_tokens", usage.Input); w.WriteNumber("output_tokens", usage.Output);
            w.WriteNumber("cache_read_input_tokens", usage.Cached); w.WriteNumber("cache_creation_input_tokens", usage.Created); w.WriteEndObject(); w.WriteEndObject();
        }
        return buffer.ToArray();
    }
    public static async Task<AggregateResult> RunAsync(Stream upstream, StreamOptions options, long outputLimit, CancellationToken cancellation)
    {
        await using (upstream)
        {
            // Non-streaming aggregation has its overall output budget rather
            // than the smaller streaming-only delayed-identity queue budget.
            var state = new MessageStreamState(options.Model, options.WantThinking, options.Hidden, options.RetainedLimit, (int)Math.Min(int.MaxValue, options.RetainedLimit),options.Budget);
            var accumulator = new MessageAggregate(outputLimit,options.Budget); var reader = new SseReader(upstream);
            bool receivedOutput = false;
            var restorers = new Dictionary<(int Choice, string Channel), ResponseIdentityRestorer>();
            try
            {
                while (await reader.NextAsync(cancellation) is { } chunk)
                {
                    if (chunk.HasOutput && !receivedOutput) options.FirstOutput?.Invoke();
                    receivedOutput |= chunk.HasOutput;
                    if (chunk.Choices.Any(c => c.Finish.Length > 0))
                    {
                        options.UpstreamCompleted?.Invoke();
                        await reader.ThrowIfFailedFinishAsync(chunk, cancellation);
                    }
                    if (options.ResponseRestore is { Active: true } plan) chunk = plan.RestoreChunk(chunk, restorers);
                    foreach (var e in state.Feed(chunk)) e.Accept(accumulator);
                }
                foreach (var e in state.Finish()) e.Accept(accumulator);
                return new(true, "", accumulator.Encode(state.Model), state.Usage, state.RawUsageJson, receivedOutput);
            }
            catch (ConversionFailure e) { return new(false, e.Code, null, state.Usage, e.RawUsageJson??state.RawUsageJson, receivedOutput); }
            catch (IOException) { return new(false, "read_error", null, state.Usage, state.RawUsageJson, receivedOutput); }
            catch (OperationCanceledException) when (cancellation.IsCancellationRequested) { return new(false, "cancelled", null, state.Usage, state.RawUsageJson, receivedOutput); }
        }
    }
    sealed class BoundedMemory(long limit,IRetainedOutputBudget? budget) : MemoryStream
    {
        void Check(int count) { if (count > limit - Length) throw new ConversionFailure("output_budget_exceeded");budget?.Scratch(2*(Length+count)); }
        public override void Write(byte[] buffer, int offset, int count) { Check(count); base.Write(buffer, offset, count); }
        public override void Write(ReadOnlySpan<byte> buffer) { Check(buffer.Length); base.Write(buffer); }
        public override void WriteByte(byte value) { Check(1); base.WriteByte(value); }
    }
}
