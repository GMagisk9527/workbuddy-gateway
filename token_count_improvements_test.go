package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenEstimateIncludesThinkingAndNestedImages(t *testing.T) {
	text := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": strings.Repeat("reason ", 200)}}}}}
	if got := estimateAnthropicInputTokens(text); got < 200 {
		t.Fatalf("thinking omitted: %d", got)
	}
	image := map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://synthetic.invalid/image.png"}}
	for _, content := range []any{[]any{image}, []any{map[string]any{"type": "tool_result", "tool_use_id": "synthetic", "content": []any{image}}}} {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": content}}}
		if got := estimateAnthropicInputTokens(body); got < 100 {
			t.Fatalf("image omitted: %d", got)
		}
	}
}

func TestTokenEstimateDoesNotCopyLongText(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("synthetic text ", 300000)}}}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < 3; i++ {
		if estimateAnthropicInputTokens(body) <= 0 {
			t.Fatal("empty estimate")
		}
	}
	runtime.ReadMemStats(&after)
	if used := after.TotalAlloc - before.TotalAlloc; used > 1<<20 {
		t.Fatalf("counting copied long history: allocated %d bytes", used)
	}
}

func TestTokenEstimateAcceptsBoundedModelCalibration(t *testing.T) {
	if _, err := parseRuntimeConfig([]byte(`{"tokenEstimation":{"modelMultipliers":{"synthetic-model":1.25}}}`)); err != nil {
		t.Fatal(err)
	}
}

type retryPoolCloseObserver struct {
	calls  atomic.Int32
	closes atomic.Int32
}

func (p *retryPoolCloseObserver) RoundTrip(r *http.Request) (*http.Response, error) {
	if p.calls.Add(1) == 1 {
		return nil, io.EOF
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}
func (p *retryPoolCloseObserver) CloseIdleConnections() { p.closes.Add(1) }
func TestSafeRetryDoesNotFlushSharedIdlePool(t *testing.T) {
	isolatePolicies(t)
	prior := cfg.HttpClient
	defer func() { cfg.HttpClient = prior }()
	oldBackoff := upstreamRetryBackoff
	upstreamRetryBackoff = time.Millisecond
	defer func() { upstreamRetryBackoff = oldBackoff }()
	upstreamConfigMu.Lock()
	upstreamTransientRetries = 2
	upstreamConfigMu.Unlock()
	transport := &retryPoolCloseObserver{}
	cfg.HttpClient = &http.Client{Transport: transport}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	resp, cancel, err := doUpstreamRequest(r, mkAccount("synthetic-pool.json"), &profileCN, []byte(`{"model":"synthetic"}`), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer resp.Body.Close()
	if transport.calls.Load() != 2 {
		t.Fatal("safe connect retry did not happen")
	}
	if transport.closes.Load() != 0 {
		t.Fatalf("safe retry flushed shared pool %d times", transport.closes.Load())
	}
}
