using System.Text.Json;
using System.Text.Json.Serialization;
using KeySecurity;
using LedgerMigration;

namespace HttpMigration;

public sealed record LocalKeySecurity(KeyStore Store, Credential Admin, CreditLedger? Ledger = null);

internal static class KeyManagement
{
    sealed record Command(string Action, string Id = "", string Name = "", double QuotaTotal = 0, double QuotaDaily = 0,
        double QuotaMonthly = 0, long ExpiresAt = 0, string[]? Models = null, int MaxConcurrent = 0, string Note = "", bool? Enabled = null, bool Confirm = false);
    static readonly JsonSerializerOptions Json = new() { PropertyNamingPolicy = JsonNamingPolicy.CamelCase, UnmappedMemberHandling = JsonUnmappedMemberHandling.Disallow };
    static object Item(KeyView k, CreditLedger? ledger = null) => new
    {
        k.Id,
        k.Name,
        k.Prefix,
        masked = k.Prefix + "…",
        k.Enabled,
        k.CreatedAt,
        k.ExpiresAt,
        k.Models,
        maxConcurrent = k.MaxConcurrent,
        k.QuotaTotal,
        k.QuotaDaily,
        k.QuotaMonthly,
        k.Note,
        status=LedgerPresentation.Status(k,ledger),
        legacy=k.Id=="legacy",
        lastUsedAt=ledger?.GetUsage(k.Id).LastUsedAt??0,
        usage = ledger is null ? null : LedgerPresentation.Usage(k, ledger)
    };
    static IEnumerable<object> Items(LocalKeySecurity security)=>security.Store.List().Select(k=>Item(k,security.Ledger)).Concat(security.Store.HasLegacy?[Item(new KeyView("legacy","兼容旧 Key","旧部署 Key",true,0,0,[],0,0,0,0,"由服务配置管理，不在网页中显示或修改"),security.Ledger)]:[]);
    internal static async Task Handle(HttpContext context, LocalKeySecurity security)
    {
        context.Response.Headers.CacheControl = "no-store";
        string header = context.Request.Headers.Authorization.ToString();
        if (!header.StartsWith("Bearer ", StringComparison.Ordinal) || !security.Admin.Matches(header[7..]))
        { await Error(context, 401, "请使用管理 Key 登录，不接受模型 API Key"); return; }
        try
        {
            if (HttpMethods.IsGet(context.Request.Method))
            { await context.Response.WriteAsJsonAsync(new { ok = true, items = Items(security), summary = security.Ledger is null ? null : LedgerPresentation.Summary(security.Store, security.Ledger) }, context.RequestAborted); return; }
            if (!HttpMethods.IsPost(context.Request.Method)) { await Error(context, 405, "仅支持 GET/POST"); return; }
            byte[] body = await LocalGateway.ReadBounded(context.Request.Body, 64 << 10, context.RequestAborted);
            var input = JsonSerializer.Deserialize<Command>(body, Json) ?? throw new JsonException();
            if (input.Action is "rotate" or "update" or "toggle" or "delete" or "reset" && string.IsNullOrEmpty(input.Id))
                throw new KeyFailure(400, "invalid_request", "缺少有效 id");
            var options = new KeyOptions(input.Name, input.QuotaTotal, input.QuotaDaily, input.QuotaMonthly, input.ExpiresAt, input.Models, input.MaxConcurrent, input.Note);
            switch (input.Action)
            {
                case "create":
                    var created = security.Store.Create(options);
                    await context.Response.WriteAsJsonAsync(new { ok = true, key = created.TakeSecret(), item = Item(created.Item, security.Ledger), notice = "完整密钥只显示这一次。" }, context.RequestAborted); return;
                case "rotate":
                    if (!input.Confirm) throw new KeyFailure(400, "confirmation_required", "重新生成会让旧密钥立即失效，请确认后重试");
                    var rotated = security.Store.Rotate(input.Id);
                    await context.Response.WriteAsJsonAsync(new { ok = true, key = rotated.TakeSecret(), item = Item(rotated.Item, security.Ledger), notice = "旧密钥已立即失效；完整密钥只显示这一次。" }, context.RequestAborted); return;
                case "update":
                    var updated = security.Store.Update(input.Id, options);
                    await context.Response.WriteAsJsonAsync(new { ok = true, item = Item(updated, security.Ledger) }, context.RequestAborted); return;
                case "toggle":
                    if (input.Enabled is null) throw new KeyFailure(400, "invalid_request", "缺少 enabled");
                    security.Store.SetEnabled(input.Id, input.Enabled.Value); break;
                case "delete":
                    if (!input.Confirm) throw new KeyFailure(400, "confirmation_required", "删除不可撤销，请确认后重试");
                    security.Store.Delete(input.Id); break;
                case "reset":
                    if (!input.Confirm) throw new KeyFailure(400, "confirmation_required", "重置用量请确认后重试");
                    _ = security.Store.GetView(input.Id);
                    if (security.Ledger is null) throw new KeyFailure(503, "ledger_unavailable", "账本尚未接入");
                    security.Ledger.Reset(input.Id); break;
                default: throw new KeyFailure(400, "invalid_request", "未知操作");
            }
            await context.Response.WriteAsJsonAsync(new { ok = true, items = Items(security) }, context.RequestAborted);
        }
        catch (JsonException) { await Error(context, 400, "请求体格式错误"); }
        catch (RequestFailure e) { await Error(context, e.Status, e.Message); }
        catch (BadHttpRequestException e) { await Error(context, e.StatusCode, "请求体格式错误"); }
        catch (KeyFailure e) { await Error(context, e.Status, e.Message); }
        catch (System.Security.Cryptography.CryptographicException) { await Error(context, 503, "系统随机源不可用"); }
    }
    static Task Error(HttpContext context, int status, string message)
    {
        context.Response.StatusCode = status;
        return context.Response.WriteAsJsonAsync(new { ok = false, error = message }, context.RequestAborted);
    }
}
