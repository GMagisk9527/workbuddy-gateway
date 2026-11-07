using System.Security.Cryptography;
using System.Text;

namespace StreamingMigration;

// Compatible with Go's nonce || ciphertext || GCM tag envelope. The caller supplies
// credential-derived key material; the converter never reads configuration or keys.
public sealed class ReasoningCodec
{
    const string Prefix = "wbg-r1.";
    readonly byte[] key;
    readonly byte[] aad;
    public ReasoningCodec(ReadOnlySpan<byte> key, string model, string protocol = "messages")
    {
        if (key.Length != 32) throw new ArgumentException("Expected 256-bit key", nameof(key));
        this.key = key.ToArray();
        aad = Encoding.UTF8.GetBytes("workbuddy-gateway/reasoning/v1\0" + protocol + "\0" + model);
    }
    public string Seal(string text)
    {
        byte[] plain = Encoding.UTF8.GetBytes(text), envelope = new byte[12 + plain.Length + 16];
        RandomNumberGenerator.Fill(envelope.AsSpan(0, 12));
        using var aes = new AesGcm(key, 16);
        aes.Encrypt(envelope.AsSpan(0, 12), plain, envelope.AsSpan(12, plain.Length), envelope.AsSpan(12 + plain.Length), aad);
        return Prefix + Convert.ToBase64String(envelope).TrimEnd('=');
    }
    public string Open(string value)
    {
        if (!value.StartsWith(Prefix, StringComparison.Ordinal)) throw new CryptographicException("Invalid reasoning envelope");
        string encoded = value[Prefix.Length..];
        byte[] raw = Convert.FromBase64String(encoded.PadRight((encoded.Length + 3) / 4 * 4, '='));
        if (raw.Length < 28) throw new CryptographicException("Invalid reasoning envelope");
        byte[] plain = new byte[raw.Length - 28];
        using var aes = new AesGcm(key, 16);
        aes.Decrypt(raw.AsSpan(0, 12), raw.AsSpan(12, plain.Length), raw.AsSpan(12 + plain.Length), plain, aad);
        return Encoding.UTF8.GetString(plain);
    }
}
