# 请求尝试过程与网络故障回归

## 按 Trace 查询

管理页“用量与性能 → 链路分阶段性能”支持填入 `X-Trace-ID` 查询已结束请求。
同样可通过管理员接口 `GET /admin/api/performance?trace=<trace>` 读取。
记录保留最近128个请求、最长5分钟，每个请求最多保留前16次实际 `http.Client.Do`
调用，多余次数在 `attemptsDropped` 中显示。未获取重试预算、未发送的准备失败不伪装成上游尝试。
计数仍以原有 `upstreamAttempts` 为准，不等于计费次数。

`attemptTrail` 按调用顺序包含：

- `attempt`、`kind`：首次 `initial`、安全网络重试 `transport_retry`、
  换账号 `account_fallback`、同账号首增量前重放 `stream_replay`。
- `statusCode`：真实上游HTTP状态；没有响应头时省略，不合成502。
- `businessCode`：已由既有路径解析的数字业务码，不保留错误原文。
- `outcome`：`response_headers` 只说明收到了响应头；`complete` 需要原有流解析器确认结束。
  其他结果包括 `rejected`、`error`、`incomplete`。旧重放不能因后一次成功变成完整。
- `failureClass`：固定错误分类，包括网络、超时、取消、限流、额度、鉴权、上游拒绝和流不完整。
- `startedMs`：相对于入口的开始偏移；`headersMs`：本次调用至响应头的耗时；
  `durationMs`：本次调用至响应体关闭的观测时间，包含读取与下游反压，不能当作纯网络延迟。

只在请求开始、响应头、错误和关闭等边界更新，不逐个正常Token加锁。
环形记录使用固定数组，导出时复制；不保留账号文件、账号标识、Key、URL、提示词、工具参数
或错误原文。账号指针仅在当前请求内比较是否换号，结束后清除，不进入记录。
查询沿用管理员鉴权，退出管理页后清空结果，迟到响应不能恢复退出前的数据。
请求的完整性、取消和下游写失败仍看原有请求计数；尝试列表描述上游调用，不能代替交付状态。

本设计参考 [Bifrost 的 attempt trail](https://docs.getbifrost.ai/features/observability/default)，
仅扩充观测，不改变已有换号、重放、SSE或账本结算规则。

## 真实 TCP 故障测试

使用 [Shopify Toxiproxy](https://github.com/Shopify/toxiproxy) v2.13.0，由测试启动独立进程，
控制API和代理监听均在回环地址，API使用随机一次性令牌。随机端口、临时工作目录和合成账本
独立于正在运行的服务，测试完成关闭进程、连接和监听。正常构建和默认Go测试不依赖该工具。

`networkfaults` 构建标签启用以下三协议集成回归：

- 上行64 KiB/s限速时，64 KiB合成输入可以完成，只有一次上游调用。
- 客户端已收到有效增量后注入TCP RST或按字节截断；不重放、不发送正常完成事件。
- 使用校验合成证书的HTTP/2上游，确认协商为H2后在输出中注入TCP重置。
- 客户端取消时，上游请求停止，准入、准备、输入/输出内存与重试名额释放。
- 各场景检查合成Key已确认的3积分只结算一次，没有未知用量误记。
- 可选原生压缩代理：三个入口的 zstd 请求通过真实代理、Toxiproxy分段传输、网关解压及SSE转换。

这套测试验证TCP链路，不模拟QUIC/UDP，也不代表公网CDN测速。H3仍由压缩代理自身的
TLS/QUIC与UDP阻断测试覆盖。所有请求与数据都在本机，不调用真实模型、不产生线上额度消耗。
CI执行Go三协议故障用例；原生Rust代理链路需另行提供可执行文件，未提供时明确跳过该项。

安装与运行命令见 [测试入口](../tests/README.md#tcp-网络故障)。
