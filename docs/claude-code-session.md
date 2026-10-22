# Claude Code 协议兼容

## 会话标识

Claude Code 在请求头中提供 `x-claude-code-session-id`，子代理额外提供
`x-claude-code-agent-id`。这是客户端的会话元数据，含义见
[Claude Code 官方网关兼容文档](https://code.claude.com/docs/en/llm-gateway-protocol#request-headers)。

网关需要在协议转换前读取这些字段，为同一调用方的同一会话维持稳定的上游
`X-Conversation-ID`；不同子代理按会话和 agent 共同隔离。父 agent 标识不作为
子代理的会话键，避免把嵌套代理与父代理合并。

- 既有显式头继续优先：`X-Conversation-ID`、`thread-id`、`X-Session-ID`、
  `session-id`、`session_id`。
- 接着读取 Claude Code 会话头，再回退到既有的 body/metadata 会话字段。
- 无有效会话键时，每次 HTTP 请求独立生成会话；单独的 agent 头不足以建立会话。
- 会话标识按调用方规范化，不把原始会话／agent 字符串写入日志或上游正文。
- 每次 HTTP 请求仍独立生成轮次和消息 ID；同一请求的安全重试沿用已有标识。

此处理不改变客户端正文、工具、鉴权、SSE 或计费，不制造客户端凭证。
它修复的是会话元数据丢失，不能据此认定所有 `11128` 都会消失。
`channel_verification_failed` 仍按上游原错误返回，渠道授权需要由上游确认。

## Anthropic 归因元数据

Claude Code 会把 `x-anthropic-billing-header: ...` 放在第一个独立 system
文本块中。网关原先把它和后面的指令合并成一个字符串，发送给 WorkBuddy。
在 Claude Code 2.1.308 的完整合成请求中（14 个工具、adaptive/high），
保持同一会话、请求头和其余正文的对照结果如下：

| 请求 | 上游结果 |
| --- | --- |
| 原样 | HTTP 400 / 11128 |
| 仅省略首个归因块 | HTTP 200 |
| 恢复原样 | HTTP 400 / 11128 |

因此 Messages → WorkBuddy 转换会在合并 system 之前消费这一段已识别的
Anthropic 元数据。匹配范围刻意限制为：

- system 必须为数组，且只检查索引 0 一次。
- 首块只能有 `type: "text"` 和字符串 `text`；带 `cache_control` 或其他字段的块保留。
- 文本必须是独立单行、以精确的小写头名开头、分号结尾的 ASCII 归因格式。
- 必须有唯一的 `cc_version`、`cc_entrypoint`，可选唯一的 `cch`；未知、重复、空值或格式不符时保留。
- `cc_entrypoint` 仅识别 `cli` 和官方客户端 print 模式使用的 `sdk-cli`。

system 字符串、后续文本块、普通用户正文、代码、历史消息以及工具定义／参数／结果
均不按这个规则删除。其余 system 指令保留；仅有归因块时沿用
既有默认／配置 fallback。日志只记录固定规则名，不记录归因值或正文。
SSE、用量计算和账号调度沿用既有逻辑；后续若仍收到 11128，会原样作为错误返回，
不会因本规则重试或切换账号。

[官方文档](https://code.claude.com/docs/en/llm-gateway-protocol#system-prompt-attribution-block)
解释了该归因块的位置要求，并对需要改写 system 的网关建议在客户端设置
`CLAUDE_CODE_ATTRIBUTION_HEADER=0`。本项目固定转译到非 Anthropic 的 WorkBuddy
协议，以上消费规则只用于这条转换路径，不适用于向 Anthropic／云端 Claude
原样透传请求的网关。实测结果确认的是上述合成请求的触发项，其他 11128 仍需单独诊断。

## 固定身份声明

默认配置会把文本块开头已识别的完整 Claude Code／Claude Agent SDK 身份声明改为
`You are an AI coding assistant.`，保留后续指令。包含旧 CLI、SDK 和团队代理的
固定开头；不把替换结果描述成 Anthropic 或 WorkBuddy 的官方客户端。
Markdown 代码范围按完整消息计算，包括跨文本块的代码。用户正文、工具以及
正文中段的名称讨论不参与默认前缀规则。处理在 Anthropic system 合并前以及
共享 fallback/force 注入后执行，force 检查复用同一规则以避免重复追加。

显式的 `identity.target`／`replacements` 仍优先使用调用方配置；
`identity.enabled=false` 可关闭身份兼容。更多配置见
[Messages 身份文本说明](messages-identity.md)。
