package main

// Prompt Cache 亲和：单元测试与基准。
//
// 覆盖：会话/前缀键生成与隔离、TTL、LRU、回退、账号资格约束、统计口径、
// 并发安全，以及"关闭时与旧行为一致"。

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func silenceLogs(t *testing.T) {
	t.Helper()
	old := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(old) })
}

// enableAffinity 重置存储与指标，并按需开启。
func enableAffinity(t *testing.T, enabled bool, mode string) {
	t.Helper()
	setAffinityConfig(false, affinityModeHybrid, 0, 0, 0) // 关闭并清空
	affinityMetrics.reset()
	setAffinityConfig(enabled, mode, 1800, 64, 4096)
}

func withAccounts(t *testing.T, accs ...*Account) {
	t.Helper()
	accountMu.Lock()
	old := accounts
	oldIdx := rrIndex
	accounts = accs
	rrIndex = 0
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = old
		rrIndex = oldIdx
		accountMu.Unlock()
	})
}

func mkAccount(path string) *Account {
	return &Account{
		Path: path,
		Auth: &StoredAuth{
			Edition: "cn",
			Auth:    StoredTokens{AccessToken: "t", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Account: StoredAccount{UID: path},
		},
	}
}

func bigBody(prefix string, n int) []byte {
	var b strings.Builder
	b.WriteString(prefix)
	for b.Len() < n {
		b.WriteString(`{"role":"user","content":"0123456789abcdef"}`)
	}
	return []byte(b.String())
}

// ---------------------------------------------------------------------------
// 键生成
// ---------------------------------------------------------------------------

func TestAffinityKeyCallerIsolation(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	body := bigBody("x", 4096)

	reqA := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reqA.Header.Set("Authorization", "Bearer key-user-a")
	reqA.Header.Set("X-Session-ID", "same-session-id")
	prepareConversationContext(reqA, nil)

	reqB := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reqB.Header.Set("Authorization", "Bearer key-user-b")
	reqB.Header.Set("X-Session-ID", "same-session-id")
	prepareConversationContext(reqB, nil)

	// 不同用户 = 不同调用方指纹 = 不可能共享亲和绑定。
	a, b := affinityKeyFor(reqA, "m", body), affinityKeyFor(reqB, "m", body)
	if len(a) == 0 || len(b) == 0 {
		t.Fatal("应生成亲和键")
	}
	if a == b {
		t.Fatalf("不同调用方得到了相同的亲和键: %s", a)
	}
}

func TestAffinityKeyModelIsolation(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	body := bigBody("x", 4096)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Session-ID", "sid-12345678")
	prepareConversationContext(req, nil)
	if affinityKeyFor(req, "model-a", body) == affinityKeyFor(req, "model-b", body) {
		t.Fatal("不同模型必须隔离")
	}
}

func TestAffinityKeyIgnoresRandomConversationAndUsesPrefix(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeHybrid)
	body := bigBody("prefix-head", 8192)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	prepareConversationContext(req, nil) // 无客户端会话头 → Source=独立生成
	if h := conversationHeadersFor(req); h == nil || h.Source != "独立生成" {
		t.Fatalf("前提不成立: %+v", h)
	}
	a := affinityKeyFor(req, "m", body)
	b := affinityKeyFor(req, "m", body)
	if a == "" || a != b {
		t.Fatalf("无会话标识时应回退到稳定的前缀键: %q vs %q", a, b)
	}
	if affinityMetrics.prefixKey.Load() == 0 {
		t.Fatal("应记录前缀键计数")
	}
}

func TestAffinityKeyPrefixStableWhenAppending(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModePrefix)
	head := bigBody("system+tools", 8192)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	prepareConversationContext(req, nil)

	first := affinityKeyFor(req, "m", head)
	grown := append(append([]byte{}, head...), []byte(`{"role":"assistant","content":"new turn"}`)...)
	second := affinityKeyFor(req, "m", grown)
	if first != second {
		t.Fatal("只在尾部追加时，前缀键必须保持不变（否则亲和永远失效）")
	}
}

func TestAffinityKeyChangesWhenToolsChange(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModePrefix)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	prepareConversationContext(req, nil)

	bodyA := append([]byte(`{"tools":[{"name":"Bash"}],"messages":`), bigBody("x", 4096)...)
	bodyB := append([]byte(`{"tools":[{"name":"Read"}],"messages":`), bigBody("x", 4096)...)
	if string(bodyA) == string(bodyB) {
		t.Fatal("测试数据构造有误")
	}
	if affinityKeyFor(req, "m", bodyA) == affinityKeyFor(req, "m", bodyB) {
		t.Fatal("工具定义变化必须产生不同的前缀键")
	}
}

func TestAffinityKeyTooShortPrefixDeclined(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModePrefix)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	prepareConversationContext(req, nil)
	if got := affinityKeyFor(req, "m", []byte(`{"a":1}`)); got != "" {
		t.Fatalf("过短的前缀不应建立绑定，得到 %q", got)
	}
}

func TestAffinityDisabledProducesNoKey(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, false, affinityModeHybrid)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("X-Session-ID", "sid-12345678")
	prepareConversationContext(req, nil)
	if got := affinityKeyFor(req, "m", bigBody("x", 4096)); got != "" {
		t.Fatalf("关闭时不应产生键，得到 %q", got)
	}
}

// ---------------------------------------------------------------------------
// 存储：TTL / LRU
// ---------------------------------------------------------------------------

func TestAffinityStoreTTL(t *testing.T) {
	s := newAffinityStore(8, 30*time.Millisecond)
	now := time.Now()
	s.put("k", "acc-a", now)
	if acc, ok := s.get("k", now); !ok || acc != "acc-a" {
		t.Fatalf("未命中: %q %v", acc, ok)
	}
	if _, ok := s.get("k", now.Add(100*time.Millisecond)); ok {
		t.Fatal("超过 TTL 仍命中")
	}
	if s.stats() != 0 {
		t.Fatalf("过期项应被清除，当前 %d", s.stats())
	}
}

func TestAffinityStoreLRU(t *testing.T) {
	s := newAffinityStore(2, time.Hour)
	now := time.Now()
	s.put("a", "acc-a", now)
	s.put("b", "acc-b", now)
	s.put("c", "acc-c", now) // 淘汰 a
	if _, ok := s.get("a", now); ok {
		t.Fatal("最久未使用的条目应被淘汰")
	}
	if _, ok := s.get("c", now); !ok {
		t.Fatal("新条目应存在")
	}
	if s.stats() != 2 {
		t.Fatalf("容量应为 2，实际 %d", s.stats())
	}
}

func TestAffinityStoreTouchKeepsEntry(t *testing.T) {
	s := newAffinityStore(2, time.Hour)
	now := time.Now()
	s.put("a", "acc-a", now)
	s.put("b", "acc-b", now)
	if _, ok := s.get("a", now); !ok { // 触碰 a，使其成为最近使用
		t.Fatal("a 应命中")
	}
	s.put("c", "acc-c", now) // 应淘汰 b
	if _, ok := s.get("b", now); ok {
		t.Fatal("b 应为最久未使用并被淘汰")
	}
	if _, ok := s.get("a", now); !ok {
		t.Fatal("a 被触碰后不应被淘汰")
	}
}

// ---------------------------------------------------------------------------
// 调度器：亲和只调整顺序，不放宽资格
// ---------------------------------------------------------------------------

func TestAffinityPickerPrefersBoundAccount(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	a, b := mkAccount("a.json"), mkAccount("b.json")
	withAccounts(t, a, b)

	acc, _, err := nextAccountForModelPrefer("m", nil, "b.json")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Path != "b.json" {
		t.Fatalf("应优先选择绑定账号，实际 %s", acc.Path)
	}
}

func TestAffinityPickerSkipsDisabledAccount(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	a, b := mkAccount("a.json"), mkAccount("b.json")
	b.Disabled = true
	withAccounts(t, a, b)

	acc, _, err := nextAccountForModelPrefer("m", nil, "b.json")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Path != "a.json" {
		t.Fatalf("禁用账号不应被亲和选中，实际 %s", acc.Path)
	}
}

func TestAffinityPickerRespectsCooldown(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	a, b := mkAccount("a.json"), mkAccount("b.json")
	b.CooldownUntil = time.Now().Add(time.Hour)
	withAccounts(t, a, b)

	acc, _, err := nextAccountForModelPrefer("m", nil, "b.json")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Path != "a.json" {
		t.Fatalf("冷却账号不应被亲和选中，实际 %s", acc.Path)
	}
}

func TestAffinityPickerRespectsQuotaExhausted(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	a, b := mkAccount("a.json"), mkAccount("b.json")
	b.QuotaExhausted = true // 无模型状态 → 进入受控探测档，仍可用但顺序靠后
	withAccounts(t, a, b)

	acc, kind, err := nextAccountForModelPrefer("m", nil, "b.json")
	if err != nil {
		t.Fatal(err)
	}
	// 关键约束：正常账号先于耗尽探测账号，亲和不得越过档位优先级。
	if acc.Path != "a.json" || kind != selectionNormal {
		t.Fatalf("配额耗尽账号不应越过正常账号，实际 %s/%s", acc.Path, kind)
	}
}

func TestAffinityPickerRespectsModelAllowlist(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	a, b := mkAccount("a.json"), mkAccount("b.json")
	withAccounts(t, a, b)

	if err := setModelAccountFilter(map[string]modelAccountFileConfig{
		"only-a": {Allowlist: []string{"a.json"}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = setModelAccountFilter(nil) })

	acc, _, err := nextAccountForModelPrefer("only-a", nil, "b.json")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Path != "a.json" {
		t.Fatalf("亲和不得越过模型账号名单，实际 %s", acc.Path)
	}
}

func TestAffinityPickerSkipsAttempted(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	a, b := mkAccount("a.json"), mkAccount("b.json")
	withAccounts(t, a, b)

	attempted := map[*Account]bool{b: true}
	acc, _, err := nextAccountForModelPrefer("m", attempted, "b.json")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Path != "a.json" {
		t.Fatalf("已尝试过的账号不应再被选中，实际 %s", acc.Path)
	}
}

func TestAffinityPickerLegacyWhenNoPreference(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, false, affinityModeHybrid)
	a, b := mkAccount("a.json"), mkAccount("b.json")
	withAccounts(t, a, b)

	got1, kind1, err1 := nextAccountForModel("m", nil)
	if err1 != nil {
		t.Fatal(err1)
	}
	acc2, kind2, err2 := nextAccountForModelPrefer("m", nil, "")
	if err2 != nil {
		t.Fatal(err2)
	}
	// rrIndex 会前进，因此比较"档位"而不是同一账号。
	if kind1 != kind2 || got1 == nil || acc2 == nil {
		t.Fatalf("无偏好时行为应与原调度一致，得到 %s/%s vs %s/%s", got1.Path, kind1, acc2.Path, kind2)
	}
}

// ---------------------------------------------------------------------------
// 统计口径
// ---------------------------------------------------------------------------

func TestStatsCachedValueShapes(t *testing.T) {
	cases := []struct {
		name  string
		usage map[string]any
		want  int64
		src   string
		ok    bool
	}{
		{"prompt_cache_hit_tokens", map[string]any{"prompt_cache_hit_tokens": float64(128)}, 128, "prompt_cache_hit_tokens", true},
		{"cache_read_input_tokens", map[string]any{"cache_read_input_tokens": float64(64)}, 64, "cache_read_input_tokens", true},
		{"cached_tokens", map[string]any{"cached_tokens": float64(32)}, 32, "cached_tokens", true},
		{"prompt_tokens_details", map[string]any{"prompt_tokens_details": map[string]any{"cached_tokens": float64(16)}}, 16, "prompt_tokens_details.cached_tokens", true},
		{"input_tokens_details", map[string]any{"input_tokens_details": map[string]any{"cached_tokens": float64(8)}}, 8, "input_tokens_details.cached_tokens", true},
		{"零值仍算有字段", map[string]any{"prompt_cache_hit_tokens": float64(0)}, 0, "prompt_cache_hit_tokens", true},
		{"字段缺失", map[string]any{"prompt_tokens": float64(10)}, 0, "", false},
		{"空 usage", nil, 0, "", false},
		// 多字段同时存在时只取一个，不能相加
		{"不得相加", map[string]any{
			"prompt_cache_hit_tokens": float64(100),
			"cache_read_input_tokens": float64(100),
			"prompt_tokens_details":   map[string]any{"cached_tokens": float64(100)},
		}, 100, "prompt_cache_hit_tokens", true},
	}
	for _, c := range cases {
		got, src, ok := statsCachedValue(c.usage)
		if got != c.want || src != c.src || ok != c.ok {
			t.Fatalf("%s: 得到 (%d,%q,%v)，期望 (%d,%q,%v)", c.name, got, src, ok, c.want, c.src, c.ok)
		}
	}
}

func TestAffinitySnapshotShape(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeHybrid)
	snap := affinitySnapshot()
	for _, key := range []string{"enabled", "mode", "ttlSeconds", "capacity", "entries", "hit", "miss", "fallback"} {
		if _, ok := snap[key]; !ok {
			t.Fatalf("快照缺少字段 %s", key)
		}
	}
	if snap["enabled"] != true {
		t.Fatal("enabled 应为 true")
	}
}

// ---------------------------------------------------------------------------
// 并发
// ---------------------------------------------------------------------------

func TestAffinityConcurrent(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeHybrid)
	body := bigBody("concurrent", 4096)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			req.Header.Set("Authorization", "Bearer caller-"+string(rune('a'+g)))
			prepareConversationContext(req, nil)
			for i := 0; i < 200; i++ {
				key := affinityKeyFor(req, "m", body)
				affinityRemember(key, "acc.json")
				_ = affinityLookup(key)
				_ = affinitySnapshot()
			}
		}(g)
	}
	wg.Wait()
	if affinityStore.stats() > 64 {
		t.Fatalf("存储超出容量上限: %d", affinityStore.stats())
	}
}

// ---------------------------------------------------------------------------
// 基准
// ---------------------------------------------------------------------------

func BenchmarkAffinityKeyPrefix64K(b *testing.B) {
	quietBench()
	setAffinityConfig(false, affinityModeHybrid, 0, 0, 0)
	setAffinityConfig(true, affinityModePrefix, 1800, 4096, 64<<10)
	body := bigBody("bench", 64<<10)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	prepareConversationContext(req, nil)
	b.SetBytes(64 << 10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if affinityKeyFor(req, "m", body) == "" {
			b.Fatal("空键")
		}
	}
}

func BenchmarkAffinityLookupHit(b *testing.B) {
	quietBench()
	setAffinityConfig(false, affinityModeHybrid, 0, 0, 0)
	setAffinityConfig(true, affinityModeHybrid, 1800, 4096, 64<<10)
	affinityRemember("key-under-bench", "acc.json")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if affinityLookup("key-under-bench") == "" {
			b.Fatal("未命中")
		}
	}
}

func BenchmarkAffinityPickerWithPreference(b *testing.B) {
	quietBench()
	setAffinityConfig(false, affinityModeHybrid, 0, 0, 0)
	setAffinityConfig(true, affinityModeHybrid, 1800, 4096, 64<<10)
	a, c := mkAccount("a.json"), mkAccount("b.json")
	accountMu.Lock()
	old := accounts
	accounts = []*Account{a, c}
	accountMu.Unlock()
	defer func() { accountMu.Lock(); accounts = old; accountMu.Unlock() }()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := nextAccountForModelPrefer("m", nil, "b.json"); err != nil {
			b.Fatal(err)
		}
	}
}

func quietBench() { log.SetOutput(io.Discard) }

var _ = bytes.Equal

// 未启用 API Key 时用来源地址隔离，避免不同用户共享同一条绑定。
func TestAffinityKeyIsolatesByClientIPWhenNoAPIKey(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	body := bigBody("x", 4096)

	reqA := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reqA.RemoteAddr = "203.0.113.10:1234"
	reqA.Header.Set("X-Session-ID", "shared-session")
	prepareConversationContext(reqA, nil)

	reqB := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reqB.RemoteAddr = "198.51.100.20:1234"
	reqB.Header.Set("X-Session-ID", "shared-session")
	prepareConversationContext(reqB, nil)

	a, b := affinityKeyFor(reqA, "m", body), affinityKeyFor(reqB, "m", body)
	if a == "" || b == "" {
		t.Fatal("应生成亲和键")
	}
	if a == b {
		t.Fatal("无 API Key 时，不同来源地址不应共享绑定")
	}
}

// BenchmarkAffinityKeyStructuralBody 测"结构化前缀摘要"的真实开销：
// 请求体是网关实际会发的那种（含 messages 数组与 tools），大小 ~80 KiB。
func BenchmarkAffinityKeyStructuralBody(b *testing.B) {
	quietBench()
	setAffinityConfig(false, affinityModeHybrid, 0, 0, 0)
	setAffinityConfig(true, affinityModePrefix, 1800, 4096, 64<<10)
	msgs := []string{strings.Repeat("系统提示与工具说明。", 800), strings.Repeat("第一轮用户消息。", 200), "第二轮"}
	body := buildUpstreamBody(msgs, `[{"name":"Bash"},{"name":"Read"}]`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	prepareConversationContext(req, nil)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if affinityKeyFor(req, "m", body) == "" {
			b.Fatal("空键")
		}
	}
}