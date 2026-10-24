package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type admissionBlockedBody struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	reader  io.Reader
}

func (b *admissionBlockedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.reader.Read(p)
}
func (b *admissionBlockedBody) Close() error { return nil }

type admissionCountBody struct {
	io.Reader
	reads atomic.Int32
}

func (b *admissionCountBody) Read(p []byte) (int, error) { b.reads.Add(1); return b.Reader.Read(p) }
func (b *admissionCountBody) Close() error               { return nil }

func TestAdmissionWaitDoesNotReadBody(t *testing.T) {
	isolatePolicies(t)
	c, err := parseRuntimeConfig([]byte(`{"admission":{"maxActive":1,"maxWaiting":1,"waitTimeoutMs":40}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = applyRuntimeConfig(c, false, ""); err != nil {
		t.Fatal(err)
	}
	h := newAPIHandler()
	body := &admissionBlockedBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)}
	var once sync.Once
	unblock := func() { once.Do(func() { close(body.release) }) }
	defer unblock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest("POST", "/v1/messages/count_tokens", nil)
		r.Body = body
		h.ServeHTTP(httptest.NewRecorder(), r)
	}()
	defer func() { unblock(); <-done }()
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("first request did not enter")
	}
	queued := &admissionCountBody{Reader: strings.NewReader(`{}`)}
	r := httptest.NewRequest("POST", "/v1/messages/count_tokens", nil)
	r.Body = queued
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 429 || queued.reads.Load() != 0 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("queue must reject before body read: status=%d reads=%d headers=%v", w.Code, queued.reads.Load(), w.Header())
	}
	health := httptest.NewRecorder()
	h.ServeHTTP(health, httptest.NewRequest("GET", "/health", nil))
	if health.Code != 200 {
		t.Fatalf("health blocked by admission: %d", health.Code)
	}
}

func TestSchedulingSoftAffinityUsesIdleAccount(t *testing.T) {
	isolatePolicies(t)
	modelConcurrency.configure(concurrentFileConfig{MaxAcc: 1})
	enableAffinity(t, true, affinityModePrefix)
	a, b := mkAccount("synthetic-soft-a.json"), mkAccount("synthetic-soft-b.json")
	withAccounts(t, a, b)
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("held"))}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	bytes := bigBody("stable-system-and-tools", 8192)
	request := func() (*http.Request, context.CancelFunc) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx)
		prepareConversationContext(r, nil)
		return r, cancel
	}
	r, stop := request()
	defer stop()
	key := affinityKeyFor(r, "m", bytes)
	if key == "" {
		t.Fatal("missing affinity fixture")
	}
	affinityRemember(key, a.Path)
	first, acc, _, ok := upstreamChat(httptest.NewRecorder(), r, 1, "m", bytes, time.Now())
	if !ok || acc != a {
		t.Fatal("first request must hold preferred account")
	}
	defer first.Body.Close()
	r2, stop2 := request()
	defer stop2()
	second, acc2, _, ok := upstreamChat(httptest.NewRecorder(), r2, 2, "m", bytes, time.Now())
	if !ok {
		t.Fatal("busy preferred account blocked an idle eligible account")
	}
	defer second.Body.Close()
	if acc2 != b {
		t.Fatalf("expected idle account, got %s", acc2.Path)
	}
	second.Body.Close()
	first.Body.Close()
	if got := affinityLookup(key); got != b.Path {
		t.Fatalf("fallback affinity was not retained: %q", got)
	}
}

type commonStreamTerminalReader struct {
	data      []byte
	afterDone bool
}

func (r *commonStreamTerminalReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	r.afterDone = true
	return 0, errors.New("read: connection reset by peer")
}
func TestCommonStreamTerminalAcrossProtocols(t *testing.T) {
	for _, route := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(route+map[bool]string{false: "/aggregate", true: "/stream"}[stream], func(t *testing.T) {
				isolatePolicies(t)
				reader := &commonStreamTerminalReader{data: []byte("event: message\n: heartbeat\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"complete\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":0.25}}\n\ndata: [DONE]\n\n")}
				resp := &http.Response{Body: io.NopCloser(reader)}
				r := httptest.NewRequest("POST", "/v1/"+route, nil)
				w := httptest.NewRecorder()
				acc := &Account{Path: "synthetic-common-stream.json"}
				switch route {
				case "chat":
					if stream {
						streamChatResponse(w, r, resp, "m", 1, acc, &profileCN, time.Now())
					} else {
						writeChatAggregate(w, r, resp, "m", 1, acc, &profileCN, time.Now())
					}
				case "responses":
					if stream {
						streamResponsesResponse(w, r, resp, "m", 1, acc, &profileCN, time.Now())
					} else {
						writeResponsesAggregate(w, r, resp, "m", 1, acc, &profileCN, time.Now())
					}
				case "messages":
					if stream {
						streamMessagesResponse(w, r, resp, "m", 1, acc, &profileCN, time.Now(), false)
					} else {
						writeMessagesAggregate(w, r, resp, "m", 1, acc, &profileCN, time.Now(), false)
					}
				}
				if reader.afterDone || w.Code != 200 {
					t.Fatalf("terminal handling diverged: status=%d readAfterDone=%t", w.Code, reader.afterDone)
				}
			})
		}
	}
}
