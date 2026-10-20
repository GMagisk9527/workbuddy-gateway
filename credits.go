package main

// 临期积分优先（Expiring Credits First）。
//
// 上游证据（2026-10 实测 /billing/meter/get-user-resource-free-packages）：
//   - 每个积分包都带 DeductionEndTime（epoch 毫秒），这才是"积分作废时刻"；
//   - CycleEndTime 只是月度计费周期边界，个人体验版两个字段相差 9 年
//     （CycleEndTime=2026-10-31，DeductionEndTime=2035-01-19），
//     用 CycleEndTime 判断到期会把长期有效的积分误判成临期；
//   - 每个包带 CycleCapacityRemainPrecise（剩余额度）。
//
// 因此本模块只认 DeductionEndTime；字段缺失时把该包标记为"到期未知"，
// 不参与临期判断、也不影响任何调度——缺失数据永远不比现有策略更优先。

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/google/uuid"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	creditPackagesFreePath = "/billing/meter/get-user-resource-free-packages"
	creditPackagesPaidPath = "/billing/meter/get-user-resource-paid-packages"

	creditMaxPackages   = 64 // 单账号最多保留多少个包（防止异常响应撑爆内存）
	creditFetchTimeoutS = 15 // 单次抓取超时（秒）
	defaultCreditWindow = 7  // 默认临期窗口（天）
)

// creditPackage 是一个积分包的可判定信息。ExpiryKnown=false 表示上游没给
// DeductionEndTime，此时既不显示"临期"，也不参与调度。
type creditPackage struct {
	Name        string    `json:"name"`
	Remain      float64   `json:"remain"`
	ExpiresAt   time.Time `json:"-"`
	ExpiryKnown bool      `json:"expiryKnown"`
	ExpiresUnix int64     `json:"expiresAt,omitempty"`
}

// accountCredits 是账号维度的积分包快照（受 accountMu 保护）。
type accountCredits struct {
	Known      bool            // 最近一次抓取是否成功
	FetchedAt  int64           // 抓取时间（Unix 秒）
	Err        string          // 失败原因（脱敏短文本）
	Packages   []creditPackage // 至多 creditMaxPackages 个
	SoonestAt  time.Time       // 有剩余且到期已知的包中最早的作废时间
	SoonestAmt float64         // 该包的剩余额度
	AnyUnknown bool            // 是否存在"有剩余但到期未知"的包
}

// reset 清空，用于账号禁用/凭据变更后避免展示过期数据。
func (c *accountCredits) reset() { *c = accountCredits{} }

// expiringWithin 返回窗口内会作废的剩余额度合计（只统计到期已知的包）。
func (c *accountCredits) expiringWithin(window time.Duration, now time.Time) (amount float64, earliest time.Time) {
	if !c.Known {
		return 0, time.Time{}
	}
	deadline := now.Add(window)
	for _, p := range c.Packages {
		if !p.ExpiryKnown || p.Remain <= 0 || p.ExpiresAt.IsZero() {
			continue
		}
		if p.ExpiresAt.After(deadline) || !p.ExpiresAt.After(now) {
			continue // 不在窗口内，或已经过期
		}
		amount += p.Remain
		if earliest.IsZero() || p.ExpiresAt.Before(earliest) {
			earliest = p.ExpiresAt
		}
	}
	return amount, earliest
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

type creditSettings struct {
	Fetch        bool          // 是否抓取积分包明细（面板展示用）
	ExpiringNext bool          // 是否启用临期优先调度
	Window       time.Duration // 临期窗口
}

var (
	creditConfigMu sync.RWMutex
	creditCfg      = creditSettings{Fetch: true, ExpiringNext: false, Window: defaultCreditWindow * 24 * time.Hour}
	creditPicked   atomic.Int64 // 因临期优先而选中的次数
)

func currentCreditSettings() creditSettings {
	creditConfigMu.RLock()
	s := creditCfg
	creditConfigMu.RUnlock()
	return s
}

func setCreditConfig(fetch, expiringFirst bool, windowDays int) {
	if windowDays <= 0 {
		windowDays = defaultCreditWindow
	}
	if windowDays > 3650 {
		windowDays = 3650
	}
	creditConfigMu.Lock()
	creditCfg = creditSettings{Fetch: fetch, ExpiringNext: expiringFirst, Window: time.Duration(windowDays) * 24 * time.Hour}
	creditConfigMu.Unlock()
}

func creditSnapshot() map[string]any {
	s := currentCreditSettings()
	return map[string]any{
		"fetch":          s.Fetch,
		"expiringFirst":  s.ExpiringNext,
		"windowDays":     int(s.Window / (24 * time.Hour)),
		"expiringPicked": creditPicked.Load(),
	}
}

// ---------------------------------------------------------------------------
// 抓取与解析
// ---------------------------------------------------------------------------

type creditPackageResponse struct {
	Accounts []map[string]any `json:"Accounts"`
}

// fetchAccountCredits 读取一个账号的积分包明细（只读计费接口，不消耗模型积分）。
// 返回的 error 只用于日志，不影响额度主流程。
func fetchAccountCredits(ctx context.Context, prof *upstreamProfile, headers func(*http.Request), packageCodes []string, includePaid bool) (*accountCredits, error) {
	if len(packageCodes) == 0 {
		return &accountCredits{Known: false, FetchedAt: time.Now().Unix(), Err: "上游未返回套餐编码"}, nil
	}
	out := &accountCredits{Known: true, FetchedAt: time.Now().Unix()}
	seen := map[string]bool{}
	add := func(list []map[string]any) {
		for _, raw := range list {
			if len(out.Packages) >= creditMaxPackages {
				return
			}
			p := parseCreditPackage(raw)
			if id, ok := raw["AccountId"].(string); ok && id != "" {
				if seen[id] {
					continue
				}
				seen[id] = true
			}
			out.Packages = append(out.Packages, p)
		}
	}

	call := func(path string, body map[string]any) []map[string]any {
		data, status, err := doJSONContext(ctx, cfg.HttpClient, http.MethodPost, prof.Base+path, headers, jsonReader(body))
		if err != nil {
			out.Known = false
			out.Err = "计费接口失败 HTTP=" + credItoa(status)
			return nil
		}
		// doJSONContext 已经校验 code 并返回 data 段。
		var payload creditPackageResponse
		if err := json.Unmarshal(data, &payload); err != nil {
			out.Known = false
			out.Err = "计费响应结构异常"
			return nil
		}
		return payload.Accounts
	}

	add(call(creditPackagesFreePath, map[string]any{
		"PackageCodes": packageCodes, "PageNumber": 1, "PageSize": 200, "Status": []int{0},
	}))
	if includePaid {
		add(call(creditPackagesPaidPath, map[string]any{
			"PackageCodes": packageCodes, "PageNumber": 1, "PageSize": 200,
			"Status": []int{0, 3}, "NeedRenewInfo": true,
		}))
	}
	out.computeSoonest(time.Now())
	return out, nil
}

// computeSoonest 预计算"最早作废且有剩余"的包，避免调度时遍历列表。
func (c *accountCredits) computeSoonest(now time.Time) {
	c.SoonestAt, c.SoonestAmt, c.AnyUnknown = time.Time{}, 0, false
	for _, p := range c.Packages {
		if p.Remain <= 0 {
			continue
		}
		if !p.ExpiryKnown || p.ExpiresAt.IsZero() {
			c.AnyUnknown = true
			continue
		}
		if !p.ExpiresAt.After(now) {
			continue // 已过期，不再作为可用额度
		}
		if c.SoonestAt.IsZero() || p.ExpiresAt.Before(c.SoonestAt) {
			c.SoonestAt, c.SoonestAmt = p.ExpiresAt, p.Remain
		}
	}
}

func parseCreditPackage(raw map[string]any) creditPackage {
	p := creditPackage{Name: strings.TrimSpace(stringField(raw, "PackageName"))}
	if p.Name == "" {
		p.Name = "未命名积分包"
	}
	p.Remain = firstNumber(raw,
		"CycleCapacityRemainPrecise", "CycleCapacityRemain",
		"CapacityRemainPrecise", "CapacityRemain")
	// 只认 DeductionEndTime：这是"积分作废时刻"。
	// CycleEndTime 是计费周期边界（体验版能差 9 年），绝不能拿来判断临期。
	if ts, ok := epochMillis(raw["DeductionEndTime"]); ok && ts > 0 {
		p.ExpiresAt = time.Unix(ts, 0)
		p.ExpiryKnown = true
		p.ExpiresUnix = ts
	}
	return p
}

func epochMillis(v any) (int64, bool) {
	f, ok := numberValue(v)
	if !ok || f <= 0 {
		return 0, false
	}
	if f > 1e11 { // 毫秒
		return int64(f) / 1000, true
	}
	return int64(f), true
}

func numberValue(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	case string:
		return parseFloatLoose(x)
	}
	return 0, false
}

func firstNumber(raw map[string]any, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := numberValue(raw[k]); ok {
			return v
		}
	}
	return 0
}

// ---------------------------------------------------------------------------
// 调度：选一个"临期且有剩余"的账号作为新绑定的优先项
// ---------------------------------------------------------------------------

// expiringCandidate 在已通过资格检查的账号里，选最早作废且落在窗口内的那个。
// 只在"没有亲和绑定"时使用——已有绑定的会话优先保住缓存。
func expiringCandidateLocked(accounts []*Account, eligible func(*Account) bool, now time.Time) (*Account, time.Time) {
	settings := currentCreditSettings()
	if !settings.ExpiringNext {
		return nil, time.Time{}
	}
	deadline := now.Add(settings.Window)
	var best *Account
	var bestAt time.Time
	for _, acc := range accounts {
		if !eligible(acc) {
			continue
		}
		c := &acc.credits
		if !c.Known || c.SoonestAmt <= 0 || c.SoonestAt.IsZero() {
			continue
		}
		if !c.SoonestAt.After(now) || c.SoonestAt.After(deadline) {
			continue // 已过期或不在窗口内
		}
		if best == nil || c.SoonestAt.Before(bestAt) {
			best, bestAt = acc, c.SoonestAt
		}
	}
	return best, bestAt
}

// sortCreditsForDisplay 仅用于面板展示：按到期时间升序，未知到期排最后。
func sortCreditsForDisplay(list []creditPackage) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.ExpiryKnown != b.ExpiryKnown {
			return a.ExpiryKnown
		}
		if !a.ExpiryKnown {
			return a.Remain > b.Remain
		}
		return a.ExpiresAt.Before(b.ExpiresAt)
	})
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func credItoa(n int) string { return strconv.Itoa(n) }

func jsonReader(v any) *bytes.Reader {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte("{}")
	}
	return bytes.NewReader(b)
}

func parseFloatLoose(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// refreshCreditsForAccount 抓取积分包明细。
//
// 关键约束：本函数**不持有 acc.lock**，也不在持有 accountMu 的情况下发请求。
// 只读计费接口最长 4 秒，如果占着账号串行锁，正常模型请求会全部排队。
func refreshCreditsForAccount(ctx context.Context, acc *Account) {
	if !currentCreditSettings().Fetch || acc == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}

	// 拍快照：令牌、套餐编码、可见性判断都在锁内完成，出锁后不再读账号字段。
	accountMu.Lock()
	if acc.Auth == nil || acc.Disabled || acc.Auth.Auth.AccessToken == "" || len(acc.creditCodes) == 0 {
		if acc.Disabled || acc.Auth == nil {
			acc.credits.reset()
			acc.creditCodes = nil
		} else {
			// 上游这次没给套餐编码（偶发空响应或企业账号）：不能沿用上一次的
			// 到期数据，否则面板会展示已经无法核实的"临期"信息。
			acc.credits = accountCredits{Known: false, FetchedAt: time.Now().Unix(), Err: "上游未返回套餐编码"}
		}
		accountMu.Unlock()
		return
	}
	auth := *acc.Auth
	paid := acc.IsPaidUser
	codes := append([]string(nil), acc.creditCodes...)
	accountMu.Unlock()

	prof := profileForEdition(auth.Edition)
	traceID := uuid.NewString()
	headers := func(r *http.Request) {
		commonHeaders(r, prof)
		r.Header.Set("Authorization", "Bearer "+auth.Auth.AccessToken)
		r.Header.Set("X-Client-Platform", "web")
		r.Header.Set("X-Trace-ID", traceID)
	}

	fetchCtx, cancel := context.WithTimeout(ctx, creditFetchTimeoutS*time.Second)
	defer cancel()
	info, err := fetchAccountCredits(fetchCtx, prof, headers, codes, paid)
	if err != nil {
		info = &accountCredits{Known: false, FetchedAt: time.Now().Unix(), Err: "抓取失败"}
	}

	// 写回前再确认身份没变：热加载换了凭据/账号被禁用时，丢弃这次结果。
	accountMu.Lock()
	stale := acc.Auth == nil || acc.Disabled || acc.Auth.Auth.AccessToken != auth.Auth.AccessToken
	if !stale {
		acc.credits = *info
	}
	path := acc.Path
	accountMu.Unlock()
	if stale {
		return
	}

	if err != nil {
		log.Printf("[积分包] 账号=%s 结果=抓取失败 原因=%v 业务影响=本次不参与临期优先，沿用原调度", path, err)
		return
	}
	if info.Known && !info.SoonestAt.IsZero() {
		log.Printf("[积分包] 账号=%s 包数=%d 最早作废=%s 剩余=%.2f 到期未知包=%t",
			path, len(info.Packages), info.SoonestAt.Format("2006-01-02 15:04"), info.SoonestAmt, info.AnyUnknown)
	} else {
		log.Printf("[积分包] 账号=%s 结果=无可用到期信息 包数=%d 业务影响=不参与临期优先", path, len(info.Packages))
	}
}
