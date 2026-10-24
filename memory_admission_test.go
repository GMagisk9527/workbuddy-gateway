package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func configureMemoryAdmission(t *testing.T, budget int64) {
	t.Helper()
	c, err := parseRuntimeConfig([]byte(fmt.Sprintf(`{"admission":{"maxActive":4,"maxBodyBytes":%d}}`, budget)))
	if err != nil {
		t.Fatal(err)
	}
	if err := applyRuntimeConfig(c, false, "synthetic-memory-test"); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryAdmissionPressureHysteresisAndBypass(t *testing.T) {
	isolatePolicies(t)
	configureMemoryAdmission(t, 4096)
	var used atomic.Uint64
	ingressAdmission.memory.sample = func() memorySample { return memorySample{cgroups: []memoryUsage{{used: used.Load(), limit: 100}}} }
	var completed atomic.Int32
	h := memoryReadHandler(&completed)
	for _, fixture := range []struct {
		used   uint64
		status int
	}{{84, 200}, {85, 503}, {80, 503}, {76, 503}, {75, 200}, {80, 200}} {
		used.Store(fixture.used)
		counter := &admissionCountBody{Reader: strings.NewReader("{}")}
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		r.Body, r.ContentLength = counter, 2
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != fixture.status {
			t.Fatalf("used=%d status=%d want=%d", fixture.used, w.Code, fixture.status)
		}
		if w.Code == 503 && (counter.reads.Load() != 0 || w.Header().Get("Retry-After") != "1" || !strings.Contains(w.Body.String(), "overloaded_error")) {
			t.Fatalf("pressure rejection consumed body or used wrong error: %s", w.Body.String())
		}
	}
	used.Store(99)
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: true, APIKey: "synthetic-memory-key"}); err != nil {
		t.Fatal(err)
	}
	api := newAPIHandler()
	w := httptest.NewRecorder()
	api.ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	if w.Code != 200 {
		t.Fatal("health was blocked by memory pressure")
	}
	counter := &admissionCountBody{Reader: strings.NewReader("{}")}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Body = counter
	w = httptest.NewRecorder()
	api.ServeHTTP(w, r)
	if w.Code != 401 || counter.reads.Load() != 0 {
		t.Fatal("memory protection preceded authentication")
	}
}

func TestMemoryAdmissionPressureDoesNotInterruptInflight(t *testing.T) {
	isolatePolicies(t)
	configureMemoryAdmission(t, 4096)
	var used atomic.Uint64
	ingressAdmission.memory.sample = func() memorySample { return memorySample{runtime: memoryUsage{used.Load(), 100}} }
	entered, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := withRequestBodyDecode(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Error(err)
			return
		}
		close(entered)
		<-finish
		_, _ = io.WriteString(w, "data: synthetic-complete\n\ndata: [DONE]\n\n")
	})
	first := httptest.NewRecorder()
	go func() {
		defer close(done)
		h.ServeHTTP(first, httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}")))
	}()
	defer func() {
		close(finish)
		<-done
		if !strings.Contains(first.Body.String(), "[DONE]") {
			t.Error("in-flight output interrupted")
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first request did not enter")
	}
	used.Store(90)
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}")))
	if second.Code != 503 {
		t.Fatalf("pressure admitted new request: %d", second.Code)
	}
}

func TestMemoryAdmissionRechecksQueuedRequest(t *testing.T) {
	isolatePolicies(t)
	configureMemoryAdmission(t, 4096)
	ingressAdmission.configure(admissionFileConfig{MaxActive: 1, MaxBodyBytes: 4096})
	var used atomic.Uint64
	ingressAdmission.memory.sample = func() memorySample { return memorySample{runtime: memoryUsage{used.Load(), 100}} }
	release, err := ingressAdmission.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var completed atomic.Int32
	h := memoryReadHandler(&completed)
	counter := &admissionCountBody{Reader: strings.NewReader("{}")}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Body = counter
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); h.ServeHTTP(w, r) }()
	defer func() { release(); <-done }()
	waitAdmissionQueue(t, ingressAdmission, 1)
	used.Store(90)
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("queued request did not wake")
	}
	if w.Code != 503 || counter.reads.Load() != 0 || completed.Load() != 0 {
		t.Fatalf("queued request bypassed watermark: %d", w.Code)
	}
	_, active, waiting := ingressAdmission.snapshot()
	if active != 0 || waiting != 0 {
		t.Fatal("queued rejection leaked count permit")
	}
}

func TestMemoryAdmissionHotShrinkAndDisable(t *testing.T) {
	isolatePolicies(t)
	configureMemoryAdmission(t, 128)
	lease := &requestMemoryLease{gate: ingressAdmission.memory}
	defer lease.release()
	reader, err := lease.reader(strings.NewReader(strings.Repeat("x", 128)), 128)
	if err != nil {
		t.Fatal(err)
	}
	ingressAdmission.configure(admissionFileConfig{MaxBodyBytes: 64})
	got, err := io.ReadAll(reader)
	if err != nil || len(got) != 128 {
		t.Fatalf("shrinking interrupted reserved body: len=%d err=%v", len(got), err)
	}
	if release, err := ingressAdmission.acquire(context.Background()); err != errRequestBodyBudget {
		if release != nil {
			release()
		}
		t.Fatalf("shrink did not protect new requests: %v", err)
	}
	lease.release()
	lease.release()
	used, _, _ := ingressAdmission.memory.snapshot()
	if used != 0 {
		t.Fatal("double release changed byte count")
	}
	disabled := false
	ingressAdmission.configure(admissionFileConfig{MemoryProtection: &disabled, MaxBodyBytes: 1})
	ingressAdmission.memory.sample = func() memorySample { return memorySample{runtime: memoryUsage{99, 100}} }
	var completed atomic.Int32
	h := memoryReadHandler(&completed)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(strings.Repeat("x", 128))))
	if w.Code != 200 || completed.Load() != 1 {
		t.Fatal("explicit memoryProtection:false did not disable new controls")
	}
}

func TestMemoryAdmissionAutoBudgetAndMultipleScopes(t *testing.T) {
	g := newRequestMemoryAdmission()
	g.sample = func() memorySample {
		return memorySample{runtime: memoryUsage{0, 48 << 20}, cgroups: []memoryUsage{{0, 128 << 20}, {0, 256 << 20}}}
	}
	_, limit, _ := g.snapshot()
	if limit != 12<<20 {
		t.Fatalf("automatic budget=%d", limit)
	}
	g.configure(admissionFileConfig{MaxBodyBytes: 20 << 20})
	_, limit, _ = g.snapshot()
	if limit != 20<<20 {
		t.Fatalf("explicit budget=%d", limit)
	}
	var goUsed, cgroupUsed atomic.Uint64
	g.sample = func() memorySample {
		return memorySample{runtime: memoryUsage{goUsed.Load(), 100}, cgroups: []memoryUsage{{cgroupUsed.Load(), 100}}}
	}
	goUsed.Store(90)
	if g.checkAdmission() != errRequestMemoryPressure {
		t.Fatal("Go memory pressure ignored")
	}
	goUsed.Store(70)
	cgroupUsed.Store(80)
	if g.checkAdmission() != errRequestMemoryPressure {
		t.Fatal("recovered before every scope reached low watermark")
	}
	cgroupUsed.Store(75)
	if err := g.checkAdmission(); err != nil {
		t.Fatalf("did not recover: %v", err)
	}
}

func TestMemoryAdmissionReleasesAfterPanicAndReadFailure(t *testing.T) {
	for _, panicHandler := range []bool{false, true} {
		t.Run(fmt.Sprint(panicHandler), func(t *testing.T) {
			isolatePolicies(t)
			configureMemoryAdmission(t, 128)
			h := withRequestBodyDecode(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				if panicHandler {
					panic("synthetic panic")
				}
			})
			r := httptest.NewRequest("POST", "/v1/messages", nil)
			r.Body = io.NopCloser(&dataThenErrorReader{data: []byte("partial"), err: io.ErrUnexpectedEOF})
			func() {
				defer func() {
					if caught := recover(); panicHandler && caught == nil {
						t.Error("panic fixture missing")
					}
				}()
				h.ServeHTTP(httptest.NewRecorder(), r)
			}()
			used, _, _ := ingressAdmission.memory.snapshot()
			_, active, _ := ingressAdmission.snapshot()
			if used != 0 || active != 0 {
				t.Fatalf("leaked after failure: bytes=%d active=%d", used, active)
			}
		})
	}
}

func TestMemoryAdmissionConfigValidation(t *testing.T) {
	isolatePolicies(t)
	for _, raw := range []string{
		`{"admission":{"maxBodyBytes":-1}}`, `{"admission":{"maxBodyBytes":1099511627777}}`, `{"admission":{"maxBodyBytes":1.5}}`,
		`{"admission":{"memoryProtection":"true"}}`, `{"admission":{"memoryHighWatermarkPercent":101}}`, `{"admission":{"memoryLowWatermarkPercent":-1}}`,
		`{"admission":{"memoryHighWatermarkPercent":70}}`, `{"admission":{"memoryLowWatermarkPercent":85}}`,
	} {
		if _, err := parseRuntimeConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid memory config accepted: %s", raw)
		}
	}
}

func TestMemoryAdmissionRejectedRequestsDoNotReachUpstreamOrLedger(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, pressure := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/pressure=%t", protocol, pressure), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				configureMemoryAdmission(t, 128)
				if pressure {
					ingressAdmission.memory.sample = func() memorySample { return memorySample{cgroups: []memoryUsage{{90, 100}}} }
				}
				path, body := memoryProtocolRequest(protocol, strings.Repeat("x", 256))
				status, _ := h.send(t, path, body, nil, 0)
				want := 413
				if pressure {
					want = 503
				}
				if status != want || h.calls.Load() != 0 {
					t.Fatalf("rejected input reached upstream: status=%d calls=%d", status, h.calls.Load())
				}
				h.key.mu.Lock()
				usage := h.key.usage
				h.key.mu.Unlock()
				if usage.Requests != 0 || usage.Total != 0 || usage.Unknown != 0 {
					t.Fatalf("rejected input was billed: %+v", usage)
				}
				used, _, _ := ingressAdmission.memory.snapshot()
				if used != 0 {
					t.Fatal("rejected input leaked reservation")
				}
			})
		}
	}
}

func memoryProtocolRequest(protocol, content string) (string, map[string]any) {
	body := map[string]any{"model": compatModel, "stream": true, "messages": []any{map[string]any{"role": "user", "content": content}}}
	path := "/v1/chat/completions"
	if protocol == "responses" {
		path = "/v1/responses"
		delete(body, "messages")
		body["input"] = content
	}
	if protocol == "messages" {
		path = "/v1/messages"
		body["max_tokens"] = 64
	}
	return path, body
}

func TestMemoryAdmissionConcurrentBudgetAndRefund(t *testing.T) {
	g := newRequestMemoryAdmission()
	g.sample = func() memorySample { return memorySample{} }
	g.configure(admissionFileConfig{MaxBodyBytes: 4096})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				lease := &requestMemoryLease{gate: g}
				reader, err := lease.reader(strings.NewReader(strings.Repeat("x", 128)), -1)
				if err == nil {
					_, _ = io.ReadAll(reader)
				}
				used, limit, _ := g.snapshot()
				if used < 0 || used > limit {
					t.Errorf("concurrent budget overflow: %d/%d", used, limit)
				}
				lease.release()
			}
		}()
	}
	wg.Wait()
	used, _, _ := g.snapshot()
	if used != 0 {
		t.Fatalf("concurrent reservations leaked: %d", used)
	}
}

func TestMemoryAdmissionCompressedSuccessPreservesBody(t *testing.T) {
	isolatePolicies(t)
	configureMemoryAdmission(t, 4096)
	payload := []byte(`{"tools":[{"input":{"id":9007199254740993,"text":"Keep exact user text."}}]}`)
	var completed atomic.Int32
	h := memoryReadHandler(&completed)
	for _, encoding := range []string{"gzip", "deflate"} {
		encoded := gzipBytes(t, payload)
		if encoding == "deflate" {
			encoded = zlibBytes(t, payload)
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(encoded))
		r.Header.Set("Content-Encoding", encoding)
		h.ServeHTTP(w, r)
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), payload) {
			t.Fatalf("compressed input changed: %s %d", encoding, w.Code)
		}
	}
}

func memoryReadHandler(completed *atomic.Int32) http.HandlerFunc {
	return withRequestBodyDecode(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			if !writeRequestBodyLimitError(w, r, err) {
				w.WriteHeader(400)
			}
			return
		}
		completed.Add(1)
		_, _ = w.Write(body)
	})
}

func TestMemoryAdmissionBodyBudget(t *testing.T) {
	for _, fixture := range []struct {
		name, path, encoding string
		unknown              bool
	}{
		{"known-chat", "/v1/chat/completions", "", false},
		{"chunked-responses", "/v1/responses", "", true},
		{"gzip-messages", "/v1/messages", "gzip", false},
		{"deflate-count", "/v1/messages/count_tokens", "deflate", true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			isolatePolicies(t)
			configureMemoryAdmission(t, 128)
			payload := []byte(strings.Repeat("x", 129))
			if fixture.encoding == "gzip" {
				payload = gzipBytes(t, payload)
			}
			if fixture.encoding == "deflate" {
				payload = zlibBytes(t, payload)
			}
			counter := &admissionCountBody{Reader: bytes.NewReader(payload)}
			r := httptest.NewRequest("POST", fixture.path, nil)
			r.Body, r.ContentLength = counter, int64(len(payload))
			if fixture.unknown {
				r.ContentLength = -1
			}
			r.Header.Set("Content-Encoding", fixture.encoding)
			var completed atomic.Int32
			h := memoryReadHandler(&completed)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 413 || completed.Load() != 0 {
				t.Fatalf("body budget bypassed: status=%d completed=%d body=%s", w.Code, completed.Load(), w.Body.String())
			}
			if fixture.name == "known-chat" && counter.reads.Load() != 0 {
				t.Fatal("known oversized body read before budget rejection")
			}
			if strings.HasPrefix(fixture.path, "/v1/messages") && !strings.Contains(w.Body.String(), "request_too_large") {
				t.Fatalf("wrong Anthropic error: %s", w.Body.String())
			}
			// Rejection must release both the count permit and all byte reservations.
			next := httptest.NewRecorder()
			h.ServeHTTP(next, httptest.NewRequest("POST", fixture.path, strings.NewReader("{}")))
			if next.Code != 200 || next.Body.String() != "{}" {
				t.Fatalf("budget leaked after rejection: %d %s", next.Code, next.Body.String())
			}
		})
	}
}

func TestMemoryAdmissionBudgetHeldUntilResponseEnds(t *testing.T) {
	isolatePolicies(t)
	configureMemoryAdmission(t, 96)
	entered, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var completed atomic.Int32
	h := withRequestBodyDecode(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err != nil {
			if !writeRequestBodyLimitError(w, r, err) {
				w.WriteHeader(400)
			}
			return
		}
		if completed.Add(1) == 1 {
			_ = r.Body.Close()
			close(entered)
			<-finish
		}
		w.WriteHeader(200)
	})
	first := httptest.NewRecorder()
	go func() {
		defer close(done)
		h.ServeHTTP(first, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(strings.Repeat("x", 64))))
	}()
	defer func() {
		close(finish)
		<-done
		used, _, _ := ingressAdmission.memory.snapshot()
		if first.Code != 200 || used != 0 {
			t.Errorf("response completion did not release budget: status=%d bytes=%d", first.Code, used)
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first request did not enter")
	}
	second := httptest.NewRecorder()
	h.ServeHTTP(second, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(strings.Repeat("y", 64))))
	if second.Code != 503 || completed.Load() != 1 {
		t.Fatalf("stream released its byte budget early: status=%d completed=%d", second.Code, completed.Load())
	}
}

func TestMemoryAdmissionExactBudgetPreservesBytes(t *testing.T) {
	isolatePolicies(t)
	configureMemoryAdmission(t, 128)
	var completed atomic.Int32
	h := memoryReadHandler(&completed)
	for _, unknown := range []bool{false, true} {
		payload := strings.Repeat("a", 128)
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload))
		if unknown {
			r.ContentLength = -1
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != payload {
			t.Fatalf("exact-budget request changed: unknown=%t status=%d", unknown, w.Code)
		}
	}
}
