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

func TestAffinityTTLHotUpdate(t *testing.T) {
	silenceLogs(t)
	enableAffinity(t, true, affinityModeSession)
	key := "b" + strings.Repeat("0", 31)
	affinityRemember(key, "acc.json")
	if acc := affinityLookup(key); acc != "acc.json" {
		t.Fatal("前提：应命中")
	}
	// 热更新 TTL 到 10ms：既有条目按新的 TTL 判定
	setAffinityConfig(true, affinityModeSession, 0, 4096, 65536)
	affinityStoreMu.RLock()
	affinityStore.mu.Lock()
	affinityStore.ttl = 10 * time.Millisecond
	affinityStore.mu.Unlock()
	affinityStoreMu.RUnlock()

	time.Sleep(30 * time.Millisecond)
	if acc := affinityLookup(key); acc != "" {
		t.Fatalf("TTL 缩短后应立即过期，实际 %q", acc)
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
		"prompt_tokens":              float64(100),
		"completion_tokens":          float64(10),
		"cache_read_input_tokens":    float64(30),
		"cache_creation_input_tokens": float64(7),
	}}
	u := s.anthropicUsage()
	if got := u["cache_read_input_tokens"]; got != 30 {
		t.Fatalf("cache_read 应为 30，实际 %v", got)
	}
	// Anthropic 语义：input_tokens 不含缓存读取
	if got := u["input_tokens"]; got != 70 {
		t.Fatalf("input_tokens 应为 100-30=70，实际 %v", got)
	}
	if got := u["cache_creation_input_tokens"]; got != 7 {
		t.Fatalf("上游给出写入量时应透传 7，实际 %v", got)
	}

	// 上游没给写入量时必须是 0，不能编造
	s2 := &anthropicStreamState{usage: map[string]any{"prompt_tokens": float64(10)}}
	if got := s2.anthropicUsage()["cache_creation_input_tokens"]; got != 0 {
		t.Fatalf("无写入量时应为 0，实际 %v", got)
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
