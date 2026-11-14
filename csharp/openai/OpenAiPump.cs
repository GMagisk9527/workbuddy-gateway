using System.Text;
using System.Text.Json;
using System.Text.Encodings.Web;
using System.Text.Json.Nodes;
using StreamingMigration;
using static OpenAiProtocols.OpenWire;

namespace OpenAiProtocols;

public sealed record OpenAiResult(StreamOutcome Outcome, string Code, byte[]? Body, string? RawUsageJson, bool ReceivedOutput);
public static class OpenAiPump
{
    static readonly byte[] Done = "data: [DONE]\n\n"u8.ToArray();
    sealed class FrameWriter(long limit,IRetainedOutputBudget? budget) : IDisposable
    {
        readonly SseFrameBuffer buffer = new(budget is null ? null : budget.Scratch,
            static () => throw new ConversionFailure("output_budget_exceeded"));
        Utf8JsonWriter? json;
        void Begin(string? type)
        {
            buffer.Reset(limit);
            if(type is not null)
            {
                buffer.Write("event: "u8);int count=Encoding.UTF8.GetByteCount(type);
                buffer.Advance(Encoding.UTF8.GetBytes(type,buffer.GetSpan(count)));buffer.Write("\ndata: "u8);
            }
            else buffer.Write("data: "u8);
            if(json is null)json=new(buffer,new(){Encoder=JavaScriptEncoder.UnsafeRelaxedJsonEscaping,MaxDepth=256});
            else json.Reset(buffer);
        }
        ReadOnlyMemory<byte> Finish(){buffer.Write("\n\n"u8);return buffer.WrittenMemory;}
        internal ReadOnlyMemory<byte> Write(JsonObject obj,bool response)
        {
            try
            {
                Begin(response?Text(obj["type"]):null);EncodeFrame(json!,obj);return Finish();
            }
            catch { json?.Reset(buffer);throw; } // Dispose must not flush a rejected partial frame.
        }
        internal ReadOnlyMemory<byte> Write(ResponsesEvent e)
        {
            try {Begin(e.Type);e.WriteTo(json!);return Finish();}
            catch {json?.Reset(buffer);throw;}
        }
        public void Dispose()=>json?.Dispose();
    }
    public static async Task<OpenAiResult> RunAsync(Stream upstream, Stream downstream, OpenAiRequest request, byte[] key,
        long limit = 64L << 20, CancellationToken cancellation = default, Action<OpenAiResult>? beforeTerminal = null,Action? firstOutput=null,Action? upstreamCompleted=null,IRetainedOutputBudget? budget=null)
    {
        await using (upstream)
        {
            var state = new ChatState(request.Model, limit,budget,request.ResponseRestore,retainText:!request.Stream||request.Protocol==ClientProtocol.Responses); var reader = new SseReader(upstream);
            var responses = request.Protocol == ClientProtocol.Responses ? new ResponsesStream(request, key) : null;
            var pending = new List<byte[]>(); long written = 0, held = 0; bool notified = false, downstreamFailed = false;
            string? failureUsage = null;
            OpenAiResult Result(StreamOutcome outcome, string code, byte[]? body = null) => new(outcome, code, body, failureUsage ?? state.Usage?.ToJsonString(), state.ReceivedOutput);
            OpenAiResult Notify(StreamOutcome outcome, string code, byte[]? body = null)
            { var r = Result(outcome, code, body); if (!notified) { notified = true; beforeTerminal?.Invoke(r); } return r; }
            using var frames = new FrameWriter(limit,budget);
            async Task Send(ReadOnlyMemory<byte> bytes)
            {
                if (bytes.Length > limit - written) throw new ConversionFailure("output_budget_exceeded"); written += bytes.Length;
                try { await downstream.WriteAsync(bytes, cancellation); await downstream.FlushAsync(cancellation); }
                catch (IOException) { downstreamFailed = true; throw; }
                catch (ObjectDisposedException) { downstreamFailed = true; throw; }
            }
            try
            {
                if (request.Stream && responses is not null) foreach (var e in responses.Start()) await Send(frames.Write(e));
                while (await reader.NextPayloadUtf8Async(cancellation) is { } payload)
                {
                    bool hadOutput=state.ReceivedOutput;
                    JsonObject? raw = null; ChatChunk chunk;
                    if (request.Protocol == ClientProtocol.Responses) chunk = state.FeedResponses(payload);
                    else (raw, chunk) = state.Feed(payload);
                    if(!hadOutput&&state.ReceivedOutput)firstOutput?.Invoke();
                    if (chunk.Choices.Any(c => c.Finish.Length > 0))
                    {
                        upstreamCompleted?.Invoke();
                        await reader.ThrowIfFailedFinishAsync(chunk, cancellation);
                    }
                    if (!request.Stream) continue;
                    if (responses is not null) foreach (var e in responses.Feed(chunk, state)) await Send(frames.Write(e));
                    else
                    {
                        var frame = frames.Write(ChatRequestAdapter.Clean(raw!), false);
                        if (state.FinishReason != "") { if (frame.Length > limit - written - held) throw new ConversionFailure("output_budget_exceeded");budget?.Add(frame.Length); held += frame.Length; pending.Add(frame.ToArray()); }
                        else await Send(frame);
                    }
                }
                state.Validate();
                if (!request.Stream)
                {
                    byte[] body = Encode(responses is null ? state.Completion() : ResponsesOutput.Complete(state, request, key), limit,budget);
                    return Notify(StreamOutcome.Complete, "", body);
                }
                if (responses is not null)
                {
                    foreach (var e in responses.Finish(state))
                    {
                        var frame = frames.Write(e); if (frame.Length > limit - written - held) throw new ConversionFailure("output_budget_exceeded");
                        budget?.Add(frame.Length);held += frame.Length; pending.Add(frame.ToArray());
                    }
                }
                if (14 > limit - written - held) throw new ConversionFailure("output_budget_exceeded");
                Notify(StreamOutcome.Complete, "");
                foreach (var frame in pending) await Send(frame);
                await Send(Done); return Result(StreamOutcome.Complete, "");
            }
            catch (OperationCanceledException) when (cancellation.IsCancellationRequested) { return Notify(StreamOutcome.Cancelled, "cancelled"); }
            catch (Exception e) when (e is ConversionFailure or OpenAiFailure or IOException or ObjectDisposedException)
            {
                if (e is ConversionFailure conversionFailure) failureUsage = conversionFailure.RawUsageJson;
                string code = e is ConversionFailure conversion ? conversion.Code : e is OpenAiFailure protocol ? protocol.Code : "stream_interrupted";
                if (downstreamFailed) return Notify(StreamOutcome.DownstreamFailed, "downstream_write");
                var result = Notify(StreamOutcome.Failed, code);
                if (request.Stream)
                {
                    var error = responses?.Failed(code) ?? new JsonObject { ["error"] = new JsonObject { ["code"] = code, ["type"] = "upstream_error", ["message"] = "上游响应不完整" } };
                    try { using var errors = new FrameWriter(65536,null); var frame = errors.Write(error, responses is not null); await downstream.WriteAsync(frame, cancellation); await downstream.FlushAsync(cancellation); }
                    catch (OperationCanceledException) when (cancellation.IsCancellationRequested) { return Result(StreamOutcome.Cancelled, "cancelled"); }
                    catch (Exception write) when (write is IOException or ObjectDisposedException) { return Result(StreamOutcome.DownstreamFailed, "downstream_write"); }
                }
                return result;
            }
        }
    }
}
