using System.Text.Json;
using System.Text.Json.Nodes;
using System.Text.Json.Serialization;
using AccountLogin;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

// Management responses combine maps and a closed set of record types.
// Register every possible runtime type for values held as object.
[JsonSourceGenerationOptions(JsonSerializerDefaults.Web)]
[JsonSerializable(typeof(object))]
[JsonSerializable(typeof(object[]))]
[JsonSerializable(typeof(Dictionary<string, object>))]
[JsonSerializable(typeof(Dictionary<string, object>[]))]
[JsonSerializable(typeof(List<Dictionary<string, object>>))]
[JsonSerializable(typeof(List<object>))]
[JsonSerializable(typeof(string[]))]
[JsonSerializable(typeof(double?[]))]
[JsonSerializable(typeof(ConsoleAccount[]))]
[JsonSerializable(typeof(ConsoleAttempt[]))]
[JsonSerializable(typeof(ConsoleApi.AccountRow[]))]
[JsonSerializable(typeof(CredentialDocument))]
[JsonSerializable(typeof(LoginSnapshot))]
[JsonSerializable(typeof(CheckinView))]
[JsonSerializable(typeof(CheckinDocument))]
[JsonSerializable(typeof(CredentialMaintenanceStatus))]
[JsonSerializable(typeof(ResourceWorkerStatus))]
[JsonSerializable(typeof(ResourceCacheStatus))]
[JsonSerializable(typeof(RuntimeCacheStatus))]
[JsonSerializable(typeof(ModelRuntimeStatus))]
[JsonSerializable(typeof(AccountResourceStatus))]
[JsonSerializable(typeof(LedgerHealth))]
[JsonSerializable(typeof(JsonObject))]
[JsonSerializable(typeof(JsonElement))]
[JsonSerializable(typeof(string))]
[JsonSerializable(typeof(bool))]
[JsonSerializable(typeof(int))]
[JsonSerializable(typeof(long))]
[JsonSerializable(typeof(double))]
internal partial class GatewayJson : JsonSerializerContext
{
    internal static Task WriteAsync(HttpContext context, object value) =>
        context.Response.WriteAsJsonAsync(value, Default.Object, cancellationToken: context.RequestAborted);

    internal static byte[] Encode(object value) => JsonSerializer.SerializeToUtf8Bytes(value, Default.Object);

    internal static IResult Result(object value) => Results.Json(value, Default.Object);
}

// These formats use the previous default JSON casing, unlike the HTTP responses.
[JsonSerializable(typeof(ScheduledTaskJournal.Entry))]
[JsonSerializable(typeof(ScheduledTaskJournal.Entry[]))]
[JsonSerializable(typeof(List<ModelPriceProbeWorker.Probe>))]
[JsonSerializable(typeof(Dictionary<string, CreditUsage>))]
[JsonSerializable(typeof(DeploymentState))]
internal partial class StorageJson : JsonSerializerContext { }

internal sealed record DeploymentState(
    bool ok, int accounts, int disabled, int keys, LedgerHealth ledger, string usageDigest,
    RuntimeCacheStatus runtime, ResourceCacheStatus resources);
