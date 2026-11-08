using System.Text.Json;
using System.Text.Json.Nodes;
using AccountScheduling;
using AccountMaintenance;
using AccountLogin;

namespace HttpMigration;

internal static class ConsoleApi
{
    static long Now=>DateTimeOffset.UtcNow.ToUnixTimeSeconds();
    static async Task Error(HttpContext c,int status,string text){c.Response.StatusCode=status;await GatewayJson.WriteAsync(c, new Dictionary<string, object?> { ["ok"] = false, ["error"] = text });}
    static string Text(JsonElement r,string name)=>r.TryGetProperty(name,out var v)&&v.ValueKind==JsonValueKind.String?v.GetString()!:"";
    static async Task<JsonDocument> Body(HttpContext c,int limit=65536)
    {using var timeout=CancellationTokenSource.CreateLinkedTokenSource(c.RequestAborted);timeout.CancelAfter(TimeSpan.FromSeconds(5));return JsonDocument.Parse(await LocalGateway.ReadBounded(c.Request.Body,limit,timeout.Token),new JsonDocumentOptions{MaxDepth=16});}
    internal static object Settings(LocalGatewayOptions o)=>new Dictionary<string, object?>
{
    ["apiKeyEnabled"] = o.Security!.Store.RequireAuth,
    ["apiKeyConfigured"] = o.Security.Store.HasLegacy,
    ["apiPort"] = 10717,
    ["webPort"] = 10716,
    ["configuredApiPort"] = 10717,
    ["configuredWebPort"] = 10716,
    ["restartRequired"] = false,
    ["apiPortOverridden"] = true,
    ["capabilities"] = new Dictionary<string, object?>
    {
        ["accountLogin"] = o.AccountOwner?.CanReload == true,
        ["accountDelete"] = o.AccountOwner?.CanReload == true,
        ["portHotReload"] = false,
        ["legacyKeyEdit"] = false,
        ["accountRefresh"] = true,
        ["configEdit"] = o.ConsoleDirectory is not null,
        ["modelToggle"] = true
    },
    ["note"] = "端口和旧部署Key由Nginx/OpenRC管理；受管Key请在API Key页管理。"
};
    internal static async Task Handle(HttpContext c,LocalGatewayOptions o,ConsoleRuntime console,SemaphoreSlim accountGate)
    {
        c.Response.Headers.CacheControl="no-store";
        string route=c.Request.Path.Value?.Split('/').Last()??"";bool get=HttpMethods.IsGet(c.Request.Method),post=HttpMethods.IsPost(c.Request.Method);
        if(route=="setup"&&get){await GatewayJson.WriteAsync(c, new Dictionary<string, object?> { ["setupRequired"] = false, ["canInitialize"] = false });return;}
        string auth=c.Request.Headers.Authorization.ToString();if(!auth.StartsWith("Bearer ",StringComparison.Ordinal)||!o.Security!.Admin.Matches(auth[7..])){await Error(c,401,"请使用原管理Key登录，不接受模型API Key");return;}
        try
        {
            if(!get&&!post){await Error(c,405,"仅支持GET/POST");return;}
            object result;
            switch(route)
            {
                case "settings":if(!get){await Error(c,409,"端口和旧部署Key当前由服务配置管理，请使用API Key页管理受管Key");return;}result=Settings(o);break;
                case "status":RequireGet(get);result=Status(o,console);break;
                case "models":RequireGet(get);result=Models(o,console);break;
                case "credentials":RequireGet(get);result=new Dictionary<string, object?> { ["hint"] = "列表不包含令牌明文；仅管理鉴权可主动读取完整凭据。", ["files"] = o.Credentials!.CredentialViews, ["generatedAt"] = Now };break;
                case "credential-edit":
                    if(o.AccountOwner?.CanReload!=true){await Error(c,409,"此模式没有可编辑账号清单");return;}
                    using(var deadline=CancellationTokenSource.CreateLinkedTokenSource(c.RequestAborted))
                    {
                        deadline.CancelAfter(TimeSpan.FromSeconds(30));
                        if(get)result=await o.AccountOwner.ReadCredentialAsync(c.Request.Query["file"].ToString(),deadline.Token);
                        else
                        {
                            using var doc=await Body(c,(256<<10)*6+4096);var r=doc.RootElement;string id=Text(r,"path");
                            string revision=await o.AccountOwner.EditCredentialAsync(id,Text(r,"content"),Text(r,"revision"),deadline.Token);
                            console.Telemetry.Audit("credential_saved");result=new Dictionary<string, object?> { ["path"] = id, ["saved"] = true, ["revision"] = revision };
                        }
                    }break;
                case "stats":RequireGet(get);result=console.Telemetry.Stats(c.Request.Query);break;
                case "performance":RequireGet(get);result=console.Telemetry.Performance(c.Request.Query["trace"]);break;
                case "logs":
                    RequireGet(get);string file=c.Request.Query["file"].ToString();if(file is not("" or "requests.log" or "tasks.log"))throw new ArgumentException("日志文件不存在");int n=int.TryParse(c.Request.Query["lines"],out int number)?number:200;
                    result=new Dictionary<string, object?> { ["file"] = file=="tasks.log"?file:"requests.log", ["files"] = new[]{new Dictionary<string, object?> { ["name"] = "requests.log" },new Dictionary<string, object?> { ["name"] = "tasks.log" }}, ["lines"] = file=="tasks.log"?console.TaskJournal.Lines(n,console.Settings.RetentionDays):console.Telemetry.Logs(n), ["truncated"] = false };break;
                case "account-alerts":RequireGet(get);result=new Dictionary<string, object?> { ["sourceLabel"] = "C#实时账号状态；请求事件见日志页", ["alerts"] = AccountRows(o,console).Where(a=>a.State!="active").Take(128).Select(a=>new Dictionary<string, object?> { ["account"] = a.Path, ["state"] = a.State, ["severity"] = a.State=="disabled"?"important":"warning", ["label"] = a.State=="disabled"?"账号已停用":a.State=="cooldown"?"账号冷却":"账号需关注", ["message"] = a.State=="disabled"?"凭据保留，需通过验证后才可重新启用":"当前状态由调度器实时计算", ["at"] = 0 }).ToArray(), ["stale"] = false };break;
                case "config":RequireGet(get);var view=console.Settings.View();result=new Dictionary<string, object?> { ["exists"] = true, ["path"] = "console/config.json", ["content"] = view.Content, ["revision"] = view.Revision };break;
                case "config-edit":
                    if(get){if(c.Request.Query["acknowledged"]!="1")throw new ArgumentException("请先确认编辑范围");var edit=console.Settings.View();result=new Dictionary<string, object?> { ["content"] = edit.Content, ["revision"] = edit.Revision };}
                    else{using var doc=await Body(c);var r=doc.RootElement;if(!r.GetProperty("acknowledged").GetBoolean())throw new ArgumentException("请确认编辑范围");console.Settings.Update(Text(r,"content"),Text(r,"revision"));console.Telemetry.Audit("config_saved");result=new Dictionary<string, object?> { ["ok"] = true, ["settings"] = Settings(o), ["adminKeyChanged"] = false };}break;
                case "model-toggle":
                    RequirePost(post);using(var doc=await Body(c)){var r=doc.RootElement;string id=Text(r,"target");if(!o.Catalog!.List(includeDisabled:true)["data"]!.AsArray().Any(m=>m!["id"]!.GetValue<string>()==id))throw new ArgumentException("模型不在目录中");console.Settings.Toggle(id,r.GetProperty("enabled").GetBoolean());}console.Telemetry.Audit("model_policy_saved");result=new Dictionary<string, object?> { ["ok"] = true };break;
                case "account-toggle":
                    RequirePost(post);if(!await accountGate.WaitAsync(0)){await Error(c,429,"账号管理忙，请稍后重试");return;}
                    try{using var doc=await Body(c);var r=doc.RootElement;console.Settings.ToggleAccount(Text(r,"target"),r.GetProperty("enabled").GetBoolean());}
                    finally{accountGate.Release();}console.Telemetry.Audit("account_state_saved");result=new Dictionary<string, object?> { ["ok"] = true };break;
                case "log-settings":
                    if(post){using var doc=await Body(c);console.Settings.SetRetention(doc.RootElement.GetProperty("retentionDays").GetInt32());console.Telemetry.Cleanup();console.Telemetry.Audit("log_retention_saved");}var logOptions=console.Settings.Logs;result=new Dictionary<string, object?> { ["retentionDays"] = console.Settings.RetentionDays, ["maxFiles"] = logOptions.MaxFiles, ["maxFileBytes"] = logOptions.MaxFileBytes, ["maxTotalBytes"] = logOptions.MaxTotalBytes, ["sampleFirst"] = logOptions.SampleFirst, ["sampleEvery"] = logOptions.SampleEvery, ["writeFailed"] = console.Telemetry.LogWriteFailed };break;
                case "log-cleanup":RequirePost(post);int deleted=c.RequestServices.GetRequiredService<ConsoleMaintenanceWorker>().Cleanup();result=new Dictionary<string, object?> { ["deleted"] = 0, ["deletedBytes"] = 0, ["skippedActive"] = 0, ["failed"] = 0, ["removedLines"] = deleted };break;
                case "tasks":if(!get){c.Response.Headers.Allow="GET";await Error(c,405,"任务接口只支持GET");return;}result=Tasks(c,o);break;
                case "rate-detail":RequireGet(get);result=o.Catalog!.PriceDetails();break;
                case "client-events":RequirePost(post);using(var doc=await Body(c)){/* UI events are intentionally not copied into logs: they can contain sensitive client text. */}result=new Dictionary<string, object?> { ["ok"] = true };break;
                case "account-delete":
                    RequirePost(post);if(o.AccountOwner?.CanReload!=true){await Error(c,409,"此模式没有可编辑账号清单");return;}
                    using(var doc=await Body(c))
                    {using var deadline=CancellationTokenSource.CreateLinkedTokenSource(c.RequestAborted);deadline.CancelAfter(TimeSpan.FromSeconds(30));await o.AccountOwner.DeleteAsync(Text(doc.RootElement,"file"),deadline.Token);}
                    console.Telemetry.Audit("account_deleted");result=new Dictionary<string, object?> { ["ok"] = true };break;
                case "account-add":case "account-cancel":
                    var login=c.RequestServices.GetService<ConsoleLoginService>();
                    if(login is null){await Error(c,409,"此模式没有可编辑账号清单");return;}
                    if(route=="account-cancel")
                    {RequirePost(post);using var doc=await Body(c);result=new Dictionary<string, object?> { ["ok"] = login.Cancel(Text(doc.RootElement,"id")) };}
                    else if(get)
                    {string id=c.Request.Query["id"].ToString();result=id.Length==0?new Dictionary<string, object?> { ["defaultFile"] = login.SuggestedFile }:login.Find(id)??throw new KeyNotFoundException();}
                    else
                    {using var doc=await Body(c);string site=Text(doc.RootElement,"site");if(site is not("cn" or "intl"))throw new ArgumentException("Invalid site");result=await login.Start(Text(doc.RootElement,"file"),site=="cn"?AccountSite.Cn:AccountSite.Intl,c.RequestAborted);}break;
                default:await Error(c,404,"管理接口不存在");return;
            }
            await GatewayJson.WriteAsync(c, result);
        }
        catch(ConsoleConflict){await Error(c,409,"配置版本已变化，当前输入未写入，请重新加载后核对");}
        catch(LoginFailure e){await Error(c,409,e.Code);}
        catch(KeyNotFoundException){await Error(c,404,"数据不存在或Trace已过期");}
        catch(CredentialFailure e){await Error(c,e.Code=="credential_missing"?404:409,e.Code);}
        catch(Exception e)when(e is ArgumentException or JsonException or InvalidOperationException or OverflowException){await Error(c,400,"输入不合法，修改未生效");}
        catch(Exception e)when(e is IOException or UnauthorizedAccessException){await Error(c,503,"存储不可用，已有配置与数据保留");}
        catch(RequestFailure e){await Error(c,e.Status,"请求体超过限制");}
        catch(OperationCanceledException)when(c.RequestAborted.IsCancellationRequested){c.Abort();}
        catch(OperationCanceledException){await Error(c,504,"读取请求超时");}
    }
    static void RequireGet(bool get){if(!get)throw new ArgumentException("只读接口");}
    static void RequirePost(bool post){if(!post)throw new ArgumentException("仅支持POST");}
    internal sealed record AccountRow(string Path,string DisplayPath,string Nickname,string Edition,string State,bool SchedulingEnabled,string SchedulingReason,long TokenExpiresAt,bool QuotaKnown,bool QuotaUnlimited,double? QuotaTotal,double? QuotaUsed,double? QuotaRemaining,string PlanLabel,bool PlanStale,int ModelCooldowns,long LastRefreshTime,int RefreshFailCount,bool CreditKnown,string CreditError,long CreditFetchedAt,long CreditSoonestAt,double CreditSoonestAmt,object[] CreditPackages,object Checkin);
    static AccountRow[] AccountRows(LocalGatewayOptions o,ConsoleRuntime console)
    {
        var checkins=o.Credentials!.CheckinTargets.ToDictionary(t=>t.Id.Value);
        var snapshots=o.Accounts!.Snapshots().ToDictionary(a=>a.Id);var resources=o.Credentials!.ResourceStatus.ToDictionary(a=>a.Id);var display=o.Credentials.DisplayInfo;
        return o.Credentials.ManagementStatus.Where(s=>snapshots.ContainsKey(new(s.Id))&&display.ContainsKey(new(s.Id))&&checkins.ContainsKey(s.Id)).Select(s=>
        {
            var a=snapshots[new(s.Id)];var resource=resources.GetValueOrDefault(s.Id);var meta=display[new(s.Id)];bool cooling=a.CooldownUntil>DateTimeOffset.UtcNow;string state=s.Disabled?"disabled":s.Degraded||s.ExpiresAt<=Now?"expired":cooling?"cooldown":a.QuotaExhausted?"paid_exhausted":"active";
            return new AccountRow(s.Id,console.Labels.GetValueOrDefault(s.Id)??s.Id,meta.Name,s.Site==AccountSite.Cn?"cn":"intl",state,o.Accounts.SchedulingEnabled(new(s.Id)),o.Accounts.SchedulingEnabled(new(s.Id))?"允许调度":"全局账号名单已停用",s.ExpiresAt>100000000000?s.ExpiresAt/1000:s.ExpiresAt,resource?.Known??false,resource?.Unlimited??false,resource?.Total,resource?.Used,resource?.Remaining,"",resource?.Stale??true,o.Accounts.ModelStatus(new(s.Id))?.CoolingModels??0,meta.LastRefresh,s.Failures,(resource?.PackagesKnown??false)&&resource?.Stale!=true,resource?.Error??"",resource?.FetchedAt??0,resource?.CreditExpiresAt??0,resource?.ExpiringCredit??0,o.Credentials.PackagesFor(new(s.Id)).Select(p=>(object)new Dictionary<string, object?> { ["name"] = p.Name, ["remain"] = p.Remain, ["expiresAt"] = p.ExpiresAt, ["expiryKnown"] = p.ExpiryKnown }).ToArray(),(object?)(console.Checkins?.View(checkins[s.Id]))??new Dictionary<string, object?> { ["status"] = "disabled", ["message"] = "当前配置未开启每日签到" });
        }).ToArray();
    }
    internal static object Status(LocalGatewayOptions o,ConsoleRuntime console)
    {var accounts=AccountRows(o,console);int usable=accounts.Count(a=>a.State=="active"&&a.SchedulingEnabled);return new Dictionary<string, object?> { ["updatedAt"] = Now, ["stale"] = false, ["gateway"] = new Dictionary<string, object?> { ["version"] = "C# 1.0", ["listen"] = "127.0.0.1:"+o.Port, ["modelSource"] = o.Catalog!.Source, ["apiKeyEnabled"] = o.Security!.Store.RequireAuth, ["webuiEnabled"] = true }, ["health"] = new Dictionary<string, object?> { ["percent"] = accounts.Length==0?(double?)null:100.0*usable/accounts.Length, ["state"] = accounts.Length==0?"empty":usable==accounts.Length?"healthy":"degraded" }, ["accounts"] = accounts };}
    static object Models(LocalGatewayOptions o,ConsoleRuntime console)
    {
        var accounts=o.Accounts!.Snapshots();var rows=console.Telemetry.Rows().Where(r=>r.At>=DateTimeOffset.UtcNow.AddHours(-5).ToUnixTimeMilliseconds()).ToArray();
        var models=o.Catalog!.List(includeDisabled:true)["data"]!.AsArray().Select(m=>
        {
            string id=m!["id"]!.GetValue<string>();var history=rows.Where(r=>r.Model==id).ToArray();var sum=ConsoleTelemetry.Aggregate(history);return new Dictionary<string, object?> { ["id"] = id, ["enabled"] = o.Catalog.Allows(id), ["policyReason"] = o.Catalog.Allows(id)?"允许调用":"控制台规则已停用", ["cnMultiplier"] = o.Catalog.PriceLabel(AccountSite.Cn,id), ["intlMultiplier"] = o.Catalog.PriceLabel(AccountSite.Intl,id), ["availableAccounts"] = accounts.Count(a=>o.Accounts.Allows(a.Id,id)&&SchedulingRules.Eligibility(a,id,DateTimeOffset.UtcNow) is not null), ["requests"] = history.Length, ["avgTtftMs"] = sum["avgTtftMs"], ["hasTtft"] = history.Any(r=>r.TtftMs.HasValue), ["ttftMetric"] = "first_token_v2", ["avgLatencyMs"] = sum["avgLatencyMs"], ["hasLatency"] = history.Length>0, ["tokens"] = sum["tokens"], ["lastStatus"] = history.LastOrDefault()?.Code };
        }).ToArray();return new Dictionary<string, object?> { ["models"] = models, ["rateDisplayRevision"] = o.Catalog.RateRevision, ["stale"] = false, ["policyMode"] = "blocklist", ["updatedAt"] = Now };
    }
    static object Tasks(HttpContext c,LocalGatewayOptions o)
    {
        return ScheduledTasksView.Build(c.RequestServices,o);
    }
}
