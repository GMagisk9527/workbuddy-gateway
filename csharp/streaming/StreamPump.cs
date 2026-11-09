namespace StreamingMigration;

public enum StreamOutcome { Complete, Failed, Cancelled, DownstreamFailed }
public sealed record StreamResult(StreamOutcome Outcome, string Code, Usage Usage, string? RawUsageJson, bool ReceivedOutput = false);
public sealed record StreamOptions(string Model = "test-model", bool WantThinking = false, ReasoningCodec? Hidden = null, long RetainedLimit = 64L << 20,Action? FirstOutput=null,Action? UpstreamCompleted=null,IRetainedOutputBudget? Budget=null,ResponseIdentityPlan? ResponseRestore=null);

public static class StreamPump
{
    // Takes ownership of upstream. Caller owns downstream. No retry/replay path exists.
    public static async Task<StreamResult> RunAsync(Stream upstream, Stream downstream, StreamOptions options, CancellationToken cancellation = default, Action<StreamResult>? beforeTerminal = null)
    {
        await using (upstream)
        {
            var state = new MessageStreamState(options.Model, options.WantThinking, options.Hidden, options.RetainedLimit,budget:options.Budget);
            var reader = new SseReader(upstream); var writer = new EventWriter();
            var restorers = new Dictionary<(int Choice, string Channel), ResponseIdentityRestorer>();
            bool receivedOutput = false, notified = false; string? failureUsage=null;
            StreamResult Result(StreamOutcome outcome, string code) => new(outcome, code, state.Usage, failureUsage??state.RawUsageJson, receivedOutput);
            StreamResult Notify(StreamOutcome outcome, string code)
            { var result = Result(outcome, code); if (!notified) { notified = true; beforeTerminal?.Invoke(result); } return result; }
            bool downstreamFailed = false, firstFrameYielded=false;
            async Task Send(IReadOnlyList<ProtocolEvent> events)
            {
                foreach (var e in events)
                {
                    cancellation.ThrowIfCancellationRequested();
                    try {var frame=e.Accept(writer);if(e is not StreamError)options.Budget?.Scratch(frame.Length);await downstream.WriteAsync(frame, cancellation); await downstream.FlushAsync(cancellation); }
                    catch (OperationCanceledException) { throw; }
                    catch (IOException) { downstreamFailed = true; throw; }
                    catch (ObjectDisposedException) { downstreamFailed = true; throw; }
                    // A cached upstream may complete every await synchronously.
                    // Give Kestrel's transport a turn after the first visible frame.
                    if(!firstFrameYielded&&e is TextDelta or ThinkingDelta or ToolStart or ToolDelta)
                    {firstFrameYielded=true;await Task.Yield();}
                }
            }
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
                    await Send(state.Feed(chunk));
                }
                var terminal = state.Finish();
                Notify(StreamOutcome.Complete, "");
                await Send(terminal);
                return Result(StreamOutcome.Complete, "");
            }
            catch (OperationCanceledException) when (cancellation.IsCancellationRequested) { return Notify(StreamOutcome.Cancelled, "cancelled"); }
            catch (Exception e) when (e is ConversionFailure or IOException or ObjectDisposedException)
            {
                if(e is ConversionFailure failure)failureUsage=failure.RawUsageJson;
                if (downstreamFailed) return Notify(StreamOutcome.DownstreamFailed, "downstream_write");
                string code = e is ConversionFailure f ? f.Code : "read_error";
                Notify(StreamOutcome.Failed, code);
                try { await Send([new StreamError(code)]); }
                catch (OperationCanceledException) when (cancellation.IsCancellationRequested) { return Result(StreamOutcome.Cancelled, "cancelled"); }
                catch (Exception writeError) when (writeError is IOException or ObjectDisposedException) { return Result(StreamOutcome.DownstreamFailed, "downstream_write"); }
                return Result(StreamOutcome.Failed, code);
            }
        }
    }
}
