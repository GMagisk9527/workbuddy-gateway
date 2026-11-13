using System.Text;
using System.Text.Json.Nodes;
using StreamingMigration;
using static OpenAiProtocols.OpenWire;

namespace OpenAiProtocols;

public sealed record OpenAiResult(StreamOutcome Outcome, string Code, byte[]? Body, string? RawUsageJson, bool ReceivedOutput);
public static class OpenAiPump
{
    public static async Task<OpenAiResult> RunAsync(Stream upstream, Stream downstream, OpenAiRequest request, byte[] key,
        long limit = 64L << 20, CancellationToken cancellation = default, Action<OpenAiResult>? beforeTerminal = null,Action? firstOutput=null,Action? upstreamCompleted=null,IRetainedOutputBudget? budget=null)
    {
        await using (upstream)
        {
            var state = new ChatState(request.Model, limit,budget,request.ResponseRestore); var reader = new SseReader(upstream);
            var responses = request.Protocol == ClientProtocol.Responses ? new ResponsesStream(request, key) : null;
            var pending = new List<byte[]>(); long written = 0, held = 0; bool notified = false, downstreamFailed = false;
            string? failureUsage = null;
            OpenAiResult Result(StreamOutcome outcome, string code, byte[]? body = null) => new(outcome, code, body, failureUsage ?? state.Usage?.ToJsonString(), state.ReceivedOutput);
            OpenAiResult Notify(StreamOutcome outcome, string code, byte[]? body = null)
            { var r = Result(outcome, code, body); if (!notified) { notified = true; beforeTerminal?.Invoke(r); } return r; }
            byte[] Frame(JsonObject obj, bool response, bool failure = false)
            {
                byte[] data = Encode(obj, failure ? 65536 : limit,failure?null:budget); string prefix = response ? "event: " + Text(obj["type"]) + "\ndata: " : "data: ";
                var header = Encoding.UTF8.GetBytes(prefix); byte[] frame = new byte[header.Length + data.Length + 2];
                header.CopyTo(frame, 0); data.CopyTo(frame, header.Length); frame[^2] = (byte)'\n'; frame[^1] = (byte)'\n'; return frame;
            }
            async Task Send(byte[] bytes)
            {
                if (bytes.Length > limit - written) throw new ConversionFailure("output_budget_exceeded"); written += bytes.Length;
                try { await downstream.WriteAsync(bytes, cancellation); await downstream.FlushAsync(cancellation); }
                catch (IOException) { downstreamFailed = true; throw; }
                catch (ObjectDisposedException) { downstreamFailed = true; throw; }
            }
            try
            {
                if (request.Stream && responses is not null) foreach (var e in responses.Start()) await Send(Frame(e, true));
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
                    if (responses is not null) foreach (var e in responses.Feed(chunk, state)) await Send(Frame(e, true));
                    else
                    {
                        byte[] frame = Frame(ChatRequestAdapter.Clean(raw!), false);
                        if (state.FinishReason != "") { if (frame.Length > limit - written - held) throw new ConversionFailure("output_budget_exceeded");budget?.Add(frame.Length); held += frame.Length; pending.Add(frame); }
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
                        byte[] frame = Frame(e, true); if (frame.Length > limit - written - held) throw new ConversionFailure("output_budget_exceeded");
                        budget?.Add(frame.Length);held += frame.Length; pending.Add(frame);
                    }
                }
                if (14 > limit - written - held) throw new ConversionFailure("output_budget_exceeded");
                Notify(StreamOutcome.Complete, "");
                foreach (var frame in pending) await Send(frame);
                await Send("data: [DONE]\n\n"u8.ToArray()); return Result(StreamOutcome.Complete, "");
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
                    try { var frame = Frame(error, responses is not null, true); await downstream.WriteAsync(frame, cancellation); await downstream.FlushAsync(cancellation); }
                    catch (OperationCanceledException) when (cancellation.IsCancellationRequested) { return Result(StreamOutcome.Cancelled, "cancelled"); }
                    catch (Exception write) when (write is IOException or ObjectDisposedException) { return Result(StreamOutcome.DownstreamFailed, "downstream_write"); }
                }
                return result;
            }
        }
    }
}
