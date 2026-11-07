using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using StreamingMigration;
using KeySecurity;
using LedgerMigration;
using AccountLogin;
using AccountMaintenance;
using AccountScheduling;

namespace HttpMigration;

internal static partial class Program
{
    internal static byte[] TestKey => SHA256.HashData(Encoding.UTF8.GetBytes("workbuddy-gateway/reasoning/key/v1\0offline-key"));
    static readonly JsonSerializerOptions Json = new() { PropertyNamingPolicy = JsonNamingPolicy.CamelCase, PropertyNameCaseInsensitive = true };
    sealed record Spec(string Name, JsonElement Request, string? RawRequest = null, string Sse = "", bool Thinking = false, bool Hidden = false);
    /// <summary>Identity options from the probe JSON (`identity` object), null keeps defaults.</summary>
    internal static IdentityOptions? IdentityFromJson(JsonElement root)
    {
        if(!root.TryGetProperty("identity",out var id)||id.ValueKind!=JsonValueKind.Object)return null;
        bool? enabled=id.TryGetProperty("enabled",out var e)&&e.ValueKind is JsonValueKind.True or JsonValueKind.False?e.GetBoolean():null;
        string target=id.TryGetProperty("target",out var t)&&t.ValueKind==JsonValueKind.String?t.GetString()??"":"";
        Dictionary<string,string>? replacements=null;
        if(id.TryGetProperty("replacements",out var rp)&&rp.ValueKind==JsonValueKind.Object)
        {
            replacements=[];
            foreach(var p in rp.EnumerateObject())replacements[p.Name]=p.Value.ValueKind==JsonValueKind.String?p.Value.GetString()??"":"";
        }
        return new IdentityOptions(enabled,target,replacements);
    }

    static async Task Main(string[] args)
    {
        Console.InputEncoding = new UTF8Encoding(false); Console.OutputEncoding = new UTF8Encoding(false);
        if(args is ["--calendar-check",var calendarScratch]){await CalendarTests.Run(calendarScratch);return;}
        if(args is ["--scheduled-tasks-check"]){ScheduledTaskTests.Run();return;}
        if(args is ["--identity-check"]){IdentityTests.Run();return;}
        if(args is ["--conversation-check",var conversationScratch]){await ConversationTests.Run(conversationScratch);return;}
        if(args is ["--body-lifetime-check",var bodyScratch]){await BodyLifetimeTests.Run(bodyScratch);return;}
        if(args is ["--console-check",var consoleScratch]){await ConsoleTests.Run(consoleScratch);return;}
        if(args is ["--admin-preview",var previewScratch]){await ConsoleTests.Run(previewScratch,true);return;}
        if(args is ["--state-check",var checkAccounts,var checkKeys]){DeploymentSupport.Check(checkAccounts,checkKeys);return;}
        if(args is ["--runtime-cache-check",var runtimeScratch]){await RuntimeCacheTests.Run(runtimeScratch);return;}
        if(args is ["--runtime-restore-probe"])
        {
            while(await Console.In.ReadLineAsync() is {} line)
            {
                using var doc=JsonDocument.Parse(line);var r=doc.RootElement;var now=DateTimeOffset.FromUnixTimeSeconds(1800000000);var id=new AccountId("a");var binding=new[]{new RuntimeIdentity(id,AccountSite.Cn,new string('a',64))};
                long Stamp(string name){long n=r.GetProperty(name).GetInt64();return n==0?0:now.ToUnixTimeSeconds()+n;}
                var snapshot=JsonSerializer.SerializeToUtf8Bytes(new{version=1,at=now.ToUnixTimeSeconds(),accounts=new[]{new{id="a",site=0,identity=new string('a',64),exhausted=r.GetProperty("exhausted").GetBoolean(),rateUntil=0,models=new{m=new{cost=r.GetProperty("cost").GetInt32(),cooldown=Stamp("cooldown"),blocked=r.GetProperty("blocked").GetBoolean(),probe=Stamp("probe"),observed=now.ToUnixTimeSeconds()-30}}}}});
                var pool=new AccountPool([new(id,SiteEndpoints.Model(AccountSite.Cn))],time:new RestoreProbeClock(now));pool.ImportRuntime(snapshot,binding);var a=pool.Snapshot(id)!;var state=a.Models!["m"];var kind=SchedulingRules.Eligibility(a,"m",now);
                Console.WriteLine(JsonSerializer.Serialize(new{cost=state.Cost.ToString().ToLowerInvariant(),blocked=state.QuotaBlocked,exhausted=a.QuotaExhausted,cooling=state.CooldownUntil>now,probeWaiting=state.NextProbeAt>now,kind=kind switch{SelectionKind.Normal=>"normal",SelectionKind.FreeExhausted=>"free_exhausted",SelectionKind.ProbeExhausted=>"probe_exhausted",_=>null}}));
            }
            return;
        }
        if(args is ["--model-runtime-check",var modelScratch]){await ModelRuntimeTests.Run(modelScratch);return;}
        if(args is ["--model-runtime-probe"])
        {
            while(await Console.In.ReadLineAsync() is {} line)
            {
                using var doc=JsonDocument.Parse(line);var r=doc.RootElement;string? raw=r.GetProperty("usage").ValueKind==JsonValueKind.Null?null:r.GetProperty("usage").GetRawText();
                var rejection=ModelObservations.Failure(r.GetProperty("status").GetInt32(),null,r.GetProperty("body").GetString(),DateTimeOffset.FromUnixTimeSeconds(1800000000));
                Console.WriteLine(JsonSerializer.Serialize(new{cost=ModelObservations.Cost(raw).ToString().ToLowerInvariant(),kind=rejection.Kind.ToString().ToLowerInvariant(),reset=ModelObservations.Reset(r.GetProperty("reset").GetString()!)?.ToUnixTimeSeconds()??0}));
            }
            return;
        }
        if(args is ["--background-check",var backgroundScratch]){await BackgroundTests.Run(backgroundScratch);return;}
        if(args is ["--resource-observation-probe"])
        {
            while(await Console.In.ReadLineAsync() is {} line)
            {
                using var doc=JsonDocument.Parse(line);var r=doc.RootElement;
                try{object value=r.GetProperty("kind").GetString()=="package"?ResourceParser.Package(r.GetProperty("data")):ResourceParser.Quota(r.GetProperty("data"),r.GetProperty("kind").GetString()=="enterprise");Console.WriteLine(JsonSerializer.Serialize(value,Json));}
                catch(CredentialFailure){Console.WriteLine("{\"error\":true}");}
            }
            return;
        }
        if(args is ["--agent-tools-check",var toolScratch]){await AgentToolTests.Run(toolScratch);return;}
        if(args is ["--agent-tools-probe"])
        {
            while(await Console.In.ReadLineAsync() is {} line)
            {
                using var doc=JsonDocument.Parse(line);var root=doc.RootElement;string code="",message="";JsonElement? payload=null;
                try{using var prepared=JsonDocument.Parse(AgentToolProtocol.Prepare(JsonSerializer.SerializeToUtf8Bytes(root.GetProperty("request")),false));payload=prepared.RootElement.Clone();}
                catch(RequestFailure e){code=e.ErrorType;message=e.Message;}
                var projection=JsonSerializer.SerializeToElement(AgentToolProtocol.Project(root.GetProperty("response"),false));
                Console.WriteLine(JsonSerializer.Serialize(new{code,message,payload,results=projection.GetProperty("results")}));
            }
            return;
        }
        if(args is ["--count-tokens-check",var countScratch]){await CountTokensTests.Run(countScratch);return;}
        if(args is ["--count-tokens-probe"])
        {
            while(await Console.In.ReadLineAsync() is {} line)
            {
                try{using var doc=JsonDocument.Parse(line,new JsonDocumentOptions{MaxDepth=256});TokenEstimation.Validate(doc.RootElement);Console.WriteLine(JsonSerializer.Serialize(new{status=200,body=new{input_tokens=TokenEstimation.Estimate(doc.RootElement)}}));}
                catch(RequestFailure e){Console.WriteLine(JsonSerializer.Serialize(new{status=e.Status,body=new{type="error",error=new{type=e.ErrorType,message=e.Message}}}));}
                catch(JsonException){Console.WriteLine(JsonSerializer.Serialize(new{status=400,body=new{type="error",error=new{type="invalid_request_error",message="无效的 JSON 请求体"}}}));}
            }
            return;
        }
        if (args is ["--self-test"]) { await HttpTests.Run(); return; }
        if (args is ["--allocation-check"]) { AllocationTests.Run(); return; }
        if (args is ["--resource-check"]) { await ResourceTests.Run(); return; }
        if (args is ["--catalog-check"]) { await CatalogTests.Run(); return; }
        if (args is ["--live-catalog-check",var catalogScratch]) { await LiveCatalogTests.Run(catalogScratch);return; }
        if (args is ["--catalog-cache-check",var cacheScratch]) { await CatalogCacheTests.Run(cacheScratch);return; }
        if(args is ["--outbound-probe"])
        {
            while(await Console.In.ReadLineAsync() is {} line)
            {
                using var doc=JsonDocument.Parse(line);var r=doc.RootElement;
                var settings=new SystemPromptOptions(r.GetProperty("fallback").GetString()!,r.GetProperty("force").GetString()!,IdentityFromJson(r));
                var bytes=OutboundPreparation.Prepare(JsonSerializer.SerializeToUtf8Bytes(r.GetProperty("body")),settings,_=>{},32<<20);
                Console.WriteLine(Encoding.UTF8.GetString(bytes));
            }
            return;
        }
        if(args is ["--outbound-check"]){OutboundTests.Run();return;}
        if(args is ["--live-catalog-probe"])
        {
            while(await Console.In.ReadLineAsync() is {} line)
            {
                using var doc=JsonDocument.Parse(line);var root=doc.RootElement;var catalog=new ModelCatalog();
                catalog.LoadLive(root.GetProperty("site").GetString()=="cn"?AccountSite.Cn:AccountSite.Intl,JsonSerializer.SerializeToUtf8Bytes(new{code=0,data=root.GetProperty("data")}));
                Console.WriteLine(JsonSerializer.Serialize(new{models=catalog.List(),defaultOutput=catalog.DefaultOutput(root.GetProperty("model").GetString()!)}));
            }
            return;
        }
        if (args is ["--catalog-probe"])
        {
            while(await Console.In.ReadLineAsync() is {} line)
            {
                using var doc=JsonDocument.Parse(line);var catalog=new ModelCatalog();
                catalog.Load(JsonSerializer.SerializeToUtf8Bytes(new{schema=5,catalogs=doc.RootElement.GetProperty("catalogs")}));
                Console.WriteLine(JsonSerializer.Serialize(new{models=catalog.List(),defaultOutput=catalog.DefaultOutput(doc.RootElement.GetProperty("model").GetString()!)}));
            }
            return;
        }
        if(args is ["--npm-catalog-check"]){await NpmCatalogTests.Run();return;}
        if(args is ["--console-login-check",var loginScratch]){await ConsoleLoginTests.Run(loginScratch);return;}
        if (args is ["--operations-parity-check", var operationsScratch]) { await OperationsParityTests.Run(operationsScratch); return; }
        if (args is ["--standalone-check", var scratch]) { await StandaloneTests.Run(scratch); return; }
        if (args is ["--failover-check", var failoverScratch]) { await FailoverTests.Run(failoverScratch); return; }
        if (args is ["--import-account", var sourceAccount, var ownedAccount]) { StandaloneAccountImport.Run(sourceAccount,ownedAccount); return; }
        if(args is ["--accounts-check",var accountManifest])
        {using var runtime=StandaloneAccounts.OpenManifest(accountManifest);Console.WriteLine(JsonSerializer.Serialize(new{accounts=runtime.Pool.Count,disabled=runtime.Credentials.Status.Count(s=>s.Disabled),networkRequests=0}));return;}
        if(args is ["--accounts-self-test",var accountScratch]){await StandaloneAccountsTests.Run(accountScratch);return;}
        if(args is ["--account-management-check",var managementScratch]){await AccountManagementTests.Run(managementScratch);return;}
        if(args is ["--affinity-check",var affinityScratch]){await AffinityTests.Run(affinityScratch);return;}
        if (args.Length is 4 or 5 && args[0] is "--standalone" or "--standalone-pool")
        {await RunStandalone(args);return;}
        if (args is ["--bridge", var backend, var bind])
        {
            int port = int.Parse(bind, System.Globalization.CultureInfo.InvariantCulture);
            await using var app = LocalGateway.Build(new(new Uri(backend), new byte[32], Port: port, MaxConcurrent: 16, HeaderTimeout: TimeSpan.FromMinutes(15), IdleTimeout: TimeSpan.FromSeconds(60), GoBridge: true));
            await app.StartAsync(); Console.WriteLine(JsonSerializer.Serialize(new { address = LocalGateway.Address(app), mode = "csharp-protocols-go-accounts" })); await app.WaitForShutdownAsync(); return;
        }
        if (args is ["--login-preview", var loginOrigin, var credentialDirectory, var edition])
        {
            if (edition is not ("cn" or "intl")) throw new ArgumentException("Site must be cn or intl");
            var origin = new Uri(loginOrigin); var site = edition == "intl" ? AccountSite.Intl : AccountSite.Cn;
            await using var coordinator = new LoginCoordinator(s => new LoopbackLoginFlow(origin, s));
            var operation = await coordinator.StartAsync(site, new PreviewLoginDestination(credentialDirectory));
            Console.WriteLine(JsonSerializer.Serialize(operation.Snapshot, Json));
            ConsoleCancelEventHandler cancel = (_, e) => { e.Cancel = true; operation.Cancel(); };
            Console.CancelKeyPress += cancel;
            try { var completed = await operation.Completion; Console.WriteLine(JsonSerializer.Serialize(completed, Json)); if (completed.Status != "done") Environment.ExitCode = 1; }
            finally { Console.CancelKeyPress -= cancel; }
            return;
        }
        if (args is ["--serve-account-preview", var modelUrl, var controlOrigin, var credentialPath, var keyPath])
        {
            var admin = new Credential(Environment.GetEnvironmentVariable("WORKBUDDY_PREVIEW_ADMIN_KEY") ?? throw new ArgumentException("Set WORKBUDDY_PREVIEW_ADMIN_KEY"));
            using var files = CredentialFiles.Open(credentialPath); var saved = files.Load();
            var endpoint = new Uri(modelUrl); var id = new AccountId("preview-account");
            var pool = new AccountPool([new(id, endpoint, saved.Credential.Identity.Site)]);
            using var control = new LoopbackControlPlane(new Uri(controlOrigin));
            var credentials = new CredentialManager(pool, [new(id, endpoint, files, control)]);
            using var keys = PreviewKeyDirectory.Open(keyPath); using var ledger = CreditLedger.Open(keyPath, keys.List().Select(k => k.Id));
            await using var app = LocalGateway.Build(new(endpoint, RandomNumberGenerator.GetBytes(32), Security: new(keys, admin, ledger), Accounts: pool, Credentials: credentials));
            await app.StartAsync(); Console.WriteLine(JsonSerializer.Serialize(new { address = LocalGateway.Address(app) })); await app.WaitForShutdownAsync(); return;
        }
        if (args is ["--serve", var upstream])
        {
            await using var app = LocalGateway.Build(new(new Uri(upstream), RandomNumberGenerator.GetBytes(32)));
            await app.StartAsync(); Console.WriteLine(JsonSerializer.Serialize(new { address = LocalGateway.Address(app) })); await app.WaitForShutdownAsync(); return;
        }
        if (args is ["--serve-managed", var managedUpstream, var directory])
        {
            string admin = Environment.GetEnvironmentVariable("WORKBUDDY_PREVIEW_ADMIN_KEY") ?? throw new ArgumentException("Set WORKBUDDY_PREVIEW_ADMIN_KEY for the local preview");
            var adminCredential = new Credential(admin);
            using var store = PreviewKeyDirectory.Open(directory);
            using var ledger = CreditLedger.Open(directory, store.List().Select(k => k.Id));
            await using var app = LocalGateway.Build(new(new Uri(managedUpstream), RandomNumberGenerator.GetBytes(32), Security: new(store, adminCredential, ledger)));
            await app.StartAsync(); Console.WriteLine(JsonSerializer.Serialize(new { address = LocalGateway.Address(app) })); await app.WaitForShutdownAsync(); return;
        }
        if (args is not ["--prepare"] and not ["--aggregate"]) throw new ArgumentException("Use --prepare, --aggregate, --self-test, --serve <loopback upstream>, --serve-managed <loopback upstream> <key directory>, --login-preview <loopback origin> <new credential directory> <cn|intl>, or --serve-account-preview <loopback model URL> <loopback control origin> <credential directory> <key directory>");
        while (await Console.In.ReadLineAsync() is { } line)
        {
            var spec = JsonSerializer.Deserialize<Spec>(line, Json) ?? throw new InvalidDataException();
            if (args[0] == "--prepare")
            {
                try
                {
                    var prepared = MessagesRequestAdapter.Prepare(spec.RawRequest is not null ? Encoding.UTF8.GetBytes(spec.RawRequest) : JsonSerializer.SerializeToUtf8Bytes(spec.Request), TestKey);
                    Console.WriteLine(JsonSerializer.Serialize(new { spec.Name, status = 200, prepared.Model, prepared.Stream, prepared.Thinking, prepared.Hidden, body = JsonSerializer.Deserialize<JsonElement>(prepared.UpstreamBody) }, Json));
                }
                catch (RequestFailure e) { Console.WriteLine(JsonSerializer.Serialize(new { spec.Name, status = e.Status, error = e.Message }, Json)); }
            }
            else
            {
                var result = await MessageAggregate.RunAsync(new MemoryStream(Encoding.UTF8.GetBytes(spec.Sse)), new("test-model", spec.Thinking, spec.Hidden ? new ReasoningCodec(TestKey, "test-model") : null), 64 << 20, CancellationToken.None);
                Console.WriteLine(JsonSerializer.Serialize(new { spec.Name, result.Code, result.Complete, body = result.Body is null ? (JsonElement?)null : JsonSerializer.Deserialize<JsonElement>(result.Body), result.Usage }, Json));
            }
        }
    }
    sealed class RestoreProbeClock(DateTimeOffset now):TimeProvider{public override DateTimeOffset GetUtcNow()=>now;}
}
