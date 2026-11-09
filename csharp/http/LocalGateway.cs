using System.Buffers;
using System.IO.Compression;
using System.IO.Pipelines;
using System.Net;
using System.Net.Http.Headers;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;
using Microsoft.AspNetCore.Hosting.Server;
using Microsoft.AspNetCore.Hosting.Server.Features;
using StreamingMigration;
using KeySecurity;
using LedgerMigration;
using AccountScheduling;
using AccountMaintenance;
using OpenAiProtocols;

namespace HttpMigration;

public sealed record LocalGatewayOptions(Uri Upstream, byte[] ReasoningKey, int Port = 0, int MaxRequestBytes = 32 << 20,
    long MaxOutputBytes = 64L << 20, int MaxConcurrent = 4, TimeSpan? HeaderTimeout = null, TimeSpan? IdleTimeout = null, LocalKeySecurity? Security = null,
    AccountPool? Accounts = null, CredentialManager? Credentials = null, bool GoBridge = false,
    long MaxInFlightBodyBytes = 0, int MaxPreparing = 2, bool DirectUpstream = false, HttpMessageHandler? UpstreamHandler = null, ModelCatalog? Catalog = null, TimeSpan? CatalogRefreshInterval = null, CatalogCache? CatalogCache = null, SystemPromptOptions? SystemPrompts = null, AffinityOptions? Affinity = null,CredentialMaintenanceOptions? Maintenance=null,AccountResourceOptions? Resources=null,ResourceSnapshotCache? ResourceCache=null,RuntimeSnapshotCache? RuntimeCache=null,string? ConsoleDirectory=null,CalendarMaintenanceOptions? Calendar=null,StandaloneAccounts? AccountOwner=null,TimeSpan? CredentialReloadInterval=null,bool PriceProbe=false,bool NpmCatalog=false)
{internal TimeSpan RestrictedReplayDelay{get;init;}=RestrictedReplay.DefaultDelay;public AdmissionOptions? Admission{get;init;}public TimeSpan? FirstTokenTimeout{get;init;}internal ServiceTimeouts? ServiceTimeouts{get;init;}}

// Listens on loopback. Direct upstream mode requires independent credential and ledger owners.
public static class LocalGateway
{
    public static WebApplication Build(LocalGatewayOptions options)
    {
        bool direct = options.DirectUpstream;
        if(options.Calendar is not null&&!direct)throw new ArgumentException("Calendar maintenance requires direct mode");
        options.Maintenance?.Validate();if(options.Maintenance is not null&&(!direct||options.Credentials is null))throw new ArgumentException("Credential worker requires independent accounts");
        options.Resources?.Validate();if(options.Resources is not null&&(!direct||options.Credentials is null))throw new ArgumentException("Resource worker requires independent accounts");
        if(options.ResourceCache is not null&&(!direct||options.Credentials is null))throw new ArgumentException("Resource cache requires independent accounts");
        if(options.RuntimeCache is not null&&(!direct||options.Credentials is null||options.Accounts is null))throw new ArgumentException("Runtime cache requires independent accounts");
        options.Affinity?.Validate();
        if(options.Affinity?.Enabled==true&&(!direct||options.Security is null))throw new ArgumentException("Affinity requires authenticated independent direct mode");
        options.SystemPrompts?.Validate();
        if(options.SystemPrompts is not null&&!direct)throw new ArgumentException("System prompt preparation requires independent direct mode");
        if(options.GoBridge&&options.Catalog is not null)throw new ArgumentException("Go bridge owns no independent catalog");
        if(options.CatalogCache is not null&&!direct)throw new ArgumentException("Persistent catalog requires independent direct mode");
        if(direct && options.Catalog is null)options=options with{Catalog=new ModelCatalog()};
        if(options.Catalog is not null && options.Security is null)throw new ArgumentException("Model catalog requires key authentication");
        if(options.CatalogRefreshInterval is { } refreshInterval && (!direct || options.Credentials is null || refreshInterval<TimeSpan.FromSeconds(10) || refreshInterval>TimeSpan.FromDays(1)))throw new ArgumentException("Live catalog requires direct mode credentials and a bounded interval");
        if (direct && (options.GoBridge || options.Security?.Ledger is null || options.Accounts is null || options.Credentials is null ||
            !(options.Upstream == SiteEndpoints.Model(AccountSite.Cn) || options.Upstream == SiteEndpoints.Model(AccountSite.Intl))))
            throw new ArgumentException("Direct upstream requires pinned endpoints and independent account/key/ledger owners");
        if (!direct && (!options.Upstream.IsLoopback || options.Upstream.Scheme != "http" || options.Upstream.UserInfo.Length != 0))
            throw new ArgumentException("Offline preview requires an HTTP loopback upstream");
        if (options.ReasoningKey.Length != 32 || options.MaxConcurrent < 1 || options.MaxRequestBytes < 1 || options.MaxOutputBytes < 1 || options.Port is < 0 or > 65535 || options.HeaderTimeout is { Ticks: <= 0 } || options.IdleTimeout is { Ticks: <= 0 })
            throw new ArgumentException("Invalid preview limits or reasoning key");
        options = options with { ReasoningKey = options.ReasoningKey.ToArray() };
        if (options.MaxInFlightBodyBytes < 0 || options.MaxPreparing < 0) throw new ArgumentException("Invalid preparation limits");
        options.Admission?.Validate();
        if(options.FirstTokenTimeout is{} first&&(first<TimeSpan.Zero||first>TimeSpan.FromHours(1)))throw new ArgumentException("Invalid first-token timeout");
        if (options.GoBridge && (options.Security is not null || options.Accounts is not null || options.Credentials is not null))
            throw new ArgumentException("Go bridge owns no local credential or billing stores");
        if (options.Credentials is not null && options.Accounts is null) throw new ArgumentException("Credential maintenance requires an account pool");
        if (options.Security is { Ledger: { } accounting } securityContext) securityContext.Store.AttachAccounting(accounting);
        if(options.CatalogCache is { } cache && !cache.TryLoad(options.Catalog!) && !options.Catalog!.IsEmpty)cache.TrySave(options.Catalog);
        options.ResourceCache?.TryLoad(options.Credentials!);
        options.RuntimeCache?.TryLoad(options.Accounts!,options.Credentials!);
        if(direct)options.Catalog!.AttachScheduling(options.Accounts!);
        var builder = WebApplication.CreateSlimBuilder();
        if(options.Maintenance is not null||options.Resources is not null||options.RuntimeCache is not null)builder.Services.Configure<HostOptions>(o=>o.ShutdownTimeout=TimeSpan.FromSeconds(60));
        builder.Logging.ClearProviders();
        builder.WebHost.ConfigureKestrel(k => { k.Listen(IPAddress.Loopback, options.Port); k.Limits.MaxRequestBodySize = options.MaxRequestBytes; if(direct)k.Limits.RequestHeadersTimeout=TimeSpan.FromSeconds(15);if (options.GoBridge||direct) k.Limits.MinRequestBodyDataRate = null; });
        builder.Services.AddSingleton(_ => new HttpClient(options.UpstreamHandler ?? new SocketsHttpHandler { UseProxy = false, AllowAutoRedirect = false, UseCookies = false, AutomaticDecompression = DecompressionMethods.GZip | DecompressionMethods.Deflate, PooledConnectionLifetime = TimeSpan.FromMinutes(5) }) { Timeout = Timeout.InfiniteTimeSpan });
        builder.Services.AddSingleton(_ => new SemaphoreSlim(options.MaxConcurrent));
        builder.Services.AddSingleton(_=>new SessionAffinity(options.Affinity??new()));
        if(direct)
        {
            builder.Services.AddSingleton(_=>new ConsoleRuntime(options));
            builder.Services.AddSingleton(_=>new OutputAdmission(new SystemMemorySampler().Snapshot));
            builder.Services.AddSingleton<RetryAdmission>();
            builder.Services.AddSingleton(sp=>new GracefulRequests(sp.GetRequiredService<IHostApplicationLifetime>(),sp.GetRequiredService<ConsoleRuntime>().Settings));
            builder.Services.AddHostedService(sp=>new ConsoleHistoryWorker(sp.GetRequiredService<ConsoleRuntime>().Telemetry,sp.GetRequiredService<ConsoleRuntime>().TaskJournal,options.Catalog,options.CatalogCache));
            builder.Services.AddSingleton(sp=>new ConsoleMaintenanceWorker(sp.GetRequiredService<ConsoleRuntime>()));
            builder.Services.AddHostedService(sp=>sp.GetRequiredService<ConsoleMaintenanceWorker>());
        }
        if(options.ResourceCache is { } resourceCache)builder.Services.AddSingleton(resourceCache);
        if(options.RuntimeCache is { } runtimeCache)
        {
            builder.Services.AddSingleton(runtimeCache);
            builder.Services.AddSingleton(_=>new RuntimeCheckpointWorker(options.Accounts!,options.Credentials!,runtimeCache));
            builder.Services.AddHostedService(sp=>sp.GetRequiredService<RuntimeCheckpointWorker>());
        }
        builder.Services.AddSingleton(_ => new PreparationAdmission(options.MaxInFlightBodyBytes, Math.Max(1,Math.Min(options.MaxPreparing, options.MaxConcurrent)),direct?new SystemMemorySampler().Snapshot:null));
        if(direct)builder.Services.AddSingleton(sp=>new RequestAdmission(options.Admission??new(MaxActive:options.MaxConcurrent,MaxPreparing:options.MaxPreparing),sp.GetRequiredService<PreparationAdmission>().CheckAdmission));
        if(options.CatalogRefreshInterval is { } interval)
        {
            builder.Services.AddSingleton(sp=>new CatalogRefresher(options.Credentials!,options.Catalog!,interval,options.CatalogCache,options.NpmCatalog?new NpmCatalogSource(sp.GetRequiredService<HttpClient>()):null));
            builder.Services.AddHostedService(sp=>sp.GetRequiredService<CatalogRefresher>());
        }
        if(options.Maintenance is { } maintenance)
        {builder.Services.AddSingleton(_=>new CredentialMaintenanceWorker(options.Credentials!,maintenance));builder.Services.AddHostedService(sp=>sp.GetRequiredService<CredentialMaintenanceWorker>());}
        if(options.Resources is { } resources)
        {if(resources.ExpiringFirst)options.Accounts!.EnableExpiringPriority();builder.Services.AddSingleton(_=>new AccountResourceWorker(options.Credentials!,resources,options.ResourceCache,options.AccountOwner));builder.Services.AddHostedService(sp=>sp.GetRequiredService<AccountResourceWorker>());}
        if(options.Calendar is{} calendar)
        {
            if(calendar.KeepaliveHours.Count>0){builder.Services.AddSingleton(_=>new KeepaliveWorker(options.Credentials!,calendar));builder.Services.AddHostedService(sp=>sp.GetRequiredService<KeepaliveWorker>());}
            if(calendar.CheckinEnabled){builder.Services.AddSingleton(sp=>new DailyCheckinWorker(sp.GetRequiredService<ConsoleRuntime>().Checkins!,options.Credentials!));builder.Services.AddHostedService(sp=>sp.GetRequiredService<DailyCheckinWorker>());}
        }
        if(options.AccountOwner is{} owner&&options.CredentialReloadInterval is{} reloadInterval)
        {
            if(!direct||!ReferenceEquals(owner.Credentials,options.Credentials)||!ReferenceEquals(owner.Pool,options.Accounts)||!owner.CanReload)throw new ArgumentException("Reload requires the live manifest owner");
            builder.Services.AddSingleton(_=>new CredentialReloadWorker(owner,reloadInterval));
            builder.Services.AddHostedService(sp=>sp.GetRequiredService<CredentialReloadWorker>());
        }
        if(options.PriceProbe)
        {
            if(!direct||options.ConsoleDirectory is null)throw new ArgumentException("Price probes require independent durable console state");
            builder.Services.AddSingleton(sp=>new ModelPriceProbeWorker(options.Credentials!,options.Catalog!,sp.GetRequiredService<ConsoleRuntime>(),sp.GetRequiredService<HttpClient>()));
            builder.Services.AddHostedService(sp=>sp.GetRequiredService<ModelPriceProbeWorker>());
        }
        if(direct&&options.AccountOwner is{CanReload:true} loginOwner)
        {
            builder.Services.AddSingleton(sp=>new ConsoleLoginService(loginOwner,sp.GetRequiredService<ConsoleRuntime>().Labels));
            builder.Services.AddHostedService(sp=>sp.GetRequiredService<ConsoleLoginService>());
        }
        var app = builder.Build();
        if(direct)ConsoleMaintenanceSetup.Configure(app.Services,options);
        if(direct)app.Use((HttpContext c,RequestDelegate next)=>app.Services.GetRequiredService<GracefulRequests>().Handle(c,next));
        app.MapGet("/readyz", (HttpClient client) => options.GoBridge ? GoBridgeRoutes.Ready(client, options) : Task.FromResult(options.Security?.Ledger is { } l && !l.Health.Healthy ? Results.StatusCode(503) : GatewayJson.Result(new Dictionary<string, object?> { ["status"] = "ready", ["implementation"] = options.DirectUpstream ? "csharp-standalone-experimental" : "csharp-local-preview" })));
        app.Map("/v1/messages", (HttpContext c, HttpClient client, SemaphoreSlim gate, PreparationAdmission resources) => Handle(c, client, gate, resources, options));
        app.Map("/v1/chat/completions", (HttpContext c, HttpClient client, SemaphoreSlim gate, PreparationAdmission resources) => Handle(c, client, gate, resources, options, ClientProtocol.Chat));
        app.Map("/v1/responses", (HttpContext c, HttpClient client, SemaphoreSlim gate, PreparationAdmission resources) => Handle(c, client, gate, resources, options, ClientProtocol.Responses));
        if (options.GoBridge) GoBridgeRoutes.Map(app, options);
        else if(options.Security is not null)app.Map("/v1/messages/count_tokens",(HttpContext c,SemaphoreSlim gate,PreparationAdmission resources)=>CountTokensApi.Handle(c,gate,resources,options));
        if(options.Catalog is { } catalog)app.Map("/v1/models",(HttpContext c,SemaphoreSlim gate)=>ModelsApi.Handle(c,gate,options.Security!,catalog));
        if (options.Security is { } security) app.Map("/admin/api/keys", (HttpContext c) => KeyManagement.Handle(c, security));
        if(options.DirectUpstream)
        {
            foreach(string path in new[]{"/","/ui","/admin"})app.MapGet(path,AdminPage.Show);
            app.MapGet("/ui/app.js",(HttpContext c)=>AdminPage.Asset(c,"app.js"));
            app.MapGet("/ui/qrcodegen.js",(HttpContext c)=>AdminPage.Asset(c,"qrcodegen.js"));
            app.MapGet("/ui/style.css",(HttpContext c)=>AdminPage.Asset(c,"style.css"));
            foreach(string path in new[]{"/health","/ping"})app.MapGet(path,()=>options.Security!.Ledger!.Health.Healthy?GatewayJson.Result(new Dictionary<string, object?> { ["status"] = "ok", ["implementation"] = "csharp-standalone" }):Results.StatusCode(503));
            app.Map("/chat/completions",(HttpContext c,HttpClient client,SemaphoreSlim gate,PreparationAdmission resources)=>Handle(c,client,gate,resources,options,ClientProtocol.Chat));
            app.Map("/responses",(HttpContext c,HttpClient client,SemaphoreSlim gate,PreparationAdmission resources)=>Handle(c,client,gate,resources,options,ClientProtocol.Responses));
            app.Map("/models",(HttpContext c,SemaphoreSlim gate)=>ModelsApi.Handle(c,gate,options.Security!,options.Catalog!));
            app.Map("/v1/search",(HttpContext c,HttpClient client,SemaphoreSlim gate,PreparationAdmission resources)=>AgentToolApi.Handle(c,client,gate,resources,options,false));
            app.Map("/v1/fetch",(HttpContext c,HttpClient client,SemaphoreSlim gate,PreparationAdmission resources)=>AgentToolApi.Handle(c,client,gate,resources,options,true));
            var accountAdminGate=new SemaphoreSlim(1);
            app.Map("/admin/api/accounts",(HttpContext c)=>AccountManagement.Handle(c,options.Security!,options.Credentials!,options.Accounts!,accountAdminGate));
            app.Map("/admin/api/{**endpoint}",(HttpContext c,ConsoleRuntime console)=>ConsoleApi.Handle(c,options,console,accountAdminGate));
        }
        if (options.Security is { Ledger: not null } metered) app.MapGet("/v1/usage", (HttpContext c) => LedgerPresentation.HandleUsage(c, metered));
        return app;
    }
    public static string Address(WebApplication app) => app.Services.GetRequiredService<IServer>().Features.Get<IServerAddressesFeature>()?.Addresses.Single() ?? throw new InvalidOperationException("Server has not started");
    static async Task Handle(HttpContext c, HttpClient client, SemaphoreSlim gate, PreparationAdmission resources, LocalGatewayOptions options, ClientProtocol? protocol = null)
    {
        c.Response.Headers["X-Trace-ID"] = Guid.NewGuid().ToString("N");
        c.Response.Headers["X-Gateway-Implementation"] = "csharp-protocols";
        var trace=options.DirectUpstream?c.RequestServices.GetRequiredService<ConsoleRuntime>().Telemetry.Begin(protocol switch{ClientProtocol.Chat=>"chat_completions",ClientProtocol.Responses=>"responses",_=>"messages"},c.Response.Headers["X-Trace-ID"].ToString()):null;
        var timeouts=options.DirectUpstream?c.RequestServices.GetRequiredService<ConsoleRuntime>().Settings.Upstream:new UpstreamTimeouts(options.HeaderTimeout??TimeSpan.FromSeconds(30),options.IdleTimeout??TimeSpan.FromSeconds(60),TimeSpan.Zero);
        using var firstToken=new FirstTokenBudget(timeouts.FirstToken,c.RequestAborted);
        using var outputBudget=options.DirectUpstream?c.RequestServices.GetRequiredService<OutputAdmission>().Acquire():null;
        using var retryScope=options.DirectUpstream?c.RequestServices.GetRequiredService<RetryAdmission>().Acquire():null;
        void FirstOutput(){firstToken.Finish();trace?.First();}
        KeyLease? keyLease = null; bool admitted = false; byte[]? derivedReasoningKey = null;RequestAdmission.Lease? admission=null;
        using var memory = resources.Acquire(); bool preparing = false;
        AccountLease? accountLease = null;
        CredentialLease? credentialLease = null;
        RestrictedReplay? replay=null;
        LedgerAttempt? attempt = null; bool sent = false, settlementAttempted = false, restricted = false; string? evidence = null, modelName=null;
        var affinity=c.RequestServices.GetRequiredService<SessionAffinity>();string? affinityKey=null,selectedIdentity=null;
        void Settle(string? raw, string code, bool receivedOutput, int status, string? errorBody=null)
        {
            if(settlementAttempted)return;settlementAttempted=true;
            raw ??= evidence;
            trace?.Observe(raw,code,status,status==200&&code.Length==0);
            if (options.DirectUpstream && code=="11140" && accountLease is not null)
            { restricted=true; affinity.Forget(affinityKey,accountLease.Account.Id); options.Credentials!.Block(accountLease.Account.Id); }
            if(options.DirectUpstream && accountLease is not null && modelName is not null)
            {
                if(status==200&&code.Length==0&&selectedIdentity==options.Credentials!.AffinityIdentity(accountLease.Account.Id))affinity.ObserveCache(affinityKey,accountLease.Account.Id,selectedIdentity,SessionAffinity.CachedTokens(raw),true);
                if(status==200||receivedOutput){options.Accounts!.ObserveModelUsage(accountLease.Account.Id,modelName,raw);options.Catalog?.ObserveUsage(accountLease.Account.Site,modelName,raw);}
                if(status!=200)options.Accounts!.ObserveModelFailure(accountLease.Account.Id,modelName,status,code,errorBody);
            }
            if (attempt is null || options.Security?.Ledger is not { } ledger) return;
            bool hasEvidence = false;
            if (raw is not null) { using var doc = JsonDocument.Parse(raw); hasEvidence = doc.RootElement.ValueKind == JsonValueKind.Object && doc.RootElement.EnumerateObject().Any(); }
            if (!hasEvidence && !receivedOutput && code is "11128" or "11140") ledger.CancelUncharged(attempt);
            else ledger.Complete(attempt, raw, status);
        }
        try
        {
            string bridgeToken = "";
            if (options.GoBridge)
            {
                bridgeToken = KeyRules.Token(c.Request.Headers.Authorization.ToString(), c.Request.Headers["x-api-key"].ToString());
                if (bridgeToken == "") { await Error(c, 401, "authentication_error", "未提供 API 密钥"); return; }
                derivedReasoningKey = KeyRules.ReasoningKey(bridgeToken);
            }
            if (options.Security is { } security) keyLease = security.Store.Acquire(KeyRules.Token(c.Request.Headers.Authorization.ToString(), c.Request.Headers["x-api-key"].ToString()));
            if (!HttpMethods.IsPost(c.Request.Method)) { await Error(c, 405, "api_error", "仅支持 POST 请求"); return; }
            if(options.DirectUpstream)admission=await c.RequestServices.GetRequiredService<RequestAdmission>().AcquireAsync(keyLease?.Identity?.Id??"",c.RequestAborted);
            else
            {
                if (!await gate.WaitAsync(0, c.RequestAborted)) { await Error(c, 503, "overloaded_error", "本地网关并发已满"); return; }
                admitted = true;
                await resources.Preparing.WaitAsync(c.RequestAborted); preparing = true;
            }
            byte[] input = await ReadBody(c, options.MaxRequestBytes, memory);
            memory.Retain(input.Length); // Compressed wire bytes are no longer retained by ReadBody.
            if (keyLease?.Identity is not null) derivedReasoningKey = keyLease.ReasoningKey.ToArray();
            byte[] reasoningKey = derivedReasoningKey ?? options.ReasoningKey;
            var currentPrompts=options.DirectUpstream?c.RequestServices.GetRequiredService<ConsoleRuntime>().Settings.Prompts:options.SystemPrompts??new();
            string? sessionDigest=null;bool usesSession=affinity.UsesSession;
            ConversationHeaders? conversation=null;
            StreamingMigration.ResponseIdentityPlan? responseRestore=null;
            string? prefixDigest=null;int prefixBytes=affinity.PrefixBytes;bool preparedReserved=false;
            Func<System.Text.Json.Nodes.JsonObject,bool>? compatibility=options.DirectUpstream?root=>
            {
                bool unchanged=OutboundPreparation.IsCompatible(root,currentPrompts);
                if(unchanged)prefixDigest=affinity.PrefixDigest(root,protocol is null,sessionDigest);
                return unchanged;
            }:null;
            Func<System.Text.Json.Nodes.JsonObject,byte[]?>? prepareOpenAi=options.DirectUpstream?root=>
                OutboundPreparation.PrepareParsed(root,currentPrompts,size=>{memory.Reserve(size);preparedReserved=true;},options.MaxRequestBytes,
                    (normalized,changed)=>prefixDigest=affinity.PrefixDigest(normalized,false,sessionDigest)):null;
            Func<System.Text.Json.Nodes.JsonObject,byte[]?>? prepareMessages=options.DirectUpstream?root=>
                OutboundPreparation.PrepareParsed(root,currentPrompts,size=>{memory.Reserve(size);preparedReserved=true;},options.MaxRequestBytes,
                    (normalized,changed)=>prefixDigest=affinity.PrefixDigest(normalized,!changed,sessionDigest),canonicalInput:true):null;
            Action<JsonElement>? inspect=options.DirectUpstream?root=>
            {
                conversation=ConversationHeaders.Prepare(c.Request.Headers,root,keyLease?.Identity?.Id??"");
                if(usesSession)sessionDigest=conversation.SessionDigest;
                if(responseRestore is null && currentPrompts.Identity?.ResponseRestore == true) responseRestore=ResponseIdentityFactory.Create(root,currentPrompts.Identity);
            }:usesSession?root=>sessionDigest=SessionAffinity.SessionDigest(c.Request.Headers,root):null;
            OpenAiRequest? openAi = protocol switch
            {
                ClientProtocol.Chat => ChatRequestAdapter.Prepare(input,compatibility,consumeInput:true,inspectInput:inspect,prepareBody:prepareOpenAi),
                ClientProtocol.Responses => ResponsesRequestAdapter.Prepare(input, reasoningKey,compatibility,inspect,prepareOpenAi),
                _ => null
            };
            var prepared = openAi is null ? MessagesRequestAdapter.Prepare(input, reasoningKey, options.MaxRequestBytes, options.DirectUpstream?currentPrompts:null,inspect,compatibility,prepareMessages) :
                new PreparedMessages(openAi.Model, openAi.Stream, false, false, openAi.Body){OutboundCompatible=openAi.OutboundCompatible};
            modelName=prepared.Model;
            trace?.Model(prepared.Model);
            if(!preparedReserved&&!ReferenceEquals(input,prepared.UpstreamBody))memory.Reserve(prepared.UpstreamBody.Length);
            input=[];
            // Streaming adapters need tool/response metadata, not the old request body.
            if(openAi is not null)openAi=openAi with{Body=[],ResponseRestore=responseRestore};
            prepared=prepared with{ResponseRestore=responseRestore};
            memory.Retain(prepared.UpstreamBody.Length);
            if(options.DirectUpstream)
            {
                if(prepared.OutboundCompatible)
                {if(prepared.UpstreamBody.Length>options.MaxRequestBytes)throw new RequestFailure("出站请求超过大小限制",413,"request_too_large");}
                else prepared=prepared with{UpstreamBody=OutboundPreparation.Prepare(prepared.UpstreamBody,currentPrompts,memory.Reserve,options.MaxRequestBytes)};
            }
            memory.Retain(prepared.UpstreamBody.Length);
            affinityKey=prefixDigest is not null?affinity.KeyFromPrefix(keyLease?.Identity?.Id,prepared.Model,prefixDigest,prefixBytes):null;
            affinityKey??=affinity.Key(keyLease?.Identity?.Id,prepared.Model,sessionDigest,prepared.UpstreamBody);
            admission?.FinishPreparing();if(preparing){resources.Preparing.Release(); preparing = false;}
            if(options.Catalog is { } directory && !directory.Allows(prepared.Model))throw new RequestFailure("模型被网关禁用",403,"permission_error");
            if(options.DirectUpstream&&prepared.Stream){replay=new(c,prepared.Model,true,timeouts.Keepalive);replay.StartWaiting();}
            var excluded=new HashSet<AccountId>();
            async Task<bool> RetryRestricted()
            {
                if(!options.DirectUpstream || accountLease is null || excluded.Count>0) return false;
                keyLease?.CheckModel(prepared.Model);
                var candidates=new HashSet<AccountId>(excluded){accountLease.Account.Id};
                if(!options.Accounts!.HasEligibleAccount(prepared.Model,candidates))return false;
                var id=accountLease.Account.Id; await options.Credentials!.DisableAsync(id); excluded.Add(id);
                restricted=false; accountLease.Dispose(); accountLease=null;
                attempt=null; settlementAttempted=false; sent=false; evidence=null;
                replay??=new(c,prepared.Model,prepared.Stream,timeouts.Keepalive);
                await replay.WaitAsync(options.RestrictedReplayDelay,firstToken.Token);
                return true;
            }
            while(true)
            {
            using var outboundMemory=resources.Acquire();
            keyLease?.CheckModel(prepared.Model);
            if (options.Accounts is { } pool)
            {
                options.Catalog?.RefreshPricing();
                try { var preferred=affinity.Lookup(affinityKey);var preferredIdentity=preferred is null?null:options.Credentials?.AffinityIdentity(preferred);accountLease = affinityKey is null ? await pool.AcquireAsync(prepared.Model,attempted:excluded,cancellation:c.RequestAborted) : await pool.AcquireAffinityAsync(prepared.Model, preferred:preferred, attempted: excluded, cancellation: c.RequestAborted,cacheGrace:affinity.CacheGrace(affinityKey,preferred,preferredIdentity,prepared.UpstreamBody.Length,pool.Options.CacheGrace));selectedIdentity=options.Credentials?.AffinityIdentity(accountLease.Account.Id); }
                catch(SchedulingFailure e) when(excluded.Count>0 && e.Code=="no_eligible_account") { await Error(c,403,"upstream_error","没有可回退的账号","11140"); return; }
            }
            if (options.Credentials is { } credentials && accountLease is not null)
                credentialLease = await credentials.AcquireForHeadersAsync(accountLease, c.RequestAborted);
            if (accountLease is not null) options.Accounts!.Revalidate(accountLease, prepared.Model);
            if(options.Catalog is{} currentCatalog&&!currentCatalog.Allows(prepared.Model))throw new RequestFailure("模型被网关禁用",403,"permission_error");
            using var message = new HttpRequestMessage(HttpMethod.Post, accountLease?.Account.Upstream ?? options.Upstream);
            if (options.DirectUpstream && (accountLease is null || accountLease.Account.Upstream != SiteEndpoints.Model(accountLease.Account.Site))) throw new RequestFailure("账号出站端点不匹配",503);
            credentialLease?.ApplyTo(message);
            conversation?.Apply(message);
            if (options.DirectUpstream) message.Headers.UserAgent.ParseAdd("WorkBuddy-Gateway-CSharp/0.1");
            if (options.GoBridge) message.Headers.Authorization = new AuthenticationHeaderValue("Bearer", bridgeToken);
            if (options.GoBridge)
                foreach (string name in new[] { "User-Agent", "X-Session-ID", "X-Conversation-ID", "X-Parent-Message-ID", "X-Client-Session-ID", "X-Forwarded-For", "X-Real-IP" })
                    if (c.Request.Headers.TryGetValue(name, out var value)) message.Headers.TryAddWithoutValidation(name, value.ToArray());
            message.Headers.Add("X-Trace-ID", c.Response.Headers["X-Trace-ID"].ToString());
            bool isHy4Free=options.DirectUpstream && prepared.Model.Equals("hy4free",StringComparison.OrdinalIgnoreCase);
            var site=accountLease?.Account.Site;
            int outputCap=isHy4Free?c.RequestServices.GetRequiredService<OutputAdmission>().Options.Hy4FreeMaxTokens:0;
            long? requestedOutputTokens=null;
            int? sentOutputTokens=null;
            byte[] upstreamBody;
            if(outputCap>0)
            {
                var capped=ModelCatalog.PrepareCappedOutput(prepared.UpstreamBody,outputCap,
                    options.Catalog?.DefaultOutput(prepared.Model,site)??0,outboundMemory.Reserve,options.MaxRequestBytes);
                (upstreamBody,requestedOutputTokens,sentOutputTokens)=(capped.Body,capped.Requested,capped.Effective);
            }
            else upstreamBody=options.Catalog?.PrepareOutput(prepared.UpstreamBody,prepared.Model,site,outboundMemory.Reserve,options.MaxRequestBytes)??prepared.UpstreamBody;
            bool trackStream=isHy4Free&&prepared.Stream;
            if(trackStream)trace?.TrackStream(c.Response.Body as DeadlineWriteStream,requestedOutputTokens,sentOutputTokens);
            message.Content = new ByteArrayContent(upstreamBody); message.Content.Headers.ContentType = new MediaTypeHeaderValue("application/json");
            message.Headers.Accept.Add(new MediaTypeWithQualityHeaderValue("text/event-stream"));
            if (keyLease?.Identity is { } identity && options.Security?.Ledger is { } journal) attempt = journal.Begin(identity.Id);
            c.RequestAborted.ThrowIfCancellationRequested();
            // Caller headers/cookies/credentials are not forwarded to a synthetic upstream.
            firstToken.Arm();
            SafeUpstreamSend.Result delivery;
            try { delivery=await SafeUpstreamSend.Run(client,message,upstreamBody,retryScope,options.DirectUpstream?timeouts.TransientRetries:0,timeouts.Header,firstToken.Token,value=>sent=value,()=>trace?.Attempt(accountLease?.Account.Id.Value??""),()=>{keyLease?.CheckModel(prepared.Model);if(accountLease is not null)options.Accounts!.Revalidate(accountLease,prepared.Model);},options.UpstreamHandler);trace?.Headers(); }
            catch (OperationCanceledException) when (!c.RequestAborted.IsCancellationRequested)
            { throw new RequestFailure(firstToken.Expired?"等待上游首个有效增量超时":"等待上游响应头超时", 504, firstToken.Expired?FirstTokenBudget.ErrorCode:"api_error"); }
            finally { credentialLease?.Dispose(); credentialLease = null; }
            using (delivery)
            {
                var response=delivery.Response;
                if (!response.IsSuccessStatusCode)
                {
                    // Bound and classify the upstream error before starting an SSE response.
                    using var deadline = CancellationTokenSource.CreateLinkedTokenSource(c.RequestAborted); deadline.CancelAfter(TimeSpan.FromSeconds(5));
                    byte[] raw;
                    try { raw = await ReadBounded(await response.Content.ReadAsStreamAsync(deadline.Token), 65536, deadline.Token); }
                    catch (RequestFailure) { throw new RequestFailure("上游错误响应超过大小限制", 502, "api_error"); }
                    string? code = null;
                    try { ChunkReader.Parse(Encoding.UTF8.GetString(raw)); } catch (ConversionFailure e) { if (e.Code is "11128" or "11140" or "11134" or "6004" or "14003" or "14018" or "429") code = e.Code; }
                    try { using var parsed = JsonDocument.Parse(raw); if (parsed.RootElement.ValueKind == JsonValueKind.Object && parsed.RootElement.TryGetProperty("usage", out var u) && u.ValueKind == JsonValueKind.Object) evidence = u.GetRawText(); } catch (JsonException) { }
                    int status = response.StatusCode is >= HttpStatusCode.MultipleChoices and < HttpStatusCode.BadRequest ? 502 : (int)response.StatusCode;
                    Settle(evidence, code ?? "http_error", false, status,Encoding.UTF8.GetString(raw));
                    if(code=="11140" && status==403 && await RetryRestricted()) continue;
                    await Error(c, status, "api_error", code=="11128"?"上游渠道校验未通过（11128）；请使用上游授权接口或客户端，或向上游反馈":"上游拒绝请求", code); return;
                }
                if (response.Content.Headers.ContentType?.MediaType != "text/event-stream") throw new RequestFailure("上游未返回 SSE 响应", 502, "api_error");
                Action? observeRead=trackStream&&trace is not null?trace.UpstreamRead:null;
                await using var upstream = new IdleReadStream(await response.Content.ReadAsStreamAsync(firstToken.Token), timeouts.Idle,observeRead);
                Stream source=firstToken.Wrap(upstream);
                if(options.DirectUpstream)
                {
                    var probe=await RestrictionProbe.Run(source,c.RequestAborted,raw=>evidence=raw,FirstOutput,firstToken.Finish);
                    if(probe.Restricted)
                    {
                        Settle(probe.Usage,"11140",false,400);
                        await Error(c,400,"upstream_error","账号已被上游限制","11140"); return;
                    }
                    source=probe.Replay;
                    evidence=probe.Usage;
                }
                if(accountLease is not null)affinity.Remember(affinityKey,accountLease.Account.Id);
                if(replay is not null)await replay.StopAsync();
                if (openAi is not null)
                {
                    if (openAi.Stream&&!c.Response.HasStarted) { c.Response.ContentType = "text/event-stream"; c.Response.Headers.CacheControl = "no-cache"; c.Response.Headers["X-Accel-Buffering"] = "no"; }
                    await using var heartbeat = openAi.Stream && (options.GoBridge||options.DirectUpstream) ? new SseHeartbeat(c,timeouts.Keepalive) : null;
                    var result = await OpenAiPump.RunAsync(source, (Stream?)heartbeat ?? c.Response.Body, openAi, reasoningKey, options.MaxOutputBytes, c.RequestAborted,
                        r => Settle(r.RawUsageJson, r.Code, r.ReceivedOutput, r.Outcome == StreamOutcome.Complete ? 200 : 502),FirstOutput,firstToken.Finish,outputBudget);
                    trace?.Result(result.Outcome,result.Code);
                    if (result.Outcome is StreamOutcome.Cancelled or StreamOutcome.DownstreamFailed) c.Abort();
                    else if (!openAi.Stream)
                    {
                        if (result.Outcome != StreamOutcome.Complete) { await Error(c, result.Code==FirstTokenBudget.ErrorCode?504:502, "upstream_error", "上游响应不完整", result.Code); return; }
                        c.Response.ContentType = "application/json"; await c.Response.Body.WriteAsync(result.Body!, c.RequestAborted);
                    }
                    return;
                }
                var streamOptions = new StreamOptions(prepared.Model, prepared.Thinking, prepared.Hidden ? new ReasoningCodec(reasoningKey, prepared.Model) : null, options.MaxOutputBytes,FirstOutput,firstToken.Finish,outputBudget,prepared.ResponseRestore);
                if (prepared.Stream)
                {
                    if(!c.Response.HasStarted){c.Response.ContentType = "text/event-stream"; c.Response.Headers.CacheControl = "no-cache"; c.Response.Headers["X-Accel-Buffering"] = "no";}
                    await using var heartbeat = options.GoBridge||options.DirectUpstream ? new SseHeartbeat(c,timeouts.Keepalive) : null;
                    var result = await StreamPump.RunAsync(source, (Stream?)heartbeat ?? c.Response.Body, streamOptions, c.RequestAborted, beforeTerminal: r => Settle(r.RawUsageJson, r.Code, r.ReceivedOutput, r.Outcome == StreamOutcome.Complete ? 200 : 502));
                    trace?.Result(result.Outcome,result.Code);
                    if (result.Outcome is StreamOutcome.Cancelled or StreamOutcome.DownstreamFailed) c.Abort();
                }
                else
                {
                    var result = await MessageAggregate.RunAsync(source, streamOptions, options.MaxOutputBytes, c.RequestAborted);
                    Settle(result.RawUsageJson, result.Code, result.ReceivedOutput, result.Complete ? 200 : 502);
                    if (result.Code == "cancelled") { c.Abort(); return; }
                    if (!result.Complete) { await Error(c, result.Code==FirstTokenBudget.ErrorCode?504:502, "api_error", "上游响应不完整", result.Code); return; }
                    c.Response.ContentType = "application/json"; await c.Response.Body.WriteAsync(result.Body!, c.RequestAborted);
                }
                return;
            }
            }
        }
        catch (ConversionFailure e)when(e.Code==FirstTokenBudget.ErrorCode){await Error(c,504,"upstream_error","等待上游首个有效增量超时",e.Code);}
        catch (RequestFailure e) { await Error(c, e.Status, e.ErrorType, e.Message); }
        catch (OpenAiFailure e) { await Error(c, e.Status, "invalid_request_error", e.Message, e.Code); }
        catch (SchedulingFailure e) { await Error(c, 503, "overloaded_error", "账号暂时无法准入", e.Code); }
        catch (CredentialFailure e) { await Error(c, 503, "overloaded_error", "账号凭据暂时无法使用", e.Code); }
        catch (KeyFailure e) { if (c.RequestAborted.IsCancellationRequested) c.Abort(); else await Error(c, e.Status, e.Status switch { 401 => "authentication_error", 403 => "permission_error", 429 => "rate_limit_error", _ => "overloaded_error" }, e.Message, e.Code); }
        catch (BadHttpRequestException e) { await Error(c, e.StatusCode, e.StatusCode == 413 ? "request_too_large" : "invalid_request_error", "无效的请求体"); }
        catch (InvalidDataException) { await Error(c, 400, "invalid_request_error", "无效的压缩请求体"); }
        catch (OperationCanceledException) when (c.RequestAborted.IsCancellationRequested) { c.Abort(); }
        catch (OperationCanceledException) { await Error(c, 504, "api_error", "上游读取超时",firstToken.Expired?FirstTokenBudget.ErrorCode:null); }
        catch (HttpRequestException) { await Error(c, 502, "api_error", "无法连接上游"); }
        catch (IOException) { await Error(c, 502, "api_error", "连接中断"); }
        finally
        {
            try{if(replay is not null)await replay.DisposeAsync();}
            finally
            {
            try
            {
                if (attempt is not null && !settlementAttempted && options.Security?.Ledger is { } ledger)
                {
                    settlementAttempted = true;
                    if (sent) ledger.Complete(attempt, evidence, c.Response.StatusCode); else ledger.CancelUncharged(attempt);
                }
            }
            catch (KeyFailure e) { if (c.RequestAborted.IsCancellationRequested) c.Abort(); else await Error(c, e.Status, "overloaded_error", e.Message, e.Code); }
            finally
            {
                try { if (restricted && accountLease is not null) await options.Credentials!.DisableAsync(accountLease.Account.Id); }
                finally { if (preparing) resources.Preparing.Release(); credentialLease?.Dispose(); accountLease?.Dispose(); admission?.Dispose();if (admitted) gate.Release(); keyLease?.Dispose(); if (derivedReasoningKey is not null) CryptographicOperations.ZeroMemory(derivedReasoningKey); trace?.End(c.RequestAborted.IsCancellationRequested?499:c.Response.StatusCode); }
            }
            }
        }
    }
    internal static async Task<byte[]> ReadBody(HttpContext c, int limit, PreparationAdmission.Lease memory)
    {
        string coding = c.Request.Headers.ContentEncoding.ToString().Trim().ToLowerInvariant();
        if (coding is not ("" or "identity" or "gzip" or "zstd")) throw new RequestFailure("不支持此请求压缩编码", 415);
        if (c.Request.ContentLength > limit) throw new RequestFailure("请求体超过大小限制", 413, "request_too_large");
        using var deadline = CancellationTokenSource.CreateLinkedTokenSource(c.RequestAborted);var remaining=GracefulRequests.RemainingRead(c);
        if(remaining<=TimeSpan.Zero)throw new RequestFailure("读取请求体超时",408,"request_read_timeout");deadline.CancelAfter(remaining);
        byte[] wire;
        try
        {
            if (c.Request.ContentLength is > 0 and var length)
            {
                int size = checked((int)length);
                memory.Reserve(size);
                wire = await ReadFixedLength(c.Request.BodyReader, size, deadline.Token);
            }
            else wire = await ReadBounded(c.Request.Body, limit, deadline.Token, memory.Wire(c.Request.ContentLength));
        }
        catch(OperationCanceledException)when(!c.RequestAborted.IsCancellationRequested){throw new RequestFailure("读取请求体超时",408,"request_read_timeout");}
        if (coding == "zstd") return ZstdBody.Decode(wire, limit, memory.Reserve);
        if (coding != "gzip") return wire;
        await using var compressed = new GZipStream(new MemoryStream(wire), CompressionMode.Decompress);
        return await ReadBounded(compressed, limit, deadline.Token, memory.Reserve);
    }
    static async Task<byte[]> ReadFixedLength(PipeReader reader, int size, CancellationToken cancellation)
    {
        // Wait for an initial chunk before allocating for a large, possibly slow upload.
        ReadResult read = await reader.ReadAtLeastAsync(Math.Min(size, 16 << 10), cancellation);
        byte[]? body = null;
        int written = 0;
        while (true)
        {
            var buffer = read.Buffer;
            try
            {
                if (read.IsCanceled) throw new OperationCanceledException(cancellation);
                int remaining = size - written;
                if (buffer.Length > remaining || read.IsCompleted && buffer.Length != remaining)
                    throw new BadHttpRequestException("请求体长度与 Content-Length 不符");
                if (!buffer.IsEmpty)
                {
                    body ??= GC.AllocateUninitializedArray<byte>(size);
                    buffer.CopyTo(body.AsSpan(written));
                    written += (int)buffer.Length;
                }
                if (read.IsCompleted) return body!;
            }
            finally { reader.AdvanceTo(buffer.End); }
            int next=Math.Min(size-written,32<<10);
            read = next>0?await reader.ReadAtLeastAsync(next,cancellation):await reader.ReadAsync(cancellation);
        }
    }
    internal static async Task<byte[]> ReadBounded(Stream source, int limit, CancellationToken cancellation, Action<int>? reserve = null)
    {
        using var buffer = new MemoryStream(); byte[] chunk = new byte[Math.Min(limit + 1, 16384)];
        while (true)
        {
            int n = await source.ReadAsync(chunk.AsMemory(0, (int)Math.Min(chunk.Length, (long)limit - buffer.Length + 1)), cancellation);
            if (n == 0) return buffer.ToArray();
            if (n > limit - buffer.Length) throw new RequestFailure("数据超过大小限制", 413, "request_too_large");
            reserve?.Invoke(n); buffer.Write(chunk, 0, n);
        }
    }
    static async Task Error(HttpContext c, int status, string type, string message, string? code = null)
    {
        if(await RestrictedReplay.WriteError(c,type,message,code))return;
        if (c.Response.HasStarted) { c.Abort(); return; }
        c.Response.StatusCode = status;
        if(status is 429 or 503)c.Response.Headers.RetryAfter="1";
        var error = new Dictionary<string, object> { ["type"] = type, ["message"] = message }; if (code is not null) error["code"] = code;
        if (c.Request.Path == "/v1/messages") await GatewayJson.WriteAsync(c, new Dictionary<string, object?> { ["type"] = "error", ["error"] = error });
        else await GatewayJson.WriteAsync(c, new Dictionary<string, object?> { ["error"] = error });
    }
    sealed class IdleReadStream(Stream inner, TimeSpan timeout, Action? observeRead) : Stream
    {
        public override async ValueTask<int> ReadAsync(Memory<byte> buffer, CancellationToken cancellationToken = default)
        {
            using var deadline = CancellationTokenSource.CreateLinkedTokenSource(cancellationToken); deadline.CancelAfter(timeout);
            try
            {
                int read=await inner.ReadAsync(buffer, deadline.Token);
                if(read>0)observeRead?.Invoke();
                return read;
            }
            catch (OperationCanceledException) when (!cancellationToken.IsCancellationRequested) { throw new IOException("Upstream idle timeout"); }
        }
        protected override void Dispose(bool disposing) { if (disposing) inner.Dispose(); base.Dispose(disposing); }
        public override bool CanRead => true; public override bool CanSeek => false; public override bool CanWrite => false;
        public override long Length => throw new NotSupportedException(); public override long Position { get => throw new NotSupportedException(); set => throw new NotSupportedException(); }
        public override int Read(byte[] buffer, int offset, int count) => throw new NotSupportedException(); public override void Flush() { }
        public override long Seek(long offset, SeekOrigin origin) => throw new NotSupportedException(); public override void SetLength(long value) => throw new NotSupportedException();
        public override void Write(byte[] buffer, int offset, int count) => throw new NotSupportedException();
    }
}
