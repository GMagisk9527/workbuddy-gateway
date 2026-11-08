using System.Reflection;
using System.Security.Cryptography;
using System.Text;

namespace HttpMigration;

internal static class AdminPage
{
    static readonly IReadOnlyDictionary<string,byte[]> Assets=new Dictionary<string,byte[]>(StringComparer.Ordinal)
    {
        ["index.html"]=Read("index.html"),["style.css"]=Read("style.css"),["app.js"]=Read("app.js"),["qrcodegen.js"]=Read("qrcodegen.js")
    };
    static readonly string Version=Convert.ToHexStringLower(SHA256.HashData(Assets["app.js"].Concat(Assets["style.css"]).Concat(Assets["qrcodegen.js"]).ToArray()))[..12];
    static byte[] Read(string name)
    {
        using var stream=Assembly.GetExecutingAssembly().GetManifestResourceStream("AdminAssets."+name)??throw new InvalidOperationException("Missing console asset");
        using var output=new MemoryStream();stream.CopyTo(output);return output.ToArray();
    }
    internal static IResult Show(HttpContext c)
    {
        if(c.Request.Path!="/ui/")return Results.Redirect("/ui/");
        Headers(c);c.Response.Headers.CacheControl="no-store";
        string html=Encoding.UTF8.GetString(Assets["index.html"]).Replace("href=\"style.css\"","href=\"/ui/style.css?v="+Version+"\"",StringComparison.Ordinal).Replace("src=\"app.js\"","src=\"/ui/app.js?v="+Version+"\"",StringComparison.Ordinal);
        html=html.Replace("src=\"qrcodegen.js\"","src=\"/ui/qrcodegen.js?v="+Version+"\"",StringComparison.Ordinal);
        return Results.Content(html,"text/html; charset=utf-8");
    }
    internal static IResult Asset(HttpContext c,string name)
    {
        if(name is not("app.js" or "style.css" or "qrcodegen.js"))return Results.NotFound();
        Headers(c);c.Response.Headers.CacheControl="public, max-age=300";
        string etag="\""+Convert.ToHexStringLower(SHA256.HashData(Assets[name]))+"\"";c.Response.Headers.ETag=etag;
        if(c.Request.Headers.IfNoneMatch==etag)return Results.StatusCode(304);
        return Results.Bytes(Assets[name],name.EndsWith(".js",StringComparison.Ordinal)?"text/javascript; charset=utf-8":"text/css; charset=utf-8");
    }
    static void Headers(HttpContext c)
    {
        c.Response.Headers["X-Content-Type-Options"]="nosniff";c.Response.Headers["Referrer-Policy"]="no-referrer";
        c.Response.Headers["Content-Security-Policy"]="default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'";
    }
}
