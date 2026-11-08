using System.Text.Json.Nodes;
using StreamingMigration;

namespace OpenAiProtocols;

internal static class ResponseIdentityWire
{
    internal static void Apply(JsonObject chunk, ResponseIdentityPlan plan, Dictionary<(int Choice, string Channel), ResponseIdentityRestorer> restorers)
    {
        foreach (var choice in OpenWire.Items(chunk["choices"]).OfType<JsonObject>())
        {
            int index = (int)OpenWire.Int(choice["index"]);
            var delta=choice["delta"] as JsonObject;
            bool terminal = OpenWire.Text(choice["finish_reason"]).Length > 0 || delta?["tool_calls"] is JsonArray { Count: >0 };
            if(delta is null){if(!terminal)continue;choice["delta"]=delta=new();}
            bool hasContent=OpenWire.Text(delta["content"]).Length>0,hasReasoning=OpenWire.Text(delta["reasoning_content"]).Length>0;
            foreach (string channel in new[] { "content", "reasoning_content" })
            {
                bool switched=channel=="content"?hasReasoning:hasContent||OpenWire.Text(delta["refusal"]).Length>0;
                string text = OpenWire.Text(delta[channel]); if (text.Length == 0 && !terminal&&!switched) continue;
                var key = (index, channel);
                if (!restorers.TryGetValue(key, out var restorer))
                {if(restorers.Count>=512)throw new ConversionFailure("output_budget_exceeded");restorers[key] = restorer = plan.CreateRestorer();}
                string next = text.Length == 0 ? "" : restorer.Write(text);
                if (terminal||switched) next += restorer.Flush();
                if (next.Length == 0) delta.Remove(channel); else delta[channel] = next;
            }
        }
    }
}
