package main

// 多 API Key 与积分配额的离线测试。全部使用伪造 usage，不消耗真实积分。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func resetKeyWorld(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	keyDataDir = dir
	apiKeys.mu.Lock()
	apiKeys.byID = map[string]*apiKey{}
	apiKeys.byPrefix = map[string]*apiKey{}
	apiKeys.path = filepath.Join(dir, "apikeys.json")
	apiKeys.mu.Unlock()
	apiKeys.degraded.Store(false)
	apiKeys.requireAuth = false
	authSwitchMu.Lock()
	authSwitchOn = false
	authSwitchMu.Unlock()
	registerRuntimeKeys()
	settledMu.Lock()
	settledIDs = map[string]struct{}{}
	settledOrder = nil
	settledMu.Unlock()
	ledger.mu.Lock()
	if ledger.file != nil {
		_ = ledger.file.Close()
		ledger.file = nil
	}
	ledger.writable = false
	ledger.pending = 0
	ledger.mu.Unlock()
	ledger.seq.Store(0)
	t.Cleanup(func() {
		// 其余测试假定"没有受管 Key"，必须恢复全局状态。
		apiKeys.mu.Lock()
		apiKeys.byID = map[string]*apiKey{}
		apiKeys.byPrefix = map[string]*apiKey{}
		apiKeys.mu.Unlock()
		apiKeys.degraded.Store(false)
		apiKeys.requireAuth = false
		authSwitchMu.Lock()
		authSwitchOn = false
		authSwitchMu.Unlock()
		keyDataDir = "."
		ledger.mu.Lock()
		if ledger.file != nil {
			_ = ledger.file.Close()
			ledger.file = nil
		}
		ledger.writable = false
		ledger.mu.Unlock()
	})
	return dir
}

func mustCreateKey(t *testing.T, opts keyCreateOptions) (string, *apiKey) {
	t.Helper()
	plain, k, err := keyCreate(opts)
	if err != nil {
		t.Fatalf("创建密钥失败: %v", err)
	}
	return plain, k
}

func TestKeyCreateAuthenticateAndRotate(t *testing.T) {
	resetKeyWorld(t)
	plain, k := mustCreateKey(t, keyCreateOptions{Name: "笔记本"})
	if !strings.HasPrefix(plain, apiKeyPrefix) {
		t.Fatalf("密钥前缀错误: %s", plain)
	}
	if got := authenticateAPIKey(plain); got == nil || got.ID != k.ID {
		t.Fatal("正确密钥应通过鉴权")
	}
	if authenticateAPIKey(plain+"x") != nil {
		t.Fatal("错误密钥不得通过")
	}
	if authenticateAPIKey("") != nil {
		t.Fatal("空密钥不得通过")
	}
	// 明文不落盘
	data, err := os.ReadFile(filepath.Join(keyDataDir, "apikeys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), plain) {
		t.Fatal("apikeys.json 不得包含明文密钥")
	}

	newPlain, _, err := keyRotate(k.ID)
	if err != nil {
		t.Fatal(err)
	}
	if authenticateAPIKey(plain) != nil {
		t.Fatal("轮换后旧密钥必须立即失效")
	}
	if authenticateAPIKey(newPlain) == nil {
		t.Fatal("轮换后新密钥应可用")
	}
}

func TestKeyAdmissionStates(t *testing.T) {
	resetKeyWorld(t)
	_, k := mustCreateKey(t, keyCreateOptions{Name: "k"})

	if err, release := keyAdmit(k, "m"); err != nil {
		t.Fatalf("正常状态应放行: %v", err)
	} else {
		release()
	}

	_ = keySetEnabled(k.ID, false)
	if err, _ := keyAdmit(k, "m"); err == nil || err.Code != "key_disabled" {
		t.Fatalf("禁用后应返回 key_disabled，实际 %+v", err)
	}
	_ = keySetEnabled(k.ID, true)

	k.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli()
	if err, _ := keyAdmit(k, "m"); err == nil || err.Code != "key_expired" {
		t.Fatalf("过期后应返回 key_expired，实际 %+v", err)
	}
	k.ExpiresAt = 0

	k.Models = []string{"only-this"}
	if err, _ := keyAdmit(k, "m"); err == nil || err.Code != "model_not_allowed" {
		t.Fatalf("白名单外应拒绝，实际 %+v", err)
	}
	if err, release := keyAdmit(k, "ONLY-THIS"); err != nil {
		t.Fatalf("白名单大小写应归一: %v", err)
	} else {
		release()
	}
}

func TestKeyQuotaLimits(t *testing.T) {
	resetKeyWorld(t)
	_, k := mustCreateKey(t, keyCreateOptions{Name: "q", QuotaTotal: 10, QuotaDaily: 5, QuotaMonthly: 8})

	// 总额度
	setKeyUsageForTest(k, apiKeyUsage{Total: 10, DayKey: keyDayKey(time.Now()), MonthKey: keyMonthKey(time.Now())})
	if err, _ := keyAdmit(k, "m"); err == nil || err.Code != "credit_quota_exhausted" {
		t.Fatalf("总额度用尽应拒绝，实际 %+v", err)
	}
	// 日额度
	setKeyUsageForTest(k, apiKeyUsage{Total: 1, Daily: 5, DayKey: keyDayKey(time.Now()), MonthKey: keyMonthKey(time.Now())})
	if err, _ := keyAdmit(k, "m"); err == nil {
		t.Fatal("日额度用尽应拒绝")
	}
	// 月额度
	setKeyUsageForTest(k, apiKeyUsage{Total: 1, Daily: 1, Monthly: 8, DayKey: keyDayKey(time.Now()), MonthKey: keyMonthKey(time.Now())})
	if err, _ := keyAdmit(k, "m"); err == nil {
		t.Fatal("月额度用尽应拒绝")
	}
	// 全部有余量
	setKeyUsageForTest(k, apiKeyUsage{Total: 1, Daily: 1, Monthly: 1, DayKey: keyDayKey(time.Now()), MonthKey: keyMonthKey(time.Now())})
	if err, release := keyAdmit(k, "m"); err != nil {
		t.Fatalf("有余量应放行: %v", err)
	} else {
		release()
	}
}

func setKeyUsageForTest(k *apiKey, u apiKeyUsage) {
	k.mu.Lock()
	k.usage = u
	k.mu.Unlock()
}

// 跨日/跨月必须按北京时间滚动，且不能依赖服务器时区。
func TestKeyWindowRolloverBeijing(t *testing.T) {
	resetKeyWorld(t)
	_, k := mustCreateKey(t, keyCreateOptions{Name: "w", QuotaDaily: 10, QuotaMonthly: 10})

	// 模拟"昨天"的累计
	yesterday := time.Now().In(keyCST).AddDate(0, 0, -1)
	setKeyUsageForTest(k, apiKeyUsage{
		Daily: 9, Monthly: 9, DayKey: keyDayKey(yesterday), MonthKey: keyMonthKey(time.Now()),
	})
	if err, release := keyAdmit(k, "m"); err != nil {
		t.Fatalf("跨日后日额度应重置: %v", err)
	} else {
		release()
	}
	k.mu.Lock()
	daily := k.usage.Daily
	dayKey := k.usage.DayKey
	k.mu.Unlock()
	if daily != 0 || dayKey != keyDayKey(time.Now()) {
		t.Fatalf("日窗口未按北京时间重置: daily=%v dayKey=%s", daily, dayKey)
	}

	// 上个月的累计
	lastMonth := time.Now().In(keyCST).AddDate(0, -1, 0)
	setKeyUsageForTest(k, apiKeyUsage{
		Monthly: 9, DayKey: keyDayKey(time.Now()), MonthKey: keyMonthKey(lastMonth),
	})
	if err, release := keyAdmit(k, "m"); err != nil {
		t.Fatalf("跨月后月额度应重置: %v", err)
	} else {
		release()
	}
}

func TestKeyConcurrencyLimit(t *testing.T) {
	resetKeyWorld(t)
	_, k := mustCreateKey(t, keyCreateOptions{Name: "c", MaxConcurrent: 2})

	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted, rejected := 0, 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err, release := keyAdmit(k, "m")
			mu.Lock()
			if err != nil {
				rejected++
				mu.Unlock()
				return
			}
			admitted++
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			release()
		}()
	}
	wg.Wait()
	if admitted > 2 {
		t.Fatalf("并发上限 2 被突破：放行 %d 次", admitted)
	}
	if rejected == 0 {
		t.Fatal("应至少有请求被并发限制拒绝")
	}
	k.mu.Lock()
	left := k.inflight
	k.mu.Unlock()
	if left != 0 {
		t.Fatalf("请求结束后并发位应归还，实际残留 %d", left)
	}
}

func TestKeySettleRealCredit(t *testing.T) {
	resetKeyWorld(t)
	if err := ledgerLoad(keyDataDir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "s"})

	if out := keySettle(k, "a1", map[string]any{"credit": 1.5}, 200); out != settleCharged {
		t.Fatalf("有真实 credit 应记 charged，实际 %s", out)
	}
	// 重复结算同一个 attempt 不得二次记账
	if out := keySettle(k, "a1", map[string]any{"credit": 1.5}, 200); out != settleDup {
		t.Fatalf("重复结算应返回 duplicate，实际 %s", out)
	}
	if out := keySettle(k, "a2", map[string]any{"credit": 0}, 200); out != settleZero {
		t.Fatalf("明确 0 应记 zero，实际 %s", out)
	}
	if out := keySettle(k, "a3", map[string]any{}, 200); out != settleUnknown {
		t.Fatalf("缺少字段应记 unknown，实际 %s", out)
	}
	if out := keySettle(k, "a4", map[string]any{"credit": -3}, 200); out != settleUnknown {
		t.Fatalf("负数不得作为合法扣费，实际 %s", out)
	}

	k.mu.Lock()
	u := k.usage
	k.mu.Unlock()
	if u.Total != 1.5 || u.Requests != 4 || u.Unknown != 2 || u.ZeroCost != 1 {
		t.Fatalf("账本累计不正确: %+v", u)
	}
}

func TestKeyLedgerPersistsAcrossReload(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, k := mustCreateKey(t, keyCreateOptions{Name: "p", QuotaTotal: 100})
	keySettle(k, "x1", map[string]any{"credit": 3}, 200)
	keySettle(k, "x2", map[string]any{"credit": 4}, 200)
	ledgerCompact()

	// 模拟重启：清空内存后重新载入
	apiKeys.mu.Lock()
	id := k.ID
	apiKeys.byID = map[string]*apiKey{}
	apiKeys.byPrefix = map[string]*apiKey{}
	apiKeys.mu.Unlock()
	ledger.mu.Lock()
	if ledger.file != nil {
		_ = ledger.file.Close()
		ledger.file = nil
	}
	ledger.writable = false
	ledger.mu.Unlock()

	if err := keyStoreLoad(); err != nil {
		t.Fatal(err)
	}
	registerRuntimeKeys()
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	if authenticateAPIKey(plain) == nil {
		t.Fatal("重启后密钥应仍然有效")
	}
	reloaded := keyByID(id)
	if reloaded == nil {
		t.Fatal("重启后 Key 丢失")
	}
	reloaded.mu.Lock()
	total := reloaded.usage.Total
	reloaded.mu.Unlock()
	if total != 7 {
		t.Fatalf("重启后累计应恢复为 7，实际 %v", total)
	}
}

// 快照损坏时绝不能当作空账本（否则等于免费重置全部额度）。
func TestLedgerCorruptSnapshotRefusesEmpty(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := os.WriteFile(filepath.Join(dir, "keyledger.snapshot.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ledgerLoad(dir); err == nil {
		t.Fatal("快照损坏时必须报错，不能静默初始化空账本")
	}
}

func TestKeyUsageEndpointIsolated(t *testing.T) {
	resetKeyWorld(t)
	plainA, kA := mustCreateKey(t, keyCreateOptions{Name: "A", QuotaTotal: 10})
	keySettle(kA, "u1", map[string]any{"credit": 2}, 200)

	req := httptest.NewRequest(http.MethodGet, "/v1/usage", nil)
	req.Header.Set("Authorization", "Bearer "+plainA)
	req = withKeyIdentity(req, &keyIdentity{ID: kA.ID, Prefix: kA.Prefix})
	rec := httptest.NewRecorder()
	handleKeyUsage(rec, req)
	if rec.Code != 200 {
		t.Fatalf("应有权限查询自身用量，状态码 %d", rec.Code)
	}
	var body struct {
		Usage keyUsageView `json:"usage"`
		ID    string       `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ID != kA.ID || body.Usage.Used != 2 {
		t.Fatalf("返回内容不对: %+v", body)
	}
	if body.Usage.Limit == nil || *body.Usage.Limit != 10 {
		t.Fatal("总额度应返回 10")
	}

	// 无身份 → 401
	rec2 := httptest.NewRecorder()
	handleKeyUsage(rec2, httptest.NewRequest(http.MethodGet, "/v1/usage", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("未鉴权应 401，实际 %d", rec2.Code)
	}
}

func TestLegacyKeyIsUnlimited(t *testing.T) {
	resetKeyWorld(t)
	legacy := keyByID(legacyKeyID)
	if legacy == nil {
		t.Fatal("兼容旧 Key 身份应存在")
	}
	if legacy.limited() || legacy.concurrency() != 0 {
		t.Fatal("兼容旧 Key 不应受额度或并发限制")
	}
	if err, release := keyAdmit(legacy, "any-model"); err != nil {
		t.Fatalf("兼容旧 Key 必须始终放行: %v", err)
	} else {
		release()
	}
}

func TestKeyAdminListHidesSecrets(t *testing.T) {
	resetKeyWorld(t)
	plain, _ := mustCreateKey(t, keyCreateOptions{Name: "secret-check"})
	payload := keyAdminListPayload()
	raw, _ := json.Marshal(payload)
	if strings.Contains(string(raw), plain) {
		t.Fatal("管理列表不得包含完整密钥")
	}
	if !strings.Contains(string(raw), "wbk_") {
		t.Fatal("列表应展示脱敏前缀")
	}
}

func TestKeyDeleteKeepsLedgerHistory(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "d", QuotaTotal: 5})
	keySettle(k, "d1", map[string]any{"credit": 2}, 200)
	id := k.ID
	if err := keyDelete(id); err != nil {
		t.Fatal(err)
	}
	if keyByID(id) != nil {
		t.Fatal("删除后 Key 不应仍在索引里")
	}
	// 账本里的记录仍在，重放不应 panic
	wal, err := os.ReadFile(filepath.Join(dir, "keyledger.wal"))
	if err != nil || len(wal) == 0 {
		t.Fatalf("删除后历史消费记录应保留: %v", err)
	}
	keySettle(nil, "d2", map[string]any{"credit": 1}, 200)
}

func TestKeyModelAllowlistFiltersModels(t *testing.T) {
	resetKeyWorld(t)
	_, k := mustCreateKey(t, keyCreateOptions{Name: "m", Models: []string{"model-b"}})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req = withKeyIdentity(req, &keyIdentity{ID: k.ID})
	allowed := keyAllowedModelFilter(req)
	if !allowed("model-b") || allowed("model-a") {
		t.Fatal("模型过滤必须与白名单一致")
	}
}

// 不同 Key 的调用方身份必须隔离（亲和键的 caller 段来自指纹）。
func TestDifferentKeysGetDifferentCallerFingerprint(t *testing.T) {
	a := apiKeyFingerprint("Bearer wbk_aaaaaaaaaaaaaaaaaaaa")
	b := apiKeyFingerprint("Bearer wbk_bbbbbbbbbbbbbbbbbbbb")
	if a == "" || b == "" || a == b {
		t.Fatalf("不同密钥必须产生不同调用方指纹: %q %q", a, b)
	}
}
// ---------------------------------------------------------------------------
// 基准：准入与鉴权开销
// ---------------------------------------------------------------------------

func BenchmarkKeyAuthenticate(b *testing.B) {
	dir := b.TempDir()
	keyDataDir = dir
	apiKeys.mu.Lock()
	apiKeys.byID = map[string]*apiKey{}
	apiKeys.byPrefix = map[string]*apiKey{}
	apiKeys.path = filepath.Join(dir, "apikeys.json")
	apiKeys.mu.Unlock()
	plain, _, err := keyCreate(keyCreateOptions{Name: "bench"})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if authenticateAPIKey(plain) == nil {
			b.Fatal("应命中")
		}
	}
}

func BenchmarkKeyAdmitLimited(b *testing.B) {
	dir := b.TempDir()
	keyDataDir = dir
	apiKeys.mu.Lock()
	apiKeys.byID = map[string]*apiKey{}
	apiKeys.byPrefix = map[string]*apiKey{}
	apiKeys.path = filepath.Join(dir, "apikeys.json")
	apiKeys.mu.Unlock()
	_, k, err := keyCreate(keyCreateOptions{Name: "bench", QuotaTotal: 1e9, MaxConcurrent: 64})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		admitErr, release := keyAdmit(k, "m")
		if admitErr != nil {
			b.Fatal(admitErr.Code)
		}
		release()
	}
}
// 重置后历史累计仍要能在面板上看到（/v1/usage 的 lifetime 字段）。
func TestUsageViewExposesLifetime(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "life2", QuotaTotal: 100})
	keySettle(k, "lf1", map[string]any{"credit": 12.0}, 200)
	if err := keyResetUsage(k.ID); err != nil {
		t.Fatal(err)
	}
	view := keyUsageSnapshot(k)
	if view.Used != 0 {
		t.Fatalf("准入累计应清零，实际 %v", view.Used)
	}
	if view.Lifetime != 12 {
		t.Fatalf("lifetime 应保留 12，实际 %v", view.Lifetime)
	}
	payload := keyAdminListPayload()
	summary := payload["summary"].(map[string]any)
	if summary["allTimeCredits"].(float64) != 12 {
		t.Fatalf("面板累计消费应取历史累计，实际 %v", summary["allTimeCredits"])
	}
}