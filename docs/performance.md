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
