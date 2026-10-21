# DSH 请求样本来源

`requests.json` 中的请求是**依据源码重建的离线合成样本**，不是用户会话抓包，
也不代表真实 DSH 联调成功。其中的消息、代码、工具参数、结果和会话 ID 都是虚构值。

调查对象为本机已安装的 DeepSeek Harness 0.2.1-rc.2，其中打包
MIT 授权的 `@earendil-works/pi-ai` 1.1.0。
上游项目：https://github.com/earendil-works/pi ，`packages/ai`。
重建 `requests.json` 时只读取了适配器、压缩逻辑及配置中的 provider 类型；未读取用户会话历史。

| 安装包中的源码 | SHA-256 | 样本依据 |
| --- | --- | --- |
| openai-responses.js | 8de25051531f131c4ae3d730f371fa0e15559ed7f865765a744c64d5edd2e067 | createClient、buildParams |
| openai-responses-shared.js | 21fb0ec9278949ff00cb7af59849e5e3a8c278c807fedd1d7d6e27a024153bda | convertResponsesMessages、工具结果 |
| openai-completions.js | fba125cffc926f44bbbad10ecb6301516cff027a3ef526cefe25076a96c5c8fb | createClient、buildParams、detectCompat |
| compaction.js | b7ac52f20a36430cf01a66b3a36f23a48ffdb22708d714dbe1fff541b5df4ec5 | summarizeWithLlm、frameSummary |

Responses 适配器在默认 OpenAI 会话格式下发送 `session_id` 请求头；
`prompt_cache_key` 也可以携带同一 session，但缓存键本身不保证全局唯一，因此网关
不把任意缓存键当作会话 ID。压缩调用沿用 agent.session.id，并在回放前缀后追加
user 摘要指令；摘要恢复使用 user 消息中的 `<compacted-summary>`。
这些结构在样本中保留，摘要指令正文是合成文本。

Chat 的通用第三方 provider 默认不发送会话亲和头。`chat_single_default`
保持此行为；`chat_tools_explicit_session_compat` 对应显式启用
sendSessionAffinityHeaders 后的形状，不声称通用 Chat 默认会发送它。
不提供稳定会话 ID 时，网关保留现有 Prefix Affinity，不根据提示词猜测会话身份。

样本覆盖单轮、工具调用/结果、摘要请求、摘要恢复及子会话。测试分别经过国内/国际
profile、流式/聚合 HTTP 链路，检查正文与工具参数原样保留。实际网络只连接 httptest。

## GUI 环境说明模板

`gui_context.txt` 是 MIT 授权的 `@deepseek-ai/dsh-harness-dev-context`
0.2.1-rc.2 的完整内置 `WEB_SURFACE_PROMPT`，取自安装包静态源码/README，
不是用户会话正文。其源码项目为
https://github.com/deepseek-ai/deepseek-harness/tree/main/packages/context/harness-dev-context 。
原许可证随样本保存在 [LICENSE.harness-dev-context](LICENSE.harness-dev-context)。
UTF-8 长度 1006 字节，SHA-256 为
`7496863f7f81a12cad97ff5c643339f8ddf344b6525c191c29ab086bec5a400a`。
该摘要用于验证样本来源，不作为运行时整段匹配条件。网关对 system/assistant
文本中已确认的两句开头做中性环境描述转换，后续 GUI、构建和服务管理指令保持原样。
匹配边界和线上验证范围见 [专项说明](../../docs/11128-dsh-gui-context.md)。
