using System.Text.Json;

namespace HttpMigration;

internal static class ConsoleMaintenanceSetup
{
    internal static void Configure(IServiceProvider services,LocalGatewayOptions options)
    {
        var console=services.GetRequiredService<ConsoleRuntime>();
        console.Settings.AttachOutput(services.GetRequiredService<OutputAdmission>());
        console.Settings.AttachRetries(services.GetRequiredService<RetryAdmission>());
        console.Settings.AttachService(services.GetRequiredService<Microsoft.Extensions.Options.IOptions<HostOptions>>().Value,options.ServiceTimeouts);
        console.Settings.AttachUpstream(new(options.HeaderTimeout??TimeSpan.FromSeconds(300),options.IdleTimeout??TimeSpan.FromSeconds(120),options.FirstTokenTimeout??TimeSpan.FromSeconds(300)));
        console.Settings.AttachAdmission(services.GetRequiredService<RequestAdmission>(),services.GetRequiredService<PreparationAdmission>());
        console.Settings.AttachRuntime(options.SystemPrompts??new(),options.Affinity??new(),services.GetRequiredService<SessionAffinity>());
        if(options.Credentials is not null)console.Settings.AttachCredits(options.Credentials);
        void Bind(string id,ScheduledTaskState? state){if(state is not null)console.TaskJournal.Attach(id,state);}
        var maintenance=services.GetRequiredService<ConsoleMaintenanceWorker>();
        Bind("config_reload",maintenance.ConfigState);Bind("log_cleanup",maintenance.CleanupState);
        Bind("credential_reload",services.GetService<CredentialReloadWorker>()?.TaskState);
        Bind("token_check",services.GetService<CredentialMaintenanceWorker>()?.TaskState);
        Bind("quota_scan",services.GetService<AccountResourceWorker>()?.TaskState);
        Bind("model_catalog",services.GetService<CatalogRefresher>()?.TaskState);
        Bind("daily_checkin",services.GetService<DailyCheckinWorker>()?.TaskState);
        Bind("keepalive",services.GetService<KeepaliveWorker>()?.TaskState);
        Bind("keepalive_catchup",services.GetService<KeepaliveWorker>()?.CatchupState);
        Bind("price_probe",services.GetService<ModelPriceProbeWorker>()?.TaskState);
        if(services.GetService<RuntimeCheckpointWorker>() is{} checkpoint)
        {
            Bind("status_snapshot",checkpoint.TaskState);
            if(options.ConsoleDirectory is{} directory)checkpoint.SaveStatus=()=>
            {
                try
                {
                    var status=GatewayJson.Encode(ConsoleApi.Status(options, console));
                    if(status.Length>4<<20)return false;
                    ConsoleSettings.Atomic(Path.Combine(directory,"status.json"),status);return console.TaskJournal.Save();
                }
                catch(Exception e)when(e is IOException or UnauthorizedAccessException or KeyNotFoundException or InvalidOperationException or AccountMaintenance.CredentialFailure){return false;}
            };
        }
    }
}
