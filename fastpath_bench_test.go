package main

// 快路径的基准：同一份输入，分别走原路径（fastPathOff）与新路径（fastPathOn）。
// 用于和改造前的 17.2 µs/事件（服务器 go1.26.9）/ 12.9 µs（参考机）对比。

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

type benchNullWriter struct{ h http.Header }

func (w *benchNullWriter) Header() http.Header         { return w.h }
func (w *benchNullWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *benchNullWriter) WriteHeader(int)             {}
func (w *benchNullWriter) Flush()                      {}

func benchChunk(tok string) string {
	return `{"id":"chatcmpl-9pQ2xLm4","object":"chat.completion.chunk","created":1730000000,"model":"hunyuan-t1-2025","system_fingerprint":"fp_abc123","choices":[{"index":0,"delta":{"content":"` + tok + `"},"finish_reason":"","logprobs":null}],"usage":null}`
}

func benchBody(events int) string {
	var sb strings.Builder
	sb.Grow(events * 240)
	tok := "hello world streaming token "
	for i := 0; i < events; i++ {
		sb.WriteString("data: ")
		sb.WriteString(benchChunk(tok))
		sb.WriteString("\n\n")
	}
	sb.WriteString(`data: {"id":"chatcmpl-9pQ2xLm4","object":"chat.completion.chunk","created":1730000000,"model":"hunyuan-t1-2025","choices":[{"index":0,"delta":{},"finish_reason":"stop","logprobs":null}],"usage":{"prompt_tokens":1200,"completion_tokens":128,"total_tokens":1328}}` + "\n\n")
	sb.WriteString("data: [DONE]\n\n")
	return sb.String()
}

var quietOnce sync.Once

func quiet() { quietOnce.Do(func() { log.SetOutput(io.Discard) }) }

func runStreamBench(b *testing.B, mode fastPathModeT, events int) {
	b.Helper()
	quiet()
	old := fastPathMode.Load()
	fastPathMode.Store(int32(mode))
	defer fastPathMode.Store(old)

	body := benchBody(events)
	acc := &Account{Path: "bench.json", Auth: &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: "t", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	prof := profileForEdition("cn")

	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := &benchNullWriter{h: make(http.Header)}
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
		streamChatResponse(w, req, resp, "hunyuan-t1-2025", 7, acc, prof, time.Now())
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*events), "ns/event")
}

func BenchmarkStreamChatOld(b *testing.B) { runStreamBench(b, fastPathOff, 128) }
func BenchmarkStreamChatNew(b *testing.B) { runStreamBench(b, fastPathOn, 128) }

func BenchmarkCleanOld(b *testing.B) {
	quiet()
	chunk := benchChunk("hello world streaming token ")
	raw := []byte(chunk)
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cleanChunkJSON(chunk)
	}
}

func BenchmarkCleanNew(b *testing.B) {
	quiet()
	chunk := benchChunk("hello world streaming token ")
	raw := []byte(chunk)
	buf := make([]byte, 0, 512)
	var m chunkMeta
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var ok bool
		buf, ok = cleanChunkInto(buf[:0], raw, &m)
		if !ok {
			b.Fatal("declined")
		}
	}
	if len(buf) == 0 {
		b.Fatal("empty")
	}
}

// BenchmarkCleanNewWithUnmarshal：把"新路径省掉的那次 Unmarshal"单列出来，
// 便于直接和 cleanChunkJSON 做同口径比较。
func BenchmarkMapUnmarshal(b *testing.B) {
	quiet()
	raw := []byte(benchChunk("hello world streaming token "))
	b.SetBytes(int64(len(raw)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var x map[string]any
		_ = json.Unmarshal(raw, &x)
	}
}

// BenchmarkStreamResponses 测线上真正在跑的那条出口（/v1/responses）。
func runResponsesBench(b *testing.B, mode fastPathModeT) {
	const events = 128
	quiet()
	old := fastPathMode.Load()
	fastPathMode.Store(int32(mode))
	defer fastPathMode.Store(old)
	var sb strings.Builder
	for i := 0; i < events; i++ {
		sb.WriteString("data: ")
		sb.WriteString(benchChunk("hello world streaming token "))
		sb.WriteString("\n\n")
	}
	sb.WriteString(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":128,"total_tokens":228}}` + "\n\n")
	body := sb.String()
	acc := &Account{Path: "bench.json", Auth: &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: "t", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	prof := profileForEdition("cn")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := &benchNullWriter{h: make(http.Header)}
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
		streamResponsesResponse(w, req, resp, "gpt-5-codex", 7, acc, prof, time.Now())
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*events), "ns/event")
}

func BenchmarkResponsesOld(b *testing.B) { runResponsesBench(b, fastPathOff) }

func BenchmarkResponsesNew(b *testing.B) { runResponsesBench(b, fastPathOn) }
