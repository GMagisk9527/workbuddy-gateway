# 测试

独立于 Go 包的测试集中到本目录，Go 白盒单元测试继续与生产包同目录。所有命令默认在项目根目录执行，测试不使用生产账号或模型生成接口。

## 统一入口

```powershell
.\scripts\test.ps1                 # 全部：Go 静态检查/单测、网页单测、模拟构建回归
.\scripts\test.ps1 -Suite go
.\scripts\test.ps1 -Suite web
.\scripts\test.ps1 -Suite build
```

- 使用 Go、Node.js 与 PowerShell；缺失所选测试需要的工具会报错，不静默跳过。
- Go 测试阶段清除交叉编译及浏览器验收环境，避免误启动长时间夹具；完成或失败后恢复调用者环境和工作目录。
- 模拟构建测试不会运行真实交叉编译，不清理用户目录。
- 运行器审计：`logs/test-runner-YYYY-MM-DD.jsonl`，包含阶段、结果与脱敏输出。

## Go

```bash
go vet ./...
go test ./...
go test -run '^TestConfigEditor' -count=1
go test -run 'TestGateway.*(HotSwitch|Port)|TestHotReload|TestUpstreamSync.*(HotSwitch|Shutdown)' -count=1
go test -run 'TestAdmission|TestScheduling|TestCommonStream|TestGatewayControl' -count=1
go test -run '^TestMemory' -count=1
go test -run '^TestPerformance' -count=1
go test -run 'TestRedaction|TestMinimal(LogRedaction|RedactingWriter|DebugRedacts|RedactionConcurrent)' -count=1
go test -run 'TestIdentity|TestAssistantIdentity|TestCCIdentity|TestDSH|TestConfiguredSystemPrompt|TestUnifiedDSH' -count=1
go test -run 'TestLive|TestStreamFlushFailure|TestSlowDownstream|TestHTTP2WriteTimeout|TestDownstreamDeadline|TestGateway(Readiness|Shutdown|Forced)|TestServiceTimeout' -count=1
go test -run 'TestLargeContext|TestDecodedRequestBody|TestReadModelRequestBody' -count=1
go test -run 'TestMessagesImage|TestAnthropicImage|TestConversionJSON|TestMarshalConverted' -count=1
go test -run 'TestUpstreamTLSHandshake|TestAggregate.*(Failure|Settlement|Rejection|MissingFinish)|TestMessages.*(MissingUsage|UnknownFailures)|TestChatMultiChoice' -count=1
go test -run '^TestUpstreamHTTP(2|1Fallback)' -count=1
go test -run 'TestOfficial|TestRequestProfile|TestHarnessProtocol' -count=1
go test -run 'TestMessages(ThinkingDisplay|ToolReference|ClaudeCodeCompleteShape|UpstreamRefusal)|TestResponses(Output|EncryptedReasoning|UnsupportedCapabilities)|TestProtocolReasoning' -count=1
go test -run 'TestModelRequest|TestDecodeModelRequest|TestConversionJSON|TestMarshalConverted|TestMessages.*(ToolIdentity|ConversionLimit|RequestLimit|ToolReference)|TestResponses.*ToolIdentity|TestPendingTool|TestInvalidToolIdentity' -count=1
```

`*_test.go` 直接使用 `package main` 私有实现，不搬进普通子目录。Go 在 `go build .` 时会排除这些文件，标准测试命令仍运行完整测试，不需要额外加载器。

测试进程使用临时工作目录，配置、凭据和上游响应均为合成数据，避免污染真实文件。
内存准入测试使用合成 Go / cgroup 信号与临时 v1/v2 层级文件，覆盖水位恢复、排队复核、
在途字节预算、未知长度与压缩输入、热加载、并发释放及三协议拒绝前零上游调用/零记账。
大上下文的解码差分、缓冲交接和性能基准见[性能回归说明](../docs/performance.md)。
字符串大小预检的全字节/边界差分与模糊测试位于 `conversion_budget_scan_test.go`；可运行
`go test -run '^$' -fuzz '^FuzzConversionJSONStringBudget$' -fuzztime=10s -parallel=2`。
多文本块兼容处理的分配回归、字节偏移与延迟/提前拼接差分位于 `prompt_blocks_performance_test.go`；
可运行 `go test -run '^$' -fuzz '^FuzzPromptBlockCompatibilityParity$' -fuzztime=15s -parallel=2`。
分阶段指标的固定窗口、请求内累计与失败口径见[请求性能说明](../docs/request-performance.md)。
写入/刷新错误、在途现场、HTTP/2 流隔离与结算收尾见[流与生命周期说明](../docs/stream-lifecycle.md)。
异常结算回归覆盖三协议的读取失败、缺少结束原因与坏 JSON，区分已知费用、零费用和未知费用；
本地 TLS 黑洞测试验证握手独立超时，多候选流测试覆盖乱序、缺失、重复与快路径回退。
出站兼容测试覆盖三协议 × 流式/聚合、gzip、显式参数保留、热加载及请求身份。
协议回归另覆盖 CC 完整合成结构、ToolSearch 引用往返、拒答事件、思考展示与加密回传、
跨 Key／模型／协议及篡改拒绝、metadata 回显和能力边界。所有正文、标识及工具参数均为
合成数据，全部夹具在本地构造，不读取用户会话或凭据。
跨编译后的测试程序若在另一台机器运行，应提供同一提交的源码目录，并用 `WORKBUDDY_TEST_SOURCE_ROOT` 指定其绝对路径；这样即使使用 `-trimpath`，示例配置等文件测试也能正确定位，不必跳过这些测试。
`identity_request_test.go` 验证文本替换仅由 `identity` 控制，七条内置规则显式启用、旧配置不改写及热加载保持有效；旧 `promptCompatibility` 字段只兼容读取并告警，不执行规则，不影响用户/工具内容，也不会放宽其他未知字段的校验。
可选历史采集离线重放见[出站兼容说明](../docs/outbound-compatibility.md#验证与边界)，默认不会读取私有材料。

输出、额外尝试和日志边界回归集中在 `output_budget_test.go`、`retry_budget_test.go`、
`log_budget_test.go`、`resource_budgets_test.go`。覆盖三协议实测费用保留、统一重放预算、
取消退避、并发文件轮转和配置最后有效值。运行方式及口径见
[资源预算说明](../docs/resource-budgets.md)。

`first_token_timeout_test.go`、`sse_liveness_test.go`、`sse_framing_http_test.go` 覆盖
静默保活、首增量期限、重放取消与结算、SSE 事件分帧和网络分段模糊测试；
语义及兼容边界见 [SSE 稳定性说明](../docs/sse-liveness.md)。

## TCP 网络故障

额外的真实TCP回归使用固定版本Toxiproxy，普通 `go test ./...` 不需要安装。
先在独立工具目录构建（不修改应用的 `go.mod`）：

```powershell
$env:GOBIN = Join-Path (Get-Location) '.codex-tmp/tools'
go install github.com/Shopify/toxiproxy/v2/cmd/server@v2.13.0
.\scripts\test-network-faults.ps1 -ToxiproxyPath .codex-tmp/tools/server.exe
# 可选：加入自行构建的原生压缩代理，路径由调用者明确提供
.\scripts\test-network-faults.ps1 -ToxiproxyPath .codex-tmp/tools/server.exe -CompressProxyPath <代理可执行文件绝对路径>
```

Linux/macOS：

```bash
GOBIN="$PWD/.codex-tmp/tools" go install github.com/Shopify/toxiproxy/v2/cmd/server@v2.13.0
WORKBUDDY_TOXIPROXY="$PWD/.codex-tmp/tools/server" go test -race -tags=networkfaults -run '^TestNetworkFault' -count=1 -timeout=3m -v .
```

Go需1.27以上以构建该版本测试工具；Windows的 `-Race` 还需可用的C编译器。
所选工具缺失或启动失败会报错，只有未指定的可选Rust代理链路会跳过。
CI在Linux运行TCP故障回归。行为和隐私边界见[请求尝试与网络故障说明](../docs/attempt-trail-network-faults.md)。

## 网页

```bash
node --check web/app.js
node --test tests/web/app.test.cjs
```

`tests/web/app.test.cjs` 加载真实 `web/app.js`，使用合成 DOM / fetch 响应，不需要浏览器依赖、不调用生产接口。审计写入 `logs/webui-frontend-tests-dev-YYYY-MM-DD.jsonl`。

可选真实浏览器回归需要 Python Playwright 和可用 Chromium，不纳入默认测试入口：

```bash
python tests/web/polling.browser.py
python tests/web/polling.browser.py --browser "本机 Chromium 兼容浏览器的可执行文件路径"
```

该测试只加载仓库中的网页文件，将所有网络请求拦截为合成响应，验证成功/错误提示、页面位移、账号启停、表格局部更新、倍率详情保留及图表复用；不访问生产或上游，验收记录写入 `logs/polling-flicker-*.jsonl`。

## 构建脚本

```powershell
.\tests\build\build_script.test.ps1
```

使用模拟 Go 命令验证九目标矩阵、平台/架构筛选、已知文件清理、失败处理和环境恢复。临时材料在 `.codex-tmp/`，审计在 `logs/build-tests-YYYY-MM-DD.jsonl`。此测试不能替代真实交叉编译。

## 浏览器手动验收

默认 Go 测试不会运行浏览器夹具。明确开启后，它只启动本地 `127.0.0.1:18316` 管理页面，使用合成账号和管理 Key，不调用上游：

```powershell
$env:WORKBUDDY_BROWSER_FIXTURE = '1'
$env:WORKBUDDY_BROWSER_DURATION = '90s'
go test -run '^TestPolicyBrowserFixture$' -count=1 -timeout 3m -v
Remove-Item Env:WORKBUDDY_BROWSER_FIXTURE, Env:WORKBUDDY_BROWSER_DURATION
```

应让夹具时长小于测试超时。不要使用生产地址、真实 Key 或真实凭据替换该夹具。

## 目录与命名

见 [docs/project-layout.md](../docs/project-layout.md)。不要把一次性实验或可能含环境信息的报告加入公开测试目录。
