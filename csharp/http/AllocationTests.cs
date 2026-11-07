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
    }
}
