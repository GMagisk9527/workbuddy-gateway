# 输出、额外尝试与日志预算

这三组配置限制单个长回复、上游故障和日志积累带来的资源增长，可热加载。
已有配置省略新字段也会启用默认值；不会自动改写配置文件。完整示例见根目录
`config.example.json`。

## 输出保留预算

```json
"output": {
  "maxRequestBytes": 0,
  "maxSharedBytes": 0
}
```

两个值以字节为单位，允许 0～1 TiB；0 表示自动计算，而非无限制：

| 预算 | 默认最大值 | 已知 Go 内存限制 | 已知各层 cgroup 限制 |
|---|---|---|---|
| 单请求 | 16 MiB | 限制的 1/4 | 限制的 1/8 |
| 所有请求共享 | 32 MiB | 限制的 1/2 | 限制的 1/4 |

自动模式取各项最小值；显式正数覆盖相应自动值。以 `GOMEMLIMIT=48MiB`、
cgroup 128 MiB 为例，单请求 12 MiB，共享 24 MiB。单请求上限在首次保留内容时固定；
共享配置在后续申请时重新检查，热加载不会强行撤销已发出的预算。

计入需要跨增量保留的正文、拒答、推理、工具参数及少量块元数据，并预留 JSON
编码、解码与签名所需的临时空间。聚合正文使用 `strings.Builder`，避免反复复制
整个前缀。Chat 流式正文和 Messages 已转发的普通文本不会累计计入；Messages
待确认工具身份的事件以及推理回传仍需保留。Responses 终态会包含完整输出，
因此其流式正文也计入保留预算。

共享申请按最多 64 KiB 的小块预留；请求结束，包括原有实测费用结算后，统一归还。
`runtime.outputBytes` 是预留字节，可能高于实际使用；临时空间采用请求内高水位。
这是保留内容的逻辑预算，并非 Go 堆或 RSS 的硬上限：Builder 余量、对象开销、
网络缓冲及单个正在解析的 SSE 增量仍会占用额外内存。应配合原有输入准入、
SSE 行大小限制、GOMEMLIMIT 和容器限制使用。

超限会关闭上游并明确返回不完整响应，不静默截断、不追加成功结束事件，
不触发额外重试。流式错误使用 `output_budget_exceeded`；聚合错误携带预算说明。
此前已收到的真实费用仍沿原路径结算；未知费用保持未知，不按零费用处理。

## 统一额外尝试预算

```json
"retry": {
  "maxExtraAttempts": 4,
  "maxConcurrent": 2,
  "ratioPercent": 20,
  "minAllowance": 2,
  "backoffMaxMs": 2000
}
```

首次模型 `Do` 独立于此额度。同一个逻辑请求此后的安全网络重试、换号和允许的
首字节前重放共用 `maxExtraAttempts`；不再让各层次数彼此叠加。
原有 `upstream.transientRetries` 仍限制网络重试循环，并受统一额度约束。

全局额外尝试的同时占用上限为：

```text
min(maxConcurrent, minAllowance + floor(在途原始请求数 × ratioPercent / 100))
```

在途原始请求从首次发送持续到请求收尾。额外尝试名额覆盖退避等待和响应体生命周期，
读到 EOF、关闭响应体或发送失败时释放。默认并发硬上限为 2；提高该值后，比例才会
随负载增加可用名额。额度不够时立即拒绝，不新增无界队列。

额外尝试采用指数增长的随机退避，每次为当次基准的 50%～100%，默认初始基准
300 毫秒、最大基准 2 秒。等待响应取消后释放名额，不再执行下一次发送。
达到统一额度时，尚未开始下游流的请求返回 503、`Retry-After: 1` 和
`upstream_retry_budget_exhausted`；Messages 映射为 `overloaded_error`。
已开始的 SSE 沿现有失败事件返回不完整响应。

本配置不扩大可重试条件。发送过请求头的 POST 仍不会走网络自动重试；首字节前
重放仍受原有安全条件约束；11128 仍如实返回；免费站点优先与已尝试账号排除不变。
它也不限制客户端另外发送的新请求，客户端重试应自行设置上限。

`maxExtraAttempts` 范围 0～64，`maxConcurrent` 0～1024，显式 0 禁用额外尝试；
省略或 null 使用默认。`ratioPercent` 1～100，`minAllowance` 1～1024，
`backoffMaxMs` 1～60000；这后三个值的省略或 0 使用默认。

## 日志大小与采样

```json
"logs": {
  "retentionDays": 0,
  "maxFileBytes": 8388608,
  "maxTotalBytes": 67108864,
  "maxFiles": 16,
  "sampleFirst": 20,
  "sampleEvery": 100
}
```

新增字段的 0 或省略使用表中默认值：

| 字段 | 默认 | 有效范围 |
|---|---|---|
| maxFileBytes | 8 MiB | 1 字节～1 GiB |
| maxTotalBytes | 64 MiB | 单文件上限～1 TiB |
| maxFiles | 16 | 1～10000 |
| sampleFirst / sampleEvery | 20 / 100 | 1～100000 |

原有 `retentionDays: 0` 仅关闭按日期删除，大小和份数上限仍然生效。上线后首次
写入就可能按上限删除旧的受管日志，需长期保留的审计应提前归档到外部存储。
日志设置页展示生效的大小、份数和采样值；修改保留天数会保留其他配置。

四类受管文件 gateway、debug、deployment、frontend 共用大小和份数预算。
按天文件写满后，新建同日 `.part-000001` 分段，后续递增；原文件不重命名，
已有网页查询文件名和偏移保持有效，直到文件按保留策略删除。重启后继续最新分段。
超限时优先删除最旧的已关闭受管文件，活跃文件保留，不处理其他文件、符号链接、
凭据、API Key 或账本。限制在写入时执行；外部改动通过定期刷新发现，不能替代磁盘配额。

若活跃文件已占满可用预算，或单条记录大于单文件上限，文件写入会被拒绝并计入
`runtime.logRejected`，不会无限轮转。普通主审计日志沿既有 MultiWriter 同时写入
stderr；独立 debug 文件不保证有同样的 stderr 副本。错误事件不参与采样，但历史
错误日志同样受归档删除策略约束，文件故障也不能保证持久化成功。

目前只采样重复的“站点免费判断中跳过不可调度账号”说明；实际收费/免费判定、
回退到付费站点、账号状态变化、请求进出、CORS、鉴权、错误、Key 和 WAL 记录全部
保留。每类固定事件每秒保留前 `sampleFirst` 条，之后每 `sampleEvery` 条保留一条。
不为 Trace ID、模型或账号建立采样桶，`runtime.logSampled` 统计省略条数。

## 观察与验证

管理性能接口新增 `outputBytes/outputBudget/outputRejected`、
`retryActive/retryLimit/retryRejected` 和 `logSampled/logRejected`。
拒绝与采样值从进程启动累计，不属于五分钟窗口；不带动态标签。当前值是各子系统
分别获取的快照，不保证跨系统原子一致。

离线测试覆盖三协议的输出超限与真实费用保留、流终态、快慢路径、并发归还、
重放预算与取消退避、日志并发轮转和总量、网页查询及配置热加载。
`BenchmarkAggregateTextJoin` 仅对比 128 × 4 KiB 的字符串拼接，不含 JSON、
网络或完整中间件，不能解释为网关整体吞吐提升。

设计参考 [Envoy retry budget](https://www.envoyproxy.io/docs/envoy/latest/intro/arch_overview/upstream/circuit_breaking)、
[Envoy buffer limits](https://www.envoyproxy.io/docs/envoy/latest/faq/configuration/flow_control)
和 [Caddy 日志大小/份数管理](https://caddyserver.com/docs/caddyfile/directives/log)，
按本项目的流式协议和现有结算路径独立实现。
