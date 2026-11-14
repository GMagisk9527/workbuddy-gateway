using System.Text.Json;
using System.Text.Json.Nodes;

namespace OpenAiProtocols;

// High-frequency deltas are value events, not mutable JSON object trees.
// Complex lifecycle envelopes retain the existing DOM/escaping path.
internal readonly record struct ResponsesEvent(string Type,int Sequence,JsonObject? Body=null,
    string ItemId="",int OutputIndex=0,int PartIndex=0,string Delta="")
{
    static readonly JsonEncodedText ItemIdKey=JsonEncodedText.Encode("item_id"),OutputIndexKey=JsonEncodedText.Encode("output_index"),
        ContentIndexKey=JsonEncodedText.Encode("content_index"),SummaryIndexKey=JsonEncodedText.Encode("summary_index"),
        DeltaKey=JsonEncodedText.Encode("delta"),TypeKey=JsonEncodedText.Encode("type"),SequenceKey=JsonEncodedText.Encode("sequence_number");
    static readonly JsonEncodedText TextType=JsonEncodedText.Encode("response.output_text.delta"),RefusalType=JsonEncodedText.Encode("response.refusal.delta"),
        ReasoningType=JsonEncodedText.Encode("response.reasoning_summary_text.delta"),ArgumentsType=JsonEncodedText.Encode("response.function_call_arguments.delta");
    internal void WriteTo(Utf8JsonWriter writer)
    {
        if(Body is not null){OpenWire.EncodeFrame(writer,Body);return;}
        writer.WriteStartObject();
        writer.WriteString(ItemIdKey,ItemId);writer.WriteNumber(OutputIndexKey,OutputIndex);
        JsonEncodedText type;
        switch(Type)
        {
            case "response.output_text.delta":writer.WriteNumber(ContentIndexKey,PartIndex);type=TextType;break;
            case "response.refusal.delta":writer.WriteNumber(ContentIndexKey,PartIndex);type=RefusalType;break;
            case "response.reasoning_summary_text.delta":writer.WriteNumber(SummaryIndexKey,PartIndex);type=ReasoningType;break;
            case "response.function_call_arguments.delta":type=ArgumentsType;break;
            default:throw new InvalidOperationException("Unsupported typed Responses event");
        }
        writer.WriteString(DeltaKey,Delta);writer.WriteString(TypeKey,type);writer.WriteNumber(SequenceKey,Sequence);
        writer.WriteEndObject();writer.Flush();
    }
}
