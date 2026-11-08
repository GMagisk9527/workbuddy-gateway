using System.Formats.Tar;
using System.IO.Compression;
using System.Text.Json;
using AccountScheduling;

namespace HttpMigration;

// Catalog data only: never execute package code, extract files or send credentials.
public sealed class NpmCatalogSource(HttpClient client)
{
    static readonly string[] Registries=["https://registry.npmjs.org/@tencent-ai/codebuddy-code","https://registry.npmmirror.com/@tencent-ai/codebuddy-code"];
    async Task<byte[]> Get(string url,int limit,CancellationToken token)
    {
        using var timeout=CancellationTokenSource.CreateLinkedTokenSource(token);timeout.CancelAfter(TimeSpan.FromSeconds(30));
        using var request=new HttpRequestMessage(HttpMethod.Get,url);
        using var response=await client.SendAsync(request,HttpCompletionOption.ResponseHeadersRead,timeout.Token);
        response.EnsureSuccessStatusCode();
        if(response.Content.Headers.ContentLength>limit)throw new InvalidDataException("Catalog response budget");
        return await LocalGateway.ReadBounded(await response.Content.ReadAsStreamAsync(timeout.Token),limit,timeout.Token);
    }
    static bool Failure(Exception e)=>e is HttpRequestException or IOException or JsonException or ArgumentException or InvalidOperationException or RequestFailure;
    public async Task<string?> Version(CancellationToken token)
    {
        foreach(string registry in Registries)
        {
            try
            {
                using var doc=JsonDocument.Parse(await Get(registry+"/latest",1<<20,token));
                string? value=doc.RootElement.GetProperty("version").GetString();
                if(value is{Length:>0 and <100}&&value.All(c=>char.IsAsciiLetterOrDigit(c)||c is '.' or '-' or '+'))return value;
            }
            catch(OperationCanceledException)when(!token.IsCancellationRequested){}
            catch(Exception e)when(Failure(e)||e is KeyNotFoundException){}
        }
        return null;
    }
    public async Task<byte[]?> Fetch(string version,AccountSite site,CancellationToken token)
    {
        string file=site==AccountSite.Cn?"product.internal.json":"product.cloudhosted.json";
        try{var bytes=await Get($"https://unpkg.com/@tencent-ai/codebuddy-code@{version}/{file}",4<<20,token);Validate(bytes);return bytes;}
        catch(OperationCanceledException)when(!token.IsCancellationRequested){}
        catch(Exception e)when(Failure(e)){}
        foreach(string registry in Registries)
        {
            try
            {
                var compressed=await Get(registry+"/-/codebuddy-code-"+version+".tgz",80<<20,token);
                using var input=new MemoryStream(compressed);using var gzip=new GZipStream(input,CompressionMode.Decompress);using var limited=new LimitStream(gzip,256L<<20);
                using var tar=new TarReader(limited);
                for(int count=0;count<10000;count++)
                {
                    var entry=await tar.GetNextEntryAsync(false,token);if(entry is null)break;
                    if(entry.Name!="package/"+file&&entry.Name!=file)continue;
                    if(entry.EntryType is not(TarEntryType.RegularFile or TarEntryType.V7RegularFile)||entry.Length>4<<20||entry.DataStream is null)throw new InvalidDataException("Invalid catalog archive entry");
                    var bytes=await LocalGateway.ReadBounded(entry.DataStream,4<<20,token);Validate(bytes);return bytes;
                }
            }
            catch(OperationCanceledException)when(!token.IsCancellationRequested){}
            catch(Exception e)when(Failure(e)){}
        }
        return null;
    }
    static void Validate(byte[] bytes){var catalog=new ModelCatalog();catalog.LoadNpm(AccountSite.Cn,bytes,false);}
    sealed class LimitStream(Stream source,long limit):Stream
    {
        long read;
        int Count(int count){read+=count;if(read>limit)throw new InvalidDataException("Catalog archive expansion budget");return count;}
        public override int Read(byte[] b,int offset,int count)=>Count(source.Read(b,offset,(int)Math.Min(count,limit-read+1)));
        public override async ValueTask<int> ReadAsync(Memory<byte> b,CancellationToken token=default)=>Count(await source.ReadAsync(b[..(int)Math.Min(b.Length,limit-read+1)],token));
        public override bool CanRead=>true;public override bool CanSeek=>false;public override bool CanWrite=>false;
        public override long Length=>throw new NotSupportedException();public override long Position{get=>read;set=>throw new NotSupportedException();}
        public override void Flush(){}public override long Seek(long o,SeekOrigin s)=>throw new NotSupportedException();public override void SetLength(long n)=>throw new NotSupportedException();public override void Write(byte[] b,int o,int n)=>throw new NotSupportedException();
    }
}
