# 11128 调查与协议兼容修复

调查日期：2026-10-20 至 2026-10-21。开始时实际 main HEAD 为
5bfe872ab179a35e33ed648245faa4c374b7ffcf，2026-10-21 再次 fetch 后未变化；
不是最初提供的 aab0709。补丁没有部署或重启生产服务。

## 证据与结论

生产只做了读取日志的检查，没有发送模型探测请求。现存日志有 4 条 11128：
其中 3 条国内站明确返回 “Illegal API invocation from an unapproved channel”，
日志时间为 2026/10/20 15:21:04、15:22:41、15:23:50；
另有 2026/10/19 13:08:22 的较早记录无法细分。日志时区未确认。
原日志没有足够的协议及会话形状信息，**无法确定是单轮、多轮、工具还是压缩触发**，
也不能由此断定国际站相同或不同。

确认存在以下独立缺陷：

1. 本机 DSH 的 OpenAI Responses 适配器发送 session_id 请求头，网关原来未读取。
   压缩摘要请求仍沿用 agent.session.id。原来这些请求会生成互不相关的上游会话，
   无法使用显式会话亲和。这是可复现的会话兼容缺口，尚未证明是生产渠道拒绝的根因。
2. Codex 的 thread-id/session-id 没有被识别。线程应优先于客户端进程会话，
   否则子任务可能与父线程混淆。原 UUID 会话还缺少不同 API Key 间的隔离。
3. sanitizeBlockedTemplates 会修改所有消息正文，包括用户代码和工具结果里的特定
   字符串。已经撤掉正文替换；仅保留 developer → system 的角色转换。
4. Responses 转译遗漏 strict、图像 detail、结构化输出及若干标准模型/缓存参数。
   previous_response_id、conversation 和不可回放项以前还会被静默忽略，可能导致
   历史丢失。现在不支持的状态在上游调用前明确返回 400，响应如实标记 store=false。
5. HTTP 200 的 SSE 业务错误会丢失原始拒绝原因，退化为“没有 finish_reason”。
   现在所有入口均保留 11128 和原始错误正文，并拒绝发出成功完成事件。

11128 不是单一根因的充分证据。分类优先使用具体 msg/message，避免 displayTips
中的笼统渠道提示盖过明确的消息结构、角色或会话字段错误：

| 分类 | 判断依据 |
| --- | --- |
| message_structure | 首条消息不是 system |
| unsupported_role | 明确不支持 developer role |
| conversation_metadata_missing | 明确缺少 conversation/session |
| prompt_policy_rejected | 明确的 prompt policy/blocked |
| channel_verification_failed | unapproved channel / channel verification |
| invocation_rejected_unknown | 仅有错误码或无法确定原因 |

这些分类描述上游返回的证据，不证明服务端内部判断机制。当前生产的 3 条明确消息属于
channel_verification_failed，不能声称补齐会话头即可解决。若规范请求仍被渠道拒绝，
需要 WorkBuddy 确认授权渠道；可使用其官方支持的客户端，或向官方申请明确受支持的
API/SDK 接入方式。本次调查没有确认一个公开、获授权、可直接替代当前接口的第三方
API 地址，不能把逆向插件接口称为官方第三方 API。
补丁没有新增官方签名、设备凭证、产品身份或绕过渠道检查的提示词伪装。

## 参考材料与客户端路径

- https://github.com/XDTrees/dsh-xdpool/issues/48 ：会话元数据线索，未得到完整的上游
  成功验证；没有照搬其客户端身份字段。调查源码固定于
  2def4d01509784c7ebc4fe9b5d860f12a398a64f。
- https://github.com/ardeyouxipianyi/workbuddy2api-hub/issues/61
  与 https://github.com/ardeyouxipianyi/workbuddy2api-hub/pull/62 ：
  OmO/OpenCode/Sisyphus-Junior 的提示词兼容案例表明触发条件可能不同；
  不能推出所有 11128 都是模板问题。未采用其中的规避性正文替换。
- https://github.com/ardeyouxipianyi/workbuddy2api-hub ：参考其工具调用与 SSE 转译。
- https://github.com/2861292267/DSH-Official-WorkBuddy-Credit-Proxy ：
  调查 HEAD 17a02c085af35ab08ceb3075b003a981afa76d5c。
  插件直接访问 WorkBuddy 的路径与通过本网关的 OpenAI provider 是不同路径，
  插件请求不经过网关时，本补丁不能改变其行为。
- https://developers.openai.com/api/reference/resources/responses/methods/create ：
  Responses 标准请求字段与 conversation/previous_response_id。
- https://github.com/openai/codex/blob/78d4c7c7fae278d2b0a904316c274681c62461a8/codex-rs/codex-api/src/requests/headers.rs ：
  Codex 的 thread-id/session-id。

本机 DSH 配置的 provider 类型实际为 openai-responses，baseURL 指向网关的 /v1。
请求样本依据本机已安装的 DSH 0.2.1-rc.2 / pi-ai 1.1.0 适配器重建；
版本、文件 SHA-256 和行为差异见 [样本来源](../testdata/dsh/README.md)。
样本是合成协议输入，不是生产抓包，也没有启动真实 DSH 完成联调。

通用第三方 Chat provider 默认可能不发送稳定会话头；这种情况下保留 Prefix
Affinity，但无法可靠地跨摘要恢复推测原会话。不把 x-client-request-id 或任意
prompt_cache_key 当作唯一会话标识。不同会话必须由客户端明确区分。

## 三入口执行顺序与改动

三入口统一在转换前 prepareConversationContext，记录原始协议、角色、stream 和
显式会话是否存在；会话元数据不会因 Responses/Messages → Chat 转译丢失。
随后转换请求（Chat 无此步骤）、强制上游 stream=true、applyThinkingRules、
sanitizeMessages、prepareSystemPromptForUpstream、输出预算归一化、工具序列与
推理历史修复、序列化、计算亲和键、调度、backendHeaders，最后转换响应。
prepareSystemPromptForUpstream 内先 ensureLeadingSystemMessage，再按原配置追加
force；现在重复运行不会重复追加。没有对 system prompt 做大规模替换。

| 文件/函数 | 修复 |
| --- | --- |
| conversation.go / prepareConversationContext、normalizedConversationID | DSH/Codex 会话来源、调用方隔离；重试沿用同一请求元组 |
| main.go / sanitizeMessages、upstreamChat、streamChatResponse、aggregateCompletion | 不改正文；11128 不换号重放、不误禁用凭据；保留 HTTP/SSE 业务错误 |
| responses.go / responsesToChatRequest、convertResponsesTools、convertResponsesContent、streamResponsesResponse | 保留参数/strict/detail、命名工具选择、SSE 失败状态及 store=false |
| responses_validation.go / validateResponsesReplay | 拒绝无法合法回放的服务端引用或不透明状态，防止静默丢历史 |
| messages.go / scanUpstreamSSE、响应出口 | 同样识别 SSE 11128，保留 Anthropic 错误形状 |
| systemprompt.go、toolrepair.go | 配置提示词追加幂等；只记录兼容规则命中，不改变原工具修复策略 |
| upstream_diagnostics.go | 业务码分类、低开销 SSE 检测、脱敏形状日志、错误返回 |

原有 applyThinkingRules 的显式 high 等设置及 off/none 的上游处理约定保持不变。
标准正文、工具参数、结果和关联 ID 的保留由真实 HTTP 链路断言。

## 验证范围

新增 protocol_compat_integration_test.go，客户端 → 网关 → 假上游均为真实 httptest
HTTP server，不使用收费模型。国内/国际 profile 都指向本地假上游，不能据此推断
两个真实站点的授权策略或全部新透传字段的接受情况。

| 要求 | 离线覆盖 |
| --- | --- |
| Codex 单轮/多轮 | Responses 单轮、网关输出回放、reasoning、function_call/output |
| DSH 单轮/多轮/工具 | 源码形状的 Responses 与 Chat、完整参数/结果/ID |
| 子代理/重试/压缩 | 父子会话隔离、调用方隔离、客户端重试、摘要请求与恢复 |
| Anthropic Messages | thinking/tool_use/tool_result、流式与聚合 |
| developer/system/正常内容 | 角色转换，指令、代码字面量、工具参数/结果保持不变 |
| 关键模型参数 | strict、schema、detail、预算、温度、top_p、缓存及其他已映射字段 |
| 幂等 | 重复执行角色、首条 system、配置追加及工具修复 |
| 亲和 | 同一显式会话跨摘要保持账号；无显式会话保留前缀亲和且不合并身份 |
| 11128 | 6 类原因、数字/字符串业务码、HTTP 400/401/429、HTTP 200 SSE |
| 如实返回/低开销日志 | 快慢路径、三个入口、流式/聚合、不伪造完成、不重放、不泄密 |
| API Key/积分/WAL | 拒绝不凭空扣分；成功仅记假 usage.credit；账本重放一致 |

同时运行仓库原有的调度、积分配额、WAL/Snapshot、SSE、工具、推理历史及亲和测试。
2026-10-21 在 Go 1.27.2 windows/amd64 上完成：

- go vet ./...：通过。
- go test ./...：通过，主包 22.106 秒。
- go test -race ./...：通过，主包 82.458 秒；启用 CGO，使用本地 GCC。

Windows 测试把 TEMP/TMP/GOCACHE 指向仓库外的任务临时目录，避免沙箱的默认缓存/
重命名权限问题；不影响生产配置。

## 尚未验证与运维影响

- 未做真实收费上游请求；未完成真实 DSH/Codex 对生产网关的端到端验证。
- 不支持 previous_response_id、服务器 conversation/store=true、item_reference、
  不透明 compaction 或仅加密 reasoning 回放；明确返回 400，不能宣称完整 OpenAI
  服务端状态能力。支持客户端回放完整文本、明文 reasoning 和函数调用历史。
- 持续的明确渠道拒绝必须由上游确认授权范围；本补丁不能赋予第三方渠道准入权。
- 不需要修改生产配置。现有亲和开关仍遵循原配置；启用亲和时这些修复才改善账号
  亲和命中。没有更改账号调度算法、现有 API Key 数据、账本、端口或 OpenRC。
- 没有部署、重启服务、替换生产二进制或发送诊断收费模型请求。

## 部署后复查：Nginx 的会话头转发

用户随后授权部署，并回报原会话仍有 11128。对应脱敏日志确认：请求经过 Responses、
国内站、流式入口；developer_to_system 已执行，但 session_supplied=false，
实际使用 session_id_generated。运行进程的二进制校验和与提交 36410b6 的构建一致，
因此不能归因于仍运行旧二进制，也不能声称客户端会话头已经成功穿过整条链路。

反代使用 Nginx，现有配置未启用 underscores_in_headers。使用服务器上相同 Nginx
二进制、隔离的临时配置、合成请求头及本地假上游进行 A/B 检查：默认配置丢弃
session_id，但保留 X-Session-ID；启用后两者均能到达假上游。测试没有访问真实模型，
也未改变生产监听或生产配置。该行为与
https://nginx.org/en/docs/http/ngx_http_core_module.html#underscores_in_headers 一致。

DSH 默认 Responses 适配器使用下划线拼写，因此在使用 Nginx 时，还应在此网关专用
server 块启用：

```nginx
server {
    underscores_in_headers on;
    # 保留原来的 listen、TLS、鉴权和 proxy_pass 等设置。
}
```

只在专用 server 块加这一行即可，无需全局关闭无效请求头检查。应先备份、执行
nginx -t，再平滑 reload。调查时准备的单行候选配置已通过 nginx -t；实际应用生产
配置需要用户另行授权，因为最初任务明确禁止修改生产配置。

这是可确认的会话元数据传递缺陷；缺少这项设置并不能独立证明上游 11128 的根因。
修复后仍应根据真实客户端下一次请求的脱敏诊断判断：session_supplied 是否为 true，
是否命中 session_id_normalized，以及上游是否继续明确返回渠道拒绝。不能把反代头
兼容修复称为官方渠道授权，也不应通过修改正文或身份伪装继续尝试绕过。
生产当时的亲和模式为 prefix，传递会话头本身不改变这一既有调度设置。
