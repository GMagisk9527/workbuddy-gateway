# WorkBuddy Local Gateway

纯 Go、零 CGO 依赖的跨平台单二进制网关，支持国内站与国际站、多账号调度、OpenAI / Anthropic 协议及可选网页控制台。

## 快速开始

需要 `go.mod` 声明的 Go 1.26.5 或更新工具链。在项目根目录执行：

```bash
go build -o workbuddy-gateway .
./workbuddy-gateway login
./workbuddy-gateway serve -webui
```

Windows 可使用 `.\workbuddy-gateway.exe`。管理端口默认 `8316`，模型 API 默认 `8317`。

- 首次启动会在工作目录生成完整的默认 `config.json`；已有配置不会自动补写或改写。默认单账号模型并发 `4`，全局不限；修改 `concurrent.MaxAcc/MaxGlobal` 可热加载。可按需要设置管理 Key、API 鉴权和身份替换。
- 新生成配置最多保留 `8` 个在途请求，其中 `2` 个可同时读入/转换；最多排队 `8` 个、等待 `15` 秒。旧配置省略 `maxPreparing` 保持原整请求准入。账号选择感知负载，软亲和通常等待 `250` 毫秒，大上下文可根据近期上游缓存证据延长到 `1500` 毫秒；详见[分阶段准入与缓存亲和](docs/staged-admission-cache-affinity.md)。`admission` 和 `scheduling` 均可热加载。
- 默认同 Key 内 FIFO、跨 Key 公平排队：`fairQueue` 省略或为 `true` 时启用，显式 `false` 恢复全局 FIFO。`maxWaitingPerKey` 仅在公平模式且有鉴权 Key 身份时生效，`0` 表示自动取总排队上限的一半向上取整；完整字段见 [config.example.json](config.example.json)，行为与 Trace 诊断见[公平准入与请求诊断](docs/fair-queue-request-diagnostics.md)。
- **后台价格探测默认关闭**，只有显式传入 `-allow-price-probes` 才开启；主动生成请求可能消耗积分。
- 网页配置页支持风险确认后的完整 JSON 编辑；API/Web 监听端口修改可热切换，已有连接与流继续完成。新端口被占用或配置无法应用时保留旧运行状态；显式 `-port` 仍优先于配置文件。
- `config.json` 和账号凭据含敏感信息，已由 Git 忽略，不要公开分享。
- 多 Harness 可使用 `upstream.requestProfile: "official"` 开启统一出站兼容；旧配置省略该项保持原行为。见[出站兼容说明](docs/outbound-compatibility.md)。

## 文档

| 内容 | 位置 |
|---|---|
| 完整使用手册、配置、客户端接入与部署 | [docs/user-guide.md](docs/user-guide.md) |
| 完整默认配置示例，无预置密钥 | [config.example.json](config.example.json) |
| 目录职责与命名约定 | [docs/project-layout.md](docs/project-layout.md) |
| 贡献者及方案署名 | [docs/contributors.md](docs/contributors.md) |
| 测试入口与说明 | [tests/README.md](tests/README.md) |
| 请求分阶段性能、连接复用与百分位口径 | [docs/request-performance.md](docs/request-performance.md) |
| 同 Key FIFO、跨 Key 公平排队与 Trace 请求诊断 | [docs/fair-queue-request-diagnostics.md](docs/fair-queue-request-diagnostics.md) |
| 本地 token 计数、模型校准与隔离安全重试 | [docs/token-estimation-retry-isolation.md](docs/token-estimation-retry-isolation.md) |
| 状态快照锁隔离、写入合并与心跳 | [docs/status-snapshot-efficiency.md](docs/status-snapshot-efficiency.md) |
| 下游写入超时、在途指标与停机收尾 | [docs/stream-lifecycle.md](docs/stream-lifecycle.md) |
| SSE 静默保活、首增量期限与事件分帧 | [docs/sse-liveness.md](docs/sse-liveness.md) |
| 输出保留、统一额外尝试与日志大小预算 | [docs/resource-budgets.md](docs/resource-budgets.md) |
| 多 Harness 出站对齐、协议参数与兼容边界 | [docs/outbound-compatibility.md](docs/outbound-compatibility.md) |
| Messages / Responses 转换能力及推理回传 | [Messages](docs/messages-chat-compatibility.md) / [Responses](docs/responses-chat-compatibility.md) |
| DSH 历史回放、GUI 环境句与 11128 兼容说明 | [docs/11128-dsh-history.md](docs/11128-dsh-history.md) |

## 构建与测试

```bash
go vet ./...
go test ./...
```

PowerShell：

```powershell
.\scripts\test.ps1                 # Go、网页和构建脚本回归
.\build.ps1                       # 校验并构建全部 9 个平台/架构目标
.\build.ps1 -Only linux -Architecture amd64
```

Go 单元测试保持与 `package main` 源码同目录，以保留私有函数测试和标准 `go test ./...` 行为；它们不进入生产构建。独立网页与构建脚本测试集中在 `tests/`。完整文档集中在 `docs/`，根目录保留构建入口与配置示例。

本项目仅用于个人学习与技术研究，请遵守上游服务条款。完整安全提示与免责声明见[使用手册](docs/user-guide.md#安全提示)。
