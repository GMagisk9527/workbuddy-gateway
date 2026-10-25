package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func outputTestLease(t *testing.T, limit int64) *outputLease {
	t.Helper()
	g := newOutputAdmission()
	g.sample = func() memorySample { return memorySample{} }
	g.configure(outputFileConfig{MaxRequestBytes: limit, MaxSharedBytes: limit * 2})
	l := g.lease()
	t.Cleanup(l.close)
	return l
}

func TestOutputLeaseSharedLimitReleaseAndEscaping(t *testing.T) {
	g := newOutputAdmission()
	g.sample = func() memorySample {
		return memorySample{runtime: memoryUsage{limit: 48 << 20}, cgroups: []memoryUsage{{limit: 128 << 20}}}
	}
	g.mu.Lock()
	request, shared := g.limitsLocked(g.sample())
	g.mu.Unlock()
	if request != 12<<20 || shared != 24<<20 {
		t.Fatalf("automatic limits wrong: %d/%d", request, shared)
	}
	g.configure(outputFileConfig{MaxRequestBytes: 4096, MaxSharedBytes: 4096})
	a, b := g.lease(), g.lease()
	defer a.close()
	defer b.close()
	if err := a.add(3072); err != nil {
		t.Fatal(err)
	}
	if err := b.add(2048); !errors.Is(err, errOutputBudget) {
		t.Fatal("shared limit bypassed")
	}
	a.close()
	a.close()
	if err := b.add(2048); err != nil {
		t.Fatal("completed request retained shared budget")
	}
	if _, err := marshalBudgetedOutput(b, map[string]any{"text": strings.Repeat("<", 400)}); !errors.Is(err, errOutputBudget) {
		t.Fatal("escaped JSON bypassed preflight")
	}
	b.close()
	if g.used != 0 {
		t.Fatalf("budget leaked: %d", g.used)
	}
}

type outputErrorWriter struct {
	*httptest.ResponseRecorder
	body             *trackingBody
	closeBeforeError bool
}

func (w *outputErrorWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "预算") {
		w.closeBeforeError = w.body.closed
	}
	return w.ResponseRecorder.Write(p)
}
func TestOutputBudgetHTTPPreservesUsageAndStopsUpstream(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		for _, tc := range []struct {
			route  string
			stream bool
		}{{"/v1/chat/completions", false}, {"/v1/responses", false}, {"/v1/messages", false}, {"/v1/responses", true}, {"/v1/messages", true}} {
			for _, known := range []bool{true, false} {
				t.Run(tc.route+map[bool]string{true: "/stream", false: "/aggregate"}[tc.stream]+map[bool]string{true: "/known", false: "/unknown"}[known]+map[fastPathModeT]string{fastPathOff: "/slow", fastPathOn: "/fast"}[mode], func(t *testing.T) {
					withStreamTestAccount(t)
					s := isolatedPerformance(t)
					oldMode := fastPathMode.Load()
					fastPathMode.Store(int32(mode))
					t.Cleanup(func() { fastPathMode.Store(oldMode) })
					dir := resetKeyWorld(t)
					if err := ledgerLoad(dir); err != nil {
						t.Fatal(err)
					}
					plain, key := mustCreateKey(t, keyCreateOptions{Name: "output-budget", QuotaTotal: 10})
					retainedOutputs.configure(outputFileConfig{MaxRequestBytes: 4096, MaxSharedBytes: 8192})
					args := `{"value":"` + strings.Repeat("x", 8192) + `"}`
					chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_synthetic", "function": map[string]any{"name": "synthetic", "arguments": args}}}}, "finish_reason": "tool_calls"}}}
					if known {
						chunk["usage"] = map[string]any{"credit": 3}
					}
					raw, _ := json.Marshal(chunk)
					body := &trackingBody{r: strings.NewReader("data: " + string(raw) + "\n\ndata: [DONE]\n\n")}
					var calls atomic.Int32
					cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
						calls.Add(1)
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, Request: req}, nil
					})}
					reqBody := `{"model":"synthetic","max_tokens":16,"stream":` + map[bool]string{true: "true", false: "false"}[tc.stream] + `,"messages":[{"role":"user","content":"hi"}],"input":"hi"}`
					r := httptest.NewRequest("POST", tc.route, strings.NewReader(reqBody))
					r.Header.Set("Authorization", "Bearer "+plain)
					writer := &outputErrorWriter{ResponseRecorder: httptest.NewRecorder(), body: body}
					newAPIHandler().ServeHTTP(writer, r)
					key.mu.Lock()
					u := key.usage
					key.mu.Unlock()
					want, unknown := float64(3), int64(0)
					if !known {
						want, unknown = 0, 1
					}
					if u.Total != want || u.Unknown != unknown || u.Requests != 1 || calls.Load() != 1 || !body.closed || !writer.closeBeforeError {
						t.Fatalf("budget failure lost usage or cleanup: usage=%+v calls=%d closed=%t before=%t body=%s", u, calls.Load(), body.closed, writer.closeBeforeError, writer.Body.String())
					}
					if strings.Contains(writer.Body.String(), "message_stop") || strings.Contains(writer.Body.String(), "response.completed") || strings.Contains(writer.Body.String(), "data: [DONE]") {
						t.Fatal("budget failure pretended completion")
					}
					v := s.snapshot(time.Now()).Protocols[0]
					if v.Failed != 1 || v.InFlight.Active != 0 || retainedOutputs.used != 0 {
						t.Fatalf("output/live state leaked: %+v used=%d", v.performanceCounters, retainedOutputs.used)
					}
				})
			}
		}
	}
}

func TestOutputBudgetDoesNotAccumulateForwardedMessagesText(t *testing.T) {
	for _, route := range []string{"/v1/messages", "/v1/chat/completions"} {
		t.Run(route, func(t *testing.T) {
			withStreamTestAccount(t)
			retainedOutputs.configure(outputFileConfig{MaxRequestBytes: 2048, MaxSharedBytes: 4096})
			data := strings.Repeat(respChunk(`{"content":"`+strings.Repeat("a", 1024)+`"}`, ""), 32) + respChunk(`{}`, "stop") + "data: [DONE]\n\n"
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &trackingBody{r: strings.NewReader(data)}, Request: req}, nil
			})}
			r := httptest.NewRequest("POST", route, strings.NewReader(`{"model":"synthetic","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`))
			w := httptest.NewRecorder()
			newAPIHandler().ServeHTTP(w, r)
			if strings.Contains(w.Body.String(), "预算") || !strings.Contains(w.Body.String(), map[string]string{"/v1/messages": "message_stop", "/v1/chat/completions": "data: [DONE]"}[route]) {
				t.Fatalf("forwarded body was treated as retained output: %s", w.Body.String())
			}
		})
	}
}

func TestOutputSharedBudgetConcurrentRelease(t *testing.T) {
	g := newOutputAdmission()
	g.sample = func() memorySample { return memorySample{} }
	g.configure(outputFileConfig{MaxRequestBytes: 4096, MaxSharedBytes: 16384})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				l := g.lease()
				_ = l.add(1024)
				l.close()
			}
		})
	}
	wg.Wait()
	if g.used != 0 {
		t.Fatal("concurrent lease leak")
	}
}
func TestAggregateOutputBudgetRejectsRetainedText(t *testing.T) {
	l := outputTestLease(t, 1024)
	reader := newTTFTReader(strings.NewReader(respChunk(`{"content":"`+strings.Repeat("x", 2048)+`"}`, "stop")), time.Now())
	reader.output = l
	_, err := aggregateCompletion(reader, "synthetic")
	if !errors.Is(err, errOutputBudget) {
		t.Fatalf("unbounded aggregate accepted: %v", err)
	}
}

func TestOutputScratchRejectsInvalidSizeAndClosedLease(t *testing.T) {
	l := outputTestLease(t, 4096)
	if err := l.temp(-1); !errors.Is(err, errOutputBudget) {
		t.Fatal("negative scratch size accepted")
	}
	if err := l.temp(256); err != nil {
		t.Fatal(err)
	}
	l.close()
	if err := l.temp(128); !errors.Is(err, errOutputBudget) {
		t.Fatal("released scratch reservation reused")
	}
	if _, err := marshalBudgetedOutput(l, map[string]any{"text": "small"}); !errors.Is(err, errOutputBudget) {
		t.Fatal("serialization accepted a closed lease")
	}
}
