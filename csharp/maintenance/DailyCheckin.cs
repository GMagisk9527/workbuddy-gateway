using System.Net;
using System.Net.Http.Headers;
using System.Text.Json;
using AccountScheduling;

namespace AccountMaintenance;

public enum CheckinResult { Success, Already }
public interface IAccountCheckinControlPlane
{ Task<CheckinResult> CheckinAsync(AccountCredential credential,CancellationToken cancellation); }
public sealed record CheckinTarget(string Key,AccountId Id,bool Supported,bool Eligible);

public static class CheckinResponse
{
    public static CheckinResult Parse(int status,ReadOnlyMemory<byte> bytes)
    {
        try
        {
            if(bytes.Length>1<<20)throw new CredentialFailure("checkin_response_too_large");
            using var doc=JsonDocument.Parse(bytes,new JsonDocumentOptions{MaxDepth=32});var r=doc.RootElement;
            int code=r.TryGetProperty("code",out var c)&&c.TryGetInt32(out int n)?n:-1;
            string msg=r.TryGetProperty("msg",out var m)&&m.ValueKind==JsonValueKind.String?m.GetString()!:"";
            if(status is >=200 and <300&&code==0)return CheckinResult.Success;
            bool already=code is 10001 or 14001||code==-1&&(msg.Contains("已签到",StringComparison.Ordinal)||msg.Contains("already checked in",StringComparison.OrdinalIgnoreCase));
            if((status is >=200 and <300 or 400 or 409)&&already)return CheckinResult.Already;
        }
        catch(Exception e)when(e is JsonException or InvalidOperationException){throw new CredentialFailure("checkin_invalid_response");}
        throw new CredentialFailure("checkin_rejected");
    }
}
