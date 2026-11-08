using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using KeySecurity;
using LedgerMigration;

namespace HttpMigration;

internal static class AgentToolApi
{
    internal static async Task Handle(HttpContext c,HttpClient client,SemaphoreSlim gate,PreparationAdmission resources,LocalGatewayOptions options,bool fetch)
    {
        c.Response.Headers.CacheControl="no-store";
        c.Response.Headers["X-Trace-ID"]=Guid.NewGuid().ToString("N");
        string model=fetch?"web-fetch":"web-search";
        bool sent=false,settled=false,cancelSafe=false,restricted=false;RequestAdmission.Lease? admission=null;
        int upstreamStatus=0;string? evidence=null;
        KeyLease? key=null;AccountLease? account=null;CredentialLease? credential=null;LedgerAttempt? attempt=null;
        using var memory=resources.Acquire();using var outputMemory=resources.Acquire();
        var ledger=options.Security!.Ledger!;
        void Settle()
        {
            if(attempt is null||settled)return;settled=true;
            if(!sent||cancelSafe&&evidence is null)ledger.CancelUncharged(attempt);
            else ledger.Complete(attempt,evidence,upstreamStatus==0?502:upstreamStatus);
        }
        async Task Error(int status,string code,string message)
        {
            // Finish accounting before exposing the terminal response to the caller.
            Settle();
            if(c.RequestAborted.IsCancellationRequested){c.Abort();return;}
            c.Response.StatusCode=status;
            if(status is 429 or 503)c.Response.Headers.RetryAfter="1";
            if(status==413&&c.Request.Protocol.StartsWith("HTTP/1",StringComparison.Ordinal))c.Response.Headers.Connection="close";
            await GatewayJson.WriteAsync(c, new Dictionary<string, object?> { ["error"] = new Dictionary<string, object?> { ["type"] = "upstream_error", ["code"] = code, ["message"] = message } });
        }
        try
        {
            key=options.Security.Store.Acquire(KeyRules.Token(c.Request.Headers.Authorization.ToString(),c.Request.Headers["x-api-key"].ToString()));
            if(!HttpMethods.IsPost(c.Request.Method)){await Error(405,"method_not_allowed","仅支持 POST 请求");return;}
            admission=await c.RequestServices.GetRequiredService<RequestAdmission>().AcquireAsync(key.Identity?.Id??"",c.RequestAborted);
            byte[] input=await LocalGateway.ReadBody(c,Math.Min(options.MaxRequestBytes,AgentToolProtocol.Limit),memory);
            byte[] payload=AgentToolProtocol.Prepare(input,fetch);memory.Reserve(payload.Length);
            using var original=JsonDocument.Parse(input);
            var conversation=ConversationHeaders.Prepare(c.Request.Headers,original.RootElement,key.Identity?.Id??"");
            key.CheckModel(model);
            admission.FinishPreparing();
            using var deadline=CancellationTokenSource.CreateLinkedTokenSource(c.RequestAborted);deadline.CancelAfter(TimeSpan.FromSeconds(fetch?30:20));
            var attempted=new HashSet<AccountId>();int lastStatus=503;string lastCode="upstream_error";
            for(int tries=0;tries<Math.Min(3,options.Accounts!.Count);tries++)
            {
            key.CheckModel(model);
            options.Catalog?.RefreshPricing();
            try{account=await options.Accounts.AcquireAsync(model,attempted:attempted,cancellation:deadline.Token);}
            catch(SchedulingFailure)when(attempted.Count>0){await Error(lastStatus,lastCode,"上游工具接口拒绝请求");return;}
            attempted.Add(account.Account.Id);
            credential=await options.Credentials!.AcquireForHeadersAsync(account,deadline.Token);
            options.Accounts.Revalidate(account,model);
            if(account.Account.Upstream!=SiteEndpoints.Model(account.Account.Site))throw new CredentialFailure("credential_endpoint_mismatch");
            using var request=new HttpRequestMessage(HttpMethod.Post,SiteEndpoints.AgentTool(account.Account.Site,fetch));
            credential.ApplyToAgentTool(request,fetch);
            request.Headers.UserAgent.ParseAdd("WorkBuddy-Gateway-CSharp/0.1");request.Headers.Add("X-Trace-ID",c.Response.Headers["X-Trace-ID"].ToString());
            conversation.Apply(request);
            request.Content=new ByteArrayContent(payload);request.Content.Headers.ContentType=new("application/json");
            request.Headers.Accept.ParseAdd("application/json");
            if(key.Identity is {} identity)attempt=ledger.Begin(identity.Id);
            deadline.Token.ThrowIfCancellationRequested();
            HttpResponseMessage response;
            try{sent=true;response=await client.SendAsync(request,HttpCompletionOption.ResponseHeadersRead,deadline.Token);}
            finally{credential.Dispose();credential=null;}
            using(response)
            {
                upstreamStatus=(int)response.StatusCode;
                // Do not assume a truncated response has no usage or is a safe cancellation.
                byte[] raw;
                try{raw=await LocalGateway.ReadBounded(await response.Content.ReadAsStreamAsync(deadline.Token),AgentToolProtocol.Limit,deadline.Token,outputMemory.Reserve);}
                catch(RequestFailure e) when(e.Status==413){throw new RequestFailure("上游工具响应超过上限",502,"upstream_error");}
                cancelSafe=upstreamStatus is >=400 and <500;
                using var doc=ParseResponse(raw,upstreamStatus);var root=doc.RootElement;
                if(root.ValueKind!=JsonValueKind.Object)throw new RequestFailure("解析上游工具响应失败",502,"upstream_error");
                var usage=JsonFields.Get(root,"usage");if(usage.ValueKind==JsonValueKind.Object)evidence=usage.GetRawText();
                string? code=AgentToolProtocol.Code(root);
                if(!response.IsSuccessStatusCode||code is not (null or "0"))
                {
                    cancelSafe=upstreamStatus is >=400 and <500||code is "11128" or "11140";
                    if(code=="11140"){restricted=true;options.Credentials.Block(account.Account.Id);}
                    Settle();
                    int status=upstreamStatus<400?502:upstreamStatus;
                    // An explicit HTTP rejection with no usage is the only paid-tool
                    // response that permits failover. Unknown/network/5xx outcomes and
                    // channel/restriction codes never replay across accounts.
                    if(evidence is null&&upstreamStatus is 401 or 403 or 429&&code is not("11128" or "11140")&&tries+1<Math.Min(3,options.Accounts.Count))
                    {
                        if(upstreamStatus==429)options.Accounts.ObserveModelFailure(account.Account.Id,model,429,code);
                        if(upstreamStatus==401)await options.Credentials.DisableAsync(account.Account.Id);
                        lastStatus=status;lastCode=code is not(null or "0")?code:"upstream_error";
                        account.Dispose();account=null;attempt=null;settled=false;sent=false;cancelSafe=false;evidence=null;upstreamStatus=0;
                        continue;
                    }
                    await Error(status,code is not (null or "0")?code:"upstream_error","上游工具接口拒绝请求");return;
                }
                object result=AgentToolProtocol.Project(root,fetch);
                Settle();await GatewayJson.WriteAsync(c, result);
                return;
            }
            }
            await Error(lastStatus,lastCode,"暂无可用账号");
        }
        catch(RequestFailure e){await Error(e.Status,e.ErrorType,e.Message);}
        catch(KeyFailure e){await Error(e.Status,e.Code,e.Message);}
        catch(SchedulingFailure e){await Error(503,e.Code,"暂无可用账号");}
        catch(CredentialFailure e){await Error(503,e.Code,"账号凭据暂时不可用");}
        catch(BadHttpRequestException e){await Error(e.StatusCode,"invalid_request_error","无效的请求体");}
        catch(InvalidDataException){await Error(400,"invalid_request_error","无效的压缩请求体");}
        catch(JsonException){await Error(502,"upstream_error","解析上游工具响应失败");}
        catch(InvalidOperationException e) when(e.Message.Contains("UTF",StringComparison.Ordinal)){await Error(502,"upstream_error","解析上游工具响应失败");}
        catch(OperationCanceledException) when(c.RequestAborted.IsCancellationRequested){c.Abort();}
        catch(OperationCanceledException){await Error(504,"upstream_timeout","上游工具请求超时");}
        catch(HttpRequestException){await Error(502,"upstream_error","上游工具连接失败");}
        catch(IOException){await Error(502,"upstream_error","上游工具连接中断");}
        finally
        {
            try{Settle();}
            finally
            {
                try{if(restricted&&account is not null)await options.Credentials!.DisableAsync(account.Account.Id);}
                finally{credential?.Dispose();account?.Dispose();key?.Dispose();admission?.Dispose();}
            }
        }
    }
    static JsonDocument ParseResponse(byte[] raw,int status)
    {
        try{return JsonDocument.Parse(raw,new JsonDocumentOptions{MaxDepth=256});}
        catch(JsonException){throw new RequestFailure(status>=400?"上游工具接口拒绝请求":"解析上游工具响应失败",status>=400?status:502,"upstream_error");}
    }
}
