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
        var block = new byte[16384]; var inputPin = GCHandle.Alloc(source, GCHandleType.Pinned); var outputPin = GCHandle.Alloc(block, GCHandleType.Pinned);
        try
        {
            Validate(ZSTD_initDStream(context)); Validate(ZSTD_DCtx_setParameter(context, 100, 25));
            var input = new Buffer { Data = inputPin.AddrOfPinnedObject(), Size = (nuint)source.Length };
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
        finally { inputPin.Free(); outputPin.Free(); ZSTD_freeDStream(context); }
    }
}
