namespace AccountMaintenance;

// FIFO admission with batched readers. A queued writer prevents new readers from starving refresh.
internal sealed class AsyncCredentialGate
{
    readonly object sync = new();
    readonly LinkedList<Waiter> queue = new();
    int readers; bool writer;
    sealed class Waiter(bool write)
    {
        public bool Write { get; } = write;
        public TaskCompletionSource<Permit> Source { get; } = new(TaskCreationOptions.RunContinuationsAsynchronously);
        public LinkedListNode<Waiter>? Node;
    }
    internal sealed class Permit(Action release) : IDisposable
    {
        Action? action = release;
        public void Dispose() => Interlocked.Exchange(ref action, null)?.Invoke();
    }
    public Task<Permit> ReadAsync(CancellationToken cancellation) => Acquire(false, cancellation);
    public Task<Permit> WriteAsync(CancellationToken cancellation) => Acquire(true, cancellation);
    async Task<Permit> Acquire(bool write, CancellationToken cancellation)
    {
        Waiter pending;
        lock (sync)
        {
            cancellation.ThrowIfCancellationRequested();
            pending = new(write); pending.Node = queue.AddLast(pending); Drain();
        }
        try
        {
            var permit = await pending.Source.Task.WaitAsync(cancellation);
            if (cancellation.IsCancellationRequested) { permit.Dispose(); cancellation.ThrowIfCancellationRequested(); }
            return permit;
        }
        catch (OperationCanceledException)
        {
            Permit? granted = null;
            lock (sync)
            {
                if (pending.Node.List is not null) queue.Remove(pending.Node);
                else if (pending.Source.Task.IsCompletedSuccessfully) granted = pending.Source.Task.Result;
                Drain();
            }
            granted?.Dispose(); throw;
        }
    }
    void Drain()
    {
        while (!writer && queue.First is { } node)
        {
            if (node.Value.Write && readers != 0) return;
            queue.RemoveFirst(); var wait = node.Value;
            if (wait.Write) writer = true; else readers++;
            wait.Source.SetResult(new(() => Release(wait.Write)));
            if (writer) return;
        }
    }
    void Release(bool write)
    {
        lock (sync) { if (write) writer = false; else readers--; Drain(); }
    }
}
