# 请求分阶段性能指标

管理页“用量与性能 → 链路分阶段性能”可手动读取当前指标并按协议查看。
只读接口为 `GET /admin/api/performance`，沿用管理 Key 鉴权，不开放在模型 API 端口。

## 窗口与样本

- 进程内固定 5 个自然分钟桶，包含当前分钟；请求在结束时归入完成所在的分钟。
  进行中的长流尚未计入，进程重启后清空。历史日期、账号和模型筛选不作用于此面板。
- 只记录 POST Chat Completions、Responses、Messages 模型路由及其已有别名。
  本地 count_tokens、健康检查、管理请求和后台探测不计入；鉴权和准入拒绝仍算失败请求。
- 一个请求每个阶段至多贡献一个样本，多次换号、网络重试、首字节前重放的阶段时间累计。
  DNS、TCP、TLS 可能并行，连接获取也包含其中部分时间；不能将各阶段直接相加。
- P95/P99 是固定延迟桶的上界估计，不是保留原始样本得到的精确分位数。
  最后一桶超过 1 小时时用观察到的最大值作上界。没有样本为 null，网页显示“—”；真实零值显示 0。
- “异常样本”表示该阶段至少出现一次已观测错误的请求数，包含后来已恢复的尝试；
  因此同一请求可以有多个异常阶段，总请求最终仍可能成功。

## 阶段边界

| 字段 | 口径 |
|---|---|
| admission_wait | 解码前共享准入的获取过程，包含排队与内存复核；不包含 API Key 验证 |
| request_read | 请求体读取；压缩输入包括压缩字节读取和已解压缓冲交接，不重复读正文 |
| request_decompress | 客户端 gzip/deflate 解压；未压缩请求无样本 |
| json_decode | 协议 JSON 解码，不包含后续转换 |
| request_prepare | JSON 解码成功后至共享上游入口的校验、协议转换、兼容处理及编码；拒绝时至 handler 返回 |
| account_wait | 账号候选选择与模型并发名额等待 |
| credential_ready | 取得模型账号凭据读锁并确保凭据就绪 |
| upstream_encode | 按出站配置编码请求，包括启用时的 gzip |
| connection_acquire | httptrace.GetConn 至 GotConn，包含连接池等待及可能发生的建连 |
| dns / tcp_connect / tls_handshake | HTTP 客户端回调的累计操作时间；复用连接通常不产生这些样本 |
| request_upload | GotConn 至 WroteRequest，包含请求头和请求体发送 |
| response_wait | 完整上传至首响应字节，不是首个模型增量；服务器提前回应且已知上传完成时为 0 |
| upstream_read | 读取上游响应体的累计阻塞/读取时间，包含非成功响应的有限错误体读取；不把原始字节当作有效 Token |
| first_token | 入口至首个有效文本、推理或工具增量被解析；重放不重新累计；包含后来失败但已见增量的请求 |
| downstream_write | ResponseWriter.Write 与 Flush 的累计耗时，包含写入错误；原有 void Flush 无法报告全部刷新错误 |
| settlement | 原有 statsFinishAttempt 的执行时间，包含实测用量结算和统计记录，不改变调用顺序 |
| total | 入口至 handler 返回并完成原有结算；异常退出也记录失败，但保持原有 panic 传播 |

首增量的观测不替换既有 TTFT v2 和“仅完整响应参与均值”的历史统计口径。
HTTP trace 的并行操作采用结束偏移总和减开始偏移总和；成功响应中尚未配齐的回调
不伪造成功耗时，而是省略该阶段并增加 droppedTraceEvents。请求结束后迟到的回调忽略。

## 计数与隐私

- upstreamAttempts：模型上游 `http.Client.Do` 调用次数，包含换号和允许的重试，不能当作计费次数。
- transportRetries：doUpstreamRequest 内尚未发送请求头时的安全网络重试；不包含其他换号或重放。
- connections / reusedConnections：GotConn 的连接交付次数与其中的复用次数；复用率未观测时为 null。
- failed：HTTP 错误、取消、未确认完整的上游流、下游写失败或 handler 异常退出。
  incompleteStreams、downstreamErrors、canceled 可以重叠，不是互斥原因。
  已确认完整且下游写入未报错后发生的取消，不回溯计为失败；原有结算口径不变。

全局只保留三种固定协议和固定阶段、时间桶，不记录模型名、Key、账号、主机/IP、正文或 Trace ID。
请求内累计，结束时合并一次；不为每个 SSE 增量获取全局指标锁。聚合器自身固定存储在 amd64
本地测得约 63KB，不含请求上下文、网络回调状态及管理响应临时对象。没有后台采样循环。
开启既有 debug 日志时，同一 Trace ID 下增加 `request_performance` 的固定阶段汇总。

## 验证

本地模拟上游覆盖三协议流式/聚合、TLS 与连接复用、重试发送边界、取消与解码拒绝、
HTTP 200 内的流失败、分钟窗口过期、并发回调和管理鉴权。网页覆盖协议选择、未知/零值、
错误保留和退出后过期请求。未调用真实模型。

`BenchmarkPerformanceMessagesStream` 对比同一 128 文本增量流在有/无采样上下文时的路径；
该基准不含真实网络建连和完整 HTTP 中间件，不能用作整机吞吐或线上延迟承诺。
2026-10-25 本机 Windows amd64、Go 1.27.3 的三轮中位数为 0.587 / 0.611 ms，
采样约增加 4.2% 耗时、846 B 累计分配和 3 次分配；单次请求内阶段累计为 0 B/op、0 allocs/op。
