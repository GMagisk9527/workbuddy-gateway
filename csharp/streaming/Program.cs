using System.Security.Cryptography;
using System.Text;
using System.Text.Json;

namespace StreamingMigration;

sealed record InputCase(string Name, string Sse = "", int ReadChunk = 8192, int? FailAfter = null,
    bool Thinking = false, bool Hidden = false, string Model = "test-model", string[]? Signatures = null);

static class Program
{
    static readonly JsonSerializerOptions Json = new() { PropertyNamingPolicy = JsonNamingPolicy.CamelCase, PropertyNameCaseInsensitive = true };
    internal static byte[] TestKey => Enumerable.Range(0, 32).Select(x => (byte)x).ToArray();
    static async Task Main(string[] args)
    {
        if (args is ["--self-test"]) { await SelfTests.RunAsync(); return; }
        Console.InputEncoding = new UTF8Encoding(false); Console.OutputEncoding = new UTF8Encoding(false);
        while (await Console.In.ReadLineAsync() is { } line)
        {
            var spec = JsonSerializer.Deserialize<InputCase>(line, Json) ?? throw new InvalidDataException("Missing case");
            var codec = new ReasoningCodec(TestKey, spec.Model);
            using var source = new FragmentedInput(Encoding.UTF8.GetBytes(spec.Sse), spec.ReadChunk, spec.FailAfter);
            using var destination = new MemoryStream();
            var result = await StreamPump.RunAsync(source, destination, new(spec.Model, spec.Thinking, spec.Hidden ? codec : null));
            var opened = (spec.Signatures ?? []).Select(s =>
            {
                try { return codec.Open(s); } catch (Exception e) when (e is CryptographicException or FormatException) { return "!invalid"; }
            }).ToArray();
            Console.WriteLine(JsonSerializer.Serialize(new
            {
                spec.Name,
                Sse = Encoding.UTF8.GetString(destination.ToArray()),
                result.Code,
                result.Usage,
                Closed = source.Closed,
                Opened = opened
            }, Json));
        }
    }
}

internal sealed class FragmentedInput(byte[] data, int chunk = 8192, int? failAfter = null) : Stream
{
    int position;
    public bool Closed { get; private set; }
    public int Reads { get; private set; }
    public override ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default)
    {
        cancellationToken.ThrowIfCancellationRequested(); Reads++;
        if (failAfter is int limit && position >= limit) throw new IOException("Injected upstream failure");
        int count = Math.Min(buffer.Length, Math.Min(Math.Max(1, chunk), data.Length - position));
        if (failAfter is int bound) count = Math.Min(count, bound - position);
        data.AsMemory(position, count).CopyTo(buffer); position += count; return ValueTask.FromResult(count);
    }
    protected override void Dispose(bool disposing) { Closed = true; base.Dispose(disposing); }
    public override int Read(byte[] buffer, int offset, int count) => ReadAsync(buffer.AsMemory(offset, count)).AsTask().GetAwaiter().GetResult();
    public override bool CanRead => true; public override bool CanSeek => false; public override bool CanWrite => false;
    public override long Length => data.Length; public override long Position { get => position; set => throw new NotSupportedException(); }
    public override void Flush() { }
    public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException();
    public override void SetLength(long value) => throw new NotSupportedException(); public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
}
