using System.Net;
using System.Net.Sockets;

namespace HttpMigration;

internal static class SafeUpstreamSend
{
    // ConnectionError alone is insufficient: resets/timeouts can follow a POST.
    // Only DNS failure and an explicit refused TCP connection establish no send.
    internal static bool DefinitelyUnsent(HttpRequestException error)
    {
        if(error.StatusCode is not null)return false;
        if(error.HttpRequestError==HttpRequestError.NameResolutionError)return true;
        return error.HttpRequestError==HttpRequestError.ConnectionError&&error.InnerException is SocketException socket&&socket.SocketErrorCode==SocketError.ConnectionRefused;
    }
    internal static HttpClient CreateClient(HttpMessageHandler? handler=null)=>new(handler??new SocketsHttpHandler{UseProxy=false,AllowAutoRedirect=false,UseCookies=false,AutomaticDecompression=DecompressionMethods.GZip|DecompressionMethods.Deflate,PooledConnectionLifetime=TimeSpan.FromMinutes(5)},handler is null){Timeout=Timeout.InfiniteTimeSpan};
    internal sealed class Result(HttpResponseMessage response,IDisposable? permit,HttpClient? isolated):IDisposable
    {
        internal HttpResponseMessage Response{get;}=response;
        public void Dispose(){try{Response.Dispose();}finally{permit?.Dispose();isolated?.Dispose();}}
    }
    internal static async Task<Result> Run(HttpClient client,HttpRequestMessage template,byte[] body,RetryAdmission.Scope? scope,int retries,TimeSpan headerTimeout,CancellationToken cancellation,Action<bool> sent,Action attempt,Action revalidate,HttpMessageHandler? testHandler=null)
    {
        for(int index=0;;index++)
        {
            IDisposable? permit=null;HttpClient? isolated=null;
            try
            {
                permit=scope is null?null:await scope.BeginAsync(cancellation);revalidate();
                using var message=new HttpRequestMessage(template.Method,template.RequestUri){Version=template.Version,VersionPolicy=template.VersionPolicy,Content=new ByteArrayContent(body)};
                foreach(var header in template.Headers)message.Headers.TryAddWithoutValidation(header.Key,header.Value);
                if(template.Content is not null)foreach(var header in template.Content.Headers)message.Content.Headers.TryAddWithoutValidation(header.Key,header.Value);
                if(index>0)isolated=CreateClient(testHandler);
                using var deadline=CancellationTokenSource.CreateLinkedTokenSource(cancellation);deadline.CancelAfter(headerTimeout);
                sent(true);attempt();
                var response=await (isolated??client).SendAsync(message,HttpCompletionOption.ResponseHeadersRead,deadline.Token);
                return new(response,permit,isolated);
            }
            catch(HttpRequestException e)when(DefinitelyUnsent(e))
            {
                permit?.Dispose();isolated?.Dispose();sent(false);
                if(index>=retries||cancellation.IsCancellationRequested)throw;
            }
            catch{permit?.Dispose();isolated?.Dispose();throw;}
        }
    }
}
