using System.Security.Cryptography;
using System.Text;
using TypedMigration;

namespace StreamingMigration;

public sealed class MessageStreamState(string model, bool wantThinking, ReasoningCodec? hidden = null, long retainedLimit = 64L << 20, int pendingLimit = 1 << 20,IRetainedOutputBudget? budget=null)
{
    sealed class TextSlot(int index) { public int Index { get; } = index; public bool Open = true; public StringBuilder Buffer { get; } = new(); }
    sealed class ToolSlot(int index)
    {
        public int Index { get; } = index; public string Id = "", Name = "";
        public bool Open; public StringBuilder Arguments { get; } = new(); public int Sent;
    }
    readonly EventBufferWriter writer = new(budget is null ? null : budget.Scratch);
    readonly Dictionary<int, ToolSlot> tools = [];
    readonly List<int> toolOrder = [];
    readonly List<(ProtocolEvent Event, int Bytes)> pending = [];
    readonly Dictionary<int, int> pendingArguments = [];
    readonly string id = "msg_" + Guid.NewGuid().ToString("N");
    List<ProtocolEvent> output = [];
    TextSlot? text, thinking;
    string model = model, finish = "";
    int nextIndex, pendingIndex = -1, pendingBytes, pendingArgumentBytes;
    long retained, scratch;
    bool started, refused, completed;
    public Usage Usage { get; private set; } = new(0, 0, 0, 0);
    // Owned JSON text survives document disposal and includes upstream credit
    // evidence which must not be inferred from the normalized client token view.
    public string? RawUsageJson { get; private set; }
    public bool HasFinish => finish.Length != 0;
    public string Model => model;

    static int Bytes(string value) => Encoding.UTF8.GetByteCount(value);
    void Reserve(long n)
    {
        if (n < 0 || n > retainedLimit - retained) throw new ConversionFailure("output_budget_exceeded");
        budget?.Add(n);retained += n;
    }
    void Temporary(long n) { if (n > scratch) { Reserve(n - scratch); scratch = n; } }
    void Emit(ProtocolEvent e)
    {
        if (e.BlockIndex is int index && pendingIndex >= 0 && index > pendingIndex)
        {
            int size = e.Accept(writer).Length;
            if (size > pendingLimit - pendingBytes - pendingArgumentBytes) throw new ConversionFailure("pending_tool_limit");
            Reserve(size); pendingBytes += size; pending.Add((e, size));
        }
        else output.Add(e);
    }
    void FlushPending()
    {
        var waiting = pending.OrderBy(x => x.Event.BlockIndex).ToArray();
        pending.Clear();
        foreach (var p in waiting)
            if (pendingIndex < 0 || p.Event.BlockIndex < pendingIndex)
            { output.Add(p.Event); pendingBytes -= p.Bytes; retained -= p.Bytes;budget?.Remove(p.Bytes); }
            else pending.Add(p);
    }
    public IReadOnlyList<ProtocolEvent> Feed(ChatChunk chunk)
    {
        if (completed) throw new InvalidOperationException("Stream is already terminal");
        output = [];
        if (chunk.Model.Length > 0) model = chunk.Model;
        if (!started) { started = true; Emit(new MessageStart(id, model)); }
        if (chunk.Usage is not null) { Usage = chunk.Usage.Tokens; RawUsageJson = chunk.Usage.RawJson; }
        foreach (var choice in chunk.Choices)
        {
            if (wantThinking && choice.Reasoning.Length > 0) AppendThinking(choice.Reasoning);
            if (choice.Text.Length > 0) AppendText(choice.Text);
            if (choice.Refusal.Length > 0) { refused = true; AppendText(choice.Refusal); }
            foreach (var fragment in choice.Tools) AppendTool(fragment);
            if (choice.Finish.Length > 0) finish = choice.Finish;
        }
        return output;
    }
    void StopText()
    {
        if (text is { Open: true }) { Emit(new BlockStop(text.Index)); text.Open = false; }
    }
    void StopThinking()
    {
        if (thinking is not { Open: true }) return;
        thinking.Open = false;
        string reasoning = thinking.Buffer.ToString();
        Temporary(3L * Bytes(reasoning) + 512);
        string signature = hidden?.Seal(reasoning) ?? Convert.ToBase64String(SHA256.HashData(Encoding.UTF8.GetBytes(reasoning)));
        Emit(new SignatureDelta(thinking.Index, signature)); Emit(new BlockStop(thinking.Index));
    }
    void AppendThinking(string value)
    {
        Reserve(Bytes(value)); StopText();
        if (thinking is not { Open: true }) { Reserve(512); thinking = new(nextIndex++); Emit(new ThinkingStart(thinking.Index)); }
        thinking.Buffer.Append(value);
        if (hidden is null) Emit(new ThinkingDelta(thinking.Index, value));
    }
    void AppendText(string value)
    {
        StopThinking();
        if (text is not { Open: true }) { Reserve(512); text = new(nextIndex++); Emit(new TextStart(text.Index)); }
        // Streaming text is not retained a second time.
        Emit(new TextDelta(text.Index, value));
    }
    void AppendTool(ToolFragment f)
    {
        bool exists = tools.TryGetValue(f.Index, out var slot);
        Reserve((long)Bytes(f.Id) + Bytes(f.Name) + Bytes(f.Arguments) + (exists ? 0 : 512));
        if (slot is null) { slot = new(nextIndex++); tools.Add(f.Index, slot); toolOrder.Add(f.Index); }
        StopThinking(); StopText();
        if (f.Id.Length != 0) slot.Id = f.Id;
        if (f.Name.Length != 0) slot.Name = f.Name;
        bool ready = slot.Id.Length != 0 && slot.Name.Length != 0;
        if (ready) { pendingArgumentBytes -= pendingArguments.GetValueOrDefault(f.Index); pendingArguments.Remove(f.Index); }
        else
        {
            int added = Bytes(f.Arguments);
            if (added > pendingLimit - pendingBytes - pendingArgumentBytes) throw new ConversionFailure("pending_tool_limit");
            pendingArgumentBytes += added;
            if (added > 0) pendingArguments[f.Index] = pendingArguments.GetValueOrDefault(f.Index) + added;
        }
        slot.Arguments.Append(f.Arguments);
        if (!ready) { if (pendingIndex < 0) pendingIndex = slot.Index; return; }
        if (!slot.Open)
        {
            slot.Open = true;
            if (pendingIndex == slot.Index)
            {
                pendingIndex = -1;
                foreach (int index in toolOrder)
                    if (tools[index].Id.Length == 0 || tools[index].Name.Length == 0) { pendingIndex = tools[index].Index; break; }
            }
            Emit(new ToolStart(slot.Index, new ToolCallId(slot.Id), new ToolName(slot.Name)));
            FlushPending();
        }
        if (slot.Sent < slot.Arguments.Length)
        {
            string unsent = slot.Arguments.ToString(slot.Sent, slot.Arguments.Length - slot.Sent);
            slot.Sent = slot.Arguments.Length; Emit(new ToolDelta(slot.Index, unsent));
        }
    }
    public IReadOnlyList<ProtocolEvent> Finish()
    {
        if (completed) throw new InvalidOperationException("Stream is already terminal");
        if (!HasFinish) throw new ConversionFailure("missing_finish");
        foreach (int index in toolOrder)
        {
            var t = tools[index];
            if (t.Id.Length == 0 || t.Name.Length == 0) throw new ConversionFailure("incomplete_tool_identity");
            Temporary(Bytes(t.Arguments.ToString())); ChunkReader.ValidateArguments(t.Arguments.ToString());
        }
        output = []; StopThinking(); StopText();
        foreach (int index in toolOrder) { var t = tools[index]; if (t.Open) { Emit(new BlockStop(t.Index)); t.Open = false; } }
        string stop = refused && finish == "stop" && tools.Count == 0 ? "refusal" : finish switch
        {
            "tool_calls" or "function_call" => "tool_use",
            "length" => "max_tokens",
            _ => "end_turn"
        };
        Emit(new MessageDelta(stop, Usage)); Emit(new MessageStop()); completed = true;
        return output;
    }
}
