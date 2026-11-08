namespace HttpMigration;

internal static class ScheduledTasksView
{
    internal static object Build(IServiceProvider services,LocalGatewayOptions o)
    {
        object Row(string id,string name,TimeSpan? interval,ScheduledTaskState? tracker,string description,bool supported=true,bool oneShot=false)
        {
            bool enabled=interval is not null&&tracker is not null;var s=tracker?.Snapshot??new();
            return new Dictionary<string, object?>
{
    ["id"] = id,
    ["name"] = name,
    ["enabled"] = enabled,
    ["supported"] = supported,
    ["oneShot"] = oneShot,
    ["intervalSeconds"] = (long)(interval?.TotalSeconds ?? 0),
    ["schedule"] = interval is { } span ? $"每 {span.TotalSeconds:0} 秒；{description}" : description,
    ["status"] = enabled ? s.Status : "disabled",
    ["success"] = enabled ? s.Success : null,
    ["lastRunAt"] = enabled ? s.LastRunAt : 0,
    ["lastFinishedAt"] = enabled ? s.LastFinishedAt : 0,
    ["nextRunAt"] = enabled ? s.NextRunAt : 0,
    ["nextEstimated"] = enabled && s.NextRunAt > 0,
    ["traceId"] = enabled ? s.TraceId : "",
    ["runs"] = s.Runs,
    ["logFile"] = "tasks.log",
    ["detail"] = !supported ? description : !enabled ? "当前配置未启用" : s.Status == "unknown" ? "本次启动尚无执行记录" : $"已完成 {s.Runs} 轮；本轮处理 {s.Attempted}，成功 {s.Succeeded}，失败 {s.Failed}" + (s.CacheWriteFailed ? "；快照写入失败" : "")
};
        }
        var keepalive=services.GetService<KeepaliveWorker>();var checkin=services.GetService<DailyCheckinWorker>();
        object CalendarRow(string id,string name,bool enabled,ScheduledTaskState? tracker,string schedule,bool oneShot=false,bool blocked=false)
        {var state=tracker?.Snapshot??new();return new Dictionary<string, object?> { ["id"] = id, ["name"] = name, ["enabled"] = enabled, ["supported"] = true, ["oneShot"] = oneShot, ["intervalSeconds"] = 0, ["schedule"] = schedule, ["status"] = blocked?"failed":enabled?state.Status:"disabled", ["success"] = blocked?false:enabled?state.Success:null, ["lastRunAt"] = state.LastRunAt, ["lastFinishedAt"] = state.LastFinishedAt, ["nextRunAt"] = state.NextRunAt, ["nextEstimated"] = false, ["traceId"] = state.TraceId, ["runs"] = state.Runs, ["detail"] = blocked?"签到状态文件不可用；为防止重复请求已暂停":!enabled?"当前配置未启用":$"本轮处理 {state.Attempted}，成功 {state.Succeeded}，失败 {state.Failed}" };}
        return new Dictionary<string, object?>
{
    ["timezone"] = TimeZoneInfo.Local.Id,
    ["generatedAt"] = DateTimeOffset.UtcNow.ToUnixTimeSeconds(),
    ["source"] = "process_execution_state",
    ["truncated"] = false,
    ["tasks"] = new[]
    {
        Row("config_reload", "配置热加载", o.ConsoleDirectory is null ? null : TimeSpan.FromSeconds(1), services.GetService<ConsoleMaintenanceWorker>()?.ConfigState, "控制台配置文件；非法修改保留最后有效配置"),
        Row("credential_reload", "凭据文件扫描", o.CredentialReloadInterval, services.GetService<CredentialReloadWorker>()?.TaskState, "账号清单及凭据变更；非法文件保留当前账号池"),
        Row("status_snapshot", "状态快照写入", o.RuntimeCache is null ? null : TimeSpan.FromSeconds(3), services.GetService<RuntimeCheckpointWorker>()?.TaskState, "调度运行态变更后写入；未变更跳过"),
        Row("token_check", "令牌有效期检查", o.Maintenance?.Interval, services.GetService<CredentialMaintenanceWorker>()?.TaskState, "仅临近过期且可续期账号"),
        CalendarRow("keepalive", "主动续期保活", keepalive is not null, keepalive?.TaskState, "服务器本地小时：" + string.Join("、", o.Calendar?.KeepaliveHours ?? []) + "；账号间隔1秒"),
        CalendarRow("keepalive_catchup", "启动续期补跑", keepalive is not null, keepalive?.CatchupState, "启动90秒后；仅超过20小时未续期账号", true),
        Row("quota_scan", "额度及套餐查询", o.Resources?.Interval, services.GetService<AccountResourceWorker>()?.TaskState, "默认遍历全部有效账号；启动及账号变更时执行"),
        CalendarRow("daily_checkin", "每日签到", checkin is not null, checkin?.TaskState, "北京时间08:00–09:30随机；失败09:30–10:00一次重试；启动待签账号间隔1分钟", blocked: services.GetService<ConsoleRuntime>()?.Checkins?.Blocked ?? false),
        Row("model_catalog", "模型目录刷新", o.CatalogRefreshInterval, services.GetService<CatalogRefresher>()?.TaskState, "按站点刷新；启动即执行"),
        CalendarRow("price_probe", "模型价格探测", o.PriceProbe, services.GetService<ModelPriceProbeWorker>()?.TaskState, "显式开启；启动20秒，待探测2分钟/空闲30分钟，同站点模型12小时限制；可能消耗积分"),
        Row("log_cleanup", "过期日志清理", services.GetService<ConsoleRuntime>()?.Settings.RetentionDays > 0 ? TimeSpan.FromHours(1) : null, services.GetService<ConsoleMaintenanceWorker>()?.CleanupState, "按服务器日期清理请求日志；保留规则变化提前执行；不删除统计或账本")
    }
};
    }
}
