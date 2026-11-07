using System.Text.Json;
using KeySecurity;

namespace HttpMigration;

internal static class CountTokensApi
{
    internal static async Task Handle(HttpContext c,SemaphoreSlim gate,PreparationAdmission resources,LocalGatewayOptions options)
    {
        c.Response.Headers.CacheControl="no-store";
        bool admitted=false,preparing=false;using var memory=resources.Acquire();RequestAdmission.Lease? admission=null;
        async Task Error(int status,string type,string message)
        {
            c.Response.StatusCode=status;
            if(status is 429 or 503)c.Response.Headers.RetryAfter="1";
            // An oversized unread HTTP/1 body cannot be safely drained by Kestrel.
            if(status==413&&c.Request.Protocol.StartsWith("HTTP/1",StringComparison.Ordinal))c.Response.Headers.Connection="close";
            await GatewayJson.WriteAsync(c, new Dictionary<string, object?> { ["type"] = "error", ["error"] = new Dictionary<string, object?> { ["type"] = type, ["message"] = message } });
        }
        try
        {
            using var key=options.Security!.Store.Acquire(KeyRules.Token(c.Request.Headers.Authorization.ToString(),c.Request.Headers["x-api-key"].ToString()));
            if(!HttpMethods.IsPost(c.Request.Method)){await Error(405,"api_error","仅支持 POST 请求");return;}
            if(options.DirectUpstream)admission=await c.RequestServices.GetRequiredService<RequestAdmission>().AcquireAsync(key.Identity?.Id??"",c.RequestAborted);
            else{
            if(!await gate.WaitAsync(0,c.RequestAborted))throw new RequestFailure("网关并发已满，请稍后重试",503,"overloaded_error");
            admitted=true;
            if(!await resources.Preparing.WaitAsync(0,c.RequestAborted))throw new RequestFailure("请求准备队列已满",503,"overloaded_error");
            preparing=true;
            }
            byte[] body=await LocalGateway.ReadBody(c,options.MaxRequestBytes,memory);
            using var doc=JsonDocument.Parse(body,new JsonDocumentOptions{MaxDepth=256});
            string model=TokenEstimation.Validate(doc.RootElement);key.CheckModel(model);
            int count=TokenEstimation.Estimate(doc.RootElement);
            if(options.DirectUpstream)count=(int)Math.Min(int.MaxValue,Math.Ceiling(count*c.RequestServices.GetRequiredService<ConsoleRuntime>().Settings.TokenMultiplier(model)));
            await GatewayJson.WriteAsync(c, new Dictionary<string, object?> { ["input_tokens"] = count });
        }
        catch(RequestFailure e){await Error(e.Status,e.ErrorType,e.Message);}
        catch(KeyFailure e){await Error(e.Status,e.Status switch{401=>"authentication_error",403=>"permission_error",429=>"rate_limit_error",_=>"overloaded_error"},e.Message);}
        catch(JsonException){await Error(400,"invalid_request_error","无效的 JSON 请求体");}
        catch(InvalidDataException){await Error(400,"invalid_request_error","无效的压缩请求体");}
        catch(BadHttpRequestException e){await Error(e.StatusCode,e.StatusCode==413?"request_too_large":"invalid_request_error","无效的请求体");}
        catch(OperationCanceledException) when(c.RequestAborted.IsCancellationRequested){c.Abort();}
        catch(OperationCanceledException){await Error(408,"api_error","读取请求超时");}
        catch(IOException){c.Abort();}
        finally{admission?.Dispose();if(preparing)resources.Preparing.Release();if(admitted)gate.Release();}
    }
}
