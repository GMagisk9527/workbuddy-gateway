# DSH Search / Fetch 适配

同步自上游 `d301562`（实现提交 `f803975`）。这两个接口独立于 Chat、Responses、Messages，需在支持自定义 Search / Fetch API 的 DSH 版本中分别配置网关地址和 API Key。仅配置模型 Provider 不表示 DSH 会自动使用这两个接口。

- `POST /v1/search`：`{"query":"golang","count":5,"need_content":false,"ori_question":"Go language"}`。返回 `{"results":[{"title":"...","url":"...","snippet":"...","date":null}]}`。count 默认 5，最多 20；query 为 2–2000 个字符。need_content 和 ori_question 按上游适配保留接收兼容，但不传给 WorkBuddy，也不承诺返回完整页面。
- `POST /v1/fetch`：`{"url":"https://example.com"}`。返回 `{"content":"..."}`。抓取由上游完成，网关不直接访问用户指定 URL。

两接口使用已有 API Key 鉴权、禁用/过期/额度准入、全局请求与账号并发限制，支持现有 gzip/deflate/zstd 解码。解码后工具 JSON 上限为 1 MiB，上游响应上限同为 1 MiB。若 Key 配置了模型白名单，需分别允许 `web-search`、`web-fetch`；没有白名单则沿用现有开放权限。

实际调用先写 WAL 意图，返回 `usage.credit` 时按实测记账，缺失时计入未知消耗，遵循现有未知消耗准入策略，不能据此认为搜索免费。不会用余额差猜测费用。HTTP 4xx 或明确的 `11128`/`11140` 拒绝且无用量时取消意图；网络失败、响应截断等不确定结果保留未知消耗。会话头沿用现有规范化及 API Key 隔离逻辑。

不自动重试、换号或跟随上游重定向。明确 `11140` 保留凭据并持久化停止该账号后续调度；普通 403 不停用健康账号。`11128` 业务码如实返回，未增加官方桌面身份断言或伪造设备凭据；若上游要求官方渠道，本适配无法解除限制。错误不回显上游正文，避免其中夹带查询、URL 或凭据。

验证为来源明确的合成离线请求：参考上游 `f803975` 的 `websearch_test.go`，由真实 `httptest` HTTP 链路验证国内/国际站、鉴权、会话隔离、用量/WAL、大小限制、错误及重定向。未调用真实收费接口，真实 DSH 与上游成功验证尚未完成。
