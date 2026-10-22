# Claude Code 会话标识

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
