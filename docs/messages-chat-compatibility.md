# Messages → WorkBuddy Chat 兼容范围

`POST /v1/messages` 把 Anthropic 请求转为固定 WorkBuddy Chat 上游。
流式与非流式共用同一份转换，非流式在网关内聚合上游 SSE。

| 输入 | 转换行为 |
| --- | --- |
| system、用户文本 | 保留指令和正文，沿用现有身份前缀、fallback/force 规则 |
| base64／URL 图片 | 转为 Chat `image_url`；工具结果中的图片沿用既有提升逻辑 |
| 自定义工具 | `input_schema` 转为 `function.parameters`，保留 `strict: true/false` |
| `tool_use` / `tool_result` | 保留调用 ID、名称、参数和结果关联，沿用现有工具序列修复 |
| `tool_result.is_error=true` | Chat 没有等价字段，在原结果前加 `[Tool execution error]`；空失败结果只保留该标记 |
| 工具参数中的 JSON 数字 | 请求历史和非流式响应都避免浮点中转，保留大整数及高精度小数 |
| enabled／adaptive thinking | 沿用现有 effort 转换及明文历史回放；不把不透明签名、redacted 数据当正文 |
| cache_control、metadata 等 | 保留既有网关兼容处理；不会把 Anthropic 缓存或延迟工具加载语义冒充为 Chat 原生能力 |

`strict` 只是完整传递工具约束，不代表网关增加了采样器或 JSON Schema 验证器。
参数约束能否由模型强制执行取决于上游，调用方仍应验证实际工具输入。

## 明确返回本地 400 的情况

错误使用 Anthropic `invalid_request_error` 形状，包含字段路径，不记录正文。
这些请求不选号、不调用上游、不消耗模型额度：

- 缺少或空的 `messages`，即使提供了 system，也不能当成有效对话。
- 不能转换的消息或内容形状；文档／PDF、search_result、未知内容块包括嵌套在工具结果中的形态。
- Anthropic 托管工具及需要专属 schema 的预定义工具；不能把它们伪装成普通自定义函数。
- 非空 `output_config.format`。普通 `output_config.effort` 仍可使用。

2026-10-23 使用两个低额度合成 Chat 请求检查当前上游：带 `strict` 的普通工具调用
返回了匹配参数；`response_format=json_schema` 虽返回 200，输出却没有遵循 schema。
因此 Messages 入口明确拒绝结构化输出要求，避免返回“成功”但已经丢失约束的结果。
这不改变其他协议入口的既有行为，也不表示所有模型都能或都不能执行这些约束。

## 上游流错误

SSE 注释、event/id/retry 等控制行继续兼容。`data:` 中的坏 JSON 不再静默跳过：
流式返回 error 并停止，不伪造 message_stop；非流式返回 502。
原有读取中断、无 finish_reason、业务拒绝、TTFT 和用量结算路径继续沿用。

本次没有新增厂商服务端工具执行环境、PDF 提取器、搜索引用系统或完整 schema 验证器。
`count_tokens` 仍为既有本地估算，不能代替上游 tokenizer 或计费数据。

协议依据：[工具结果和 is_error](https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls)、
[strict 工具约束](https://platform.claude.com/docs/en/agents-and-tools/tool-use/strict-tool-use)、
[结构化输出](https://platform.claude.com/docs/en/build-with-claude/structured-outputs)、
[流式事件](https://platform.claude.com/docs/en/build-with-claude/streaming)。
