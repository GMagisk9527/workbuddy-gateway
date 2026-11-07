using System.Formats.Tar;
using System.IO.Compression;
using System.Net;
using System.Text;
using System.Text.Json;
using AccountScheduling;
namespace HttpMigration;

internal static class NpmCatalogTests
{
    static int checks;
    static void Check(bool value,string why){if(!value)throw new InvalidOperationException(why);checks++;}
    const string Static="""{"models":[{"id":"m","credits":"x2","maxInputTokens":8000,"maxOutputTokens":500},{"id":"fallback","credits":0}]}""";
    sealed class Transport:HttpMessageHandler
    {
        public bool TarOnly,Fail,HostileVersion;public byte[]? Archive;
        public List<string> Calls=[];
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request,CancellationToken token)
        {
            Calls.Add(request.RequestUri!.ToString());Check(request.Headers.Authorization is null&&!request.Headers.Contains("Cookie")&&!request.Headers.Contains("X-User-Id"),"Public catalog requests contain no account identity");
            if(Fail)return Task.FromResult(new HttpResponseMessage(HttpStatusCode.BadGateway));
            string uri=request.RequestUri.ToString();
            if(uri.EndsWith("/latest"))return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK){Content=new StringContent(HostileVersion?"{\"version\":\"../../bad\"}":"{\"version\":\"1.2.3\"}")});
            if(uri.EndsWith(".tgz"))return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK){Content=new ByteArrayContent(Archive??[])});
            return Task.FromResult(new HttpResponseMessage(TarOnly?HttpStatusCode.NotFound:HttpStatusCode.OK){Content=new StringContent(Static)});
        }
    }
    static byte[] Archive(string name,string contents)
    {
        using var output=new MemoryStream();using(var gzip=new GZipStream(output,CompressionLevel.Fastest,true))using(var tar=new TarWriter(gzip,TarEntryFormat.Pax,true))
        {using var data=new MemoryStream(Encoding.UTF8.GetBytes(contents));tar.WriteEntry(new PaxTarEntry(TarEntryType.RegularFile,name){DataStream=data});}
        return output.ToArray();
    }
    internal static async Task Run()
    {
        var handler=new Transport();using var client=new HttpClient(handler);var source=new NpmCatalogSource(client);
        Check(await source.Version(CancellationToken.None)=="1.2.3","Registry version parsed");
        var bytes=await source.Fetch("1.2.3",AccountSite.Cn,CancellationToken.None);Check(bytes is not null&&handler.Calls.Count==2,"Single-file catalog preferred");
        var catalog=new ModelCatalog();catalog.LoadLive(AccountSite.Cn,"""{"code":0,"data":{"models":[{"id":"m","credits":1,"maxOutputTokens":100}]}}"""u8.ToArray());
        catalog.LoadNpm(AccountSite.Cn,bytes!,true);
        Check(catalog.PriceLabel(AccountSite.Cn,"m")=="1x"&&catalog.PriceLabel(AccountSite.Cn,"fallback")=="0x","Live prices win; static-only models are retained");
        using(var snapshot=JsonDocument.Parse(catalog.Export()))
        {var model=snapshot.RootElement.GetProperty("catalogs").GetProperty("cn")[0];Check(model.GetProperty("maxInputTokens").GetInt32()==8000&&model.GetProperty("maxOutputTokens").GetInt32()==100,"Static capabilities fill gaps without overwriting live values");}
        handler.TarOnly=true;handler.Archive=Archive("package/product.cloudhosted.json",Static);
        Check(await source.Fetch("1.2.3",AccountSite.Intl,CancellationToken.None) is not null,"Registry tarball fallback reads exact site entry");
        handler.Archive=Archive("../../product.internal.json",Static);
        Check(await source.Fetch("1.2.3",AccountSite.Cn,CancellationToken.None) is null,"Archive path traversal entries cannot supply catalog or write files");
        handler.HostileVersion=true;Check(await source.Version(CancellationToken.None) is null,"Invalid version cannot alter URL path");
        handler.Fail=true;string before=Encoding.UTF8.GetString(catalog.Export());
        Check(await source.Fetch("1.2.3",AccountSite.Cn,CancellationToken.None) is null&&Encoding.UTF8.GetString(catalog.Export())==before,"Unavailable remote sources leave published catalog unchanged");
        bool rejected=false;try{catalog.LoadNpm(AccountSite.Cn,"{}"u8.ToArray(),false);}catch(ArgumentException){rejected=true;}
        Check(rejected&&Encoding.UTF8.GetString(catalog.Export())==before,"Malformed static catalog cannot partially replace live state");
        Console.WriteLine(JsonSerializer.Serialize(new{npmCatalogChecks=checks,realNetworkCalls=0,modelCreditsUsed=0}));
    }
}
