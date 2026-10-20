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
// 该请求实际使用的账号（日志里有 requestId 与账号文件名），或把账号池临时
// 收敛到单个账号再跑。
package main

import (
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
	filler   int // 前缀重复单元数量，约等于 token 量的粗调
	gap      time.Duration
	timeout  time.Duration
}

type probeResult struct {
	phase        string
	round        int
	sessionID    string
	promptTokens int64
	cachedTokens int64
	cacheSource  string
	totalTokens  int64
	ttft         time.Duration
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
	flag.Parse()

	if cfg.apiKey == "" {
		fmt.Fprintln(os.Stderr, "缺少 -key（或 WBG_API_KEY）。网关未启用 API Key 校验时可留空。")
	}

	prefix := buildPrefix(cfg.filler)
	toolsA := buildTools("Bash")
	toolsB := buildTools("ReadFile")

	scenarios := []struct {
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

	fmt.Printf("网关=%s 模型=%s 协议=%s 每场景 %d 轮 前缀填充=%d 段\n\n", cfg.url, cfg.model, cfg.protocol, cfg.rounds, cfg.filler)
	fmt.Printf("%-40s %6s %10s %10s %10s %10s %9s\n", "场景", "轮次", "prompt", "cached", "命中率", "TTFT", "状态")

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
			status := fmt.Sprintf("%d", r.status)
			if r.err != "" {
				status = "ERR"
			}
			fmt.Printf("%-40s %6d %10d %10d %10s %10s %9s\n",
				sc.phase, i+1, r.promptTokens, r.cachedTokens, ratio, r.ttft.Round(time.Millisecond), status)
			if r.err != "" {
				fmt.Printf("    ! %s\n", r.err)
			}
		}
	}

	fmt.Println()
	summarize(all)
}

// buildPrefix 构造一段稳定、可重复的长前缀（模拟 system + 历史）。
func buildPrefix(n int) string {
	var b strings.Builder
	b.WriteString("You are a coding agent. Follow the repository conventions strictly.\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "规则 %d：在修改任何文件之前先阅读相关实现，并在回复中说明改动理由与影响范围。\n", i)
	}
	return b.String()
}

// buildTools 构造一份工具定义；名字不同即代表"前缀发生变化"。
func buildTools(name string) string {
	return `[{"type":"function","function":{"name":"` + name + `","description":"run a command","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}]`
}

func runOne(cfg probeConfig, prefix, tools, sessionID string) probeResult {
	var res probeResult
	res.sessionID = sessionID

	var body []byte
	var path string
	switch cfg.protocol {
	case "chat":
		path = "/v1/chat/completions"
		payload := map[string]any{
			"model":    cfg.model,
			"stream":   true,
			"messages": []any{map[string]any{"role": "system", "content": prefix}, map[string]any{"role": "user", "content": "回答：ok"}},
		}
		if tools != "" {
			var t any
			if json.Unmarshal([]byte(tools), &t) == nil {
				payload["tools"] = t
			}
		}
		body, _ = json.Marshal(payload)
	default:
		path = "/v1/responses"
		payload := map[string]any{
			"model":        cfg.model,
			"stream":       true,
			"instructions": prefix,
			"input":        []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "回答：ok"}}}},
		}
		if tools != "" {
			var t any
			if json.Unmarshal([]byte(tools), &t) == nil {
				payload["tools"] = t
			}
		}
		body, _ = json.Marshal(payload)
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
	req.Header.Set("X-Cache-Probe", "1")

	client := &http.Client{Timeout: cfg.timeout}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		res.err = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.status = resp.StatusCode

	dec := json.NewDecoder(resp.Body)
	for {
		var ev map[string]any
		if err := dec.Decode(&ev); err != nil {
			if err != io.EOF && res.err == "" && resp.StatusCode >= 400 {
				res.err = err.Error()
			}
			break
		}
		if res.ttft == 0 {
			res.ttft = time.Since(start)
		}
		if u, ok := ev["usage"].(map[string]any); ok && u != nil {
			readUsage(u, &res)
		}
		if r, ok := ev["response"].(map[string]any); ok {
			if u, ok := r["usage"].(map[string]any); ok && u != nil {
				readUsage(u, &res)
			}
		}
	}
	res.elapsed = time.Since(start)
	return res
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
	if v, src := cacheField(u); v >= 0 {
		res.cachedTokens = v
		res.cacheSource = src
	}
}

// cacheField 复刻网关的取值优先级：这些字段是同一数量的不同表示，不能相加。
func cacheField(u map[string]any) (int64, string) {
	for _, k := range []string{"prompt_cache_hit_tokens", "cache_read_input_tokens", "cached_tokens"} {
		if v, ok := numField(u[k]); ok {
			return v, k
		}
	}
	for _, k := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if d, ok := u[k].(map[string]any); ok {
			if v, ok := numField(d["cached_tokens"]); ok {
				return v, k + ".cached_tokens"
			}
		}
	}
	if _, ok := u["cache_creation_input_tokens"]; ok {
		return 0, "cache_creation_input_tokens"
	}
	return -1, ""
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

// summarize 只做描述性统计，不给"提升百分比"结论——那需要真实账号与 A/B。
func summarize(all []probeResult) {
	fmt.Println("判读方法：")
	fmt.Println("  1) 若所有场景 cached 恒为 0 且 cacheSource 为空 → 上游没有回报缓存字段，")
	fmt.Println("     任何基于亲和的路由优化都无法被验证，应先确认上游是否支持显式/自动缓存。")
	fmt.Println("  2) 若 A 组第 2 轮起 cached>0、B 组（换 session）也 >0 → 缓存按前缀命中，")
	fmt.Println("     与 session 标识无关；此时前缀亲和有效。")
	fmt.Println("  3) 若 A 组命中而 B 组不命中 → 缓存与 session/会话维度相关，应优先使用会话亲和。")
	fmt.Println("  4) 若 D 组（改工具定义）仍命中 → 上游前缀匹配粒度很粗，需进一步缩小改动再测。")
	fmt.Println("  5) 网关是多账号轮询：同一场景内落到不同账号会干扰判读，")
	fmt.Println("     建议先把账号池收敛到 1 个账号跑一遍作为基线，再放开对比。")
	fmt.Println()
	var withCache, without int
	for _, r := range all {
		if r.cachedTokens > 0 {
			withCache++
		} else {
			without++
		}
	}
	fmt.Printf("本次共 %d 个请求：cached>0 的 %d 个，cached==0 的 %d 个。\n", len(all), withCache, without)
}
