using System.Text;
using System.Text.Json;

namespace HttpMigration;

internal static class AllocationTests
{
    internal static void Run()
    {
        string line = "synthetic source: 中文 <tag> \"quoted\" \\ path\n";
        string text = string.Concat(Enumerable.Repeat(line, (1 << 20) / Encoding.UTF8.GetByteCount(line)));
        byte[] input = JsonSerializer.SerializeToUtf8Bytes(new { model = "test-model", stream = true, max_tokens = 512,
            messages = new object[] { new { role = "assistant", content = new object[] { new { type = "tool_use", id = "call", name = "read", input = new { path = "source" } } } },
                new { role = "user", content = new object[] { new { type = "tool_result", tool_use_id = "call", content = text } } } } });
        _ = MessagesRequestAdapter.Prepare(input, Program.TestKey);
        long before = GC.GetAllocatedBytesForCurrentThread();
        var prepared = MessagesRequestAdapter.Prepare(input, Program.TestKey);
        long allocated = GC.GetAllocatedBytesForCurrentThread() - before;
        long budget = (16L << 20);
        Console.WriteLine(JsonSerializer.Serialize(new { inputBytes = input.Length, outputBytes = prepared.UpstreamBody.Length, allocatedBytes = allocated, allocationBudgetBytes = budget }));
        if (allocated > budget) throw new InvalidOperationException($"1MiB context conversion allocated {allocated} bytes, budget {budget}");
        var duplicate = MessagesRequestAdapter.Prepare(Encoding.UTF8.GetBytes("{\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],\"tools\":[{\"name\":\"read\",\"input_schema\":{\"type\":\"string\",\"type\":\"object\"}}]}"), Program.TestKey);
        using var doc = JsonDocument.Parse(duplicate.UpstreamBody);
        if (doc.RootElement.GetProperty("tools")[0].GetProperty("function").GetProperty("parameters").GetProperty("type").GetString() != "object") throw new InvalidOperationException("Schema duplicate keys must retain last value");
        FixedLengthBody();
        Utf8ToolResultTests.Run();
        OutboundFastPathTests.Run();
    }

    static void FixedLengthBody()
    {
        byte[] payload = new byte[4 << 20];
        Array.Fill(payload, (byte)'x');
        using var admission = new PreparationAdmission(payload.Length, 1);

        (byte[] Body, long Allocated) Read()
        {
            using var stream = new MemoryStream(payload, writable: false);
            var context = new DefaultHttpContext();
            context.Request.Body = stream;
            context.Request.ContentLength = payload.Length;
            using var lease = admission.Acquire();
            long start = GC.GetAllocatedBytesForCurrentThread();
            byte[] body = LocalGateway.ReadBody(context, payload.Length, lease).GetAwaiter().GetResult();
            long allocated = GC.GetAllocatedBytesForCurrentThread() - start;
            if (admission.Used != body.Length) throw new InvalidOperationException("Body should be reserved once");
            return (body, allocated);
        }

        _ = Read();
        var (body, allocated) = Read();
        Console.WriteLine(JsonSerializer.Serialize(new { fixedLengthBytes = payload.Length, fixedLengthAllocatedBytes = allocated }));
        if (!body.AsSpan().SequenceEqual(payload) || admission.Used != 0)
            throw new InvalidOperationException("Fixed-length reading must preserve bytes and release its budget");
        if (allocated > payload.Length + (256 << 10))
            throw new InvalidOperationException($"Fixed-length reading allocated {allocated} bytes for a {payload.Length}-byte body");

        foreach (int declared in new[] { 2, 4 })
        {
            using var stream = new MemoryStream("abc"u8.ToArray(), writable: false);
            var context = new DefaultHttpContext();
            context.Request.Body = stream;
            context.Request.ContentLength = declared;
            try
            {
                using var lease = admission.Acquire();
                _ = LocalGateway.ReadBody(context, payload.Length, lease).GetAwaiter().GetResult();
                throw new InvalidOperationException("Body framing accepted a length mismatch");
            }
            catch (BadHttpRequestException error) when (error.StatusCode == 400) { }
            if (admission.Used != 0) throw new InvalidOperationException("Malformed body leaked its budget");
        }
    }
}
