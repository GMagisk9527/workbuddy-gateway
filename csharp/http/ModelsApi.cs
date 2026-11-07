using KeySecurity;

namespace HttpMigration;

internal static class ModelsApi
{
    internal static async Task Handle(HttpContext c,SemaphoreSlim gate,LocalKeySecurity security,ModelCatalog catalog)
    {
        c.Response.Headers.CacheControl="no-store";
        bool admitted=false;
        try
        {
            using var lease=security.Store.Acquire(KeyRules.Token(c.Request.Headers.Authorization.ToString(),c.Request.Headers["x-api-key"].ToString()));
            if(!HttpMethods.IsGet(c.Request.Method)){c.Response.StatusCode=405;return;}
            if(!await gate.WaitAsync(0,c.RequestAborted))throw new KeyFailure(503,"request_queue_full","网关并发已满，请稍后重试");
            admitted=true;
            var models=lease.Identity is {Legacy:false} id?security.Store.GetView(id.Id).Models:[];
            c.Response.Headers["X-Model-Source"]=catalog.Source;
            await GatewayJson.WriteAsync(c, catalog.List(m=>KeyRules.Allows(models,m)));
        }
        catch(KeyFailure e)
        {
            c.Response.StatusCode=e.Status;
            await GatewayJson.WriteAsync(c, new Dictionary<string, object?> { ["error"] = new Dictionary<string, object?> { ["type"] = e.Code, ["message"] = e.Message } });
        }
        finally { if(admitted)gate.Release(); }
    }
}
