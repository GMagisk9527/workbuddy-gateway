using System.Text.Json;
using System.Text.Json.Serialization;
using AccountMaintenance;
using AccountScheduling;
namespace HttpMigration;
internal static partial class AccountManagement
{
    sealed record Command(string Action,string Id);
    [JsonSourceGenerationOptions(JsonSerializerDefaults.Web, UnmappedMemberHandling = JsonUnmappedMemberHandling.Disallow)]
    [JsonSerializable(typeof(Command))]
    partial class CommandJson : JsonSerializerContext { }
    internal static async Task Handle(HttpContext c,LocalKeySecurity security,CredentialManager credentials,AccountPool pool,SemaphoreSlim gate)
    {
        c.Response.Headers.CacheControl="no-store";string auth=c.Request.Headers.Authorization.ToString();
        if(!auth.StartsWith("Bearer ",StringComparison.Ordinal)||!security.Admin.Matches(auth[7..])){await Error(c,401,"admin_auth_required");return;}
        bool admitted=false;
        try
        {
            if(HttpMethods.IsGet(c.Request.Method))
            {
                var loads=pool.Status.Loads;var resources=credentials.ResourceStatus.ToDictionary(r=>r.Id,StringComparer.Ordinal);await GatewayJson.WriteAsync(c, new Dictionary<string, object?> { ["ok"] = true, ["maintenance"] = c.RequestServices.GetService<CredentialMaintenanceWorker>()?.Status, ["resourcePolling"] = c.RequestServices.GetService<AccountResourceWorker>()?.Status, ["resourceCache"] = c.RequestServices.GetService<ResourceSnapshotCache>()?.Status, ["runtimeCache"] = c.RequestServices.GetService<RuntimeSnapshotCache>()?.Status, ["items"] = credentials.ManagementStatus.Select(s=>new Dictionary<string, object?> { ["id"] = s.Id, ["site"] = s.Site==AccountSite.Cn?"cn":"intl", ["expiresAt"] = s.ExpiresAt, ["disabled"] = s.Disabled, ["configuredDisabled"] = s.ConfiguredDisabled, ["degraded"] = s.Degraded, ["failures"] = s.Failures, ["credentialCooldownUntil"] = s.CooldownUntil, ["active"] = loads.GetValueOrDefault(new(s.Id)), ["modelScheduling"] = pool.ModelStatus(new(s.Id)), ["upstreamQuota"] = resources.GetValueOrDefault(s.Id) }).ToArray() });return;
            }
            if(!HttpMethods.IsPost(c.Request.Method)){await Error(c,405,"method_not_allowed");return;}
            if(!await gate.WaitAsync(0,c.RequestAborted)){await Error(c,429,"account_management_busy");return;}admitted=true;
            using var timeout=CancellationTokenSource.CreateLinkedTokenSource(c.RequestAborted);timeout.CancelAfter(TimeSpan.FromSeconds(5));
            byte[] body=await LocalGateway.ReadBounded(c.Request.Body,8192,timeout.Token);
            var command=JsonSerializer.Deserialize(body, CommandJson.Default.Command)??throw new JsonException();
            if(command.Action is not ("enable" or "disable" or "refresh_credentials" or "refresh_resources")||string.IsNullOrWhiteSpace(command.Id))throw new JsonException();
            var id=new AccountId(command.Id);
            if(command.Action=="enable")await credentials.EnableAsync(id,c.RequestAborted);
            else if(command.Action=="disable")
            {
                await credentials.DisableAsync(id);
                if(credentials.Status.Single(s=>s.Id==id).Degraded)throw new CredentialFailure("credential_persist_failed");
            }
            else if(command.Action=="refresh_credentials")await credentials.RefreshAsync(id,c.RequestAborted);
            else
            {
                using var resourceDeadline=CancellationTokenSource.CreateLinkedTokenSource(c.RequestAborted);resourceDeadline.CancelAfter(TimeSpan.FromSeconds(45));
                await credentials.FetchResourcesAsync(id,resourceDeadline.Token);
                if(c.RequestServices.GetService<ResourceSnapshotCache>() is { } resourceCache&&!resourceCache.TrySave(credentials))throw new CredentialFailure("resource_snapshot_persist_failed");
            }
            await GatewayJson.WriteAsync(c, new Dictionary<string, object?> { ["ok"] = true });
        }
        catch(CredentialFailure e){await Error(c,e.Code=="credential_missing"?404:e.Code is "credential_configured_disabled" or "credential_expired" or "credential_verification_rejected" or "credential_changed_during_enable"?409:503,e.Code);}
        catch(Exception e) when(e is JsonException or ArgumentException){await Error(c,400,"invalid_request");}
        catch(RequestFailure e){await Error(c,e.Status,"invalid_request");}
        catch(BadHttpRequestException e){await Error(c,e.StatusCode,"invalid_request");}
        catch(OperationCanceledException) when(c.RequestAborted.IsCancellationRequested){c.Abort();}
        catch(OperationCanceledException){await Error(c,504,"account_management_timeout");}
        catch(Exception e) when(e is HttpRequestException or IOException){await Error(c,503,"account_management_unavailable");}
        finally{if(admitted)gate.Release();}
    }
    static Task Error(HttpContext c,int status,string code){c.Response.StatusCode=status;return GatewayJson.WriteAsync(c, new Dictionary<string, object?> { ["ok"] = false, ["error"] = code });}
}
