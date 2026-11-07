using System.Net;
using System.Net.Sockets;
using System.Text;

namespace HttpMigration;

internal static class SafeUpstreamTests
{
    sealed class Backend(HttpRequestException? failure,int failures=1):HttpMessageHandler
    {
        internal int Calls;internal List<string> Bodies=[];internal List<string> Auth=[];
        protected override async Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken token)
        {
            Calls++;Bodies.Add(await request.Content!.ReadAsStringAsync(token));Auth.Add(request.Headers.Authorization?.ToString()??"");
            if(failure is not null&&Calls<=failures)throw failure;
            return new(HttpStatusCode.OK){Content=new StringContent("synthetic")};
        }
    }
    internal static async Task<int> Run()
    {
        int checks=0;void Check(bool ok,string why){if(!ok)throw new InvalidOperationException(why);checks++;}
        var dns=new HttpRequestException(HttpRequestError.NameResolutionError,"synthetic");
        var refused=new HttpRequestException(HttpRequestError.ConnectionError,"synthetic",new SocketException((int)SocketError.ConnectionRefused));
        var reset=new HttpRequestException(HttpRequestError.ConnectionError,"synthetic",new SocketException((int)SocketError.ConnectionReset));
        foreach(var error in new[]{dns,refused,reset,new HttpRequestException(HttpRequestError.ResponseEnded,"synthetic"),new HttpRequestException(HttpRequestError.SecureConnectionError,"synthetic")})
        {
            var backend=new Backend(error);using var client=new HttpClient(backend,false);using var template=new HttpRequestMessage(HttpMethod.Post,"https://synthetic.invalid/model"){Content=new ByteArrayContent("synthetic-body"u8.ToArray())};template.Headers.Authorization=new("Bearer","synthetic-token");
            var gate=new RetryAdmission();gate.Configure(new(BackoffMaxMs:0));using var scope=gate.Acquire();bool sent=false;int attempts=0,validated=0;bool safe=ReferenceEquals(error,dns)||ReferenceEquals(error,refused);
            try
            {
                using var delivery=await SafeUpstreamSend.Run(client,template,"synthetic-body"u8.ToArray(),scope,2,TimeSpan.FromSeconds(1),default,v=>sent=v,()=>attempts++,()=>validated++,backend);
                Check(safe&&backend.Calls==2&&attempts==2&&validated==2&&sent,"Only definitely-unsent failures retry and revalidate");
                Check(gate.Status.Active==1&&backend.Bodies.All(b=>b=="synthetic-body")&&backend.Auth.All(a=>a=="Bearer synthetic-token"),"Retry retains request and holds shared permit through response lifetime");
            }
            catch(HttpRequestException){Check(!safe&&backend.Calls==1&&sent,"Ambiguous send/reset/TLS error never replays POST");}
            Check(gate.Status.Active==0,"Delivery or failure releases retry permits");
        }
        foreach(int retries in new[]{0,2})
        {
            var backend=new Backend(dns,10);using var client=new HttpClient(backend,false);using var template=new HttpRequestMessage(HttpMethod.Post,"https://synthetic.invalid/");
            var gate=new RetryAdmission();gate.Configure(new(BackoffMaxMs:0));using var scope=gate.Acquire();bool sent=true;
            try{using var ignored=await SafeUpstreamSend.Run(client,template,[],scope,retries,TimeSpan.FromSeconds(1),default,v=>sent=v,()=>{},()=>{},backend);throw new InvalidOperationException("Expected send failure");}
            catch(HttpRequestException){Check(backend.Calls==retries+1&&!sent&&gate.Status.Active==0,"Failed unsent attempts obey limit and remain safe to cancel uncharged");}
        }
        return checks;
    }
}
