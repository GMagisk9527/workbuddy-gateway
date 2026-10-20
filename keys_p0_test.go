package main

// 上线前安全修复的回归测试：先证明问题存在，再验证修复。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 构造真实的中间件链（与 newAPIHandler 完全一致），用于鉴权集成测试。
func testAPIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// 与真实 handler 一致：解析出 model 后再校验白名单。
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		if !keyModelAllowed(w, r, body.Model) {
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/v1/usage", handleKeyUsage)
	mux.HandleFunc("/health", handleHealth)
	return requestAuditMiddleware(corsMiddleware(authMiddleware(keyAdmissionMiddleware(mux))))
}

func withEmptyLegacyKey(t *testing.T) {
	t.Helper()
	old := currentAPIKey()
	if err := applyGatewayConfig(gatewayFileConfig{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: old != "", APIKey: old})
	})
}

// P0-1：把所有 Key 禁用后，匿名请求绝不能被当成"未配置鉴权"放行。
func TestAuthStaysClosedWhenAllKeysDisabled(t *testing.T) {
	resetKeyWorld(t)
	withEmptyLegacyKey(t)

	_, k := mustCreateKey(t, keyCreateOptions{Name: "only-one"})
	if err := keySetEnabled(k.ID, false); err != nil {
		t.Fatal(err)
	}

	h := testAPIHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("全部 Key 禁用后匿名请求必须 401，实际 %d", rec.Code)
	}
}

// P0-1：删除最后一把 Key 也不能退回开放模式。
func TestAuthStaysClosedAfterDeletingLastKey(t *testing.T) {
	resetKeyWorld(t)
	withEmptyLegacyKey(t)

	_, k := mustCreateKey(t, keyCreateOptions{Name: "temp"})
	if err := keyDelete(k.ID); err != nil {
		t.Fatal(err)
	}

	h := testAPIHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("删除最后一把 Key 后匿名请求必须 401，实际 %d", rec.Code)
	}
}

// P0-1：账本/密钥表异常时也必须拒绝，不能 fail-open。
func TestAuthFailsClosedWhenStoreDegraded(t *testing.T) {
	resetKeyWorld(t)
	withEmptyLegacyKey(t)

	apiKeys.degraded.Store(true)
	t.Cleanup(func() { apiKeys.degraded.Store(false) })

	h := testAPIHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("存储降级时匿名请求必须 401，实际 %d", rec.Code)
	}
}

// 明确开放（从未创建过任何 Key 且未配置旧 Key）仍需保持兼容。
func TestAuthOpenModeStillWorksWhenNothingConfigured(t *testing.T) {
	resetKeyWorld(t)
	withEmptyLegacyKey(t)

	h := testAPIHandler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("从未配置任何 Key 时应保持开放，实际 %d", rec.Code)
	}
}

// P0-2：模型未知时准入层不得判白名单，否则白名单 Key 全被 403。
func TestAdmitSkipsModelCheckWhenUnknown(t *testing.T) {
	resetKeyWorld(t)
	_, k := mustCreateKey(t, keyCreateOptions{Name: "wl", Models: []string{"only-model"}})
	if err, release := keyAdmit(k, ""); err != nil {
		t.Fatalf("模型未知时准入不应判白名单，实际 %+v", err)
	} else {
		release()
	}
	// 模型已知时仍要判
	if err, _ := keyAdmit(k, "other-model"); err == nil || err.Code != "model_not_allowed" {
		t.Fatalf("已知模型不匹配应拒绝，实际 %+v", err)
	}
}

// 白名单 Key 通过真实 HTTP 链：模型合法必须放行，不合法必须 403。
func TestWhitelistKeyPassesThroughHTTPChain(t *testing.T) {
	resetKeyWorld(t)
	withEmptyLegacyKey(t)
	plain, _ := mustCreateKey(t, keyCreateOptions{Name: "wl", Models: []string{"model-a"}})

	h := testAPIHandler()
	do := func(model string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`"}`))
		req.Header.Set("Authorization", "Bearer "+plain)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do("model-a"); code == http.StatusForbidden {
		t.Fatalf("白名单内模型被误拦：%d", code)
	}
	if code := do("model-b"); code != http.StatusForbidden {
		t.Fatalf("白名单外模型应 403，实际 %d", code)
	}
}

// P0-3：WAL 存的是增量，重放后 Token 不得翻倍。
func TestWalletReplayDoesNotDoubleCountTokens(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "w"})
	keySettle(k, "a1", map[string]any{"credit": 2.0, "prompt_tokens": 100.0, "completion_tokens": 10.0}, 200)
	keySettle(k, "a2", map[string]any{"credit": 3.0, "prompt_tokens": 100.0, "completion_tokens": 10.0}, 200)

	reloadKeysAndLedger(t, dir)
	got := keyByID(k.ID)
	got.mu.Lock()
	u := got.usage
	got.mu.Unlock()
	if u.Total != 5 {
		t.Fatalf("积分重放应精确为 5，实际 %v", u.Total)
	}
	if u.Input != 200 || u.Output != 20 {
		t.Fatalf("Token 重放应按增量累加为 200/20，实际 %d/%d", u.Input, u.Output)
	}
	if u.Requests != 2 {
		t.Fatalf("请求次数应重放为 2，实际 %d", u.Requests)
	}
}

// 未知费用与零费用次数必须能重放。
func TestWalletReplayUnknownAndZero(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "u"})
	keySettle(k, "z1", map[string]any{"credit": 0.0}, 200)
	keySettle(k, "u1", map[string]any{}, 200)
	keySettle(k, "u2", map[string]any{}, 200)

	reloadKeysAndLedger(t, dir)
	got := keyByID(k.ID)
	got.mu.Lock()
	u := got.usage
	got.mu.Unlock()
	if u.ZeroCost != 1 || u.Unknown != 2 || u.Requests != 3 {
		t.Fatalf("零费用/未知次数重放不正确: %+v", u)
	}
}

// 跨日重放不能把昨天的消费算进今天的日额度。
func TestWalletReplayKeepsDayAttribution(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "d", QuotaDaily: 10})

	yesterday := time.Now().In(keyCST).AddDate(0, 0, -1)
	writeRawWAL(t, dir, ledgerEntry{
		Seq: 1, Key: k.ID, Attempt: "old", Credit: 9, Known: true, Kind: "settle",
		At: yesterday.UnixMilli(), DayKey: keyDayKey(yesterday), MonthKey: keyMonthKey(yesterday),
	})
	reloadKeysAndLedger(t, dir)
	got := keyByID(k.ID)
	got.mu.Lock()
	daily := got.usage.Daily
	total := got.usage.Total
	got.mu.Unlock()
	if total != 9 {
		t.Fatalf("总额应恢复为 9，实际 %v", total)
	}
	if daily != 0 {
		t.Fatalf("昨天的消费不能计入今日额度，实际 %v", daily)
	}
	if err, release := keyAdmit(got, ""); err != nil {
		t.Fatalf("今日额度应仍可用，实际 %+v", err)
	} else {
		release()
	}
}

// WAL 中段损坏必须拒绝启动，不能静默少计。
func TestWalletCorruptMiddleLineFailsClosed(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "c"})
	writeRawWAL(t, dir, ledgerEntry{Seq: 1, Key: k.ID, Attempt: "ok1", Credit: 1, Known: true, Kind: "settle"})
	appendRaw(t, dir, "{broken json}\n")
	writeRawWALAppend(t, dir, ledgerEntry{Seq: 3, Key: k.ID, Attempt: "ok3", Credit: 1, Known: true, Kind: "settle"})

	reloadOnly := func() error {
		apiKeys.mu.Lock()
		id := k.ID
		apiKeys.byID = map[string]*apiKey{}
		apiKeys.byPrefix = map[string]*apiKey{}
		apiKeys.mu.Unlock()
		_ = id
		ledger.mu.Lock()
		if ledger.file != nil {
			_ = ledger.file.Close()
			ledger.file = nil
		}
		ledger.writable = false
		ledger.mu.Unlock()
		if err := keyStoreLoad(); err != nil {
			return err
		}
		registerRuntimeKeys()
		return ledgerLoad(dir)
	}
	if err := reloadOnly(); err == nil {
		t.Fatal("WAL 中段损坏必须报错，不能静默跳过")
	}
}

// P1：有上游调用但完全没有 usage 时，必须记未知而不是"免费"。
func TestNoUsageSettlesAsUnknown(t *testing.T) {
	resetKeyWorld(t)
	if err := ledgerLoad(keyDataDir); err != nil {
		t.Fatal(err)
	}
	_, k := mustCreateKey(t, keyCreateOptions{Name: "n"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = withKeyIdentity(req, &keyIdentity{ID: k.ID})
	p := &statsAttempt{id: "acc", attemptID: "att-nousage", model: "m", started: time.Now()}
	req = req.WithContext(context.WithValue(req.Context(), statsRequestContextKey{}, p))
	statsFinishAttempt(req, http.StatusOK)

	k.mu.Lock()
	u := k.usage
	k.mu.Unlock()
	if u.Unknown != 1 || u.Requests != 1 {
		t.Fatalf("无 usage 的成功请求应记 1 次未知，实际 %+v", u)
	}
}

// P1：并发管理操作与准入不得有数据竞争（-race 下运行）。
func TestKeyConfigConcurrentAccess(t *testing.T) {
	resetKeyWorld(t)
	_, k := mustCreateKey(t, keyCreateOptions{Name: "race", QuotaTotal: 1000, MaxConcurrent: 4})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = keySetEnabled(k.ID, true)
			_, _ = keyUpdate(k.ID, keyCreateOptions{Name: "race", QuotaTotal: 1000, MaxConcurrent: 4, Models: []string{"m"}})
			time.Sleep(time.Millisecond)
		}
	}()
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if err, release := keyAdmit(k, "m"); err == nil {
					release()
				}
				_ = keyUsageSnapshot(k)
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// ---- 测试辅助 ----

func reloadKeysAndLedger(t *testing.T, dir string) {
	t.Helper()
	ledger.mu.Lock()
	if ledger.file != nil {
		_ = ledger.file.Close()
		ledger.file = nil
	}
	ledger.writable = false
	ledger.mu.Unlock()
	apiKeys.mu.Lock()
	apiKeys.path = filepath.Join(dir, "apikeys.json")
	apiKeys.byID = map[string]*apiKey{}
	apiKeys.byPrefix = map[string]*apiKey{}
	apiKeys.mu.Unlock()
	if err := keyStoreLoad(); err != nil {
		t.Fatalf("重载密钥表失败: %v", err)
	}
	registerRuntimeKeys()
	if err := ledgerLoad(dir); err != nil {
		t.Fatalf("重放账本失败: %v", err)
	}
}

func writeRawWAL(t *testing.T, dir string, entries ...ledgerEntry) {
	t.Helper()
	path := filepath.Join(dir, ledgerWALName)
	var buf strings.Builder
	for _, e := range entries {
		raw, _ := json.Marshal(e)
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRawWALAppend(t *testing.T, dir string, e ledgerEntry) {
	t.Helper()
	raw, _ := json.Marshal(e)
	f, err := os.OpenFile(filepath.Join(dir, ledgerWALName), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

func appendRaw(t *testing.T, dir, text string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, ledgerWALName), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, _ = f.Write([]byte(text))
}
