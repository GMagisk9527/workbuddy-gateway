package main

// 两个已提交改动的加固回归测试：
//   - SSE 快路径：快慢混用、转义、reasoning 语义、下游写失败
//   - 亲和缓存：清空、TTL 热更新、LRU 缩容、前缀摘要稳定性、并发热加载
//   - 缓存 usage：跨协议归一化

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Responses：快慢混用
// ---------------------------------------------------------------------------

// 带 Unicode 转义键的分片：快路径按字节比 key 会漏字段，必须回退到慢路径。
func respChunkEscapedKey(text string) string {
	return `data: {"choices":[{"index":0,"delta":{"\u0063ontent":"` + text + `"}}]}` + "\n\n"
}

func TestResponsesFastSlowMixedAccumulation(t *testing.T) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(log.Writer())

	tail := respChunk(`{}`, "stop") + "data: [DONE]\n\n"
	cases := map[string]string{
		"快-慢-快": respChunk(`{"content":"A"}`, "") +
			respChunkEscapedKey("B") +
			respChunk(`{"content":"C"}`, "") + tail,
		"慢-快-慢": respChunkEscapedKey("A") +
			respChunk(`{"content":"B"}`, "") +
			respChunkEscapedKey("C") + tail,
		"全部回退": respChunkEscapedKey("A") + respChunkEscapedKey("B") + tail,
		"推理与正文混用": respChunk(`{"reasoning_content":"R1"}`, "") +
			respChunkEscapedKey("B") +
			respChunk(`{"reasoning_content":"R2"}`, "") +
			respChunk(`{"content":"C"}`, "") + tail,
	}
	for name, body := range cases {
		oldOut := captureResponsesStream(t, body, fastPathOff)
		newOut := captureResponsesStream(t, body, fastPathOn)
		compareResponsesStreams(t, name, oldOut, newOut)
	}
}

// 收尾事件必须包含全部输出——直接断言文本，而不只是"两条路径一致"。
func TestResponsesFastSlowMixedFinalTextComplete(t *testing.T) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(log.Writer())

	body := respChunk(`{"content":"AAA"}`, "") +
		respChunkEscapedKey("BBB") +
		respChunk(`{"content":"CCC"}`, "") +
		respChunk(`{}`, "stop") + "data: [DONE]\n\n"

	out := captureResponsesStream(t, body, fastPathOn)
	if !strings.Contains(out, `"text":"AAABBBCCC"`) {
		t.Fatalf("output_text.done / response.completed 缺少完整文本，实际流:\n%s", out)
	}
	if n := strings.Count(out, `"text":"AAABBBCCC"`); n < 2 {
		t.Fatalf("至少 response.output_text.done 与 response.completed 两处都应含完整文本，实际 %d 处", n)
	}
}

// reasoning 与 reasoning_content 的优先级必须与原路径一致。
func TestResponsesReasoningFieldSemantics(t *testing.T) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(log.Writer())

	cases := map[string]string{
		// 只有 reasoning：原路径不产出 reasoning 增量（只认 reasoning_content）
		"仅 reasoning": respChunk(`{"reasoning":"ONLY_LEGACY"}`, "") +
			respChunk(`{"content":"x"}`, "") + respChunk(`{}`, "stop") + "data: [DONE]\n\n",
		// 两者同时出现：以 reasoning_content 为准
		"两者并存": respChunk(`{"reasoning":"LEGACY","reasoning_content":"MODERN"}`, "") +
			respChunk(`{}`, "stop") + "data: [DONE]\n\n",
	}
	for name, body := range cases {
		oldOut := captureResponsesStream(t, body, fastPathOff)
		newOut := captureResponsesStream(t, body, fastPathOn)
		compareResponsesStreams(t, name, oldOut, newOut)
	}
	if strings.Contains(captureResponsesStream(t, cases["仅 reasoning"], fastPathOn), "ONLY_LEGACY") {
		t.Fatal("仅 reasoning 字段不应产出 reasoning 增量（与原路径不一致）")
	}
}

// JSON 转义与 Unicode 必须逐片、逐聚合保持一致。
func TestResponsesEscapesAcrossChunks(t *testing.T) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(log.Writer())

	body := respChunk(`{"content":"a\"b\\\\c\nd\te"}`, "") +
		respChunk(`{"content":"中\u00e9\ud83d\ude00"}`, "") +
		respChunk(`{"content":"<&>"}`, "") +
		respChunk(`{}`, "stop") + "data: [DONE]\n\n"

	oldOut := captureResponsesStream(t, body, fastPathOff)
	newOut := captureResponsesStream(t, body, fastPathOn)
	compareResponsesStreams(t, "转义与 Unicode", oldOut, newOut)
}

// 转义键必须让解析器拒绝（否则会静默丢字段）。
func TestResponsesParserRejectsEscapedKeys(t *testing.T) {
	var v respChunkView
	cases := []string{
		`{"choices":[{"index":0,"delta":{"\u0063ontent":"x"}}]}`,
		`{"\u0063hoices":[{"index":0,"delta":{"content":"x"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"a","content":"b"}}]}`,
	}
	for i, c := range cases {
		if parseResponsesChunk([]byte(c), &v) {
			t.Fatalf("case %d 应拒绝（转义键或重复键）: %s", i, c)
		}
	}
	if !parseResponsesChunk([]byte(`{"choices":[{"index":0,"delta":{"content":"x"}}]}`), &v) {
		t.Fatal("普通分片应被接受")
	}
}

// ---------------------------------------------------------------------------
// 下游写失败：必须尽早停止，不再继续读上游
// ---------------------------------------------------------------------------

type failingWriter struct {
	h         http.Header
	writes    int
	failAfter int
}

func (w *failingWriter) Header() http.Header { return w.h }
func (w *failingWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > w.failAfter {
		return 0, errors.New("client gone")
	}
	return len(p), nil
}
func (w *failingWriter) WriteHeader(int) {}
func (w *failingWriter) Flush()          {}

type trackingBody struct {
	r      io.Reader
	closed bool
	reads  int
}

func (b *trackingBody) Read(p []byte) (int, error) { b.reads++; return b.r.Read(p) }
func (b *trackingBody) Close() error               { b.closed = true; return nil }

func TestResponsesStopsOnWriteError(t *testing.T) {
	log.SetOutput(io.Discard)
	defer log.SetOutput(log.Writer())

	var sb strings.Builder
	for i := 0; i < 60; i++ {
		sb.WriteString(respChunk(`{"content":"x"}`, ""))
	}
	sb.WriteString(respChunk(`{}`, "stop"))
	sb.WriteString("data: [DONE]\n\n")

	old := fastPathMode.Load()
	fastPathMode.Store(int32(fastPathOn))
	defer fastPathMode.Store(old)

	acc := &Account{Path: "verify.json", Auth: &StoredAuth{Edition: "cn"}}
	w := &failingWriter{h: make(http.Header), failAfter: 3}
	body := &trackingBody{r: strings.NewReader(sb.String())}
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{Body: body}
	streamResponsesResponse(w, req, resp, "m", 1, acc, profileForEdition("cn"), time.Now())

	if w.writes > 8 {
		t.Fatalf("写失败后仍继续写了 %d 次，说明没有及时停止", w.writes)
	}
	if !body.closed {
		t.Fatal("写失败后应关闭上游响应体")
	}
}

// ---------------------------------------------------------------------------
// 亲和存储：清空 / TTL 热更新 / LRU 缩容顺序
// ---------------------------------------------------------------------------

func TestAffinityDisableClearsBindings(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	key := "a" + strings.Repeat("0", 31)
	affinityRemember(key, "acc.json")
	if affinityStore.stats() == 0 {
		t.Fatal("前提：应已写入绑定")
	}
	setAffinityConfig(false, affinityModeHybrid, 1800, 4096, 65536)
	if got := affinityStore.stats(); got != 0 {
		t.Fatalf("禁用后应真正清空，实际仍有 %d 条", got)
	}
	if acc := affinityLookup(key); acc != "" {
		t.Fatalf("禁用后不应命中旧绑定，实际 %q", acc)
	}
}

// 只用公开的配置入口（setAffinityConfig）验证热加载，不直接改存储内部字段。
func TestAffinityTTLHotUpdate(t *testing.T) {
	silenceLogs(t)
	t.Cleanup(func() { enableAffinity(t, true, affinityModeHybrid) })
	enableAffinity(t, true, affinityModeSession)

	key := "b" + strings.Repeat("0", 31)
	affinityRemember(key, "acc.json")
	if acc := affinityLookup(key); acc != "acc.json" {
		t.Fatal("前提：应命中")
	}

	// 热加载：TTL 从 1800s 改成 1s（配置对象与存储参数必须同时生效）
	setAffinityConfig(true, affinityModeSession, 1, 4096, 65536)
	affinityStoreMu.RLock()
	gotTTL := affinityStore.ttl
	gotCap := affinityStore.cap
	affinityStoreMu.RUnlock()
	if gotTTL != time.Second || gotCap != 4096 {
		t.Fatalf("热加载后存储参数应与配置一致，实际 ttl=%v cap=%d", gotTTL, gotCap)
	}

	time.Sleep(1200 * time.Millisecond)
	if acc := affinityLookup(key); acc != "" {
		t.Fatalf("TTL 缩短到 1s 后旧绑定应过期，实际仍命中 %q", acc)
	}
}

// 启用 -> 禁用 -> 重新启用：绑定必须被真正丢弃，且重新启用后可以正常建立新绑定。
func TestAffinityEnableDisableReenable(t *testing.T) {
	silenceLogs(t)
	t.Cleanup(func() { enableAffinity(t, true, affinityModeHybrid) })
	enableAffinity(t, true, affinityModeSession)

	key := "c" + strings.Repeat("0", 31)
	affinityRemember(key, "acc.json")
	if affinityLookup(key) == "" {
		t.Fatal("前提：启用时应命中")
	}

	setAffinityConfig(false, affinityModeSession, 1800, 4096, 65536)
	if affinityLookup(key) != "" {
		t.Fatal("禁用后不应再命中")
	}
	affinityStoreMu.RLock()
	size := affinityStore.stats()
	affinityStoreMu.RUnlock()
	if size != 0 {
		t.Fatalf("禁用后存储应为空，实际 %d", size)
	}

	// 重新启用：旧绑定不得复活，新绑定可以建立
	setAffinityConfig(true, affinityModeSession, 1800, 4096, 65536)
	if affinityLookup(key) != "" {
		t.Fatal("重新启用后不应复活旧绑定")
	}
	affinityRemember(key, "acc2.json")
	if acc := affinityLookup(key); acc != "acc2.json" {
		t.Fatalf("重新启用后应能建立新绑定，实际 %q", acc)
	}
}

// 容量热更新：缩容后存储容量与条目数都应符合新配置。
func TestAffinityCapacityHotUpdate(t *testing.T) {
	silenceLogs(t)
	t.Cleanup(func() { enableAffinity(t, true, affinityModeHybrid) })
	enableAffinity(t, true, affinityModeSession)

	for i := 0; i < 20; i++ {
		affinityRemember("k"+string(rune('a'+i)), "acc.json")
	}
	setAffinityConfig(true, affinityModeSession, 1800, 5, 65536)

	affinityStoreMu.RLock()
	capNow := affinityStore.cap
	size := affinityStore.stats()
	affinityStoreMu.RUnlock()
	if capNow != 5 {
		t.Fatalf("容量应更新为 5，实际 %d", capNow)
	}
	if size > 5 {
		t.Fatalf("缩容后条目数不应超过 5，实际 %d", size)
	}
}

func TestAffinityResizeKeepsLRUOrder(t *testing.T) {
	silenceLogs(t)
	setAffinityConfig(false, affinityModeHybrid, 0, 0, 0)
	setAffinityConfig(true, affinityModeHybrid, 3600, 3, 65536)
	t.Cleanup(func() {
		setAffinityConfig(false, affinityModeHybrid, 0, 0, 0)
		setAffinityConfig(true, affinityModeHybrid, 1800, 64, 65536)
	})

	affinityRemember("k-a", "A")
	time.Sleep(2 * time.Millisecond)
	affinityRemember("k-b", "B")
	time.Sleep(2 * time.Millisecond)
	affinityRemember("k-c", "C") // c 最新

	affinityStoreReconfigure(2, time.Hour) // 缩容到 2

	affinityStoreMu.RLock()
	defer affinityStoreMu.RUnlock()
	store := affinityStore
	now := time.Now()
	if _, ok := store.get("k-a", now); ok {
		t.Fatal("缩容后应淘汰最久未使用的 k-a")
	}
	if _, ok := store.get("k-b", now); !ok {
		t.Fatal("k-b 应保留")
	}
	if _, ok := store.get("k-c", now); !ok {
		t.Fatal("k-c 应保留")
	}
	if front := store.lru.Front(); front == nil || front.Value.(*affinityEntry).key != "k-c" {
		t.Fatal("缩容不得反转最近使用顺序（k-c 应为最新）")
	}
}

// 热加载与持续请求并发：主要用于 -race 检测共享条目/锁序问题。
func TestAffinityHotReloadConcurrent(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeHybrid)
	body := bigBody("concurrent", 4096)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
			req.Header.Set("Authorization", "Bearer concurrent-"+string(rune('a'+g)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				key := affinityKeyFor(req, "m", body)
				affinityRemember(key, "acc.json")
				_ = affinityLookup(key)
				_ = affinitySnapshot()
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			setAffinityConfig(true, affinityModeHybrid, 60+i, 8+i, 65536)
			setAffinityConfig(true, affinityModeSession, 60, 64, 65536)
			time.Sleep(time.Millisecond)
		}
		close(stop)
	}()
	wg.Wait()
}

// ---------------------------------------------------------------------------
// 前缀摘要：追加消息不改变、改工具/改早期消息会改变
// ---------------------------------------------------------------------------

func buildUpstreamBody(messages []string, tools string) []byte {
	var sb strings.Builder
	sb.WriteString(`{"max_tokens":8192,"messages":[`)
	for i, m := range messages {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"content":"` + m + `","role":"user"}`)
	}
	sb.WriteString(`],"model":"m","stream":true`)
	if tools != "" {
		sb.WriteString(`,"tools":` + tools)
	}
	sb.WriteString(`}`)
	return []byte(sb.String())
}

func TestPromptPrefixDigestStableWhenAppending(t *testing.T) {
	base := []string{
		strings.Repeat("系统提示词与工具说明。", 60),
		strings.Repeat("第一轮用户消息。", 60),
		strings.Repeat("第一轮助手回复。", 60),
	}
	first := buildUpstreamBody(base, `[{"name":"Bash"},{"name":"Read"}]`)
	// 追加一条新消息：请求体总长仍远小于 64 KiB，旧实现会整段变化
	grown := buildUpstreamBody(append(append([]string{}, base...), "第二轮用户消息"), `[{"name":"Bash"},{"name":"Read"}]`)
	if len(first) >= 64<<10 || len(grown) >= 64<<10 {
		t.Fatalf("测试前提：请求体应小于 64 KiB（%d / %d）", len(first), len(grown))
	}
	if promptPrefixDigest(first, 64<<10) != promptPrefixDigest(grown, 64<<10) {
		t.Fatal("仅追加消息时前缀摘要必须不变（否则亲和永远失配）")
	}
}

func TestPromptPrefixDigestChangesWithToolsOrEarlyMessage(t *testing.T) {
	msgs := []string{strings.Repeat("系统提示。", 300), "问题一", "回答一"}
	a := buildUpstreamBody(msgs, `[{"name":"Bash"}]`)
	b := buildUpstreamBody(msgs, `[{"name":"Read"}]`)
	if promptPrefixDigest(a, 64<<10) == promptPrefixDigest(b, 64<<10) {
		t.Fatal("工具定义变化必须改变前缀摘要")
	}
	c := buildUpstreamBody([]string{strings.Repeat("系统提示。", 300) + "（变更）", "问题一", "回答一"}, `[{"name":"Bash"}]`)
	if promptPrefixDigest(a, 64<<10) == promptPrefixDigest(c, 64<<10) {
		t.Fatal("首条消息（可缓存前缀的头部）变化必须改变前缀摘要")
	}
}

func TestPromptPrefixDigestTooShort(t *testing.T) {
	if got := promptPrefixDigest([]byte(`{"messages":[]}`), 64<<10); got != "" {
		t.Fatalf("过短请求体不应生成摘要，得到 %q", got)
	}
}

// ---------------------------------------------------------------------------
// 缓存 usage 跨协议归一化
// ---------------------------------------------------------------------------

func TestToResponsesUsageCacheSources(t *testing.T) {
	cases := []map[string]any{
		{"prompt_tokens": float64(100), "completion_tokens": float64(10), "prompt_tokens_details": map[string]any{"cached_tokens": float64(30)}},
		{"prompt_tokens": float64(100), "completion_tokens": float64(10), "prompt_cache_hit_tokens": float64(30)},
		{"prompt_tokens": float64(100), "completion_tokens": float64(10), "cache_read_input_tokens": float64(30)},
	}
	for i, u := range cases {
		got := toResponsesUsage(u)
		details, _ := got["input_tokens_details"].(map[string]any)
		cached, _ := details["cached_tokens"].(int64)
		if cached != 30 {
			t.Fatalf("case %d: cached 应为 30，实际 %v（%v）", i, cached, got)
		}
		// OpenAI Responses 语义：input_tokens 已包含 cached，不得扣减
		if in, _ := got["input_tokens"].(int64); in != 100 {
			t.Fatalf("case %d: input_tokens 不应扣减缓存，实际 %v", i, in)
		}
	}
}

func TestAnthropicUsageCacheSemantics(t *testing.T) {
	s := &anthropicStreamState{usage: map[string]any{
		"prompt_tokens":               float64(100),
		"completion_tokens":           float64(10),
		"cache_read_input_tokens":     float64(30),
		"cache_creation_input_tokens": float64(7),
	}}
	u := s.anthropicUsage()
	if got := u["cache_read_input_tokens"]; got != 30 {
		t.Fatalf("cache_read 应为 30，实际 %v", got)
	}
	// Anthropic 语义：input_tokens 既不含缓存读取，也不含缓存写入。
	// 普通输入 63 + 读取 30 + 写入 7 = 上游 prompt_tokens 100，四类 Token 守恒。
	if got := u["input_tokens"]; got != 63 {
		t.Fatalf("input_tokens 应为 100-30-7=63，实际 %v", got)
	}
	if got := u["cache_creation_input_tokens"]; got != 7 {
		t.Fatalf("上游给出写入量时应透传 7，实际 %v", got)
	}
	if in, _ := u["input_tokens"].(int); in+30+7 != 100 {
		t.Fatalf("缓存读取/写入与普通输入之和应等于 prompt_tokens，实际 %v", u)
	}

	// 上游没给写入字段时省略，不能伪造 0（与「明确报 0」区分）
	s2 := &anthropicStreamState{usage: map[string]any{"prompt_tokens": float64(10)}}
	if _, exists := s2.anthropicUsage()["cache_creation_input_tokens"]; exists {
		t.Fatal("无写入量时不应输出 cache_creation_input_tokens")
	}
}

// shortDigest 只输出摘要，不能包含原文。
func TestShortDigestHasNoPlaintext(t *testing.T) {
	secret := "这是一段模型输出，包含代码 secret_token_abc123"
	d := shortDigest(secret)
	if len(d) != 12 {
		t.Fatalf("摘要长度应为 12，实际 %d (%q)", len(d), d)
	}
	if strings.Contains(d, "secret") || strings.Contains(secret, d) {
		t.Fatal("摘要不得包含原文片段")
	}
	if shortDigest("") != "-" {
		t.Fatal("空串应返回占位符")
	}
}

// ---------------------------------------------------------------------------
// 结构化前缀摘要（用已解析对象，不再扫描历史）
// ---------------------------------------------------------------------------

func TestDigestFromRequestObjectStableWhenAppending(t *testing.T) {
	base := map[string]any{
		"model": "m",
		"tools": []any{map[string]any{"name": "Bash"}},
		"messages": []any{
			map[string]any{"role": "system", "content": strings.Repeat("系统提示。", 200)},
			map[string]any{"role": "user", "content": "第一轮"},
		},
	}
	d1 := digestFromRequestObject(base, "m", 64<<10)

	// 只在尾部追加：首条消息与 tools 未变 → 摘要必须不变
	grown := map[string]any{
		"model": "m",
		"tools": []any{map[string]any{"name": "Bash"}},
		"messages": []any{
			map[string]any{"role": "system", "content": strings.Repeat("系统提示。", 200)},
			map[string]any{"role": "user", "content": "第一轮"},
			map[string]any{"role": "assistant", "content": "回答"},
			map[string]any{"role": "user", "content": "第二轮"},
		},
	}
	if d2 := digestFromRequestObject(grown, "m", 64<<10); d2 != d1 {
		t.Fatalf("追加消息后摘要不应变化: %q vs %q", d1, d2)
	}

	// 改 tools → 变
	withTools := map[string]any{
		"model":    "m",
		"tools":    []any{map[string]any{"name": "Read"}},
		"messages": base["messages"],
	}
	if digestFromRequestObject(withTools, "m", 64<<10) == d1 {
		t.Fatal("工具变化必须改变摘要")
	}
	// 换模型 → 变
	if digestFromRequestObject(base, "other", 64<<10) == d1 {
		t.Fatal("模型变化必须改变摘要")
	}
	// 改首条消息 → 变
	changed := map[string]any{
		"model": "m",
		"tools": []any{map[string]any{"name": "Bash"}},
		"messages": []any{
			map[string]any{"role": "system", "content": "完全不同的系统提示"},
			map[string]any{"role": "user", "content": "第一轮"},
		},
	}
	if digestFromRequestObject(changed, "m", 64<<10) == d1 {
		t.Fatal("首条消息变化必须改变摘要")
	}
}

func TestAffinityKeyUsesContextDigest(t *testing.T) {
	silenceLogs(t)
	t.Cleanup(func() { enableAffinity(t, true, affinityModeHybrid) })
	enableAffinity(t, true, affinityModePrefix)
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.RemoteAddr = "192.0.2.9:1234"
	req = withPrefixDigest(req, "digest-from-object")

	k1 := affinityKeyFor(req, "m", bigBody("x", 4096))
	k2 := affinityKeyFor(req, "m", bigBody("different-bytes", 4096))
	if k1 == "" {
		t.Fatal("应生成键")
	}
	if k1 != k2 {
		t.Fatal("有结构化摘要时应优先使用它，而不是重新扫描请求体")
	}
}

// ---------------------------------------------------------------------------
// 摘要计算只在需要时发生
// ---------------------------------------------------------------------------

func TestDigestSkippedWhenAffinityDisabled(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, false, affinityModePrefix)
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.RemoteAddr = "192.0.2.9:1"
	got := withAffinityDigestIfEnabled(req, map[string]any{"messages": []any{map[string]any{"role": "system", "content": "x"}}}, "m")
	if d := prefixDigestFromContext(got); d != "" {
		t.Fatalf("关闭亲和时不应计算摘要，实际 %q", d)
	}
}

func TestDigestSkippedInSessionMode(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	got := withAffinityDigestIfEnabled(req, map[string]any{"messages": []any{map[string]any{"role": "system", "content": "x"}}}, "m")
	if d := prefixDigestFromContext(got); d != "" {
		t.Fatalf("session 模式不需要前缀摘要，实际计算了 %q", d)
	}
}

func TestDigestComputedInPrefixMode(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModePrefix)
	req, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	got := withAffinityDigestIfEnabled(req, map[string]any{"messages": []any{map[string]any{"role": "system", "content": "x"}}}, "m")
	if d := prefixDigestFromContext(got); d == "" {
		t.Fatal("prefix 模式应计算摘要")
	}
}

// 相同 system prompt、不同首个用户消息 → 不同键（避免不同会话挤到同一账号）。
func TestDigestDistinguishesConversationsWithSameSystem(t *testing.T) {
	system := strings.Repeat("统一的系统提示词。", 100)
	a := map[string]any{
		"model":    "m",
		"tools":    []any{map[string]any{"name": "Bash"}},
		"messages": []any{map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": "任务甲"}},
	}
	b := map[string]any{
		"model":    "m",
		"tools":    []any{map[string]any{"name": "Bash"}},
		"messages": []any{map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": "任务乙"}},
	}
	if digestFromRequestObject(a, "m", 64<<10) == digestFromRequestObject(b, "m", 64<<10) {
		t.Fatal("相同 system 不同会话必须得到不同摘要")
	}
}

// 绑定计数必须与存储内容一致：重建、换账号、过期、淘汰、清空都要同步。
// （绑定占比只用于观察分布，不参与调度决策——见本文件末尾的说明。）
func TestAffinityCountersOnReconfigure(t *testing.T) {
	silenceLogs(t)
	t.Cleanup(func() { enableAffinity(t, true, affinityModeHybrid) })
	enableAffinity(t, true, affinityModeSession)

	affinityRemember("k1", "A")
	affinityRemember("k2", "A")
	affinityRemember("k3", "A")
	affinityRemember("k4", "B")

	affinityStoreReconfigure(3, time.Hour) // 缩容：保留最近 3 条（k2,k3,k4）

	got := affinityBindingsByAccount()
	total := 0
	for _, v := range got {
		total += v
	}
	affinityStoreMu.RLock()
	size := affinityStore.stats()
	affinityStoreMu.RUnlock()
	if total != size {
		t.Fatalf("计数与条目数不一致: counters=%d store=%d (%v)", total, size, got)
	}
	if got["A"] != 2 || got["B"] != 1 {
		t.Fatalf("缩容后分布应为 A=2 B=1，实际 %v", got)
	}
}

func TestAffinityCountersOnAccountSwitch(t *testing.T) {
	silenceLogs(t)
	t.Cleanup(func() { enableAffinity(t, true, affinityModeHybrid) })
	enableAffinity(t, true, affinityModeSession)

	affinityRemember("same-key", "A")
	if got := affinityBindingsByAccount(); got["A"] != 1 || got["B"] != 0 {
		t.Fatalf("初始应为 A=1，实际 %v", got)
	}
	affinityRemember("same-key", "B") // 同一键换账号
	got := affinityBindingsByAccount()
	if got["A"] != 0 || got["B"] != 1 {
		t.Fatalf("换账号后应为 A=0 B=1，实际 %v", got)
	}
	affinityRemember("same-key", "B") // 重复刷新不得重复计数
	if got := affinityBindingsByAccount(); got["B"] != 1 {
		t.Fatalf("重复刷新不应增加计数，实际 %v", got)
	}
}

func TestAffinityCountersOnExpireEvictDrop(t *testing.T) {
	silenceLogs(t)
	t.Cleanup(func() { enableAffinity(t, true, affinityModeHybrid) })
	enableAffinity(t, true, affinityModeSession)

	// drop
	affinityRemember("k-drop", "A")
	affinityForget("k-drop")
	if got := affinityBindingsByAccount(); got["A"] != 0 {
		t.Fatalf("drop 后计数应归零，实际 %v", got)
	}

	// expire
	affinityRemember("k-exp", "A")
	setAffinityConfig(true, affinityModeSession, 1, 4096, 65536)
	time.Sleep(1200 * time.Millisecond)
	if acc := affinityLookup("k-exp"); acc != "" {
		t.Fatal("前提：应已过期")
	}
	if got := affinityBindingsByAccount(); got["A"] != 0 {
		t.Fatalf("过期后计数应归零，实际 %v", got)
	}

	// evict
	setAffinityConfig(true, affinityModeSession, 1800, 2, 65536)
	for _, k := range []string{"e1", "e2", "e3", "e4"} {
		affinityRemember(k, "A")
	}
	total := 0
	for _, v := range affinityBindingsByAccount() {
		total += v
	}
	if total != 2 {
		t.Fatalf("淘汰后计数总数应为 2，实际 %d", total)
	}

	// clear
	setAffinityConfig(false, affinityModeSession, 1800, 2, 65536)
	if got := affinityBindingsByAccount(); len(got) != 0 {
		t.Fatalf("清空后不应残留计数，实际 %v", got)
	}
}
