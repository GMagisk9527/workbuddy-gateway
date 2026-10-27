# Claude Code 请求体 gzip 启动器

在当前验证过的 Windows x64 Claude Code 2.1.311 **副本**中，为自定义
WorkBuddy Gateway 开启原生请求体 gzip。不开本地监听，不经过7897，不修改
用户指令、历史消息或工具参数。普通 `claude` 命令和原安装文件保持原样。

这是非官方、针对特定二进制验证的适配组件，不是 Claude Code 插件。
只提供补丁生成代码，不分发 Claude Code 二进制。需要 Node.js22或更高版本，
不需要安装 npm 依赖。网关必须支持 `Content-Encoding: gzip`。

## 使用

在此目录执行以下命令，先生成并校验副本（不发送 API 请求）：

```powershell
.\claude-gzip.cmd --prepare-only
.\claude-gzip.cmd -- --version
```

之后启动正常交互会话：

```powershell
.\claude-gzip.cmd
```

继续已有会话：

```powershell
.\claude-gzip.cmd -- --resume
```

也可以使用 `claude-gzip.ps1`。启动时的 `[cc-gzip] gzip enabled` 说明已选择
这个启动器；普通 `claude` 不会自动启用它。启动器不读取配置文件中的凭据，
Claude Code 自行加载现有登录/密钥配置。不要把 API Key 写进命令或这个目录。

当前仅适配直连入口 `https://fandai.202695276.xyz:10717`，由独立
`direct-settings.json` 覆盖传输配置，不写回用户设置。该运行进程会清除
HTTP_PROXY/HTTPS_PROXY/ALL_PROXY，并停用副本的自动更新。不会改系统代理或原程序。
不应叠加其他 `--settings` 传输覆盖；不要通过此启动器切换到未验证的网关。

## 退出与回退

正常退出后，继续使用原来的 `claude` 即可。或显式启动原安装文件：

```powershell
.\claude-gzip.cmd --original
```

`--original` 不生成补丁，不设置 gzip，不强制直连，也不关闭原程序自动更新。
升级后原程序 SHA256 改变时，gzip 模式会停止并提示未验证，不会盲目套用旧补丁；
原版模式仍可用。副本位于 `.cache/<source-sha256>/revision-2/`，不需要放进 PATH。

## 修改范围和失败处理

仅修改原生 gzip 模块中的两处判断，并清除该模块的旧字节码缓存指针，让 Bun
重读其内嵌源码；其余模块和原文件不动。生成前验证完整源文件SHA256、PE/Bun布局、
唯一匹配位置。每次运行根据原文件计算预期副本哈希，并校验副本和收据，不能仅靠
改收据绕过文件完整性校验。

只有415或原生分类器识别到明确的无效JSON拒绝时，才允许gzip回退为明文。
普通400、401、403、429、503不增加gzip特有重试。Claude Code自身原有的重试/
错误恢复策略仍然存在。回退过程中JSON序列化空白可能变化，所有解析字段及字符串
内容保持一致，包括代码中的空白；不要将JSON字节完全一致等同于内容一致。

压缩可以减少上传字节，不减少上下文token或模型积分，不保证消除线路波动。
一旦服务器已开始输出，测试确认取消不会触发gzip重发。

## 验证记录（2026-10-27）

```powershell
npm test
npm run test:native
```

- 7项布局/指纹/入口/启动器测试通过。
- 13项真实已安装CC进程对本地模拟HTTP接口的集成测试通过；仅使用假凭据和合成内容。
- 覆盖压缩开/关、完整Unicode/代码字符串、工具调用与结果关联、SSE逐段到达、取消、
  401/403/400/429/503、明确JSON拒绝和415回退。
- 合成样本约98KB压缩到约12KB（约减少88%）；不是实际会话速度/积分收益。
- 原安装EXE测试前后SHA256一致；副本启动与 `--original -- --version` 均成功。
- 网关仓库现有gzip接收、真实中间件链、内存准入和请求上限离线测试通过。
- 基于 main `9a643c1`，`go vet ./...` 通过；Windows 上 `go test ./...` 和
  `go test -race ./...` 在现有签到状态持久化测试失败，随后因空结果索引而 panic。
  本提交只增加独立工具和文档，没有修改 Go 文件；不能将全量测试标记为通过。

当前源文件SHA256：
`d3fe4adaf1918347e76824e01e199a6cd7387e07c0e787ed7af758d21cf2ef42`。
同版本的其他安装/汉化构建也可能有不同指纹，不能直接视为兼容。

本次没有真实模型请求，没有验证生产上传的实际提速，没有修改生产服务、
账本、账户、DNS或全局客户端配置。真实会话仍需用户试用确认。

`test-results.json` 仅记录合成测试的编码、字节数、退出码和耗时，忽略提交。
