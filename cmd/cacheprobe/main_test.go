package main

// cacheprobe 的 SSE 解析测试：这是 P0 修复点，必须证明它能真正读 SSE，
// 并且能区分"字段缺失 / 真实 0 命中 / 解析失败"。

import (
	"strings"
	"testing"
	"time"
)

func TestReadSSEParsesEventsAndUsage(t *testing.T) {
	stream := strings.Join([]string{
		"event: response.created",
		`data: {"type":"response.created","response":{"id":"resp_1"}}`,
		"",
		": keepalive",
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		"",
		// 多行 data：SSE 规范要求用换行拼接成一个 JSON
		"data: {\"type\":\"response.completed\",\"response\":",
		`data: {"usage":{"prompt_tokens":120,"input_tokens_details":{"cached_tokens":64}}}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var res probeResult
	if err := readSSE(strings.NewReader(stream), time.Now(), &res); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if res.events < 3 {
		t.Fatalf("事件数应 >= 3，实际 %d", res.events)
	}
	if res.promptTokens != 120 {
		t.Fatalf("prompt_tokens 应为 120，实际 %d", res.promptTokens)
	}
	if !res.cacheSeen || res.cachedTokens != 64 {
		t.Fatalf("应解析出 cached=64，实际 seen=%v cached=%d", res.cacheSeen, res.cachedTokens)
	}
	if res.cacheSource != "input_tokens_details.cached_tokens" {
		t.Fatalf("缓存来源记录错误: %q", res.cacheSource)
	}
	if !res.ttftSet {
		t.Fatalf("应记录首个内容增量的到达时间（events=%d prompt=%d）", res.events, res.promptTokens)
	}
}

func TestReadSSEChatCompletions(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		"",
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":50,"prompt_cache_hit_tokens":32}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var res probeResult
	if err := readSSE(strings.NewReader(stream), time.Now(), &res); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !res.cacheSeen || res.cachedTokens != 32 || res.cacheSource != "prompt_cache_hit_tokens" {
		t.Fatalf("chat 协议缓存字段解析错误: %+v", res)
	}
}

// 字段存在但为 0（真实零命中）必须与"字段缺失"区分开。
func TestReadSSEDistinguishesZeroFromMissing(t *testing.T) {
	zero := "data: {\"usage\":{\"prompt_tokens\":10,\"prompt_cache_hit_tokens\":0}}\n\n"
	var a probeResult
	if err := readSSE(strings.NewReader(zero), time.Now(), &a); err != nil {
		t.Fatal(err)
	}
	if !a.cacheSeen || a.cachedTokens != 0 {
		t.Fatalf("应识别为\"字段存在且为 0\"，实际 seen=%v cached=%d", a.cacheSeen, a.cachedTokens)
	}

	missing := "data: {\"usage\":{\"prompt_tokens\":10}}\n\n"
	var b probeResult
	if err := readSSE(strings.NewReader(missing), time.Now(), &b); err != nil {
		t.Fatal(err)
	}
	if b.cacheSeen {
		t.Fatal("没有缓存字段时不应标记为已见")
	}
}

// 非 JSON 的 data 必须报错，不能被当成"零命中"静默吞掉。
func TestReadSSERejectsGarbage(t *testing.T) {
	var res probeResult
	err := readSSE(strings.NewReader("data: not-json\n\n"), time.Now(), &res)
	if err == nil {
		t.Fatal("非法事件应返回错误")
	}
	if !strings.Contains(err.Error(), "JSON") {
		t.Fatalf("错误信息应说明 JSON 解析失败，实际: %v", err)
	}
}

// 空流（200 但没有任何事件）必须显式失败。
func TestReadSSEEmptyStream(t *testing.T) {
	var res probeResult
	err := readSSE(strings.NewReader(""), time.Now(), &res)
	if err == nil || !strings.Contains(err.Error(), "没有解析出任何事件") {
		t.Fatalf("空流应返回明确错误，实际: %v", err)
	}
}
