using System.Buffers.Binary;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;

namespace HttpMigration;

internal sealed record CacheDiagnostics(string Session, string Tools, string SystemPrompt, string History, string Account, string Route)
{
    internal const int MinimumBodyBytes = 16 << 10;
    const int SectionBytes = 64 << 10;
    static readonly byte[] ProcessKey = RandomNumberGenerator.GetBytes(32);

    internal static CacheDiagnostics Read(
        ReadOnlySpan<byte> body, string caller, string? session, string account, string? credentialIdentity, string route)
    {
        Span<byte> key = stackalloc byte[32];
        HMACSHA256.HashData(ProcessKey, Encoding.UTF8.GetBytes(caller), key);
        using var tools = NewHash(key, "tools"u8);
        using var system = NewHash(key, "system"u8);
        using var history = NewHash(key, "history"u8);
        using var identity = NewHash(key, "account"u8);
        using var sessionHash = NewHash(key, "session"u8);
        CryptographicOperations.ZeroMemory(key);

        int toolsLeft = SectionBytes, systemLeft = SectionBytes, historyLeft = SectionBytes, historyCount = 0;
        var reader = new Utf8JsonReader(body, new JsonReaderOptions { MaxDepth = 512 });
        while (reader.Read())
        {
            if (reader.TokenType != JsonTokenType.PropertyName || reader.CurrentDepth != 1) continue;
            bool isTools = reader.ValueTextEquals("tools"), isMessages = reader.ValueTextEquals("messages");
            reader.Read();
            int start = (int)reader.TokenStartIndex;
            if (isMessages && reader.TokenType == JsonTokenType.StartArray)
            {
                while (reader.Read() && reader.TokenType != JsonTokenType.EndArray)
                {
                    start = (int)reader.TokenStartIndex;
                    bool isSystem = ReadMessage(ref reader);
                    var message = body[start..(int)reader.BytesConsumed];
                    if (isSystem) Add(system, message, ref systemLeft);
                    else if (historyCount++ < 2) Add(history, message, ref historyLeft);
                }
            }
            else
            {
                reader.Skip();
                if (isTools) Add(tools, body[start..(int)reader.BytesConsumed], ref toolsLeft);
            }
        }

        identity.AppendData(Encoding.UTF8.GetBytes(account));
        identity.AppendData("\0"u8);
        if (credentialIdentity is not null) identity.AppendData(Encoding.UTF8.GetBytes(credentialIdentity));
        if (session is not null) sessionHash.AppendData(Encoding.UTF8.GetBytes(session));
        return new(session is null ? "-" : Tag(sessionHash), Tag(tools), Tag(system), Tag(history), Tag(identity), route);
    }

    static bool ReadMessage(ref Utf8JsonReader reader)
    {
        if (reader.TokenType != JsonTokenType.StartObject)
        {
            reader.Skip();
            return false;
        }
        bool system = false;
        while (reader.Read() && reader.TokenType != JsonTokenType.EndObject)
        {
            bool role = reader.ValueTextEquals("role");
            reader.Read();
            if (role) system = reader.TokenType == JsonTokenType.String && reader.ValueTextEquals("system");
            reader.Skip();
        }
        return system;
    }

    static IncrementalHash NewHash(ReadOnlySpan<byte> key, ReadOnlySpan<byte> domain)
    {
        var hash = IncrementalHash.CreateHMAC(HashAlgorithmName.SHA256, key);
        hash.AppendData(domain);
        hash.AppendData("\0"u8);
        return hash;
    }

    static void Add(IncrementalHash hash, ReadOnlySpan<byte> bytes, ref int remaining)
    {
        int count = Math.Min(bytes.Length, remaining);
        if (count == 0) return;
        Span<byte> length = stackalloc byte[sizeof(int)];
        BinaryPrimitives.WriteInt32LittleEndian(length, count);
        hash.AppendData(length);
        hash.AppendData(bytes[..count]);
        remaining -= count;
    }

    static string Tag(IncrementalHash hash)
    {
        Span<byte> value = stackalloc byte[32];
        hash.GetHashAndReset(value);
        return Convert.ToHexString(value[..12]);
    }
}
