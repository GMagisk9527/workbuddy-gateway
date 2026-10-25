# 大上下文性能回归

性能改进集中在请求读入、保留数字精度的 JSON 解码及转换大小预检。

- 已通过解压与输入预算校验的正文直接交给协议 handler，省去再次 `ReadAll` 的副本。
  剩余字节只消费一次；外层另有 reader 包装时仍通过包装读取。
- 已知且不超过 32 MiB 的 Content-Length 用于一次性预分配，额外保留一个 EOF 探测字节。
  长度缺失、与实际不符、读取失败和外层大小限制继续按读取结果处理。
- Go 1.27 且启用标准库 jsonv2 时，直接从已有输入切片解码，使用标准库 v1 兼容选项，
  将 interface 中的数字保留为 json.Number。旧工具链或关闭 jsonv2 时沿用 Decoder.UseNumber。
  两条路径都保留单个 JSON 值检查，后续仅归一化既有的生成参数。
- 大小预检按连续普通 ASCII 段计数，转义、UTF-8、无效 UTF-8 与精确边界沿用原规则。
  扫描范围受剩余预算约束，超限字符串仍提前失败。

## 合成基准

2026-10-25，本地 Windows amd64、i5-13500HX、Go 1.27.3，默认 jsonv2；
基线为 main aea3b5f，使用同一份合成工具调用/工具结果，benchtime=300ms、每项三次取中位数。
数字均为微基准；JSON 转义及固定字段使实际请求略大于表中的工具结果文本量。

| 工具结果文本量 | 转换耗时：前 → 后 | 累计分配：前 → 后 |
|---|---|---|
| 1 KiB | 26.6 → 26.8 μs | 18.5 → 13.9 KB |
| 128 KiB | 1.14 → 0.77 ms | 996 → 439 KB |
| 1 MiB | 8.58 → 5.43 ms | 9.62 → 4.63 MB |
| 4 MiB | 27.8 → 23.0 ms | 36.0 → 17.7 MB |

本表的 KB/MB 为十进制累计分配量，不能当成峰值 RSS。转换计时包含
decodeAnthropicMessageBody、anthropicToChatRequest、marshalConvertedRequest，
不含 HTTP 读取、解压、全部兼容处理、出站压缩、网络和上游推理。
耗时会随环境波动；以上不代表线上请求总耗时的改善比例。

## Messages 流式输出

普通流式正文只发送增量，不再在状态机中保留完整正文副本；非流式聚合仍保留正文。
这不改变推理签名/回放、工具参数校验及待发送事件的共享预算。比如发送 1 MiB
普通正文时，原先额外保留的 1,114,112 B Builder 容量现在为 0；这不是整个请求的内存占用。

常见文本增量改用结构体 JSON 编码，直接追加到本次上游 chunk 的输出缓冲，减少临时
map、字符串拼接及逐层帧复制。JSON 转义仍由标准库完成；等待早先工具身份的事件
仍先检查共享预算，并持有独立帧，不能复用或覆盖尚未发出的字节。

同日同机、Go 1.27.3，以 main 9d32b53 为基线，`BenchmarkMessagesStream` 使用 128 个
合成文本增量、结束事件与 usage，含上游解码、协议转换和丢弃输出的 writer；
benchtime=500ms、每项五次取中位数：

| 指标 | 优化前 | 优化后 |
|---|---|---|
| 每条流累计分配 | 449,533 B | 309,513 B（约减 31%） |
| 每条流分配次数 | 7,108 | 5,046（约减 29%） |
| 每条流耗时 | 1.049 ms | 0.641 ms |

计时受本机负载影响，分配量更适合作为本次回归指标；这些数字不代表上游推理或网络变快。
可用 `go test -run '^$' -bench '^BenchmarkMessagesStream$' -benchmem` 复测。

## 出站 gzip 复用与统计快照

国内站 `upstream.requestProfile: "official"` 的模型请求压缩器最多缓存两个空闲实例。
取用前重置到当前输出，归还前重置到 `io.Discard`，输出正文缓冲仍由单次请求独占。
缓存为空时按原方式创建，满时丢弃额外实例，不增加排队；默认压缩级别、压缩字节和
重试正文保持不变。legacy 与国际站的原样正文路径不受影响。

这会保留至多两个压缩器的工作区，交换更少的重复分配；缓存不是活跃请求并发上限。
不能将分配下降解释为常驻内存下降，尤其应结合低内存主机的现有准入水位观察。

统计持久化在锁内捕获版本号和稳定快照，随后释放统计锁再执行 JSON 编码。
账号及模型状态 map 单独复制；明细和其中的可选数值在发布后保持不可变，因此只固定
有界历史视图，不复制全部请求明细。追加与裁剪不得改写旧行，快照也不保留追加容量。
`statsWriteMu` 保证一次只有一份落盘快照；保存完成仅推进其捕获的版本，期间新增记录
继续等待下一次保存。文件大小保护、原子替换、sync 和计费 WAL 保持原有语义。

2026-10-25，同机 Go 1.27.3，以 main b93524e 为基线，400ms × 3 取中位数：

| gzip 合成正文约大小 | 累计分配：前 → 后 | 分配次数：前 → 后 |
|---|---|---|
| 1 KiB | 1,076,889 → 880 B | 18 → 4 |
| 128 KiB | 1,206,929 → 130,928 B | 25 → 11 |
| 1 MiB | 2,124,435 → 1,048,432 B | 28 → 14 |

这是热复用时 `encodeModelRequest` 的隔离基准，每次仍创建独立输出缓冲；冷启动或并发
缓存未命中仍有构造成本。大正文的压缩 CPU 时间可能基本不变，不能推算模型总耗时。

单账号、单模型状态、2 万条明细的 JSON 编码原先位于锁内，基准约 19.2ms；现在编码
在锁外，快照获取（含锁操作）约 0.42µs、1,280 B，1 千条明细也为 1,280 B。
该数字不包含原有账号状态捕获、锁竞争、编码或文件 I/O；账号/模型数量增加仍会增加
map 复制成本。查询聚合路径仍沿用原逻辑。

复测：`go test -run '^$' -bench 'Benchmark(OfficialGzipEncoding|StatsDocumentEncode|StatsSnapshotCriticalSection)' -benchmem`。
生命周期设计参考 [Caddy 编码器复用](https://github.com/caddyserver/caddy/blob/master/modules/caddyhttp/encode/encode.go)，
短锁后处理的思路参考 [Prometheus Gather](https://github.com/prometheus/client_golang/blob/main/prometheus/registry.go)。

独立读取约 4 MiB 正文的累计分配：已知长度路径约 9.99 → 4.20 MB；
已验证的解压正文交接约 9.99 MB → 80 B。后者是缓冲所有权交接，不报告字节吞吐。
普通 ASCII 大小预检耗时中位数约 2.02 → 0.91 ms；纯转义输入在本次样本中约有 3% 波动。

## 验证命令

```sh
go test -run 'TestLargeContext|TestDecodedRequestBody|TestReadModelRequestBody|TestConversionJSON' -count=1
go test -run '^$' -fuzz '^FuzzRequestJSONNumbersParity$' -fuzztime=10s -parallel=2
go test -run '^$' -bench 'BenchmarkMessagesRequestConversion|BenchmarkConversionJSONString|BenchmarkRead(Decoded|Known)RequestBody' -benchmem -benchtime=300ms -count=3
```

兼容回归另覆盖三协议真实 HTTP 转发、工具参数精度、请求大小与内存预算拒绝、
SSE 不完整响应、取消/重试及用量结算。Go 1.26 与 Go 1.27 分别验证回退和直接解码路径。
