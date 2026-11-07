using System.Net;
using System.Net.Http.Headers;
using System.Text;
using System.Text.Json;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;
using Microsoft.Extensions.DependencyInjection;

namespace HttpMigration;

internal static class AffinityTests
{
    static int checks;
    static void Check(bool value,string why){if(!value)throw new InvalidOperationException(why);checks++;}
    sealed class Clock:TimeProvider
    {
        long seconds;
        public override long TimestampFrequency=>1;
        public override long GetTimestamp()=>seconds;
        internal void Advance(int count)=>seconds+=count;
    }
    sealed class Control:IAccountControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken ct)=>throw new InvalidOperationException("Unexpected refresh");
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken ct)=>Task.FromResult(CredentialVerification.Usable);
    }
    sealed class Fake:HttpMessageHandler
    {
        internal readonly List<string> Accounts=[];
        internal string? Reject;
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken ct)
        {
            string id=request.Headers.Authorization!.Parameter!;Accounts.Add(id);
            Check(!request.Headers.Contains("thread-id")&&!request.Headers.Contains("session_id")&&!request.Headers.Contains("X-Client-ID"),"No raw session/official identity forwarded");
            if(id==Reject)return Task.FromResult(new HttpResponseMessage(HttpStatusCode.Forbidden){Content=new StringContent("{\"code\":11140,\"msg\":\"synthetic\"}",Encoding.UTF8,"application/json")});
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK){Content=new StringContent("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":1}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")});
        }
    }
    internal static async Task Run(string scratch)
    {
        checks=0;ParsedSessionChecks();var settings=new AffinityOptions(true);var affinity=new SessionAffinity(settings);
        byte[] body=Encoding.UTF8.GetBytes("{\"model\":\"m\",\"messages\":[{\"role\":\"system\",\"content\":\"s\"},{\"role\":\"user\",\"content\":\"u\"}],\"tools\":[]}");
        string? Key(string caller="a",string model="m",string? session=null,byte[]? input=null)=>affinity.Key(caller,model,session,input??body);
        var headers=new HeaderDictionary{{"session_id","dsh-conversation"}};
        string? session=SessionAffinity.SessionDigest(headers,"{}"u8);
        Check(session is {Length:64}&&!session.Contains("dsh"),"Session stored only as digest");
        Check(Key(session:session)!=Key("b",session:session),"API key isolation");
        Check(Key(session:session)!=Key(model:"other",session:session),"Model isolation");
        headers["thread-id"]="child-thread";var child=SessionAffinity.SessionDigest(headers,"{}"u8);
        headers["session_id"]="another-process";Check(child==SessionAffinity.SessionDigest(headers,"{}"u8),"Codex thread wins over process session");
        headers["thread-id"]="parent-thread";Check(child!=SessionAffinity.SessionDigest(headers,"{}"u8),"Subagent isolation");
        foreach(string name in new[]{"X-Conversation-ID","thread-id","X-Session-ID","session-id","session_id"})
            Check(SessionAffinity.SessionDigest(new HeaderDictionary{{name,"dsh-conversation"}},"{}"u8)==session,"Session header spelling "+name);
        Check(SessionAffinity.SessionDigest(new HeaderDictionary(),"{\"metadata\":{\"sessionId\":\"dsh-conversation\"}}"u8)==session,"Body metadata survives conversion boundary");
        Check(SessionAffinity.SessionDigest(new HeaderDictionary(),"{\"session_id\":\"dsh-conversation\",\"metadata\":{\"sessionId\":\"other\"}}"u8)==session,"Top-level precedes metadata");
        Check(SessionAffinity.SessionDigest(new HeaderDictionary{{"X-Session-ID",new string('界',200)}},"{}"u8)==null,"UTF8 seed limit");
        Check(SessionAffinity.SessionDigest(new HeaderDictionary{{"X-Session-ID","bad\0value"}},"{}"u8)==null,"Control characters rejected");
        var claude=new HeaderDictionary{{"x-claude-code-session-id","parent"},{"x-claude-code-agent-id","child"}};
        var firstAgent=SessionAffinity.SessionDigest(claude,"{}"u8);claude["x-claude-code-agent-id"]="other";
        Check(firstAgent!=SessionAffinity.SessionDigest(claude,"{}"u8),"Claude agent isolation");
        var later=Encoding.UTF8.GetBytes(Encoding.UTF8.GetString(body).Replace("}],\"tools\"","},{\"role\":\"assistant\",\"content\":\"next\"}],\"tools\""));
        Check(Key()==Key(input:later),"Appended history leaves prefix stable");
        Check(Key()!=Key(input:Encoding.UTF8.GetBytes(Encoding.UTF8.GetString(body).Replace("\"u\"","\"different\""))),"First user separates template-identical conversations");
        Check(Key()!=Key(input:Encoding.UTF8.GetBytes(Encoding.UTF8.GetString(body).Replace("\"tools\":[]","\"tools\":[{}]"))),"Tools affect prefix");
        Check(Key(session:session)==Key(session:session,input:later),"Session wins after compression/history changes");
        Check(new SessionAffinity(new()).Key("a","m",session,body)==null,"Disabled by default");
        Check(new SessionAffinity(new(true,AffinityMode.Session)).Key("a","m",null,body)==null,"No random per-request identity affinity");
        Check(affinity.Key(null,"m",session,body)==null,"No anonymous shared binding");
        var clock=new Clock();var cache=new SessionAffinity(new(true,Capacity:2,TtlSeconds:10),clock);
        var a=new AccountId("a");var b=new AccountId("b");
        cache.Remember("one",a);cache.Remember("two",b);Check(cache.Lookup("one")==a,"Cache lookup");cache.Remember("three",b);
        Check(cache.Lookup("two")==null&&cache.Count==2,"LRU bounded eviction");clock.Advance(9);Check(cache.Lookup("one")==a,"TTL sliding refresh");clock.Advance(9);Check(cache.Lookup("one")==a,"Recently used retained");clock.Advance(11);Check(cache.Lookup("one")==null,"TTL expiration");
        cache.Remember("same",a);cache.Remember("same",b);cache.Forget("same",a);Check(cache.Lookup("same")==b,"Old failure cannot remove new binding");cache.Forget("same",b);Check(cache.Lookup("same")==null,"Matching failure clears binding");
        await Parallel.ForEachAsync(Enumerable.Range(0,2000),async(i,ct)=>{cache.Remember((i%10).ToString(),a);cache.Lookup((i%10).ToString());await Task.Yield();});
        Check(cache.Count<=2,"Concurrent updates respect capacity");
        var evidenceCache=new SessionAffinity(new(true),clock);var extra=TimeSpan.FromMilliseconds(1500);evidenceCache.Remember("evidence",a);
        evidenceCache.ObserveCache("evidence",a,"identity",32768,true);
        Check(evidenceCache.CacheGrace("evidence",a,"identity",256<<10,extra) is null,"One successful cache hit never extends grace");
        evidenceCache.Remember("evidence",a);evidenceCache.ObserveCache("evidence",a,"identity",40000,true);
        Check(evidenceCache.CacheGrace("evidence",a,"identity",256<<10,extra)==extra,"Two large cache hits survive normal affinity rebinding");
        Check(evidenceCache.CacheGrace("evidence",a,"replacement",256<<10,extra) is null&&evidenceCache.CacheGrace("evidence",a,"identity",255<<10,extra) is null,"Replacement identity and small input cannot extend grace");
        evidenceCache.ObserveCache("evidence",a,"identity",0,false);Check(evidenceCache.CacheGrace("evidence",a,"identity",256<<10,extra)==extra,"Failed request is not cache evidence");
        evidenceCache.ObserveCache("evidence",a,"identity",0,true);Check(evidenceCache.CacheGrace("evidence",a,"identity",256<<10,extra) is null,"Successful zero cache observation resets evidence");
        evidenceCache.ObserveCache("evidence",a,"identity",40000,true);evidenceCache.ObserveCache("evidence",a,"identity",40000,true);clock.Advance(300);
        Check(evidenceCache.CacheGrace("evidence",a,"identity",256<<10,extra) is null,"Cache evidence expires after five minutes");
        Check(SessionAffinity.CachedTokens("{\"prompt_tokens_details\":{\"cached_tokens\":32768}}") ==32768&&SessionAffinity.CachedTokens("{}") is null,"Only explicit upstream cache usage counts");
        await SchedulingChecks(a,b);
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses"})
            await HttpChecks(Path.Combine(scratch,path.Split('/').Last()),path);
        Console.WriteLine(JsonSerializer.Serialize(new{affinityChecks=checks,realModelCalls=0,productionChanged=false}));
    }
    static void ParsedSessionChecks()
    {
        string[] names=["conversation_id","conversationId","session_id","sessionId"];
        var random=new Random(11128);
        for(int i=0;i<300;i++)
        {
            var properties=new List<string>();
            for(int n=0,count=random.Next(0,10);n<count;n++)
            {
                string name=names[random.Next(names.Length)];
                string value=random.Next(4) switch{0=>"null",1=>"123",2=>JsonSerializer.Serialize(" "+new string('x',600)+" "),_=>JsonSerializer.Serialize(" session-"+random.Next(5)+" ")};
                properties.Add(JsonSerializer.Serialize(name)+":"+value);
                if(random.Next(3)==0)properties.Add("\"metadata\":{"+JsonSerializer.Serialize(names[random.Next(4)])+":"+value+"}");
                if(random.Next(9)==0)properties.Add("\"metadata\":null");
            }
            properties.Add("\"messages\":[{\"role\":\"user\",\"content\":\""+new string('x',8192)+"\"}]");
            byte[] bytes=Encoding.UTF8.GetBytes("{"+string.Join(',',properties)+"}");
            using var document=JsonDocument.Parse(bytes);
            var headers=new HeaderDictionary();
            if(i%3==0)headers["X-Conversation-ID"]="header-session";
            if(i%7==0){headers["x-claude-code-session-id"]="claude";headers["x-claude-code-agent-id"]="child";}
            Check(SessionAffinity.SessionDigest(headers,bytes)==SessionAffinity.SessionDigest(headers,document.RootElement),"Parsed session extraction preserves priority, duplicates and bounded seeds");
        }
    }
    static async Task SchedulingChecks(AccountId a,AccountId b)
    {
        var cn=new Uri("http://127.0.0.1:1");var intl=new Uri("http://127.0.0.1:2");
        var pool=new AccountPool([new(a,cn,Disabled:true),new(b,intl)]);
        using(var lease=await pool.AcquireAsync("m",a))Check(lease.Account.Id==b,"Affinity cannot bypass disabled state");
        pool.Replace([new(a,cn,CooldownUntil:DateTimeOffset.UtcNow.AddHours(1)),new(b,intl)]);
        using(var lease=await pool.AcquireAsync("m",a))Check(lease.Account.Id==b,"Affinity cannot bypass cooldown");
        pool.Replace([new(a,cn),new(b,intl)],global:new(Block:new HashSet<AccountId>{a}));
        using(var lease=await pool.AcquireAsync("m",a))Check(lease.Account.Id==b,"Affinity cannot bypass model/account policy");
        pool.Replace([new(a,cn,AccountSite.Cn),new(b,intl,AccountSite.Intl)],new Dictionary<string,ModelPolicy>{{"m",new(FreeSites:new HashSet<AccountSite>{AccountSite.Intl},PaidSites:new HashSet<AccountSite>{AccountSite.Cn})}});
        using(var lease=await pool.AcquireAsync("m",a))Check(lease.Account.Id==b,"Free site preference takes priority");
        pool.Replace([new(a,cn),new(b,intl)]);pool.Configure(new(MaxAccount:1,AffinityWait:TimeSpan.FromMilliseconds(1)));
        using(var busy=await pool.AcquireAsync("m",a))using(var fallback=await pool.AcquireAsync("m",a))Check(fallback.Account.Id==b,"Busy affinity falls back after grace");
        using(var lease=await pool.AcquireAsync("m",a,new HashSet<AccountId>{a}))Check(lease.Account.Id==b,"Retry exclusion wins over affinity");
        using(var busy=await pool.AcquireAsync("m",a))
        {var pending=pool.AcquireAffinityAsync("m",a,cacheGrace:TimeSpan.FromMilliseconds(1500));await Task.Delay(30);Check(!pending.IsCompleted,"Validated cache grace waits for preferred busy account");busy.Dispose();using var lease=await pending;Check(lease.Account.Id==a,"Released preferred account is selected during extended grace");}
        var c=new AccountId("expiring");
        var expiring=new AccountPool([new(a,cn,Disabled:true),new(b,intl),new(c,intl,CreditExpiresAt:DateTimeOffset.UtcNow.AddHours(1),ExpiringCredit:10)],new(ExpiringFirst:true));
        using(var lease=await expiring.AcquireAffinityAsync("m",a))Check(lease.Account.Id==c,"Stale affinity must not suppress expiring-credit priority");
    }
    static async Task HttpChecks(string dir,string path)
    {
        using var first=CredentialFiles.Create(Path.Combine(dir,"a"),new(new("uid-a"),"account-a","refresh-a",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600));
        using var second=CredentialFiles.Create(Path.Combine(dir,"b"),new(new("uid-b"),"account-b","refresh-b",DateTimeOffset.UtcNow.ToUnixTimeSeconds()+3600));
        var a=new AccountId("a");var b=new AccountId("b");var endpoint=SiteEndpoints.Model(AccountSite.Cn);
        var pool=new AccountPool([new(a,endpoint),new(b,endpoint)]);var control=new Control();var manager=new CredentialManager(pool,[new(a,endpoint,first,control),new(b,endpoint,second,control)]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(dir,"keys"));using var ledger=CreditLedger.Open(Path.Combine(dir,"keys"),keys.List().Select(k=>k.Id));keys.AttachAccounting(ledger);
        var issued=keys.Create(new("affinity",QuotaTotal:100,MaxConcurrent:2));var other=keys.Create(new("other",QuotaTotal:100));
        string token=issued.TakeSecret(),otherToken=other.TakeSecret();var fake=new Fake();
        await using var app=LocalGateway.Build(new(endpoint,new byte[32],Security:new(keys,new("synthetic-admin"),ledger),Accounts:pool,Credentials:manager,DirectUpstream:true,UpstreamHandler:fake,Affinity:new(true)){RestrictedReplayDelay=TimeSpan.FromMilliseconds(20)});
        await app.StartAsync();using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=new(LocalGateway.Address(app)),Timeout=TimeSpan.FromSeconds(10)};
        async Task Send(string auth,string? session,bool stream=false)
        {
            using var req=new HttpRequestMessage(HttpMethod.Post,path);req.Headers.Authorization=new("Bearer",auth);
            if(session is not null)req.Headers.Add("session_id",session);
            req.Content=JsonContent.Create(path=="/v1/responses"?(object)new{model="m",input="question",stream}:new{model="m",messages=new[]{new{role="user",content="question"}},stream});
            using var response=await client.SendAsync(req);var text=await response.Content.ReadAsStringAsync();Check(response.IsSuccessStatusCode&&text.Contains("ok"),"HTTP protocol completes: "+path);
        }
        await Send(token,"session-one");var chosen=fake.Accounts.Last();await Send(token,"session-one",true);Check(fake.Accounts.Last()==chosen,"HTTP stable session across stream modes");
        await Send(otherToken,"session-one");Check(fake.Accounts.Last()!=chosen,"HTTP same session different key isolation");
        await Send(token,"session-one");Check(fake.Accounts.Last()==chosen,"Other caller did not overwrite binding");
        fake.Reject=chosen;int before=fake.Accounts.Count;await Send(token,"session-one");
        Check(fake.Accounts.Count==before+2&&fake.Accounts.Last()!=chosen,"Restricted preference falls back once to eligible account");
        fake.Reject=null;await Send(token,"session-one",true);Check(fake.Accounts.Last()!=chosen,"Successful fallback rebinds session");
        Check(manager.Status.Single(x=>x.Id==(chosen=="account-a"?a:b)).Disabled,"Restricted account remains disabled");
        Check(ledger.Health.PendingAttempts==0&&ledger.GetUsage(issued.Item.Id).Total==5&&ledger.GetUsage(other.Item.Id).Total==1,"Affinity leaves per-attempt billing correct");
        Check(app.Services.GetRequiredService<SessionAffinity>().Count==2,"Only caller-scoped digest entries retained");
        await app.StopAsync();
    }
}
