using System.Net;
using System.Net.Http.Json;
using System.Net.Sockets;
using System.Text;
using System.Text.Json;
using System.Text.Json.Nodes;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;
using KeySecurity;

namespace HttpMigration;

internal static class ConversationTests
{
    static int checks;
    static void Check(bool value,string why){if(!value)throw new InvalidOperationException(why);checks++;}
    sealed record Capture(string Conversation,string Turn,string Message,JsonObject Body);
    sealed class Control:IAccountControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential current,CancellationToken cancellation)=>throw new InvalidOperationException("Unexpected refresh");
        public Task<CredentialVerification> VerifyAsync(AccountCredential candidate,CancellationToken cancellation)=>Task.FromResult(CredentialVerification.Usable);
    }
    sealed class Upstream:HttpMessageHandler
    {
        internal readonly List<Capture> Captured=[];
        internal bool RefuseOnce, FailAfterOutput;
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken cancellation)
        {
            string Header(string name)=>request.Headers.TryGetValues(name,out var values)?values.Single():"";
            var capture=new Capture(Header("X-Conversation-ID"),Header("X-Conversation-Request-ID"),Header("X-Conversation-Message-ID"),
                JsonNode.Parse(await request.Content!.ReadAsByteArrayAsync(cancellation))!.AsObject());
            Captured.Add(capture);
            Check(Guid.TryParse(capture.Conversation,out var c)&&c!=Guid.Empty&&Guid.TryParse(capture.Turn,out var t)&&t!=Guid.Empty&&Guid.TryParse(capture.Message,out var m)&&m!=Guid.Empty,"Three nonzero correlation IDs reach the HTTP upstream");
            Check(!request.Headers.Contains("session_id")&&!request.Headers.Contains("X-Session-ID")&&!request.Headers.Contains("thread-id")&&!request.Headers.Contains("X-Client-ID")&&!request.Headers.Contains("Cookie"),"No raw client session, cookie or official-client identity forwarded");
            if(RefuseOnce){RefuseOnce=false;throw new HttpRequestException(HttpRequestError.ConnectionError,"synthetic refusal",new SocketException((int)SocketError.ConnectionRefused));}
            if(FailAfterOutput)
                return new(HttpStatusCode.OK){Content=new StringContent("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"error\"}]}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")};
            return new(HttpStatusCode.OK){Content=new StringContent("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":1,\"prompt_cache_hit_tokens\":80,\"credit\":0}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")};
        }
    }
    internal static async Task Run(string scratch)
    {
        checks=0;
        UnitChecks();await TransportRetry();
        foreach(var site in new[]{AccountSite.Cn,AccountSite.Intl})
            foreach(bool affinity in new[]{false,true})
                await HttpChecks(Path.Combine(scratch,site+"-"+affinity),site,affinity);
        Console.WriteLine(JsonSerializer.Serialize(new{conversationChecks=checks,realModelCalls=0,productionChanged=false}));
    }
    static void UnitChecks()
    {
        ConversationHeaders Make(HeaderDictionary headers,string body="{}",string caller="caller-a")
        {using var document=JsonDocument.Parse(body);return ConversationHeaders.Prepare(headers,document.RootElement,caller);}
        var first=Make(new(){{"session_id","parent-private"}});
        foreach(string name in new[]{"X-Conversation-ID","thread-id","X-Session-ID","session-id","session_id"})
            Check(Make(new(){{name,"parent-private"}}).Id==first.Id,"Session spelling shares the normal conversation: "+name);
        foreach(string name in new[]{"conversation_id","conversationId","session_id","sessionId"})
        {
            Check(Make(new(),JsonSerializer.Serialize(new Dictionary<string,string>{{name,"parent-private"}})).Id==first.Id,"Top-level identity captured before translation");
            Check(Make(new(),JsonSerializer.Serialize(new{metadata=new Dictionary<string,string>{{name,"parent-private"}}})).Id==first.Id,"Metadata identity captured before translation");
        }
        Check(Make(new(){{"thread-id","child"},{"session-id","parent-private"}}).Id!=first.Id,"Subagent thread takes precedence over a process session");
        var agent=new HeaderDictionary{{"x-claude-code-session-id","parent"},{"x-claude-code-agent-id","child"}};
        var child=Make(agent);agent["x-claude-code-agent-id"]="other";
        Check(child.Id!=Make(agent).Id,"Child agents remain separate");
        Check(Make(new(){{"session_id","parent-private"}},caller:"caller-b").Id!=first.Id,"Caller isolation includes explicit client UUID/text sessions");
        string uuidSession=Guid.NewGuid().ToString();
        Check(Make(new(){{"X-Conversation-ID",uuidSession}}).Id!=Make(new(){{"X-Conversation-ID",uuidSession}},caller:"caller-b").Id,"Explicit UUID session is also scoped by caller");
        var independent=Make(new());Check(independent.Id!=Make(new()).Id&&independent.SessionDigest is null,"No prompt-derived or global default conversation");
        Check(Make(new(){{"session_id","same"}},caller:"").Id!=Make(new(){{"session_id","same"}},caller:"").Id,"Unauthenticated clients cannot create a globally shared conversation");
        var repeated=Make(new(){{"session_id","parent-private"}});
        Check(first.Id==repeated.Id&&first.RequestId!=repeated.RequestId&&first.MessageId!=repeated.MessageId,"New HTTP calls retain the session with fresh default turn/message IDs");
        var explicitIds=new HeaderDictionary{{"session_id","parent-private"},{"X-Conversation-Request-ID","private-turn"},{"X-Conversation-Message-ID","private-message"}};
        var supplied=Make(explicitIds);var same=Make(explicitIds);
        Check(supplied.Id==same.Id&&supplied.RequestId==same.RequestId&&supplied.MessageId==same.MessageId,"Explicit turn/message identifiers remain stable");
        Check(Make(new(),"{\"metadata\":{\"sessionId\":\"parent-private\",\"conversationRequestId\":\"private-turn\",\"conversationMessageId\":\"private-message\"}}").RequestId==supplied.RequestId,"Body turn metadata survives conversion");
        Check(!supplied.ToString().Contains(supplied.Id)&&!supplied.ToString().Contains("private"),"Diagnostic representation is redacted");
        foreach(string seed in new[]{"",new string('a',513),"bad\u0000id"})
            Check(Make(new(),JsonSerializer.Serialize(new{session_id=seed})).SessionDigest is null,"Unusable identity ignored");
        Check(Make(new(),"{\"metadata\":{\"session_id\":\"\\uD800\",\"conversation_request_id\":\"\\uD800\"}}").SessionDigest is null,"Malformed Unicode metadata does not reject an otherwise valid model request");
        var duplicate=new HeaderDictionary{{"session_id",new Microsoft.Extensions.Primitives.StringValues(["one","two"])}};
        Check(Make(duplicate).SessionDigest is null,"Ambiguous duplicate session headers are not forwarded");
        Check(SessionAffinity.CachedTokens("{\"prompt_tokens_details\":{\"cached_tokens\":0},\"prompt_cache_hit_tokens\":32768}")==32768,"Explicit zero alias does not mask positive upstream evidence");
        Check(SessionAffinity.CachedTokens("{\"prompt_cache_hit_tokens\":0,\"input_tokens_details\":{\"cached_tokens\":\"32768\"}}")==32768,"Cache accounting accepts the same fallback fields as protocol/ledger readers");
        foreach(string raw in new[]{"{}","{\"cached_tokens\":-1}","{\"cached_tokens\":\"bad\"}","{\"cached_tokens\":{}}"})
            Check(SessionAffinity.CachedTokens(raw) is null,"Unknown or invalid usage is not cache evidence");

        const string prefix="""{"tools":[{"name":"read"}],"messages":[{"role":"system","content":"sys"},{"role":"user","content":"one"},{"role":"assistant","content":"two"}]}""";
        CacheDiagnostics Measure(string body, string caller="caller", string account="account") =>
            CacheDiagnostics.Read(Encoding.UTF8.GetBytes(body),caller,"session",account,"credential","preferred");
        var baseline=Measure(prefix);
        var appended=Measure(prefix[..^2]+""",{"role":"user","content":"three"}]}""");
        Check(baseline==appended,"Appending turns keeps diagnostic prefix tags stable");
        var changedTool=Measure(prefix.Replace("read","write",StringComparison.Ordinal));
        var changedSystem=Measure(prefix.Replace("\"sys\"","\"new system\"",StringComparison.Ordinal));
        Check(changedTool.Tools!=baseline.Tools && changedTool.SystemPrompt==baseline.SystemPrompt && changedTool.History==baseline.History &&
            changedSystem.SystemPrompt!=baseline.SystemPrompt && changedSystem.Tools==baseline.Tools && changedSystem.History==baseline.History,
            "Tool and system changes are distinguishable");
        var switched=Measure(prefix,account:"another-account");
        var otherCaller=Measure(prefix,caller:"other-caller");
        Check(switched.Account!=baseline.Account && switched.Session==baseline.Session &&
            otherCaller.Session!=baseline.Session && otherCaller.History!=baseline.History,
            "Account changes are visible without linking request fingerprints across callers");
    }
    static async Task TransportRetry()
    {
        using var fake=new Upstream{RefuseOnce=true};using var client=SafeUpstreamSend.CreateClient(fake);
        using var document=JsonDocument.Parse("{\"session_id\":\"retry-session\"}");
        var identity=ConversationHeaders.Prepare(new HeaderDictionary(),document.RootElement,"caller");
        using var template=new HttpRequestMessage(HttpMethod.Post,"http://127.0.0.1/test"){Content=new ByteArrayContent("{}"u8.ToArray())};identity.Apply(template);
        using var result=await SafeUpstreamSend.Run(client,template,"{}"u8.ToArray(),null,1,TimeSpan.FromSeconds(2),default,_=>{},()=>{},()=>{},fake);
        Check(fake.Captured.Count==2&&fake.Captured[0].Conversation==fake.Captured[1].Conversation&&fake.Captured[0].Turn==fake.Captured[1].Turn&&fake.Captured[0].Message==fake.Captured[1].Message,"Definitely-unsent retry preserves all logical request IDs");
    }
    static async Task HttpChecks(string dir,AccountSite site,bool affinity)
    {
        using var files=CredentialFiles.Create(Path.Combine(dir,"account"),new(new("synthetic-user",Site:site),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600));
        var id=new AccountId("synthetic");var endpoint=SiteEndpoints.Model(site);var pool=new AccountPool([new(id,endpoint,site)]);
        var manager=new CredentialManager(pool,[new(id,endpoint,files,new Control())]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(dir,"keys"));using var ledger=CreditLedger.Open(Path.Combine(dir,"keys"),keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);
        var issued=keys.Create(new("first",QuotaTotal:100));var other=keys.Create(new("second",QuotaTotal:100));string token=issued.TakeSecret(),second=other.TakeSecret();
        using var fake=new Upstream();
        await using var app=LocalGateway.Build(new(endpoint,new byte[32],Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,UpstreamHandler:fake,Affinity:new(affinity)));
        await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app))};
        async Task<Capture> Send(string path,JsonObject body,string? session="synthetic-dsh-parent",string? auth=null)
        {
            using var request=new HttpRequestMessage(HttpMethod.Post,path){Content=JsonContent.Create(body)};request.Headers.Authorization=new("Bearer",auth??token);
            if(session is not null)request.Headers.Add("session_id",session);
            request.Headers.Add("Cookie","private=never-forward");request.Headers.Add("X-Client-ID","not-official");
            using var response=await client.SendAsync(request);string output=await response.Content.ReadAsStringAsync();
            Check(response.IsSuccessStatusCode&&output.Contains("ok"),"Three-protocol request completes");return fake.Captured.Last();
        }
        string? parent=null;int calls=0;
        foreach(string path in new[]{"/v1/chat/completions","/v1/responses","/v1/messages"})
        {
            foreach(bool stream in new[]{false,true})
            {
                bool responses=path=="/v1/responses";
                string tokenField=responses?"max_output_tokens":path=="/v1/chat/completions"?"max_completion_tokens":"max_tokens";
                var body=new JsonObject{["model"]="hy4free",["stream"]=stream,[tokenField]=stream?64000:128};
                body[responses?"input":"messages"]=new JsonArray(new JsonObject{["role"]="user",["content"]="synthetic normal code: session_id=123; keep exactly"});
                var captured=await Send(path,body);calls++;parent??=captured.Conversation;
                Check(captured.Body["max_tokens"]?.GetValue<int>()==(stream?32768:128)
                    && !captured.Body.ContainsKey("max_completion_tokens") && !captured.Body.ContainsKey("max_output_tokens"),
                    "HY4free uses the lower request limit or configured limit through all three protocols");
                Check(captured.Conversation==parent,"Same DSH session survives protocol/stream conversion even if affinity is disabled");
                Check(captured.Body.ToJsonString().Contains("session_id=123; keep exactly"),"Normal user code is preserved");
                var summary=new JsonObject{["model"]="hy4free",["stream"]=stream,["max_tokens"]=128,["metadata"]=new JsonObject{["sessionId"]="synthetic-dsh-parent"}};
                summary[responses?"input":"messages"]=new JsonArray(new JsonObject{["role"]="user",["content"]="Synthetic compacted summary. No pending tools."});
                var compact=await Send(path,summary,null);calls++;
                Check(compact.Conversation==parent&&compact.Turn!=captured.Turn&&compact.Message!=captured.Message,"Compaction keeps upstream identity via body metadata without reusing per-call IDs");
            }
        }
        var requestBody=JsonNode.Parse("{\"model\":\"hy4free\",\"messages\":[{\"role\":\"assistant\",\"content\":null,\"tool_calls\":[{\"id\":\"synthetic-call\",\"type\":\"function\",\"function\":{\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"private-synthetic\\\"}\"}}]},{\"role\":\"tool\",\"tool_call_id\":\"synthetic-call\",\"content\":\"synthetic-result\"},{\"role\":\"user\",\"content\":\"Continue\"}]} ")!.AsObject();
        var responseTool=JsonNode.Parse("{\"model\":\"hy4free\",\"input\":[{\"type\":\"function_call\",\"call_id\":\"synthetic-call\",\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\\\"private-synthetic\\\"}\"},{\"type\":\"function_call_output\",\"call_id\":\"synthetic-call\",\"output\":\"synthetic-result\"},{\"role\":\"user\",\"content\":\"Continue\"}]}")!.AsObject();
        var messageTool=JsonNode.Parse("{\"model\":\"hy4free\",\"max_tokens\":128,\"messages\":[{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"synthetic-call\",\"name\":\"echo\",\"input\":{\"value\":\"private-synthetic\"}}]},{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"synthetic-call\",\"content\":\"synthetic-result\"}]},{\"role\":\"user\",\"content\":\"Continue\"}]}")!.AsObject();
        foreach(var (path,body) in new[]{("/v1/chat/completions",requestBody),("/v1/responses",responseTool),("/v1/messages",messageTool)})
        {
            var tool=await Send(path,body);calls++;
            var messages=tool.Body["messages"]!.AsArray();Check(tool.Conversation==parent&&messages[1]!["tool_calls"]![0]!["id"]!.GetValue<string>()=="synthetic-call"&&messages[2]!["tool_call_id"]!.GetValue<string>()=="synthetic-call","Three-protocol tool history retains session, ordering and correlation IDs");
        }
        var otherCaller=await Send("/v1/chat/completions",requestBody,auth:second);calls++;
        Check(otherCaller.Conversation!=parent,"Same seed from different keys never shares the upstream identity");
        var child=await Send("/v1/chat/completions",requestBody,"synthetic-child");calls++;
        Check(child.Conversation!=parent,"Subagent session remains independent");
        var noSession=await Send("/v1/chat/completions",requestBody,null);calls++;
        var noSessionAgain=await Send("/v1/chat/completions",requestBody,null);calls++;
        Check(noSession.Conversation!=noSessionAgain.Conversation,"Identical prompts without a client identity do not share an inferred session");
        Check(fake.Captured.Count==calls&&ledger.Health.PendingAttempts==0&&ledger.GetUsage(issued.Item.Id).Total==0&&ledger.GetUsage(issued.Item.Id).Requests==calls-1,"Exactly one upstream call per request and zero-credit WAL settlement unchanged");
        var console=app.Services.GetRequiredService<ConsoleRuntime>();
        console.Settings.Update("""{"output":{"hy4freeMaxTokens":1024}}""",console.Settings.View().Revision);
        var capped=new JsonObject{["model"]="hy4free",["stream"]=true,["max_completion_tokens"]=4096,
            ["messages"]=new JsonArray(new JsonObject{["role"]="user",["content"]="synthetic-private-text"})};
        fake.FailAfterOutput=true;
        var failure=await Send("/v1/chat/completions",capped);
        Check(failure.Body["max_tokens"]?.GetValue<int>()==1024,"Hot-reloaded HY4free budget reaches upstream");
        string diagnostic=console.Telemetry.Logs(2).Single(line=>line.Contains("code=upstream_finish_error"));
        Check(diagnostic.Contains("requestedOutputTokens=4096 sentOutputTokens=1024") &&
            System.Text.RegularExpressions.Regex.IsMatch(diagnostic,@"lastUpstreamReadMs=[0-9.]+ downstreamBytes=[1-9][0-9]*") &&
            !diagnostic.Contains("synthetic-private-text"),"A partial upstream error logs timing, bytes and budgets without text");
        fake.FailAfterOutput=false;
        console.Settings.Update("""{"output":{"hy4freeMaxTokens":0}}""",console.Settings.View().Revision);
        capped["messages"]![0]!["content"]="synthetic-private-text"+new string('x',20<<10);
        var uncapped=await Send("/v1/chat/completions",capped);
        Check(uncapped.Body["max_completion_tokens"]?.GetValue<int>()==4096 &&
            !uncapped.Body.ContainsKey("max_tokens"),"A zero budget restores legacy forwarding");
        string cacheLine=console.Telemetry.Logs(1).Single();
        Check(cacheLine.Contains("cacheDiag=1") && cacheLine.Contains("cacheReadTokens=80") &&
            !cacheLine.Contains("synthetic-private-text") && !cacheLine.Contains("synthetic-dsh-parent"),
            "Long HY4free requests emit bounded diagnostic metadata without prompt or session text");
        console.Settings.Update("""{"logs":{"hy4CacheDiagnostics":false}}""",console.Settings.View().Revision);
        await Send("/v1/chat/completions",capped);
        Check(!console.Telemetry.Logs(1).Single().Contains("cacheDiag=1"),"Cache diagnostics can be disabled through configuration");
        await app.StopAsync();
    }
}
