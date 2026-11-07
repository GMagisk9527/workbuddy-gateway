using System.Net;
using System.Net.Sockets;
using System.Text;
using AccountMaintenance;
using AccountScheduling;
using LedgerMigration;

namespace HttpMigration;

internal static class ServiceTimeoutTests
{
    static int checks;static void Check(bool value,string message){if(!value)throw new InvalidOperationException(message);checks++;}
    sealed class Sink:Stream
    {
        internal int Writes,Delay=20;internal bool BlockFlush;
        public override async ValueTask WriteAsync(ReadOnlyMemory<byte> buffer,CancellationToken cancellationToken=default){await Task.Delay(Delay,cancellationToken);Writes++;}
        public override Task FlushAsync(CancellationToken token)=>Task.Delay(BlockFlush?Timeout.Infinite:0,token);
        public override bool CanRead=>false;public override bool CanSeek=>false;public override bool CanWrite=>true;
        public override long Length=>throw new NotSupportedException();public override long Position{get=>throw new NotSupportedException();set=>throw new NotSupportedException();}
        public override int Read(byte[] b,int o,int n)=>throw new NotSupportedException();public override void Flush(){}public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long v)=>throw new NotSupportedException();public override void Write(byte[] b,int o,int n)=>throw new NotSupportedException();
    }
    sealed class Control:IAccountControlPlane
    {
        public Task<AccountCredential> RefreshAsync(AccountCredential c,CancellationToken token)=>throw new InvalidOperationException();
        public Task<CredentialVerification> VerifyAsync(AccountCredential c,CancellationToken token)=>Task.FromResult(CredentialVerification.Usable);
    }
    sealed class Backend:HttpMessageHandler
    {
        public int Calls;public TaskCompletionSource Entered=new(TaskCreationOptions.RunContinuationsAsynchronously),Release=new(TaskCreationOptions.RunContinuationsAsynchronously);
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken token)
        {Calls++;Entered.TrySetResult();await Release.Task.WaitAsync(token);return new(HttpStatusCode.OK){Content=new StringContent("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":1}}\n\ndata: [DONE]\n\n",Encoding.UTF8,"text/event-stream")};}
    }
    internal static async Task<int> Run(string root)
    {
        checks=0;var sink=new Sink();int aborted=0;using var writer=new DeadlineWriteStream(sink,TimeSpan.FromMilliseconds(80),()=>aborted++);
        for(int i=0;i<6;i++){await writer.WriteAsync(new byte[8]);await writer.FlushAsync();}
        Check(sink.Writes==6&&aborted==0,"Per-write deadline allows total streaming duration beyond deadline");
        sink.Delay=Timeout.Infinite;try{await writer.WriteAsync(new byte[8]);throw new InvalidOperationException("Blocked write did not time out");}catch(IOException){}
        Check(aborted==1,"Blocked downstream write aborts and reports IO failure");sink.BlockFlush=true;
        try{await writer.FlushAsync(default);throw new InvalidOperationException("Blocked flush did not time out");}catch(IOException){}
        Check(aborted==2,"Blocked flush also has a write deadline");
        using var canceled=new CancellationTokenSource();canceled.Cancel();try{await writer.WriteAsync(new byte[1],canceled.Token);}catch(OperationCanceledException){}
        Check(aborted==2,"Caller cancellation is not misclassified as write timeout");
        using(var settings=new ConsoleSettings(new()))
        {
            var host=new HostOptions();settings.AttachService(host);Check(host.ShutdownTimeout==TimeSpan.FromSeconds(135)&&settings.Service==ServiceTimeouts.Default,"Shutdown defaults allow 120s drain and 15s cleanup");
            settings.Update("""{"server":{"requestReadTimeoutSeconds":1,"shutdownGraceSeconds":2,"shutdownCleanupSeconds":3},"downstream":{"writeTimeoutSeconds":4}}""",settings.View().Revision);
            Check(settings.Service==new ServiceTimeouts(TimeSpan.FromSeconds(1),TimeSpan.FromSeconds(4),TimeSpan.FromSeconds(2),TimeSpan.FromSeconds(3))&&host.ShutdownTimeout==TimeSpan.FromSeconds(5),"Service limits hot reload with host shutdown budget");
            settings.Update("""{"server":{"shutdownGraceSeconds":0},"downstream":{"writeTimeoutSeconds":0}}""",settings.View().Revision);Check(settings.Service==ServiceTimeouts.Default,"Explicit zero restores Go service defaults");
        }
        Directory.CreateDirectory(root);var endpoint=SiteEndpoints.Model(AccountSite.Cn);var id=new AccountId("a");
        using var files=CredentialFiles.Create(Path.Combine(root,"credential"),new(new("synthetic-user"),"synthetic-access","synthetic-refresh",DateTimeOffset.UtcNow.AddHours(2).ToUnixTimeSeconds()));
        var pool=new AccountPool([new AccountSnapshot(id,endpoint)]);var credentials=new CredentialManager(pool,[new(id,endpoint,files,new Control())]);
        using var keys=PreviewKeyDirectory.Open(Path.Combine(root,"keys"));using var ledger=CreditLedger.Open(Path.Combine(root,"keys"),[]);keys.AttachAccounting(ledger);var key=keys.Create(new("synthetic",QuotaTotal:100,MaxConcurrent:10));string secret=key.TakeSecret();
        var backend=new Backend();var options=new LocalGatewayOptions(endpoint,new byte[32],Security:new(keys,new("admin"),ledger),Accounts:pool,Credentials:credentials,DirectUpstream:true,UpstreamHandler:backend){ServiceTimeouts=ServiceTimeouts.Default with{Read=TimeSpan.FromMilliseconds(120),Grace=TimeSpan.FromMilliseconds(120)}};
        await using var app=LocalGateway.Build(options);await app.StartAsync();var address=new Uri(LocalGateway.Address(app));
        foreach(string path in new[]{"/v1/messages","/v1/chat/completions","/v1/responses","/v1/search","/v1/fetch","/v1/messages/count_tokens"})
        {
            using var tcp=new TcpClient();await tcp.ConnectAsync(address.Host,address.Port);var stream=tcp.GetStream();
            var request=$"POST {path} HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer {secret}\r\nContent-Type: application/json\r\nContent-Length: 1000000\r\nConnection: close\r\n\r\n{{";
            await stream.WriteAsync(Encoding.ASCII.GetBytes(request));using var reader=new StreamReader(stream,Encoding.ASCII);string? status=await reader.ReadLineAsync().WaitAsync(TimeSpan.FromSeconds(3));
            Check(status?.Contains("408")==true,"Slow incomplete body hits read deadline: "+path);
            for(int i=0;i<100&&app.Services.GetRequiredService<RequestAdmission>().Status.Active>0;i++)await Task.Delay(5);
            Check(backend.Calls==0&&ledger.Health.PendingAttempts==0&&app.Services.GetRequiredService<PreparationAdmission>().Used==0,"Read timeout releases preparation and never calls upstream");
        }
        using var client=new HttpClient(new SocketsHttpHandler{UseProxy=false}){BaseAddress=address,Timeout=TimeSpan.FromSeconds(5)};client.DefaultRequestHeaders.Authorization=new("Bearer",secret);
        var pending=client.PostAsync("/v1/chat/completions",new StringContent("""{"model":"m","messages":[{"role":"user","content":"synthetic"}]}""",Encoding.UTF8,"application/json"));
        await backend.Entered.Task.WaitAsync(TimeSpan.FromSeconds(3));await app.StopAsync().WaitAsync(TimeSpan.FromSeconds(3));
        try{using var stopped=await pending;Check(!stopped.IsSuccessStatusCode,"Shutdown cannot pretend a stalled upstream completed");}catch(HttpRequestException){checks++;}
        Check(backend.Calls==1&&pool.Status.Active==0&&ledger.Health.PendingAttempts==0&&app.Services.GetRequiredService<RequestAdmission>().Status.Active==0,"Grace expiration cancels blocked upstream and settles/releases resources");
        return checks;
    }
}
