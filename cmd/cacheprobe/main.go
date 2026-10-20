// cacheprobe 用于验证 WorkBuddy 上游（腾讯 CodeBuddy）的 Prompt Cache 行为。
//
// 它只通过网关发标准的 OpenAI Responses / Chat Completions 请求，控制"前缀"和
// "会话标识"两个变量，读回 usage 里的缓存字段。用来回答：
//
//  1. 上游到底报不报缓存命中（cached_tokens / prompt_cache_hit_tokens）？
//  2. 相同前缀重复请求，第二次是否命中？
//  3. 换一个会话标识（X-Session-ID）是否影响命中？
//  4. 前缀里改一个工具定义，命中是否消失？
//  5. 隔一段时间再请求，命中是否还在（缓存 TTL）？
//
// 用法：
//
//	go run ./cmd/cacheprobe -url http://127.0.0.1:21816 -key <APIKey> -model deepseek-v4.1-flash
//
// 说明：网关是多账号轮询，单次结果可能落在不同账号上。因此每个场景都会重复
// `-rounds` 次并打印每一次的明细；判断"账号是否共享缓存"需要在网关侧核对
// 该请求实际使用的账号，或把账号池临时收敛到单个账号再跑。
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type probeConfig struct {
	url      string
	apiKey   string
	model    string
	protocol string // responses | chat
	rounds   int
	filler   int
	gap      time.Duration
	timeout  time.Duration
	nonce    string
	maxTok   int
	only     string
}

type probeResult struct {
	phase        string
	round        int
	sessionID    string
	promptTokens int64
	cachedTokens int64
	cacheSource  string
	cacheSeen    bool // 上游是否给出了任何缓存字段
	cacheField   string
	totalTokens  int64
	events       int
	firstEvent   time.Duration // 首个 SSE 事件到达
	ttft         time.Duration // 首个有效输出增量
	ttftSet      bool          // 独立标志：避免"0 时长"与"没收到增量"混淆
	elapsed      time.Duration
	status       int
	err          string
}

func main() {
	cfg := probeConfig{}
	flag.StringVar(&cfg.url, "url", "http://127.0.0.1:21816", "网关地址（模型 API 端口）")
	flag.StringVar(&cfg.apiKey, "key", os.Getenv("WBG_API_KEY"), "网关 API Key（也可用环境变量 WBG_API_KEY）")
	flag.StringVar(&cfg.model, "model", "deepseek-v4.1-flash", "模型名（原样透传给上游）")
	flag.StringVar(&cfg.protocol, "protocol", "responses", "responses | chat")
	flag.IntVar(&cfg.rounds, "rounds", 3, "每个场景重复次数")
	flag.IntVar(&cfg.filler, "filler", 240, "前缀填充段数量（每段约 30 个中文字符）")
	flag.DurationVar(&cfg.gap, "gap", 0, "每轮之间的等待（用于测缓存 TTL，如 100s）")
	flag.DurationVar(&cfg.timeout, "timeout", 180*time.Second, "单请求超时")
	flag.StringVar(&cfg.nonce, "nonce", "", "独一无二的前缀标记（用于构造冷缓存）")
	flag.IntVar(&cfg.maxTok, "max-tokens", 16, "输出上限，控制测试成本")
	flag.StringVar(&cfg.only, "only", "", "只跑指定场景前缀（A/B/C/D/E），留空跑全部")
	flag.Parse()

	if cfg.apiKey == "" {
		fmt.Fprintln(os.Stderr, "提示：未提供 -key（或 WBG_API_KEY）。网关未启用 API Key 校验时可直接留空。")
	}

	prefix := buildPrefix(cfg.filler, cfg.nonce)
	toolsA := buildTools("Bash")
	toolsB := buildTools("ReadFile")

	allScenarios := []struct {
		phase   string
		session func(int) string
		tools   string
		prefix  string
	}{
		{"A 同会话重复（同前缀+同 session）", func(i int) string { return "probe-session-fixed" }, toolsA, prefix},
		{"B 同前缀+新 session", func(i int) string { return fmt.Sprintf("probe-session-%d", i) }, toolsA, prefix},
		{"C 同前缀+无 session（走前缀亲和）", func(i int) string { return "" }, toolsA, prefix},
		{"D 改工具定义（前缀不同）", func(i int) string { return "probe-session-fixed" }, toolsB, prefix},
		{"E 同前缀+同 session（TTL 观察）", func(i int) string { return "probe-session-fixed" }, toolsA, prefix},
	}

	scenarios := allScenarios
	if cfg.only != "" {
		var picked []struct {
			phase   string
			session func(int) string
			tools   string
			prefix  string
		}
		for _, sc := range allScenarios {
			if strings.HasPrefix(sc.phase, strings.ToUpper(cfg.only)) {
				picked = append(picked, sc)
			}
		}
		if len(picked) == 0 {
			fmt.Fprintf(os.Stderr, "未知场景 %q\n", cfg.only)
			os.Exit(2)
		}
		scenarios = picked
	}

	fmt.Printf("网关=%s 模型=%s 协议=%s 每场景 %d 轮 前缀填充=%d 段 nonce=%s\n\n", cfg.url, cfg.model, cfg.protocol, cfg.rounds, cfg.filler, cfg.nonce)
	fmt.Printf("%-40s %4s %9s %9s %8s %9s %9s %9s %6s\n", "场景", "轮次", "prompt", "cached", "命中率", "首事件", "TTFT", "耗时", "事件")

	var all []probeResult
	for _, sc := range scenarios {
		for i := 0; i < cfg.rounds; i++ {
			if cfg.gap > 0 && !(sc.phase[0] == 'A' && i == 0) {
				time.Sleep(cfg.gap)
			}
			r := runOne(cfg, sc.prefix, sc.tools, sc.session(i))
			r.phase = sc.phase
			r.round = i + 1
			all = append(all, r)

			ratio := "-"
			if r.promptTokens > 0 {
				ratio = fmt.Sprintf("%.1f%%", float64(r.cachedTokens)*100/float64(r.promptTokens))
			}

			fmt.Printf("%-40s %4d %9d %9d %8s %9s %9s %9s %6d\n",
				sc.phase, i+1, r.promptTokens, r.cachedTokens, ratio,
				r.firstEvent.Round(time.Millisecond), r.ttft.Round(time.Millisecond), r.elapsed.Round(time.Millisecond), r.events)
			if r.err != "" {
				fmt.Printf("    ! %s\n", r.err)
			}
		}
	}

	fmt.Println()
	summarize(all)
}

func buildPrefix(n int, nonce string) string {
	var b strings.Builder
	b.WriteString("You are a coding agent. Follow the repository conventions strictly.\n")
	if nonce != "" {
		b.WriteString("实验标记：" + nonce + "\n")
	}
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "规则 %d：在修改任何文件之前先阅读相关实现，并在回复中说明改动理由与影响范围。\n", i)
	}
	return b.String()
}

func buildTools(name string) string {
	return `[{"type":"function","function":{"name":"` + name + `","description":"run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}]`
}

func runOne(cfg probeConfig, prefix, tools, sessionID string) probeResult {
	var res probeResult
	res.sessionID = sessionID

	var payload map[string]any
	var path string
	if cfg.protocol == "chat" {
		path = "/v1/chat/completions"
		payload = map[string]any{
			"model":      cfg.model,
			"stream":     true,
			"max_tokens": cfg.maxTok,
			"messages":   []any{map[string]any{"role": "system", "content": prefix}, map[string]any{"role": "user", "content": "回答：ok"}},
		}
	} else {
		path = "/v1/responses"
		payload = map[string]any{
			"model":             cfg.model,
			"stream":            true,
			"max_output_tokens": cfg.maxTok,
			"instructions":      prefix,
			"input":             []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "回答：ok"}}}},
		}
	}
	if tools != "" {
		var t any
		if json.Unmarshal([]byte(tools), &t) == nil {
			payload["tools"] = t
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		res.err = "构造请求失败: " + err.Error()
		return res
	}

	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(cfg.url, "/")+path, bytes.NewReader(body))
	if err != nil {
		res.err = err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
	}
	if sessionID != "" {
		req.Header.Set("X-Session-ID", sessionID)
	}

	client := &http.Client{Timeout: cfg.timeout}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		res.err = "请求失败: " + err.Error()
		return res
	}
	defer resp.Body.Close()
	res.status = resp.StatusCode

	// 非 2xx 必须先读出错误正文：HTTP 200 之外不能假装成功。
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		res.err = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, clip(string(b), 400))
		res.elapsed = time.Since(start)
		return res
	}
	// 网关在出错时可能返回 JSON（例如鉴权失败），这时按普通 JSON 报告，而不是空流。
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(strings.ToLower(ct), "event-stream") {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		var e map[string]any
		if json.Unmarshal(b, &e) == nil {
			res.err = fmt.Sprintf("期望 SSE，收到 JSON（Content-Type=%s）: %s", ct, clip(string(b), 300))
		} else {
			res.err = fmt.Sprintf("期望 SSE，Content-Type=%s，正文: %s", ct, clip(string(b), 300))
		}
		res.elapsed = time.Since(start)
		return res
	}

	if err := readSSE(resp.Body, start, &res); err != nil {
		res.err = err.Error()
	}
	res.elapsed = time.Since(start)
	if res.err == "" && !res.cacheSeen {
		res.err = "本次流里没有出现任何缓存字段（不是 0 命中，而是上游没给该字段）"
	}
	return res
}

// readSSE 做真正的 SSE 分帧：data: 可多行，空行结束一个事件，注释行以 ':' 开头。
func readSSE(r io.Reader, start time.Time, res *probeResult) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)

	var data []byte
	parseFailures := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 {
				if err := handleSSEData(data, start, res); err != nil {
					parseFailures++
					if parseFailures == 1 {
						return err
					}
				}
				data = data[:0]
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // 注释/心跳
		}
		if !strings.HasPrefix(line, "data:") {
			continue // event: / id: / retry:
		}
		v := strings.TrimPrefix(line, "data:")
		v = strings.TrimPrefix(v, " ")
		if len(data) > 0 {
			data = append(data, '\n')
		}
		data = append(data, v...)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("读取 SSE 失败: %w", err)
	}
	if len(data) > 0 {
		_ = handleSSEData(data, start, res)
	}
	if res.events == 0 {
		return fmt.Errorf("SSE 流结束但没有解析出任何事件（可能是上游返回了非 SSE 内容）")
	}
	return nil
}

func handleSSEData(data []byte, start time.Time, res *probeResult) error {
	payload := bytes.TrimSpace(data)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil
	}
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil {
		return fmt.Errorf("SSE 事件不是合法 JSON: %v", err)
	}
	res.events++
	if res.firstEvent == 0 {
		res.firstEvent = time.Since(start)
	}
	if !res.ttftSet && hasDeltaContent(ev) {
		res.ttft = time.Since(start)
		res.ttftSet = true
	}
	readUsageFromEvent(ev, res)
	return nil
}

// hasDeltaContent 判断这个事件是否携带真正的输出增量（而不是 created/updated 之类的控制帧）。
// 覆盖两种协议：
//   - Responses: {"type":"response.output_text.delta","delta":"..."}
//   - Chat:      {"choices":[{"delta":{"content":"..."}}]}
func hasDeltaContent(ev map[string]any) bool {
	// Responses 形态
	if s, ok := ev["delta"].(string); ok && s != "" {
		return true
	}
	if d, ok := ev["delta"].(map[string]any); ok && deltaHasContent(d) {
		return true
	}
	// Chat 形态
	if choices, ok := ev["choices"].([]any); ok {
		for _, c := range choices {
			ch, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if d, ok := ch["delta"].(map[string]any); ok && deltaHasContent(d) {
				return true
			}
		}
	}
	// Responses 的增量事件有时把文本放在 part/delta 的嵌套里
	if d, ok := ev["delta"].(map[string]any); ok {
		if part, ok := d["part"].(map[string]any); ok {
			if s, ok := part["text"].(string); ok && s != "" {
				return true
			}
		}
	}
	return false
}

func deltaHasContent(d map[string]any) bool {
	for _, k := range []string{"content", "reasoning_content", "reasoning", "text"} {
		if s, ok := d[k].(string); ok && s != "" {
			return true
		}
	}
	if calls, ok := d["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	return false
}

func readUsageFromEvent(ev map[string]any, res *probeResult) {
	if u, ok := ev["usage"].(map[string]any); ok && u != nil {
		readUsage(u, res)
	}
	if r, ok := ev["response"].(map[string]any); ok {
		if u, ok := r["usage"].(map[string]any); ok && u != nil {
			readUsage(u, res)
		}
	}
}

func readUsage(u map[string]any, res *probeResult) {
	if v, ok := numField(u["prompt_tokens"]); ok {
		res.promptTokens = v
	}
	if v, ok := numField(u["input_tokens"]); ok && res.promptTokens == 0 {
		res.promptTokens = v
	}
	if v, ok := numField(u["total_tokens"]); ok {
		res.totalTokens = v
	}
	if v, src, ok := cacheField(u); ok {
		res.cacheSeen = true
		res.cachedTokens = v
		res.cacheSource = src
	}
}

// cacheField 复刻网关的取值优先级：这些字段是同一数量的不同表示，不能相加。
func cacheField(u map[string]any) (int64, string, bool) {
	for _, k := range []string{"prompt_cache_hit_tokens", "cache_read_input_tokens", "cached_tokens"} {
		if v, ok := numField(u[k]); ok {
			return v, k, true
		}
	}
	for _, k := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if d, ok := u[k].(map[string]any); ok {
			if v, ok := numField(d["cached_tokens"]); ok {
				return v, k + ".cached_tokens", true
			}
		}
	}
	return 0, "", false
}

func numField(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func summarize(all []probeResult) {
	fmt.Println("判读方法：")
	fmt.Println("  1) cacheSeen=false → 上游没有回报任何缓存字段，先确认上游是否支持缓存；")
	fmt.Println("     注意与\"字段存在但为 0\"区分：后者说明支持但本次没命中。")
	fmt.Println("  2) A 组第 2 轮起 cached>0、B 组（换 session）也 >0 → 缓存按前缀命中，与 session 无关，前缀亲和有效。")
	fmt.Println("  3) A 组命中而 B 组不命中 → 缓存与会话维度相关，应优先使用会话亲和。")
	fmt.Println("  4) D 组（改工具定义）仍命中 → 上游前缀匹配粒度很粗，需进一步缩小改动再测。")
	fmt.Println("  5) 网关是多账号轮询：同场景内落到不同账号会干扰判读，")
	fmt.Println("     建议先把账号池收敛到 1 个账号跑基线，再放开对比。")
	fmt.Println()
	var hit, zero, missing int
	for _, r := range all {
		switch {
		case r.err != "":
			missing++
		case r.cachedTokens > 0:
			hit++
		case r.cacheSeen:
			zero++
		default:
			missing++
		}
	}
	fmt.Printf("共 %d 个请求：命中>0 的 %d 个，字段存在但为 0 的 %d 个，无数据/失败的 %d 个。\n", len(all), hit, zero, missing)
}
