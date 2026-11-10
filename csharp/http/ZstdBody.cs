using System.Runtime.InteropServices;

namespace HttpMigration;

// Uses the platform zstd ABI. Input, decoded output and decoder window have independent bounds.
internal static class ZstdBody
{
    static ZstdBody()
    {
        NativeLibrary.SetDllImportResolver(typeof(ZstdBody).Assembly, (name, _, _) =>
            name == "wbg_zstd" ? NativeLibrary.Load(OperatingSystem.IsWindows() ? "zstd.dll" : "libzstd.so.1") : IntPtr.Zero);
    }
    [StructLayout(LayoutKind.Sequential)] struct Buffer { public IntPtr Data; public nuint Size, Position; }
    [DllImport("wbg_zstd", CallingConvention = CallingConvention.Cdecl)] static extern IntPtr ZSTD_createDStream();
    [DllImport("wbg_zstd", CallingConvention = CallingConvention.Cdecl)] static extern nuint ZSTD_freeDStream(IntPtr context);
    [DllImport("wbg_zstd", CallingConvention = CallingConvention.Cdecl)] static extern nuint ZSTD_initDStream(IntPtr context);
    [DllImport("wbg_zstd", CallingConvention = CallingConvention.Cdecl)] static extern nuint ZSTD_DCtx_setParameter(IntPtr context, int parameter, int value);
    [DllImport("wbg_zstd", CallingConvention = CallingConvention.Cdecl)] static extern nuint ZSTD_decompressStream(IntPtr context, ref Buffer output, ref Buffer input);
    [DllImport("wbg_zstd", CallingConvention = CallingConvention.Cdecl)] static extern ulong ZSTD_getFrameContentSize(IntPtr source, nuint size);
    [DllImport("wbg_zstd", CallingConvention = CallingConvention.Cdecl)] static extern nuint ZSTD_findFrameCompressedSize(IntPtr source, nuint size);
    [DllImport("wbg_zstd", CallingConvention = CallingConvention.Cdecl)] static extern uint ZSTD_isError(nuint code);
    static void Validate(nuint code) { if (ZSTD_isError(code) != 0) throw new InvalidDataException("Invalid zstd request"); }
    public static byte[] Decode(byte[] source, int limit, Action<int>? reserve = null)
    {
        try { return DecodeNative(source, limit, reserve); }
        catch (DllNotFoundException) { throw new RequestFailure("服务器缺少 zstd 解码库", 415); }
        catch (EntryPointNotFoundException) { throw new RequestFailure("服务器 zstd 解码库不兼容", 415); }
    }
    static byte[] DecodeNative(byte[] source, int limit, Action<int>? reserve)
    {
        IntPtr context = ZSTD_createDStream(); if (context == IntPtr.Zero) throw new InvalidDataException("Cannot create zstd decoder");
        var inputPin = GCHandle.Alloc(source, GCHandleType.Pinned);
        try
        {
            Validate(ZSTD_initDStream(context)); Validate(ZSTD_DCtx_setParameter(context, 100, 25));
            var input = new Buffer { Data = inputPin.AddrOfPinnedObject(), Size = (nuint)source.Length };
            ulong decodedSize = ZSTD_getFrameContentSize(input.Data, input.Size);
            // Unknown sizes and concatenated frames use the incremental decoder.
            if (decodedSize > 0 && decodedSize <= (ulong)limit &&
                ZSTD_findFrameCompressedSize(input.Data, input.Size) == input.Size)
            {
                reserve?.Invoke((int)decodedSize);
                return DecodeSingleFrame(context, ref input, (int)decodedSize);
            }
            return DecodeFrames(context, ref input, limit, reserve);
        }
        finally { inputPin.Free(); ZSTD_freeDStream(context); }
    }
    static byte[] DecodeSingleFrame(IntPtr context, ref Buffer input, int decodedSize)
    {
        byte[] decoded = GC.AllocateUninitializedArray<byte>(decodedSize);
        var pin = GCHandle.Alloc(decoded, GCHandleType.Pinned);
        try
        {
            var output = new Buffer { Data = pin.AddrOfPinnedObject(), Size = (nuint)decodedSize };
            for (int steps = 0; steps < 65536; steps++)
            {
                nuint previousInput = input.Position, previousOutput = output.Position;
                nuint needed = ZSTD_decompressStream(context, ref output, ref input);
                Validate(needed);
                if (needed == 0)
                {
                    if (input.Position == input.Size && output.Position == output.Size) return decoded;
                    throw new InvalidDataException("Invalid zstd frame size");
                }
                if (input.Position == previousInput && output.Position == previousOutput)
                    throw new InvalidDataException("Truncated zstd request");
            }
            throw new InvalidDataException("Too many zstd decoder steps");
        }
        finally { pin.Free(); }
    }
    static byte[] DecodeFrames(IntPtr context, ref Buffer input, int limit, Action<int>? reserve)
    {
        var block = new byte[16384];
        var outputPin = GCHandle.Alloc(block, GCHandleType.Pinned);
        try
        {
            using var decoded = new MemoryStream(); nuint needed = 1; int steps = 0;
            while (input.Position < input.Size || needed != 0)
            {
                if (++steps > 65536) throw new InvalidDataException("Too many zstd frames");
                var output = new Buffer { Data = outputPin.AddrOfPinnedObject(), Size = (nuint)block.Length }; nuint previous = input.Position;
                needed = ZSTD_decompressStream(context, ref output, ref input); Validate(needed);
                if ((long)output.Position > limit - decoded.Length) throw new RequestFailure("请求体超过大小限制", 413, "request_too_large");
                reserve?.Invoke((int)output.Position); decoded.Write(block, 0, (int)output.Position);
                if (needed == 0 && input.Position == input.Size) return decoded.ToArray();
                if (input.Position == previous && output.Position == 0) throw new InvalidDataException("Truncated zstd request");
            }
            throw new InvalidDataException("Empty zstd request");
        }
        finally { outputPin.Free(); }
    }
}
