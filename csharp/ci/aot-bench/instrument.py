"""Add phase timers only to an ephemeral benchmark checkout; no request contents."""
from pathlib import Path

root=Path(__file__).resolve().parents[2]
p=root/'http/LocalGateway.cs'
s=p.read_text()
replacements={
'            byte[] input = await ReadBody(c, options.MaxRequestBytes, memory);':'''            long perfStart=System.Diagnostics.Stopwatch.GetTimestamp();
            void Perf(string stage,int bytes)
            {
                var now=System.Diagnostics.Stopwatch.GetTimestamp();
                Console.WriteLine($"PERF|{stage}|{bytes}|{System.Diagnostics.Stopwatch.GetElapsedTime(perfStart,now).TotalMilliseconds.ToString(System.Globalization.CultureInfo.InvariantCulture)}");perfStart=now;
            }
            byte[] input = await ReadBody(c, options.MaxRequestBytes, memory);Perf("read",input.Length);''',
'            modelName=prepared.Model;':'            Perf("adapter",prepared.UpstreamBody.Length);modelName=prepared.Model;',
'            affinityKey=affinity.Key(keyLease?.Identity?.Id,prepared.Model,sessionDigest,prepared.UpstreamBody);':'''            Perf("outbound",prepared.UpstreamBody.Length);
            affinityKey=affinity.Key(keyLease?.Identity?.Id,prepared.Model,sessionDigest,prepared.UpstreamBody);Perf("affinity",prepared.UpstreamBody.Length);''',
'            message.Content = new ByteArrayContent(upstreamBody);':'            Perf("catalog-and-account",upstreamBody.Length);message.Content = new ByteArrayContent(upstreamBody);',
'},options.UpstreamHandler);trace?.Headers();':'},options.UpstreamHandler);trace?.Headers();Perf("upstream",upstreamBody.Length);'
}
for old,new in replacements.items():
    assert s.count(old)==1,old
    s=s.replace(old,new)
p.write_text(s,encoding='utf-8')
print('Injected request phase timers into isolated benchmark source')
