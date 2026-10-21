package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// 模型属性探测 (probe)
//
// 目的：让「从未被调度过」的账号（典型如余额耗尽的国际站账号）也能主动学习
// 某个模型对它是免费还是收费，而不必等客户端请求碰巧轮到它。
//
// 为什么要走 HTTP 而不是独立进程直连上游：
//   免费/收费账本保存在 serve 进程内存中（仅通过 workbuddy-status.json 周期快照），
//   独立进程写入的状态文件会被运行中的服务覆盖。因此 probe 子命令只作为客户端，
//   真正执行探测并更新账本的是运行中的服务端 /admin/probe 接口。
//
// 安全：/admin/probe 只接受回环地址来源；服务设置了 -api-key 时仍需携带该密钥。
// -----------------------------------------------------------------------------

const probePrompt = "Please write a factual paragraph of about 150 words about the water cycle, without markdown."

type probeRequest struct {
	Auth   string   `json:"auth,omitempty"`   // 凭据文件路径或文件名；空=全部账号
	Models []string `json:"models,omitempty"` // 模型列表；空=目录前 limit 个
	Limit  int      `json:"limit,omitempty"`  // Models 为空时的最大模型数，默认 5
}

type probeResult struct {
	Account string  `json:"account"`
	Edition string  `json:"edition"`
	Model   string  `json:"model"`
	Status  string  `json:"status"` // free | paid | unknown | quota | rate_limited | auth_failed | skipped | error
	Credit  float64 `json:"credit"` // usage.credit（仅成功时有意义）
	Tokens  int64   `json:"tokens"` // usage.total_tokens
	HTTP    int     `json:"http"`   // 上游状态码
	Detail  string  `json:"detail,omitempty"`
}

type probeResponse struct {
	Results []probeResult  `json:"results"`
	Summary map[string]int `json:"summary"`
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

const (
	// 单次探测最多 5 个模型、最多 10 次上游调用：这是"看得见的成本上限"，
	// 防止显式传入上千个模型把账号积分一次性烧掉。
	probeMaxModels        = 5
	probeMaxUpstreamCalls = 10
)

// probeAdminOnly 是 /admin/probe 的独立鉴权：只认管理密钥 + 回环来源。
// 普通 wbk_ Key 与旧 gateway.apiKey 一律无效——它们能通过普通 API 鉴权，
// 但探测接口会直接打上游、不走额度结算，绝不能复用那条链。
func probeAdminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		admin := currentAdminKey()
		if admin == "" {
			// 没有配置管理密钥时坚决关闭，不回退到普通 Key，也不允许匿名。
			writeOpenAIError(w, http.StatusServiceUnavailable, "admin_key_not_configured",
				"未配置管理密钥，探测接口已关闭；请在 config.json 设置 gateway.adminKey 后重试")
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			token = r.Header.Get("x-api-key")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(admin)) != 1 {
			log.Printf("[Probe] 拒绝：管理密钥无效 来源=%s 说明=探测接口不接受普通 API Key", r.RemoteAddr)
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_admin_key", "探测接口仅接受管理密钥")
			return
		}
		// 第二层防护：即使持有管理密钥，也只允许本机回环调用。
		if !isLoopbackRequest(r) {
			log.Printf("[Probe] 拒绝：非回环来源 来源=%s", r.RemoteAddr)
			writeOpenAIError(w, http.StatusForbidden, "probe_local_only", "探测接口仅允许本机回环地址调用")
			return
		}
		next(w, r)
	}
}

func handleAdminProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}
	if !isLoopbackRequest(r) {
		log.Printf("[Probe] 拒绝非回环来源的探测请求: %s", r.RemoteAddr)
		writeOpenAIError(w, http.StatusForbidden, "probe_local_only", "探测接口仅允许本机回环地址调用")
		return
	}

	var req probeRequest
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
			return
		}
	}
	if req.Limit <= 0 {
		req.Limit = probeMaxModels
	}
	if req.Limit > probeMaxModels {
		writeOpenAIError(w, http.StatusBadRequest, "probe_limit_too_large",
			fmt.Sprintf("单次探测最多 %d 个模型，请拆分请求", probeMaxModels))
		return
	}
	// 显式模型列表：去重 + 上限，绝不接受"传 1000 个"这种变相放大消耗。
	if len(req.Models) > 0 {
		seen := make(map[string]bool, len(req.Models))
		deduped := make([]string, 0, len(req.Models))
		for _, m := range req.Models {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			key := normalizeModelName(m)
			if seen[key] {
				continue
			}
			seen[key] = true
			deduped = append(deduped, m)
		}
		if len(deduped) > probeMaxModels {
			writeOpenAIError(w, http.StatusBadRequest, "probe_too_many_models",
				fmt.Sprintf("单次探测最多 %d 个模型（去重后 %d 个），请拆分请求", probeMaxModels, len(deduped)))
			return
		}
		req.Models = deduped
	}

	accountMu.Lock()
	candidates := append([]*Account(nil), accounts...)
	accountMu.Unlock()

	targets := make([]*Account, 0, len(candidates))
	for _, acc := range candidates {
		if acc.Disabled {
			continue
		}
		if req.Auth != "" && !probeAuthMatches(acc, req.Auth) {
			continue
		}
		targets = append(targets, acc)
	}
	if len(targets) == 0 {
		writeOpenAIError(w, http.StatusNotFound, "no_account", "没有匹配的账号（检查 -auth 路径/文件名）")
		return
	}

	models := req.Models
	if len(models) == 0 {
		all, _ := mergedModelIDs()
		if len(all) > req.Limit {
			all = all[:req.Limit]
		}
		models = all
	}

	// 账号数 × 模型数 = 实际上游调用次数，必须受总上限约束。
	if len(targets)*len(models) > probeMaxUpstreamCalls {
		writeOpenAIError(w, http.StatusBadRequest, "probe_too_many_calls",
			fmt.Sprintf("本次探测将产生 %d 次上游调用，超过单次上限 %d；请减少账号范围或模型数量",
				len(targets)*len(models), probeMaxUpstreamCalls))
		return
	}

	log.Printf("[Probe] 开始探测：账号数=%d，模型数=%d，账号=%s，模型=%s",
		len(targets), len(models), probeAccountNames(targets), strings.Join(models, ","))

	resp := probeResponse{Summary: map[string]int{}}
	for _, acc := range targets {
		for _, model := range models {
			if r.Context().Err() != nil {
				break
			}
			if disabled, reason := modelDisabled(model); disabled {
				result := probeResult{Account: acc.Path, Edition: acc.Profile().Key, Model: model, Status: "skipped", Detail: reason}
				resp.Results = append(resp.Results, result)
				resp.Summary[result.Status]++
				log.Printf("[Probe] traceId=%s 模型=%s 结果=跳过 原因=模型已停用 说明=不会调用上游", debugTraceID(r), model)
				continue
			}
			if allowed, reason := modelAccountAllowed(model, acc, candidates); !allowed {
				result := probeResult{Account: acc.Path, Edition: acc.Profile().Key, Model: model, Status: "skipped", Detail: "账号名单排除：" + reason}
				resp.Results = append(resp.Results, result)
				resp.Summary[result.Status]++
				log.Printf("[Probe] traceId=%s 模型=%s 凭据=%s 结果=跳过 原因=%s", w.Header().Get("X-Trace-ID"), model, filepath.Base(acc.Path), reason)
				debugEvent(r, "debug", "model_account_policy_checked", map[string]any{
					"account_file": filepath.Base(acc.Path), "model": model, "allowed": false, "reason": reason,
				})
				continue
			}
			result := probeAccountModel(r.Context(), acc, model)
			resp.Results = append(resp.Results, result)
			resp.Summary[result.Status]++
			log.Printf("[Probe] 账号 %s [%s] 模型 %s -> %s (%s)", result.Account, result.Edition, model, result.Status, result.Detail)
		}
	}
	writeStatusSnapshot()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func probeAuthMatches(acc *Account, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return true
	}
	return acc.Path == want || filepath.Base(acc.Path) == filepath.Base(want)
}

func probeAccountNames(accs []*Account) string {
	names := make([]string, 0, len(accs))
	for _, acc := range accs {
		names = append(names, filepath.Base(acc.Path))
	}
	return strings.Join(names, ",")
}

// probeAccountModel 对单个账号 + 模型发一次最小请求并学习免费/收费属性。
func probeAccountModel(ctx context.Context, acc *Account, model string) probeResult {
	result := probeResult{Account: filepath.Base(acc.Path), Edition: acc.Profile().Key, Model: model}

	if !lockAccountWithContext(ctx, &acc.lock) {
		result.Status = "error"
		result.Detail = "等待账号锁超时"
		return result
	}
	defer acc.lock.Unlock()

	accountMu.Lock()
	if allowed, reason := modelAccountAllowed(model, acc, accounts); !allowed {
		accountMu.Unlock()
		result.Status, result.Detail = "skipped", "账号名单排除："+reason
		log.Printf("[Probe] traceId=%s 模型=%s 凭据=%s 结果=跳过 原因=%s 说明=等待账号锁后重新检查热配置，未调用上游", newTraceID(), model, filepath.Base(acc.Path), reason)
		return result
	}
	if acc.Disabled || acc.Auth == nil || acc.Auth.Auth.AccessToken == "" {
		accountMu.Unlock()
		result.Status = "skipped"
		result.Detail = "账号失效或无凭据"
		return result
	}
	auth := *acc.Auth
	prof := acc.Profile()
	accountMu.Unlock()
	if disabled, reason := modelDisabled(model); disabled {
		result.Status, result.Detail = "skipped", reason
		return result
	}

	body := map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 300,
		"messages": []any{
			map[string]any{"role": "system", "content": defaultSystemPrompt},
			map[string]any{"role": "user", "content": probePrompt},
		},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		result.Status = "error"
		result.Detail = err.Error()
		return result
	}

	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, prof.chatURL(), bytes.NewReader(payload))
	if err != nil {
		result.Status = "error"
		result.Detail = err.Error()
		return result
	}
	backendHeaders(req, &auth, prof)

	resp, err := cfg.HttpClient.Do(req)
	if err != nil {
		result.Status = "error"
		result.Detail = "上游请求失败: " + err.Error()
		return result
	}
	defer resp.Body.Close()
	result.HTTP = resp.StatusCode

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		errStr := string(errBody)
		switch {
		case isQuotaExhausted(resp.StatusCode, errStr):
			markModelQuotaBlocked(acc, model, errStr)
			result.Status = "quota"
			result.Detail = "额度耗尽（已记为该账号收费模型）"
		case isModelRateLimited(errStr):
			until, ok := parseResetTime(errStr)
			if !ok {
				until = time.Now().Add(60 * time.Second)
			}
			markModelCooldown(acc, model, until, errStr)
			result.Status = "rate_limited"
			result.Detail = "模型级限流至 " + until.Format("2006-01-02 15:04:05")
		case isAuthFailure(resp.StatusCode, errStr):
			result.Status = "auth_failed"
			result.Detail = "授权失效（未自动禁用，请重新登录）：" + truncate(errStr, 120)
		default:
			result.Status = "error"
			result.Detail = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(errStr, 120))
		}
		return result
	}

	usage := readUsageFromSSE(resp.Body)
	credit, hasCredit := usageCredit(usage)
	tokens, _ := usageTotalTokens(usage)
	if !hasCredit {
		result.Status = "unknown"
		result.Detail = "上游未返回 usage.credit，无法判定"
		return result
	}
	result.Credit = credit
	result.Tokens = tokens

	if credit > 0 {
		observeModelCredit(acc, model, usage, 0)
		result.Status = "paid"
		result.Detail = fmt.Sprintf("usage.credit=%s，收费", formatQuota(credit))
		return result
	}
	if tokens < modelFreeMinTokens {
		result.Status = "unknown"
		result.Detail = fmt.Sprintf("credit=0 但样本过小（total_tokens=%d < %d），不判定免费", tokens, modelFreeMinTokens)
		return result
	}
	observeModelCredit(acc, model, usage, 0)
	result.Status = "free"
	result.Detail = fmt.Sprintf("usage.credit=0，total_tokens=%d，免费", tokens)
	return result
}

// readUsageFromSSE 从上游 SSE 流中读取最后一个 usage 对象。
func readUsageFromSSE(r io.Reader) map[string]any {
	var usage map[string]any
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		data := stripDataPrefix(scanner.Text())
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
	}
	return usage
}

// runProbe 是 probe 子命令：作为客户端调用运行中服务的 /admin/probe。
func runProbe() {
	addr := fmt.Sprintf("%s:%d", cfg.Addr, cfg.Port)
	if cfg.Addr == "0.0.0.0" || cfg.Addr == "::" {
		addr = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}
	url := "http://" + addr + "/admin/probe"

	reqBody := probeRequest{Limit: cfg.ProbeLimit}
	if cfg.AuthExplicit {
		reqBody.Auth = cfg.AuthFile
	}
	for _, m := range strings.Split(cfg.ProbeModels, ",") {
		if m = strings.TrimSpace(m); m != "" {
			reqBody.Models = append(reqBody.Models, m)
		}
	}
	payload, _ := json.Marshal(reqBody)

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		fmt.Printf("构造请求失败: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	admin := currentAdminKey()
	if admin == "" {
		fmt.Println("未配置管理密钥：探测接口只接受 gateway.adminKey。")
		fmt.Println("请在 config.json 的 gateway.adminKey 填入随机字符串（保存后约 1 秒生效），再重试。")
		return
	}
	// 探测接口只认管理密钥；普通 API Key 会被 401 拒绝。
	req.Header.Set("Authorization", "Bearer "+admin)
	fmt.Printf("正在请求 %s（账号=%s，模型=%s）...\n", url, ifEmpty(reqBody.Auth, "全部"), ifEmpty(strings.Join(reqBody.Models, ","), "目录前若干"))

	resp, err := (&http.Client{Timeout: 15 * time.Minute}).Do(req)
	if err != nil {
		fmt.Printf("调用失败: %v\n请确认 serve 正在运行，且 -addr/-port 与之一致。\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("服务返回 HTTP %d: %s\n", resp.StatusCode, truncate(string(body), 300))
		return
	}
	var out probeResponse
	if err := json.Unmarshal(body, &out); err != nil {
		fmt.Printf("解析响应失败: %v\n", err)
		return
	}

	sort.SliceStable(out.Results, func(i, j int) bool {
		if out.Results[i].Account != out.Results[j].Account {
			return out.Results[i].Account < out.Results[j].Account
		}
		return out.Results[i].Model < out.Results[j].Model
	})
	fmt.Printf("\n%-22s %-8s %-24s %-12s %-8s %-8s %s\n", "账号", "站点", "模型", "结果", "credit", "tokens", "说明")
	for _, r := range out.Results {
		fmt.Printf("%-22s %-8s %-24s %-12s %-8s %-8d %s\n",
			r.Account, ifEmpty(r.Edition, "-"), r.Model, r.Status,
			ifEmpty(formatQuota(r.Credit), "-"), r.Tokens, r.Detail)
	}
	fmt.Printf("\n汇总: ")
	keys := make([]string, 0, len(out.Summary))
	for k := range out.Summary {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s=%d ", k, out.Summary[k])
	}
	fmt.Println()
}
