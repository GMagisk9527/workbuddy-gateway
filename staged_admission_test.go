package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStagedAdmissionConfiguration(t *testing.T) {
	c, err := parseRuntimeConfig([]byte(`{"admission":{"maxActive":8,"maxPreparing":2},"scheduling":{"cacheAffinityWaitMs":1500}}`))
	if err != nil {
		t.Fatal(err)
	}
	if normalizeAdmission(c.Admission).active != 8 {
		t.Fatal("wrong total request bound")
	}
}

func TestStagedAdmissionRealModelStreamsDoNotBlockTokenCounting(t *testing.T) {
	isolatePolicies(t)
	withAccounts(t, mkAccount("synthetic-staged-stream.json"))
	ingressAdmission.configure(admissionFileConfig{MaxActive: 3, MaxPreparing: 1, WaitTimeoutMs: 250})
	modelConcurrency.configure(concurrentFileConfig{MaxAcc: 3})
	previous := cfg.HttpClient
	defer func() { cfg.HttpClient = previous }()
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	completed := make(chan struct{}, 2)
	cfg.HttpClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		go func() {
			defer writer.Close()
			_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
			entered <- struct{}{}
			<-release
			_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
	})}
	started := 0
	defer func() {
		close(release)
		for i := 0; i < started; i++ {
			select {
			case <-completed:
			case <-time.After(3 * time.Second):
				t.Error("model stream did not settle")
			}
		}
	}()
	handler := newAPIHandler()
	for i := 0; i < 2; i++ {
		started++
		go func() {
			defer func() { completed <- struct{}{} }()
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"synthetic-staged","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hello"}]}`))
			handler.ServeHTTP(w, r)
			if w.Code != 200 {
				t.Errorf("model stream=%d", w.Code)
			}
		}()
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("model stream did not start")
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{"model":"synthetic-staged","messages":[{"role":"user","content":"hello"}]}`)))
	if w.Code != 200 {
		t.Fatalf("token counting blocked by model streams: %d %s", w.Code, w.Body.String())
	}
	_, active, _ := ingressAdmission.snapshot()
	if active != 2 {
		t.Fatalf("total stream cap was released: %d", active)
	}
}

func TestStagedAdmissionLongStreamsReleaseOnlyPreparation(t *testing.T) {
	isolatePolicies(t)
	ingressAdmission.configure(admissionFileConfig{MaxActive: 3, MaxPreparing: 1, WaitTimeoutMs: 200})
	done := make(chan struct{})
	entered := make(chan struct{}, 2)
	finished := make(chan struct{}, 2)
	started := 0
	defer func() {
		close(done)
		for i := 0; i < started; i++ {
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Error("stream cleanup timed out")
			}
		}
	}()
	handler := requestAdmissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		finishRequestPreparation(r)
		finishRequestPreparation(r)
		if r.URL.Path == "/stream" {
			entered <- struct{}{}
			<-done
		}
		w.WriteHeader(200)
	}))
	for i := 0; i < 2; i++ {
		started++
		go func() {
			handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/stream", nil))
			finished <- struct{}{}
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("long stream retained the preparation permit")
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("POST", "/short", nil))
	if response.Code != 200 {
		t.Fatalf("short request blocked by streams: %d", response.Code)
	}
	_, active, _ := ingressAdmission.snapshot()
	_, preparing, _ := ingressAdmission.preparingSnapshot()
	if active != 2 || preparing != 0 {
		t.Fatalf("stage lifetimes: total=%d preparing=%d", active, preparing)
	}
}

func TestStagedAdmissionQueueTimeoutAndCancellationReleaseTotal(t *testing.T) {
	isolatePolicies(t)
	ingressAdmission.configure(admissionFileConfig{MaxActive: 3, MaxPreparing: 1, WaitTimeoutMs: 25})
	held, err := ingressAdmission.preparing.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	var calls atomic.Int32
	handler := requestAdmissionMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
		if cancelled {
			cancel()
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, request)
		cancel()
		if w.Code < 400 || calls.Load() != 0 {
			t.Fatalf("rejection reached handler: %d", w.Code)
		}
		_, active, waiting := ingressAdmission.snapshot()
		if active != 0 || waiting != 0 {
			t.Fatal("total permit leaked")
		}
	}
}

func TestStagedAdmissionDisableWakesExistingPreparationWaiters(t *testing.T) {
	g := newRequestAdmission()
	g.configure(admissionFileConfig{MaxActive: 3, MaxPreparing: 1})
	held, _ := g.preparing.acquire(context.Background())
	defer held()
	result := make(chan func(), 1)
	go func() { release, _ := g.preparing.acquire(context.Background()); result <- release }()
	waitAdmissionQueue(t, g.preparing, 1)
	g.configure(admissionFileConfig{MaxActive: 3})
	select {
	case release := <-result:
		if release == nil {
			t.Fatal("queued acquire failed")
		}
		release()
	case <-time.After(time.Second):
		t.Fatal("disabling stages stranded a waiter")
	}
}

func TestStagedAdmissionValidation(t *testing.T) {
	for _, raw := range []string{`{"admission":{"maxActive":2,"maxPreparing":3}}`, `{"admission":{"maxPreparing":-1}}`, `{"scheduling":{"cacheAffinityWaitMs":10001}}`} {
		if _, err := parseRuntimeConfig([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
