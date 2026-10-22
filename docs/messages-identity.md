# Messages API 的 Claude Code 身份文本

v1.14.29 支持通过 `identity` 配置处理 system/developer/assistant 的非代码文本。
省略该段或使用 `{}` 时只保留既有 DSH 前缀兼容，不替换 `Claude Code`。
升级程序不会自动改写旧配置。

如果希望使用统一的展示名称，可将下面这一段合入现有 `config.json`，保留其他配置：

```json
{
  "identity": {
    "enabled": true,
    "target": "WorkBuddy"
  }
}
```

该配置会使用内置名称规则，不限于 Claude Code。需要更小范围时，使用自定义
`replacements`；例如将明确的身份声明改为中性描述，而保留对客户端本身的讨论：

```json
{
  "identity": {
    "enabled": true,
    "replacements": {
      "You are Claude Code, Anthropic's official CLI tool for Claude.": "You are a coding assistant."
    }
  }
}
```

键按原文精确匹配，应以实际客户端发送的声明为准。配置可热加载，仅影响后续请求。
用户正文、工具定义/参数/结果、thinking 和 Markdown 代码不会参与名称替换，
因此不保证出站 JSON 完全不含 `Claude Code`。避免将替换结果再作为其他规则的输入，
因为兼容处理可能执行多次。

`/v1/messages` 在合并 Anthropic system 文本块前执行处理，并在共享出站步骤中
覆盖 fallback/force 注入的文本。force 去重也必须使用相同的名称规则和完整消息的
Markdown 上下文，避免把已经规范化的 force 再次追加。

离线回归 `TestMessagesClaudeCodeIdentityConfigThroughHTTP` 覆盖默认、target 和自定义
规则，字符串/分块/fallback、流式/聚合响应，以及代码和工具保护。
`TestIdentityForce*` 覆盖 force 去重、跨块代码和空替换。
测试使用本地 HTTP 上游，不消耗模型额度。

这些测试只证明网关的转换行为。仅凭提示词出现某个名称不能确定 11128 的根因；
上游仍拒绝时，网关保留原错误，不换号重放或伪装成功。
