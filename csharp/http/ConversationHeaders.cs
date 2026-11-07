using System.Security.Cryptography;
using System.Text;
using System.Text.Json;

namespace HttpMigration;

// Conversation metadata belongs to one logical HTTP request, outside the
// account/retry loop. These are ordinary correlation IDs, not client credentials.
internal sealed class ConversationHeaders
{
    internal string? SessionDigest{get;}
    internal string Id{get;}
    internal string RequestId{get;}
    internal string MessageId{get;}
    ConversationHeaders(string? session,string id,string request,string message)
    {SessionDigest=session;Id=id;RequestId=request;MessageId=message;}

    internal static ConversationHeaders Prepare(IHeaderDictionary headers,JsonElement original,string caller)
    {
        string? session=string.IsNullOrEmpty(caller)?null:SessionAffinity.SessionDigest(headers,original);
        // No session seed: independent calls stay independent. Prefix affinity
        // must not invent a conversation shared by unrelated identical prompts.
        string id=session is null?Guid.NewGuid().ToString("D"):Name(caller+"\0conversation\0"+session);
        string Resolve(string header,string snake,string camel,bool message)
        {
            string? seed=headers.TryGetValue(header,out var values)&&values.Count==1?Valid(values[0]):null;
            seed??=Field(original,snake)??Field(original,camel);
            if(seed is null&&original.TryGetProperty("metadata",out var metadata))seed=Field(metadata,snake)??Field(metadata,camel);
            if(seed is null)return Guid.NewGuid().ToString(message?"N":"D");
            if(Guid.TryParse(seed,out var uuid)&&uuid!=Guid.Empty)return uuid.ToString(seed.Length==32?"N":"D");
            return Name(caller+"\0"+id+"\0"+(message?"message":"request")+"\0"+seed);
        }
        return new(session,id,Resolve("X-Conversation-Request-ID","conversation_request_id","conversationRequestId",false),
            Resolve("X-Conversation-Message-ID","conversation_message_id","conversationMessageId",true));
    }
    static string? Field(JsonElement root,string name)
    {
        if(root.ValueKind!=JsonValueKind.Object||!root.TryGetProperty(name,out var value)||value.ValueKind!=JsonValueKind.String||value.GetRawText().Length>3074)return null;
        try{return Valid(value.GetString());}catch(InvalidOperationException){return null;}
    }
    static string? Valid(string? value)
    {
        value=value?.Trim();
        return string.IsNullOrEmpty(value)||Encoding.UTF8.GetByteCount(value)>512||value.Any(c=>c<32||c==127)?null:value;
    }
    // RFC 4122 UUIDv5 namespace hashing; SHA-1 is used for names, never authentication.
    static string Name(string value)
    {
        byte[] name=Encoding.UTF8.GetBytes(value);byte[] input=new byte[16+name.Length];
        new Guid("7518352d-f297-5ede-9583-a32f3413b74d").TryWriteBytes(input,bigEndian:true,out _);
        name.CopyTo(input,16);Span<byte> hash=stackalloc byte[20];SHA1.HashData(input,hash);
        hash[6]=(byte)((hash[6]&15)|0x50);hash[8]=(byte)((hash[8]&63)|0x80);
        return new Guid(hash[..16],bigEndian:true).ToString("D");
    }
    internal void Apply(HttpRequestMessage message)
    {
        message.Headers.Add("X-Conversation-ID",Id);
        message.Headers.Add("X-Conversation-Request-ID",RequestId);
        message.Headers.Add("X-Conversation-Message-ID",MessageId);
    }
    public override string ToString()=>"ConversationHeaders (redacted)";
}
