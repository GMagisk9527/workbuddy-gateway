using System.Text.Json.Nodes;
using StreamingMigration;
using static OpenAiProtocols.OpenWire;

namespace OpenAiProtocols;

// Output indices are assigned when identities become usable, never reserved by incomplete calls.
internal sealed class ResponsesStream(OpenAiRequest request, byte[] key)
{
    readonly string id = "resp_" + Guid.NewGuid().ToString("N");
    readonly long created = DateTimeOffset.UtcNow.ToUnixTimeSeconds();
    readonly Dictionary<int, (int Index, string Id)> tools = [];
    readonly List<bool> parts = [];
    readonly List<ResponsesEvent> output = [];
    int next, sequence, message = -1, reasoning = -1;
    string messageId = "", reasoningId = "";
    ResponsesEvent Event(string type, JsonObject fields) { fields["type"] = type; fields["sequence_number"] = sequence; return new(type,sequence++,fields); }
    ResponsesEvent Delta(string type,string itemId,int index,string delta,int part=0)=>new(type,sequence++,ItemId:itemId,OutputIndex:index,PartIndex:part,Delta:delta);
    public IReadOnlyList<ResponsesEvent> Start()
    {
        var response = ResponsesOutput.Envelope(id, request.Model, created, request.Metadata);
        return [Event("response.created", new() { ["response"] = response }), Event("response.in_progress", new() { ["response"] = response.DeepClone() })];
    }
    // Borrowed list: the pump fully sends it before the next Feed call.
    public IReadOnlyList<ResponsesEvent> Feed(ChatChunk chunk, ChatState state)
    {
        output.Clear();
        int choiceIndex = -1;
        foreach (var choice in chunk.Choices)
        {
            choiceIndex++;
            if (choice.Reasoning != "")
            {
                if (reasoning < 0)
                {
                    reasoning = next++; reasoningId = "rs_" + Guid.NewGuid().ToString("N");
                    output.Add(Event("response.output_item.added", new() { ["output_index"] = reasoning, ["item"] = new JsonObject { ["id"] = reasoningId, ["type"] = "reasoning", ["status"] = "in_progress", ["summary"] = new JsonArray() } }));
                    output.Add(Event("response.reasoning_summary_part.added", new() { ["item_id"] = reasoningId, ["output_index"] = reasoning, ["summary_index"] = 0, ["part"] = new JsonObject { ["type"] = "summary_text", ["text"] = "" } }));
                }
                output.Add(Delta("response.reasoning_summary_text.delta",reasoningId,reasoning,choice.Reasoning));
            }
            void TextPart(bool refusal, string delta)
            {
                if (delta == "") return;
                if (message < 0)
                {
                    message = next++; messageId = "msg_" + Guid.NewGuid().ToString("N");
                    output.Add(Event("response.output_item.added", new() { ["output_index"] = message, ["item"] = new JsonObject { ["id"] = messageId, ["type"] = "message", ["status"] = "in_progress", ["role"] = "assistant", ["content"] = new JsonArray() } }));
                }
                int index = parts.IndexOf(refusal);
                if (index < 0)
                {
                    index = parts.Count; parts.Add(refusal);
                    var part = refusal ? new JsonObject { ["type"] = "refusal", ["refusal"] = "" } : new JsonObject { ["type"] = "output_text", ["text"] = "", ["annotations"] = new JsonArray() };
                    output.Add(Event("response.content_part.added", new() { ["item_id"] = messageId, ["output_index"] = message, ["content_index"] = index, ["part"] = part }));
                }
                output.Add(Delta(refusal ? "response.refusal.delta" : "response.output_text.delta",messageId,message,delta,index));
            }
            TextPart(false, choice.Text); TextPart(true, choice.Refusal);
            foreach (var change in state.Changes.Where(c => c.Choice == choiceIndex))
            {
                if (change.Id == "" || change.Name == "") continue;
                bool custom = request.Tools.GetValueOrDefault(change.Name)?.Kind == ResponseToolKind.Custom;
                bool opened = false;
                if (!tools.TryGetValue(change.Index, out var identity))
                {
                    var item = ResponsesTools.Call(change.Id, change.Name, "", request.Tools); item["status"] = "in_progress";
                    identity = (next++, Text(item["id"])); tools.Add(change.Index, identity); opened = true;
                    output.Add(Event("response.output_item.added", new() { ["output_index"] = identity.Index, ["item"] = item }));
                }
                string delta = opened ? state.ArgumentsThrough(change) : change.Arguments;
                if (!custom && delta != "") output.Add(Delta("response.function_call_arguments.delta",identity.Id,identity.Index,delta));
            }
        }
        return output;
    }
    public IReadOnlyList<ResponsesEvent> Finish(ChatState state)
    {
        state.Validate(); string status = ResponsesOutput.Status(state.FinishReason); var events = new List<ResponsesEvent>(); var items = new SortedDictionary<int, JsonObject>();
        void Done(int index, JsonObject item) { items.Add(index, item); events.Add(Event("response.output_item.done", new() { ["output_index"] = index, ["item"] = item.DeepClone() })); }
        if (reasoning >= 0)
        {
            string text = state.Reasoning.ToString();
            events.Add(Event("response.reasoning_summary_text.done", new() { ["item_id"] = reasoningId, ["output_index"] = reasoning, ["summary_index"] = 0, ["text"] = text }));
            events.Add(Event("response.reasoning_summary_part.done", new() { ["item_id"] = reasoningId, ["output_index"] = reasoning, ["summary_index"] = 0, ["part"] = new JsonObject { ["type"] = "summary_text", ["text"] = text } }));
            Done(reasoning, ResponsesOutput.Reasoning(reasoningId, status, text, request.EncryptedReasoning ? new ReasoningCodec(key, request.Model, "responses") : null));
        }
        if (next == 0)
        {
            message = next++; messageId = "msg_" + Guid.NewGuid().ToString("N"); parts.Add(false);
            events.Add(Event("response.output_item.added", new() { ["output_index"] = message, ["item"] = new JsonObject { ["id"] = messageId, ["type"] = "message", ["status"] = "in_progress", ["role"] = "assistant", ["content"] = new JsonArray() } }));
            events.Add(Event("response.content_part.added", new() { ["item_id"] = messageId, ["output_index"] = message, ["content_index"] = 0, ["part"] = new JsonObject { ["type"] = "output_text", ["text"] = "", ["annotations"] = new JsonArray() } }));
        }
        if (message >= 0)
        {
            var content = new JsonArray();
            for (int i = 0; i < parts.Count; i++)
            {
                bool refusal = parts[i]; string text = refusal ? state.Refusal.ToString() : state.Content.ToString();
                var fields = new JsonObject { ["item_id"] = messageId, ["output_index"] = message, ["content_index"] = i, [refusal ? "refusal" : "text"] = text };
                events.Add(Event(refusal ? "response.refusal.done" : "response.output_text.done", fields));
                var part = refusal ? new JsonObject { ["type"] = "refusal", ["refusal"] = text } : new JsonObject { ["type"] = "output_text", ["text"] = text, ["annotations"] = new JsonArray() };
                events.Add(Event("response.content_part.done", new() { ["item_id"] = messageId, ["output_index"] = message, ["content_index"] = i, ["part"] = part.DeepClone() })); content.Add((JsonNode?)part);
            }
            Done(message, new() { ["id"] = messageId, ["type"] = "message", ["status"] = status, ["role"] = "assistant", ["content"] = content });
        }
        foreach (var call in state.Calls)
        {
            var identity = tools[call.Index]; var item = ResponsesTools.Call(call.Id, call.Name, call.Arguments.ToString(), request.Tools); item["id"] = identity.Id; item["status"] = status;
            bool custom = Text(item["type"]) == "custom_tool_call";
            if (custom) events.Add(Event("response.custom_tool_call_input.delta", new() { ["item_id"] = identity.Id, ["output_index"] = identity.Index, ["delta"] = Text(item["input"]) }));
            var done = new JsonObject { ["item_id"] = identity.Id, ["output_index"] = identity.Index, [custom ? "input" : "arguments"] = Text(item[custom ? "input" : "arguments"]) };
            if (!custom) done["name"] = Text(item["name"]);
            events.Add(Event(custom ? "response.custom_tool_call_input.done" : "response.function_call_arguments.done", done)); Done(identity.Index, item);
        }
        var response = ResponsesOutput.Envelope(id, request.Model, created, request.Metadata); response["status"] = status;
        response["output"] = new JsonArray(items.Values.Select(x => x.DeepClone()).ToArray());
        if (status == "completed") response["completed_at"] = DateTimeOffset.UtcNow.ToUnixTimeSeconds();
        else response["incomplete_details"] = new JsonObject { ["reason"] = state.FinishReason == "length" ? "max_output_tokens" : "content_filter" };
        if (state.Usage is not null) response["usage"] = ResponsesOutput.Usage(state.Usage);
        events.Add(Event("response." + status, new() { ["response"] = response })); return events;
    }
    public JsonObject Failed(string code)
    {
        var response = ResponsesOutput.Envelope(id, request.Model, created, request.Metadata); response["status"] = "failed";
        response["error"] = new JsonObject { ["code"] = code, ["message"] = "上游响应不完整" };
        var fields=new JsonObject { ["response"] = response, ["type"] = "response.failed", ["sequence_number"] = sequence++ };return fields;
    }
}
