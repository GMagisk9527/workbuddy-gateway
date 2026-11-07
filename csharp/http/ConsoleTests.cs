using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;
using Microsoft.AspNetCore.Http;

namespace HttpMigration;

internal static class ConsoleTests
{
    static int checks;
    static void Check(bool condition,string message){if(!condition)throw new InvalidOperationException(message);checks++;}
    sealed class Control:IAccountControlPlane,IAccountResourcesControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken ct)=>Task.FromResult(c);
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken ct)=>Task.FromResult(CredentialVerification.Usable);
        public Task<AccountResourceObservation> FetchResourcesAsync(AccountCredential c,CancellationToken ct,bool fetchPackages=true)=>Task.FromResult(new AccountResourceObservation(new(100,20,80),[new("合成积分包",80,DateTimeOffset.UtcNow.AddDays(5).ToUnixTimeSeconds(),true)],true));
    }
    sealed class Upstream:HttpMessageHandler
    {
        internal int Calls;
        internal List<string> Requests=[];
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage r,CancellationToken ct)
        {
            Calls++;
            Requests.Add(await r.Content!.ReadAsStringAsync(ct));
            return new HttpResponseMessage(HttpStatusCode.OK){Content=new StringContent("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"synthetic-output-secret\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0,\"prompt_tokens\":10,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")};
        }
    }
    internal static async Task Run(string scratch,bool preview=false)
    {
        checks=0;Directory.CreateDirectory(scratch);Check(!Directory.EnumerateFileSystemEntries(scratch).Any(),"Empty console scratch");
        var endpoint=SiteEndpoints.Model(AccountSite.Cn);var id=new AccountId("synthetic-account");
        using var file=CredentialFiles.Create(Path.Combine(scratch,"account"),new(new("synthetic-user-secret",Nickname:"测试账号"),"synthetic-access-secret","synthetic-refresh-secret",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600));
        var pool=new AccountPool([new(id,endpoint)]);var manager=new CredentialManager(pool,[new(id,endpoint,file,new Control())]);await manager.FetchResourcesAsync(id);
        string keysDir=Path.Combine(scratch,"keys"),consoleDir=Path.Combine(keysDir,"console");
        using var keys=PreviewKeyDirectory.Open(keysDir,new("synthetic-legacy-secret"));using var ledger=CreditLedger.Open(keysDir,keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);
        var issued=keys.Create(new("合成受管 Key",QuotaTotal:100,MaxConcurrent:4));string token=issued.TakeSecret();
        var catalog=new ModelCatalog();catalog.Load(Encoding.UTF8.GetBytes("{\"schema\":5,\"catalogs\":{\"cn\":[{\"id\":\"test-model\",\"maxOutputTokens\":1024},{\"id\":\"hy3\"}]}}"));
        var upstream=new Upstream();var options=new LocalGatewayOptions(endpoint,new byte[32],Security:new(keys,new("synthetic-ui-admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,UpstreamHandler:upstream,Catalog:catalog,ConsoleDirectory:consoleDir);
        await using var app=LocalGateway.Build(options);await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(10)};
        var console=app.Services.GetRequiredService<ConsoleRuntime>();
        if(preview)
        {
            for(int i=0;i<120;i++)console.Telemetry.Complete(new(id.Value,i%2==0?"hy3":"test-model",DateTimeOffset.UtcNow.AddHours(-i).ToUnixTimeMilliseconds(),200,true,100+i,80+i,20,0,0,500+i*10,100+i,"messages","synthetic-trace-"+i,"","v2"),[]);
            Console.WriteLine(JsonSerializer.Serialize(new{address=LocalGateway.Address(app),admin="synthetic-ui-admin",mode="synthetic-admin-preview"}));await app.WaitForShutdownAsync();return;
        }
        async Task<JsonElement> Get(string path,string auth="synthetic-ui-admin",int status=200)
        {using var r=new HttpRequestMessage(HttpMethod.Get,path);r.Headers.Authorization=new("Bearer",auth);using var response=await client.SendAsync(r);Check((int)response.StatusCode==status,"GET "+path+" status "+(int)response.StatusCode);using var doc=JsonDocument.Parse(await response.Content.ReadAsStringAsync());return doc.RootElement.Clone();}
        async Task<JsonElement> Post(string path,object body,int status=200,string auth="synthetic-ui-admin")
        {using var r=new HttpRequestMessage(HttpMethod.Post,path){Content=JsonContent.Create(body)};r.Headers.Authorization=new("Bearer",auth);using var response=await client.SendAsync(r);Check((int)response.StatusCode==status,"POST "+path+" status "+(int)response.StatusCode);using var doc=JsonDocument.Parse(await response.Content.ReadAsStringAsync());return doc.RootElement.Clone();}
        foreach(string route in new[]{"status","models","stats","performance","logs","account-alerts","config","tasks","settings","log-settings","rate-detail"}){await Get("/admin/api/"+route,token,401);await Get("/admin/api/"+route);}
        var tasks=await Get("/admin/api/tasks");var taskRows=tasks.GetProperty("tasks").EnumerateArray().ToArray();
        Check(taskRows.Length==11&&tasks.GetProperty("source").GetString()=="process_execution_state","Task API uses actual process state and Go task names");
        Check(taskRows.Where(r=>!r.GetProperty("enabled").GetBoolean()).All(r=>r.GetProperty("success").ValueKind==JsonValueKind.Null&&r.GetProperty("lastRunAt").GetInt64()==0),"Disabled workers never show success or historical timestamps");
        Check(taskRows.Single(r=>r.GetProperty("id").GetString()=="log_cleanup").GetProperty("enabled").GetBoolean(),"Hourly log cleanup is hosted");
        await Post("/admin/api/tasks",new{},405);
        using(var html=await client.GetAsync("/ui/")){string raw=await html.Content.ReadAsStringAsync();Check(raw.Contains("panel-overview")&&raw.Contains("panel-keys")&&raw.Contains("panel-tasks")&&raw.Contains("/ui/app.js?v="),"Original console panels and versioned assets");Check(html.Headers.GetValues("Content-Security-Policy").Single().Contains("script-src 'self'"),"Self-hosted CSP");}
        foreach(string asset in new[]{"app.js","style.css"})
        {using var response=await client.GetAsync("/ui/"+asset);Check(response.IsSuccessStatusCode&&response.Content.Headers.ContentType?.MediaType==(asset=="app.js"?"text/javascript":"text/css"),"Embedded asset MIME");using var request=new HttpRequestMessage(HttpMethod.Get,"/ui/"+asset);request.Headers.IfNoneMatch.Add(response.Headers.ETag!);using var cached=await client.SendAsync(request);Check(cached.StatusCode==HttpStatusCode.NotModified,"Asset conditional cache");}
        var settings=await Get("/admin/api/settings");Check(!settings.GetProperty("capabilities").GetProperty("accountLogin").GetBoolean()&&!settings.GetProperty("capabilities").GetProperty("portHotReload").GetBoolean(),"Capabilities honest");
        var list=await Get("/admin/api/keys");Check(list.GetProperty("items").GetArrayLength()==2&&list.GetProperty("summary").GetProperty("total").GetInt32()==2,"Managed and legacy keys shown");
        Check(list.GetProperty("items")[0].GetProperty("status").GetString()=="active"&&list.GetProperty("items")[1].GetProperty("legacy").GetBoolean(),"Key status and legacy read-only");
        var before=console.Settings.View();await Post("/admin/api/model-toggle",new{target="test-model",enabled=false});Check(!catalog.Allows("test-model"),"Model disable applies");
        await Post("/v1/messages",new{model="test-model",messages=new[]{new{role="user",content="synthetic-request-secret"}}},403,token);Check(upstream.Calls==0&&ledger.Health.PendingAttempts==0,"Disabled model never calls upstream or starts billing");
        await Post("/admin/api/config-edit",new{acknowledged=true,content=before.Content,revision=before.Revision},409);Check(!catalog.Allows("test-model"),"CAS conflict preserves policy");
        await Post("/admin/api/model-toggle",new{target="test-model",enabled=true});
        var runtimeConfig=System.Text.Json.Nodes.JsonNode.Parse(console.Settings.View().Content)!;
        runtimeConfig["systemPrompt"]=new System.Text.Json.Nodes.JsonObject{["force"]="synthetic-config-force"};
        runtimeConfig["tokenEstimation"]=new System.Text.Json.Nodes.JsonObject{["modelMultipliers"]=new System.Text.Json.Nodes.JsonObject{["test-model"]=2}};
        await Post("/admin/api/config-edit",new{acknowledged=true,content=runtimeConfig.ToJsonString(),revision=console.Settings.View().Revision});
        var tokenBody=new{model="test-model",messages=new[]{new{role="user",content="abcd"}}};
        var counted=await Post("/v1/messages/count_tokens",tokenBody,200,token);
        Check(counted.GetProperty("input_tokens").GetInt32()==4,"HTTP count_tokens sees hot-reloaded multiplier without model call");
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})
        {object body=path.EndsWith("responses",StringComparison.Ordinal)?new{model="test-model",input="synthetic-request-secret"}:(object)new{model="test-model",messages=new[]{new{role="user",content="synthetic-request-secret"}}};await Post(path,body,200,token);}
        Check(upstream.Calls==3&&ledger.GetUsage(issued.Item.Id).Requests==3&&ledger.GetUsage(issued.Item.Id).ZeroCost==3&&ledger.Health.PendingAttempts==0,"Telemetry preserves exact settlement for three protocols");
        Check(upstream.Requests.All(body=>body.Split("synthetic-config-force").Length==2),"Hot-reloaded prompt reaches all three actual HTTP protocols exactly once");
        Check(console.Telemetry.Rows().Count(r=>r.Complete)==3&&console.Telemetry.Rows().Where(r=>r.Complete).All(r=>r.TtftMs.HasValue&&r.TotalTokens==11&&r.Credit==0),"Actual first token and known-zero usage recorded");
        var logs=await Get("/admin/api/logs");string log=logs.GetRawText();foreach(string secret in new[]{"synthetic-user-secret","synthetic-access-secret","synthetic-request-secret","synthetic-output-secret",token})Check(!log.Contains(secret),"Request log contains no secret");
        var stats=await Get("/admin/api/stats?period=hour");Check(stats.GetProperty("buckets")[0].GetProperty("start").GetInt64()>0,"Absolute chart bucket time");
        await Get("/admin/api/stats?start=2020-01-01&end=2026-01-01",status:400);await Get("/admin/api/logs?file=../../credential.json",status:400);
        await Post("/admin/api/account-add",new{},409);await Post("/admin/api/settings",new{apiKeyEnabled=false},409);Check(keys.RequireAuth,"Unsupported settings never disable auth");
        await Post("/admin/api/account-toggle",new{target=id.Value,enabled=false});Check(!pool.SchedulingEnabled(id)&&!file.Load().Disabled,"Manual scheduling toggle does not revoke credentials");await Post("/admin/api/account-toggle",new{target=id.Value,enabled=true});Check(pool.SchedulingEnabled(id)&&!file.Load().Disabled,"Scheduling toggle restores policy without refreshing credentials");
        await Post("/admin/api/accounts",new{action="refresh_resources",id=id.Value});
        var revision=console.Settings.View();await Post("/admin/api/config-edit",new{acknowledged=true,revision=revision.Revision,content="{\"gateway\":{\"apiKeyEnabled\":false}}"},400);Check(console.Settings.View()==revision,"Unsupported config fields never publish");
        string config=Path.Combine(consoleDir,"config.json");File.Move(config,config+".saved");Directory.CreateDirectory(config);await Post("/admin/api/model-toggle",new{target="hy3",enabled=false},503);Check(catalog.Allows("hy3")&&console.Settings.View()==revision,"Failed config write leaves memory unchanged");Directory.Delete(config);File.Move(config+".saved",config);
        await Post("/admin/api/model-toggle",new{target="hy3",enabled=false});Check(console.Telemetry.Save(),"History saved");await app.StopAsync();await app.DisposeAsync();
        using(var restored=new ConsoleSettings(catalog,consoleDir)){Check(!catalog.Allows("hy3"),"Policy persists on restart");var history=new ConsoleTelemetry(restored,[]);Check(history.Rows().Count(r=>r.Complete)==3,"Request history survives restart");}
        HistoryChecks(Path.Combine(scratch,"history"));Console.WriteLine(JsonSerializer.Serialize(new{consoleChecks=checks,realNetworkCalls=0,modelCreditsUsed=0}));
    }
    static void HistoryChecks(string root)
    {
        using var settings=new ConsoleSettings(new(),root);long now=DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
        File.WriteAllText(Path.Combine(root,"history-seed.json"),JsonSerializer.Serialize(new{schema=1,requests=new[]{new{account="old",model="hy3",at=now,status=200,complete=true,latencyMs=10,ttftMs=1}}}));
        var imported=new ConsoleTelemetry(settings,[new("new","test.json","cn")],new Dictionary<string,string>{{"old","new"}});Check(imported.Rows().Single().Account=="new","Legacy identity maps into owned account");
        var stats=JsonSerializer.SerializeToElement(imported.Stats(new QueryCollection()));Check(stats.GetProperty("summary").GetProperty("ttftSamples").GetInt32()==0,"Unknown legacy TTFT never reclassified");Check(imported.Save(),"Seed persisted");
        string history=Path.Combine(root,"history.json");File.WriteAllText(history,"{\"schema\":1,\"requests\":[{\"account\":\"a\",\"model\":\"m\",\"at\":"+now+",\"latencyMs\":1},{}]}");string bad=File.ReadAllText(history);
        var corrupt=new ConsoleTelemetry(settings,[]);Check(corrupt.Storage=="invalid"&&corrupt.Rows().Length==0&&!corrupt.Save()&&File.ReadAllText(history)==bad,"Corrupt history never partially loads or overwrites");
        var memory=new ConsoleTelemetry(settings,[]);for(int i=0;i<20100;i++)memory.Complete(new("a","m",now,200,true,null,null,null,null,null,1,null),[]);Check(memory.Rows().Length==20000&&memory.Logs(2000).Length==1000,"History and logs bounded");
        var trace=memory.Begin("messages","bounded");trace.Model("m");trace.Attempt("a");trace.Observe("{\"credit\":1e308,\"prompt_tokens\":9e18,\"completion_tokens\":9e18}","bad\ncode",200,true);trace.End(200);Check(memory.Rows().Length==20000,"Extreme metadata cannot throw or grow history");
    }
}
