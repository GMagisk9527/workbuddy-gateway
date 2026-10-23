# Messages → WorkBuddy Chat 兼容范围

`POST /v1/messages` 把 Anthropic 请求转为固定 WorkBuddy Chat 上游。
流式与非流式共用同一份转换，非流式在网关内聚合上游 SSE。

| 输入 | 转换行为 |
| --- | --- |
| system、用户文本 | 保留指令和正文，沿用现有身份前缀、fallback/force 规则 |
| `messages` 内的 `system` / `developer` 文本 | 保留既有指令角色兼容；共享 Chat 预处理把 `developer` 归一为 `system`，不把用户或工具结果提升为指令 |
| base64／URL 图片 | 转为 Chat `image_url`；工具结果中的图片沿用既有提升逻辑 |
| 自定义工具 | `input_schema` 转为 `function.parameters`，保留 `strict: true/false` |
| `tool_use` / `tool_result` | 保留调用 ID、名称、参数和结果关联，沿用现有工具序列修复 |
| `tool_result.content[].tool_reference` | 把唯一声明的工具定义展开成结果文本；保留其余结果顺序、图片及数字精度，未声明或有歧义的引用返回 400 |
| `tool_result.is_error=true` | Chat 没有等价字段，在原结果前加 `[Tool execution error]`；空失败结果只保留该标记 |
| 工具参数中的 JSON 数字 | 请求历史和非流式响应都避免浮点中转，保留大整数及高精度小数 |
| enabled／adaptive thinking | 沿用现有 effort 转换及明文历史回放；不把不透明签名、redacted 数据当正文 |
| `thinking.display=omitted/updates` | 隐藏上游推理正文，返回空 thinking 和可回传的网关加密 signature；不关闭模型推理或改变 usage |
| `context_management` 保留全部 thinking | `clear_thinking_20251015` 且 `keep:"all"` 是无操作配置，继续兼容 |
| cache_control、metadata 等 | 保留既有网关兼容处理；不会把 Anthropic 缓存或延迟工具加载语义冒充为 Chat 原生能力 |

消息内的指令角色是本网关兼容扩展。Claude Code 启动入口可能使用 `[user, system]`，
同时提供顶层 system 文本块；角色白名单不能将这种结构提前拒绝。
回归测试用合成正文重建该结构，覆盖国内／国际站、legacy／official 模式及流式／聚合响应。
这里只接受指令文本，不放宽图片、thinking 或工具块在不同角色下的原有约束。

`strict` 只是完整传递工具约束，不代表网关增加了采样器或 JSON Schema 验证器。
参数约束能否由模型强制执行取决于上游，调用方仍应验证实际工具输入。

`defer_loading` 工具仍作为普通 Chat 函数注册。自定义 ToolSearch 返回的引用会展开为
已声明的函数定义；这不实现厂商原生的延迟上下文加载，也不会仅因该标记就删除工具。

## 思考展示与历史回传

`display` 省略或设为 `summarized` 时保留原有思考展示。WorkBuddy 当前没有可供转换器
识别的独立进度更新字段，因此 `updates` 与 `omitted` 都隐藏推理正文，不伪造进度摘要。
两者仍发送原 effort，保留思考块位置、工具关联及上游实际 usage。

隐藏块的 `signature` 使用 AES-GCM 认证加密携带推理文本。客户端应把整个 thinking 块
原样放回后续 assistant 历史；网关在转 Chat 前恢复 reasoning_content，无需服务器保存
会话缓存。状态绑定请求 API Key、模型及协议，篡改或跨 Key／模型回放返回本地 400。
这不是 Anthropic 厂商签名，也不能交给原生 Anthropic 上游解密。

固定 API Key 允许重启后继续回放；更换 Key 或模型时应重新开始会话或提供完整的兼容
历史。未携带 Key 时使用管理 Key；完全没有稳定密钥的开放实例只能在当前进程恢复状态，
重启会失效。不要依赖旧 SHA 标记恢复隐藏文本，它们仍只用于旧的明文块。

## 明确返回本地 400 的情况

错误使用 Anthropic `invalid_request_error` 形状，包含字段路径，不记录正文。
这些请求不选号、不调用上游、不消耗模型额度：

- 缺少或空的 `messages`，即使提供了 system，也不能当成有效对话。
- `messages[].role` 不在 `user` / `assistant` / `system` / `developer` 中。原生 Chat 的 `tool` / `function` 历史不能经文本角色兜底保留调用关联，应使用 Messages 的 `tool_use` / `tool_result` 块。
- 不能转换的消息或内容形状；文档／PDF、search_result、未知内容块包括嵌套在工具结果中的形态。
- Anthropic 托管工具及需要专属 schema 的预定义工具；不能把它们伪装成普通自定义函数。
- 非空 `output_config.format`。普通 `output_config.effort` 仍可使用。
- 实际要求裁剪历史的 context_management 编辑、非空托管 mcp_servers/container、无法保证的 service_tier（保留 auto）。
- 非法 thinking.display、无法验证的网关加密推理状态及未声明的工具引用。
- 历史 `tool_use.input` 缺失、为 null、数组或标量；工具输入必须是 JSON 对象，空对象 `{}` 有效。

2026-10-23 使用两个低额度合成 Chat 请求检查当前上游：带 `strict` 的普通工具调用
返回了匹配参数；`response_format=json_schema` 虽返回 200，输出却没有遵循 schema。
因此 Messages 入口明确拒绝结构化输出要求，避免返回“成功”但已经丢失约束的结果。
Responses 入口采用相同的结构化输出能力边界，不表示所有模型都能或都不能执行这些约束。

## 上游流错误

SSE 注释、event/id/retry 等控制行继续兼容。`data:` 中的坏 JSON 不再静默跳过：
流式返回 error 并停止，不伪造 message_stop；非流式返回 502。
工具参数分片在上游结束后检查：非空参数必须组成完整 JSON 对象，坏 JSON、数组、null
及标量同样返回上述错误，不会被包装成成功的工具调用。空参数字符串沿用无参工具的 `{}`
约定；合法对象的数字精度和参数内容保持不变。

`finish_reason=length` 截断工具参数时也返回不完整错误，调用方可增加输出预算后重试；
合法的对象和普通文本仍沿用 `max_tokens` 收尾。已收到的上游 usage 即使随后报错也保留结算，
完整性标记为 false。原有读取中断、无 finish_reason、业务拒绝及 TTFT 规则继续沿用。

非空 `delta.refusal` 会保留为 Messages 文本；无工具调用、正常 stop 的拒答使用
`stop_reason:"refusal"`，截断或工具调用仍保留相应停止原因。业务错误（包括 11128）
继续按原错误路径返回，不会被改写成模型拒答。

本次没有新增厂商服务端工具执行环境、PDF 提取器、搜索引用系统或完整 schema 验证器。
`count_tokens` 仍为既有本地估算，不能代替上游 tokenizer 或计费数据。

协议依据：[工具结果和 is_error](https://platform.claude.com/docs/en/agents-and-tools/tool-use/handle-tool-calls)、
[strict 工具约束](https://platform.claude.com/docs/en/agents-and-tools/tool-use/strict-tool-use)、
[结构化输出](https://platform.claude.com/docs/en/build-with-claude/structured-outputs)、
[流式事件](https://platform.claude.com/docs/en/build-with-claude/streaming)、
[思考展示](https://platform.claude.com/docs/en/build-with-claude/thinking)、
[工具搜索引用](https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool)。
