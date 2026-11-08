using System.Buffers.Binary;
using System.Text.Json;

namespace HttpMigration;

internal static class ImageTokenEstimate
{
    internal static int Count(JsonElement block)
    {
        var source=JsonFields.Get(block,"source");
        if(JsonFields.Text(source,"type")!="base64"||JsonFields.Text(source,"media_type")=="")return 1600;
        string data=JsonFields.Text(source,"data");
        // Only materialize a bounded header; never allocate the decoded pixel payload.
        byte[] header=new byte[65536];Span<char> quartet=stackalloc char[4];Span<byte> decoded=stackalloc byte[3];int chars=0,length=0;
        foreach(char ch in data)
        {
            if(ch is '\r' or '\n')continue;
            if(char.IsWhiteSpace(ch))break;
            quartet[chars++]=ch;if(chars!=4)continue;chars=0;
            if(!Convert.TryFromBase64Chars(quartet,decoded,out int n))break;
            int copy=Math.Min(n,header.Length-length);decoded[..copy].CopyTo(header.AsSpan(length));length+=copy;
            if(length==header.Length||n<3)break;
        }
        var (w,h)=Dimensions(header.AsSpan(0,length));if(w<=0||h<=0)return 1600;
        double scale=Math.Min(1,1568.0/Math.Max(w,h));
        return Math.Max(1,(int)Math.Ceiling(Math.Min(1200000,(double)w*h*scale*scale)/750));
    }
    static int Be(ReadOnlySpan<byte> b)=>BinaryPrimitives.ReadInt32BigEndian(b);
    static uint Crc(ReadOnlySpan<byte> b)
    {
        uint crc=uint.MaxValue;foreach(byte v in b){crc^=v;for(int j=0;j<8;j++)crc=(crc>>1)^((crc&1)!=0?0xedb88320u:0u);}return ~crc;
    }
    static (int,int) Dimensions(ReadOnlySpan<byte> b)
    {
        if(b.Length>=33&&b[..8].SequenceEqual(new byte[]{137,80,78,71,13,10,26,10}))
        {
            if(Be(b[8..])!=13||!b.Slice(12,4).SequenceEqual("IHDR"u8)||Crc(b.Slice(12,17))!=BinaryPrimitives.ReadUInt32BigEndian(b[29..]))return default;
            int w=Be(b[16..]),h=Be(b[20..]),depth=b[24],color=b[25];
            if(w<=0||h<=0||(long)w*h>long.MaxValue/8||b[26]!=0||b[27]!=0||b[28]>1)return default;
            bool valid=color switch{0=>depth is 1 or 2 or 4 or 8 or 16,3=>depth is 1 or 2 or 4 or 8,2 or 4 or 6=>depth is 8 or 16,_=>false};
            if(!valid)return default;if(color!=3)return(w,h);
            bool palette=false;int entries=0;
            for(int i=33;i<=b.Length-8;)
            {
                int n=Be(b[i..]);if(n<0)return default;var type=b.Slice(i+4,4);
                if(type.SequenceEqual("IDAT"u8))return palette?(w,h):default;
                if(n>b.Length-i-12)return default;
                if(Crc(b.Slice(i+4,n+4))!=BinaryPrimitives.ReadUInt32BigEndian(b[(i+n+8)..]))return default;
                if(type.SequenceEqual("PLTE"u8)){if(palette||n==0||n%3!=0||n/3>1<<depth)return default;palette=true;entries=n/3;}
                if(type.SequenceEqual("tRNS"u8))return palette&&n<=entries?(w,h):default;
                if(type.SequenceEqual("IHDR"u8)||type.SequenceEqual("IEND"u8))return default;
                i+=n+12;
            }
            return default;
        }
        if(b.Length>=13&&(b[..6].SequenceEqual("GIF87a"u8)||b[..6].SequenceEqual("GIF89a"u8)))
        {
            if((b[10]&128)!=0&&b.Length<13+3*(1<<((b[10]&7)+1)))return default;
            return(BinaryPrimitives.ReadUInt16LittleEndian(b[6..]),BinaryPrimitives.ReadUInt16LittleEndian(b[8..]));
        }
        if(b.Length>=4&&b[0]==255&&b[1]==216)
        {
            bool jfif=false;int width=0,height=0;
            for(int i=2;i<b.Length;)
            {
                if(b[i++]!=255)return default;while(i<b.Length&&b[i]==255)i++;if(i>=b.Length)return default;
                byte marker=b[i++];if(marker==0xd9)return default;
                if(marker is >=0xd0 and <=0xd7)continue;
                if(i>b.Length-2)return default;int n=BinaryPrimitives.ReadUInt16BigEndian(b[i..]);if(n<2||n>b.Length-i)return default;
                if(marker==0xda)return(width,height);
                if(marker==0xe0&&n>=7&&b.Slice(i+2,5).SequenceEqual("JFIF\0"u8))jfif=true;
                if(marker is 0xc0 or 0xc1 or 0xc2)
                {
                    if(width!=0||n<8||b[i+2]!=8)return default;int components=b[i+7];if(components is not (1 or 3 or 4)||n!=8+3*components)return default;
                    int maxH=0,maxV=0;
                    for(int j=0;j<components;j++)
                    {
                        int p=i+8+3*j,hv=b[p+1],hh=hv>>4,vv=hv&15;
                        if(b[p+2]>3||hh is not (1 or 2 or 4)||vv is not (1 or 2 or 4))return default;
                        for(int k=0;k<j;k++)if(b[i+8+3*k]==b[p])return default;
                        if(components==4&&(j==0&&hv is not (0x11 or 0x22)||j is 1 or 2&&hv!=0x11||j==3&&hv!=b[i+9]))return default;
                        maxH=Math.Max(maxH,hh);maxV=Math.Max(maxV,vv);
                    }
                    if(components==3)for(int j=0;j<components;j++){int hv=b[i+9+3*j];if(maxH%(hv>>4)!=0||maxV%(hv&15)!=0)return default;}
                    width=BinaryPrimitives.ReadUInt16BigEndian(b[(i+5)..]);height=BinaryPrimitives.ReadUInt16BigEndian(b[(i+3)..]);
                    if(jfif)return(width,height);
                }
                else if(marker is not (0xc4 or 0xdb or 0xdd or >=0xe0 and <=0xef or 0xfe))return default;
                i+=n;
            }
        }
        return default;
    }
}
