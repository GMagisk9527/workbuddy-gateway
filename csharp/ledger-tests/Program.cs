using System.Text.Json;
using LedgerMigration;

namespace LedgerChecks;

internal static class Program
{
    sealed record Probe(string Name, string Op, JsonElement Usage, CreditUsage? Initial = null, LedgerEntry[]? Entries = null, long At = 1793851200000, Dictionary<string, string>? Files = null);
    static async Task Main(string[] args)
    {
        if (args is ["--self-test", var directory])
        {
            Directory.CreateDirectory(directory); var journal = await JournalTests.Run(directory); var http = await HttpLedgerTests.Run();
            Console.WriteLine(JsonSerializer.Serialize(new { journalTests = journal.Count, httpTests = http.Count, journal, http }, Fixtures.Json)); return;
        }
        if (args is not ["--probe"]) throw new ArgumentException("Use --self-test <directory> or --probe");
        while (await Console.In.ReadLineAsync() is { } line)
        {
            var spec = JsonSerializer.Deserialize<Probe>(line, Fixtures.Json) ?? throw new JsonException(); object result;
            if (spec.Op == "observe")
            {
                var observed = CreditRules.Observe(spec.Usage.ValueKind == JsonValueKind.Undefined ? null : spec.Usage.GetRawText());
                result = new { kind = observed.Kind.ToString().ToLowerInvariant(), observed.Credit, observed.Input, observed.Output, observed.Cached };
            }
            else if (spec.Op == "export")
            {
                var backend = new MemoryLedger(); using (var ledger = new CreditLedger(backend.Open(), clock: new FakeClock(DateTimeOffset.FromUnixTimeMilliseconds(spec.At))))
                { ledger.Complete(ledger.Begin("key"), spec.Usage.ValueKind == JsonValueKind.Undefined ? null : spec.Usage.GetRawText(), 200); ledger.Compact(); }
                result = backend.Data.ToDictionary(p => p.Key, p => Convert.ToBase64String(p.Value));
            }
            else if (spec.Op == "load")
            {
                var backend = new MemoryLedger { Data = (spec.Files ?? []).ToDictionary(p => p.Key, p => Convert.FromBase64String(p.Value)) };
                using var ledger = new CreditLedger(backend.Open(), ["key"], new FakeClock(DateTimeOffset.FromUnixTimeMilliseconds(spec.At)));
                result = new { usage = ledger.GetUsage("key"), healthy = ledger.Health.Healthy };
            }
            else
            {
                var usage = spec.Initial ?? new(); var now = DateTimeOffset.FromUnixTimeMilliseconds(spec.At);
                foreach (var entry in spec.Entries ?? []) usage = CreditRules.Apply(usage, entry, now);
                result = CreditRules.Roll(usage, now);
            }
            Console.WriteLine(JsonSerializer.Serialize(new { spec.Name, result }, Fixtures.Json));
        }
    }
}
