package main

// 临期积分优先的单元测试：解析、窗口、候选选择、与亲和的关系、配置热加载。

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func quietCredits(t *testing.T) {
	t.Helper()
	old := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(old) })
}

// 上游把"积分作废时刻"放在 DeductionEndTime，CycleEndTime 只是计费周期边界。
func TestParseCreditPackageUsesDeductionEndTime(t *testing.T) {
	now := time.Now()
	deduction := now.AddDate(0, 0, 30).UnixMilli()
	cycleEnd := now.AddDate(0, 0, 10).Format("2006-01-02 15:04:05")

	p := parseCreditPackage(map[string]any{
		"PackageName":                "包A",
		"CycleCapacityRemainPrecise": float64(100),
		"DeductionEndTime":           float64(deduction),
		"CycleEndTime":               cycleEnd,
	})
	if !p.ExpiryKnown {
		t.Fatal("有 DeductionEndTime 时应判定为到期已知")
	}
	want := time.Unix(deduction/1000, 0)
	if !p.ExpiresAt.Equal(want) {
		t.Fatalf("到期时间应取 DeductionEndTime: want %v got %v", want, p.ExpiresAt)
	}
	if p.Remain != 100 {
		t.Fatalf("剩余额度解析错误: %v", p.Remain)
	}

	// 只有 CycleEndTime：不得拿它当到期时间（体验版会差 9 年）
	p2 := parseCreditPackage(map[string]any{
		"PackageName":      "体验版",
		"CycleEndTime":     cycleEnd,
		"CapacityRemain":   "12",
		"DeductionEndTime": "",
	})
	if p2.ExpiryKnown {
		t.Fatal("缺少 DeductionEndTime 时必须标记为到期未知，不能回退到 CycleEndTime")
	}
	if p2.Remain != 12 {
		t.Fatalf("字符串型剩余额度应能解析: %v", p2.Remain)
	}
}

func TestCreditExpiringWithinWindow(t *testing.T) {
	now := time.Now()
	c := accountCredits{Known: true, Packages: []creditPackage{
		{Name: "3天后", Remain: 100, ExpiresAt: now.Add(3 * 24 * time.Hour), ExpiryKnown: true},
		{Name: "30天后", Remain: 500, ExpiresAt: now.Add(30 * 24 * time.Hour), ExpiryKnown: true},
		{Name: "已过期", Remain: 50, ExpiresAt: now.Add(-time.Hour), ExpiryKnown: true},
		{Name: "到期未知", Remain: 70},
		{Name: "零剩余", Remain: 0, ExpiresAt: now.Add(time.Hour), ExpiryKnown: true},
	}}
	amt, earliest := c.expiringWithin(7*24*time.Hour, now)
	if amt != 100 {
		t.Fatalf("7 天窗口内应只统计 100，实际 %v", amt)
	}
	if !earliest.Equal(c.Packages[0].ExpiresAt) {
		t.Fatalf("最早到期时间错误: %v", earliest)
	}
	if amt, _ := c.expiringWithin(60*24*time.Hour, now); amt != 600 {
		t.Fatalf("60 天窗口应为 600，实际 %v", amt)
	}
	// 未知数据不影响判断
	unknown := accountCredits{Known: false, Packages: c.Packages}
	if amt, _ := unknown.expiringWithin(60*24*time.Hour, now); amt != 0 {
		t.Fatalf("Known=false 时不应返回任何额度，实际 %v", amt)
	}
}

func TestComputeSoonestIgnoresUnknownAndExpired(t *testing.T) {
	now := time.Now()
	c := accountCredits{Known: true, Packages: []creditPackage{
		{Name: "已过期", Remain: 999, ExpiresAt: now.Add(-time.Hour), ExpiryKnown: true},
		{Name: "10天", Remain: 20, ExpiresAt: now.Add(10 * 24 * time.Hour), ExpiryKnown: true},
		{Name: "3天", Remain: 30, ExpiresAt: now.Add(3 * 24 * time.Hour), ExpiryKnown: true},
		{Name: "未知", Remain: 40},
		{Name: "零", Remain: 0, ExpiresAt: now.Add(time.Hour), ExpiryKnown: true},
	}}
	c.computeSoonest(now)
	if c.SoonestAmt != 30 {
		t.Fatalf("应选中 3 天后到期的那笔，实际 %v", c.SoonestAmt)
	}
	if !c.SoonestAt.Equal(c.Packages[2].ExpiresAt) {
		t.Fatalf("最早到期时间错误: %v", c.SoonestAt)
	}
	if !c.AnyUnknown {
		t.Fatal("存在有剩余但到期未知的包时应置 AnyUnknown")
	}
}

func TestExpiringCandidateRespectsWindowAndEligibility(t *testing.T) {
	quietCredits(t)
	now := time.Now()
	t.Cleanup(func() { setCreditConfig(true, false, 7) })

	setCreditConfig(true, true, 7)
	soon := mkAccount("soon.json")
	soon.credits = accountCredits{Known: true, SoonestAt: now.Add(2 * 24 * time.Hour), SoonestAmt: 100}
	later := mkAccount("later.json")
	later.credits = accountCredits{Known: true, SoonestAt: now.Add(20 * 24 * time.Hour), SoonestAmt: 100}
	none := mkAccount("none.json")

	acc, at := expiringCandidateLocked([]*Account{later, soon, none}, func(*Account) bool { return true }, now)
	if acc == nil || acc.Path != "soon.json" || !at.Equal(soon.credits.SoonestAt) {
		t.Fatalf("应选最早作废且在窗口内的账号，实际 %v", acc)
	}

	// 窗口内没有 → 不选
	setCreditConfig(true, true, 1)
	if acc, _ := expiringCandidateLocked([]*Account{soon}, func(*Account) bool { return true }, now); acc != nil {
		t.Fatal("超出窗口不应被选中")
	}
	// 关闭 → 不选
	setCreditConfig(true, false, 7)
	if acc, _ := expiringCandidateLocked([]*Account{soon}, func(*Account) bool { return true }, now); acc != nil {
		t.Fatal("未启用时不应参与调度")
	}
	// 资格不符 → 不选
	setCreditConfig(true, true, 7)
	if acc, _ := expiringCandidateLocked([]*Account{soon}, func(*Account) bool { return false }, now); acc != nil {
		t.Fatal("未通过资格检查的账号不应被选中")
	}
}

func TestCreditConfigHotReload(t *testing.T) {
	t.Cleanup(func() { setCreditConfig(true, false, 7) })
	setCreditConfig(true, false, 7)
	if s := currentCreditSettings(); s.ExpiringNext || s.Window != 7*24*time.Hour {
		t.Fatalf("初始配置错误: %+v", s)
	}
	setCreditConfig(true, true, 3)
	s := currentCreditSettings()
	if !s.ExpiringNext || s.Window != 3*24*time.Hour {
		t.Fatalf("热更新未生效: %+v", s)
	}
	// 非法窗口回落到默认
	setCreditConfig(true, true, 0)
	if s := currentCreditSettings(); s.Window != defaultCreditWindow*24*time.Hour {
		t.Fatalf("非法窗口应回落默认: %v", s.Window)
	}
	snap := creditSnapshot()
	for _, k := range []string{"fetch", "expiringFirst", "windowDays", "expiringPicked"} {
		if _, ok := snap[k]; !ok {
			t.Fatalf("快照缺少字段 %s", k)
		}
	}
}

// 新绑定走临期优先；已有亲和绑定的会话不受影响。
func TestPickerExpiringFirstInteractionWithAffinity(t *testing.T) {
	quietCredits(t)
	now := time.Now()
	t.Cleanup(func() {
		setCreditConfig(true, false, 7)
		enableAffinity(t, false, affinityModeHybrid)
	})
	enableAffinity(t, false, affinityModeHybrid) // 先关亲和，隔离变量
	setCreditConfig(true, true, 7)

	soon := mkAccount("soon.json")
	soon.credits = accountCredits{Known: true, SoonestAt: now.Add(24 * time.Hour), SoonestAmt: 500}
	other := mkAccount("other.json")
	withAccounts(t, other, soon) // rrIndex 先指向 other

	acc, _, err := nextAccountForModelPrefer("m", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Path != "soon.json" {
		t.Fatalf("无绑定时应优先临期账号，实际 %s", acc.Path)
	}

	// 有绑定到 other 时，即使 soon 更临期也必须保住绑定（缓存优先）
	acc2, _, err := nextAccountForModelPrefer("m", nil, "other.json")
	if err != nil {
		t.Fatal(err)
	}
	if acc2.Path != "other.json" {
		t.Fatalf("已有亲和绑定时不应为临期积分搬走会话，实际 %s", acc2.Path)
	}
}

// 抓取解析：用假上游验证 envelope / 字段映射 / 失败回退。
func TestFetchAccountCreditsParsing(t *testing.T) {
	quietCredits(t)
	now := time.Now()
	deduction := now.AddDate(0, 0, 21).UnixMilli()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, creditPackagesFreePath) {
			_, _ = io.WriteString(w, `{"code":0,"data":{"Accounts":[]}}`)
			return
		}
		payload := map[string]any{"code": 0, "data": map[string]any{"Accounts": []any{
			map[string]any{
				"AccountId": "a1", "PackageName": "拉新权益包",
				"CycleCapacityRemainPrecise": 1500,
				"DeductionEndTime":           float64(deduction),
			},
			map[string]any{
				"AccountId": "a2", "PackageName": "体验版",
				"CycleCapacityRemainPrecise": 275,
				"DeductionEndTime":           nil,
				"CycleEndTime":               now.AddDate(0, 0, 10).Format("2006-01-02 15:04:05"),
			},
		}}}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	oldClient := cfg.HttpClient
	cfg.HttpClient = srv.Client()
	defer func() { cfg.HttpClient = oldClient }()

	prof := &upstreamProfile{Base: srv.URL}
	info, err := fetchAccountCredits(t.Context(), prof, func(*http.Request) {}, []string{"pkg1"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Known || len(info.Packages) != 2 {
		t.Fatalf("解析失败: %+v", info)
	}
	if info.SoonestAmt != 1500 || !info.SoonestAt.Equal(time.Unix(deduction/1000, 0)) {
		t.Fatalf("最早作废计算错误: %v %v", info.SoonestAt, info.SoonestAmt)
	}
	if !info.AnyUnknown {
		t.Fatal("体验版没有 DeductionEndTime，应标记为到期未知")
	}
}

func TestFetchAccountCreditsEmptyCodes(t *testing.T) {
	info, err := fetchAccountCredits(t.Context(), &upstreamProfile{Base: "http://127.0.0.1:1"}, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if info.Known || info.Err == "" {
		t.Fatalf("没有套餐编码时应明确标记未知: %+v", info)
	}
}

// 并发：调度器读 credits 与抓取协程写 credits 不能有数据竞争。
func TestCreditsConcurrentAccess(t *testing.T) {
	quietCredits(t)
	now := time.Now()
	t.Cleanup(func() { setCreditConfig(true, false, 7) })
	setCreditConfig(true, true, 7)

	a := mkAccount("c1.json")
	b := mkAccount("c2.json")
	withAccounts(t, a, b)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // 模拟额度刷新协程
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			accountMu.Lock()
			a.credits = accountCredits{Known: true, SoonestAt: now.Add(time.Hour), SoonestAmt: 10}
			b.credits = accountCredits{Known: true, SoonestAt: now.Add(2 * time.Hour), SoonestAmt: 20}
			accountMu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				acc, kind, err := nextAccountForModelPrefer("m", nil, "")
				if err != nil {
					t.Error(err)
					return
				}
				_ = acc
				_ = kind
				_ = creditSnapshot()
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
}

func BenchmarkExpiringCandidate(b *testing.B) {
	now := time.Now()
	accs := make([]*Account, 0, 32)
	for i := 0; i < 32; i++ {
		a := mkAccount("a.json")
		a.credits = accountCredits{Known: true, SoonestAt: now.Add(time.Duration(i) * time.Hour), SoonestAmt: 100}
		accs = append(accs, a)
	}
	setCreditConfig(true, true, 7)
	defer setCreditConfig(true, false, 7)
	eligible := func(*Account) bool { return true }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if acc, _ := expiringCandidateLocked(accs, eligible, now); acc == nil {
			b.Fatal("应命中")
		}
	}
}
