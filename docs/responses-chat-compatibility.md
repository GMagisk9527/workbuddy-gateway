# Responses → WorkBuddy Chat 兼容范围

`POST /v1/responses` 转为 Chat 上游，流式和非流式共用相同请求转换。服务器不保存
Responses 资源；客户端使用 `store:false` 并回放完整 input 历史。

| 字段或行为 | 网关处理 |
| --- | --- |
| 文本、图片、函数及 custom 工具 | 沿用现有转换、工具名映射和参数保真规则 |
| 非空上游 refusal | 保留独立 refusal 内容块及 response.refusal.delta/done；快慢路径与聚合均支持 |
| metadata | 最多 16 对字符串，键／值分别最多 64／512 字符；仅在网关响应中回显，不作为 Chat 用户标识转发 |
| 推理 token | 标准 completion_tokens_details.reasoning_tokens 优先，包括显式 0；缺失或无效时取 completion_thinking_tokens，不相加 |
| include: [reasoning.encrypted_content] | 返回网关加密的推理回传状态；completed reasoning 的 output_item.done 与最终 output 使用同一密文 |
| 网关 encrypted_content 历史 | 在校验、转 Chat 前恢复推理正文，允许只有密文的 reasoning 项；保留现有明文历史兼容 |
| reasoning.effort、text.verbosity、普通生成参数 | 沿用既有显式值及共享出站准备规则 |

加密状态采用请求 API Key 派生的 AES-GCM 密钥，绑定模型和 Responses 协议；不同调用方
不能复用。客户端应回传完整 reasoning 项，不应修改密文。更换 Key／模型、篡改或截断
密文返回本地 400。网关生成的状态只能交给本实现恢复，不能与 OpenAI 厂商密文互换。
省略 include 时仍沿用已有明文 summary 回传方式。

未携带 API Key 时使用管理 Key；完全没有稳定密钥的开放实例使用进程内随机密钥，重启后
旧状态会失效。固定 Key 的实例无需服务端历史缓存即可跨重启回放；密文和正文不落盘。

以下非默认语义无法由当前 Chat 上游提供，会在调用上游前返回 400：

- background:true、非空 prompt 模板、自动 truncation。
- 非文本 text.format，包括不能保证遵循约束的 JSON Schema／JSON object。
- 其他无法生成数据的 include 项。
- 既有的 store:true、previous_response_id、conversation 服务端引用。
- 只有原生厂商 encrypted_content 且没有可回放明文的 reasoning 历史。

空选项及默认 background:false、truncation:disabled、text.format.type:text 仍兼容。
metadata 在 created、in_progress、completed／incomplete／failed 响应 envelope 中一致回显。
未收到有效完成原因或上游读取失败时继续按原逻辑返回 failed，不伪造成功。

推理 token 回退只改善输出明细，不修改总 token、credit 或结算；非空拒答计入有效输出，
空字符串、null、心跳和 role 声明不计入。11128 等业务错误仍按原错误及禁止重放规则处理。

规范参考：[Responses 流式事件](https://developers.openai.com/api/reference/resources/responses/streaming-events)、
[推理历史回放](https://developers.openai.com/api/docs/guides/reasoning)。
