package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTokenCounterJSONMatchesASCIIAndCountsUnicode(t *testing.T) {
	values := []any{nil, true, false, float64(1e-9), float64(1e9), float64(1e22), "a<>&\"\\\n\u2028", []any{"tool", float64(2), false}, map[string]any{"x": "value", "n": float64(-3)}}
	for _, v := range values {
		var count inputTokenCounter
		count.json(v)
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if count.cjk != 0 || count.other != int64(len(data)) {
			t.Fatalf("JSON count=%+v len=%d value=%#v", count, len(data), v)
		}
	}
	var count inputTokenCounter
	count.json(map[string]any{"说明": "工具"})
	if count.cjk != 4 {
		t.Fatalf("Unicode schema ignored: %+v", count)
	}
}

func TestTokenEstimateTextCompatibilityAndOpaqueReasoning(t *testing.T) {
	for _, text := range []string{"", "abc", "hello world", "你好世界", "中文 mixed\ntext 😀"} {
		body := map[string]any{"system": []any{map[string]any{"type": "text", "text": "system"}, map[string]any{"type": "text", "text": "second"}}, "messages": []any{map[string]any{"role": "user", "content": text}}}
		if got, want := estimateAnthropicInputTokens(body), approxTokens("system\nsecond\n"+text+"\n"); got != want {
			t.Fatalf("text estimate=%d want=%d", got, want)
		}
	}
	body := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "redacted_thinking", "data": strings.Repeat("opaque", 10000)}}}}}
	if estimateAnthropicInputTokens(body) != 1 {
		t.Fatal("opaque reasoning replay counted as model text")
	}
}

func TestTokenEstimateImageHeadersAndNoRemoteFetch(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 512, 512))); err != nil {
		t.Fatal(err)
	}
	block := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(data.Bytes())}}
	if got := estimateImageInputTokens(block); got != int64(math.Ceil(512*512/750.0)) {
		t.Fatalf("dimension estimate=%d", got)
	}
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer peer.Close()
	block["source"] = map[string]any{"type": "url", "url": peer.URL + "/private-image"}
	for _, content := range []any{[]any{block}, []any{map[string]any{"type": "tool_result", "tool_use_id": "synthetic", "content": []any{block}}}} {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": content}}}
		if got := estimateAnthropicInputTokens(body); got != 1601 {
			t.Fatalf("URL fallback=%d", got)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("local count fetched a remote image")
	}
	block["source"] = map[string]any{"type": "base64", "media_type": "image/jpeg", "data": strings.Repeat("A", 1<<20)}
	if estimateImageInputTokens(block) != 1600 {
		t.Fatal("unknown image header did not use fallback")
	}
}

func TestTokenEstimateCalibrationHotReloadAndValidation(t *testing.T) {
	isolatePolicies(t)
	body := map[string]any{"model": "synthetic-model", "messages": []any{map[string]any{"role": "user", "content": "hello world"}}}
	baseline := estimateAnthropicInputTokens(body)
	cfg, err := parseRuntimeConfig([]byte(`{"tokenEstimation":{"modelMultipliers":{"SYNTHETIC-MODEL":1.5}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := applyRuntimeConfig(cfg, true, ""); err != nil {
		t.Fatal(err)
	}
	if got := estimateAnthropicInputTokens(body); got != int(math.Ceil(float64(baseline)*1.5)) {
		t.Fatalf("model calibration=%d base=%d", got, baseline)
	}
	cfg.TokenEstimation.ModelMultipliers["SYNTHETIC-MODEL"] = 4
	if got := estimateAnthropicInputTokens(body); got != int(math.Ceil(float64(baseline)*1.5)) {
		t.Fatal("configuration map remained mutable")
	}
	for _, raw := range []string{`{"tokenEstimation":{"modelMultipliers":{"x":0}}}`, `{"tokenEstimation":{"modelMultipliers":{"x":4.1}}}`, `{"tokenEstimation":{"modelMultipliers":{"x":1,"X":2}}}`} {
		if _, err := parseRuntimeConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid calibration accepted: %s", raw)
		}
	}
	tooMany := map[string]float64{}
	for i := 0; i < 129; i++ {
		tooMany[fmt.Sprintf("m%d", i)] = 1
	}
	if _, err := normalizeTokenEstimation(tokenEstimationFileConfig{ModelMultipliers: tooMany}); err == nil {
		t.Fatal("unbounded calibration accepted")
	}
	if err := applyRuntimeConfig(runtimeFileConfig{}, true, ""); err != nil {
		t.Fatal(err)
	}
	if estimateAnthropicInputTokens(body) != baseline {
		t.Fatal("removing calibration did not reset it")
	}
}

func TestCountTokensHTTPIsLocalAndCountsImages(t *testing.T) {
	isolatePolicies(t)
	old := cfg.HttpClient
	defer func() { cfg.HttpClient = old }()
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("count_tokens called an upstream")
		return nil, io.EOF
	})}
	body := `{"model":"synthetic","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://synthetic.invalid/image.png"}}]}]}`
	w := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("count rejected: %d %s", w.Code, w.Body.String())
	}
	var value struct {
		Input int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil || value.Input < 1600 {
		t.Fatalf("count response=%s err=%v", w.Body.String(), err)
	}
}

var countTokenSink int

func BenchmarkCountTokensLongHistory(b *testing.B) {
	text := strings.Repeat("synthetic text 中文 ", 200000)
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": text}}}
	b.Run("previous-join", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			var joined strings.Builder
			joined.WriteString(text)
			joined.WriteByte('\n')
			countTokenSink = approxTokens(joined.String())
		}
	})
	b.Run("stream-count", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			countTokenSink = estimateAnthropicInputTokens(body)
		}
	})
}
