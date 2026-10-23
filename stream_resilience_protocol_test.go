package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const resilienceSSEBody = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

func messagesResilienceRequest() *http.Request {
	body := `{"model":"deepseek-v4.1-flash","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	return httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
}

func responsesResilienceRequest() *http.Request {
	body := `{"model":"deepseek-v4.1-flash","stream":true,"input":"hi"}`
	return httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
}

func installReplayTransport(t *testing.T, bodies ...io.ReadCloser) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		index := int(calls.Add(1)) - 1
		body := bodies[len(bodies)-1]
		if index < len(bodies) {
			body = bodies[index]
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	return &calls
}

func TestMessagesFirstByteReplayBeforeData(t *testing.T) {
	withStreamTestAccount(t)
	first := &readErrorBody{err: errors.New("read: connection reset by peer")}
	second := &resilienceTrackingBody{Reader: strings.NewReader(resilienceSSEBody)}
	calls := installReplayTransport(t, first, second)

	recorder := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(recorder, messagesResilienceRequest())

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2", got)
	}
	if !first.closed.Load() || !second.closed.Load() {
		t.Fatalf("response bodies not closed: first=%t second=%t", first.closed.Load(), second.closed.Load())
	}
	out := recorder.Body.String()
	if !strings.Contains(out, "event: message_start") || !strings.Contains(out, "event: message_stop") ||
		!strings.Contains(out, `"text":"ok"`) || strings.Contains(out, "event: error") {
		t.Fatalf("replayed Messages stream incomplete: %q", out)
	}
}

func TestResponsesFirstByteReplayBeforeData(t *testing.T) {
	withStreamTestAccount(t)
	first := &readErrorBody{err: errors.New("read: connection reset by peer")}
	second := &resilienceTrackingBody{Reader: strings.NewReader(resilienceSSEBody)}
	calls := installReplayTransport(t, first, second)

	recorder := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(recorder, responsesResilienceRequest())

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2", got)
	}
	if !first.closed.Load() || !second.closed.Load() {
		t.Fatalf("response bodies not closed: first=%t second=%t", first.closed.Load(), second.closed.Load())
	}
	out := recorder.Body.String()
	if !strings.Contains(out, "response.completed") || !strings.Contains(out, `"delta":"ok"`) || strings.Contains(out, "response.failed") {
		t.Fatalf("replayed Responses stream incomplete: %q", out)
	}
}

func TestMessagesNoReplayAfterFirstData(t *testing.T) {
	withStreamTestAccount(t)
	first := &resilienceTrackingBody{Reader: &dataThenErrorReader{
		data: []byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"),
		err:  errors.New("read: connection reset by peer"),
	}}
	calls := installReplayTransport(t, first)

	recorder := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(recorder, messagesResilienceRequest())

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d want 1 after partial data", got)
	}
	out := recorder.Body.String()
	if !strings.Contains(out, "partial") || !strings.Contains(out, "event: error") || strings.Contains(out, "event: message_stop") {
		t.Fatalf("partial Messages stream must fail without replay: %q", out)
	}
}

func TestMessagesAggregateFirstByteReplay(t *testing.T) {
	withStreamTestAccount(t)
	first := &readErrorBody{err: errors.New("read: connection reset by peer")}
	second := &resilienceTrackingBody{Reader: strings.NewReader(resilienceSSEBody)}
	calls := installReplayTransport(t, first, second)

	body := `{"model":"deepseek-v4.1-flash","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(recorder, req)

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2", got)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"text":"ok"`) {
		t.Fatalf("aggregate replay failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestFirstByteReplayRespectsConfigSwitch(t *testing.T) {
	withStreamTestAccount(t)
	upstreamConfigMu.Lock()
	enableFirstByteRetry = false
	upstreamConfigMu.Unlock()
	first := &readErrorBody{err: errors.New("read: connection reset by peer")}
	calls := installReplayTransport(t, first)

	recorder := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(recorder, messagesResilienceRequest())

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d want 1 when first-byte replay is disabled", got)
	}
	if !strings.Contains(recorder.Body.String(), "event: error") {
		t.Fatalf("disabled replay must surface the interruption: %q", recorder.Body.String())
	}
}

func setTestStreamKeepalive(t *testing.T, interval time.Duration) {
	t.Helper()
	upstreamConfigMu.Lock()
	old := streamKeepalive
	streamKeepalive = interval
	upstreamConfigMu.Unlock()
	t.Cleanup(func() {
		upstreamConfigMu.Lock()
		streamKeepalive = old
		upstreamConfigMu.Unlock()
	})
}

func waitForCondition(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func testStreamAccount() *Account {
	return &Account{Path: "offline-synthetic.json", Edition: "cn", Auth: &StoredAuth{
		Edition: "cn",
		Auth:    StoredTokens{AccessToken: "synthetic", ExpiresAt: time.Now().Add(time.Hour).Unix()},
	}}
}

func TestMessagesHeartbeatBeforeFirstUpstreamByte(t *testing.T) {
	withStreamTestAccount(t)
	setTestStreamKeepalive(t, 10*time.Millisecond)

	pr, pw := io.Pipe()
	writer := newLockedResponseWriter()
	req := messagesResilienceRequest()
	resp := &http.Response{StatusCode: http.StatusOK, Body: pr}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if !waitForCondition(time.Second, func() bool {
			return strings.Contains(writer.String(), ": keepalive")
		}) {
			return
		}
		_, _ = pw.Write([]byte(resilienceSSEBody))
		_ = pw.Close()
	}()
	streamMessagesResponse(writer, req, resp, "deepseek-v4.1-flash", 1, testStreamAccount(), &profileCN, time.Now(), false)
	<-done

	out := writer.String()
	if !strings.Contains(out, ": keepalive") {
		t.Fatalf("Messages stream missing heartbeat: %q", out)
	}
	keepalive := strings.Index(out, ": keepalive")
	start := strings.Index(out, "event: message_start")
	stop := strings.Index(out, "event: message_stop")
	if start < 0 || stop < 0 || keepalive > start {
		t.Fatalf("heartbeat must precede the first data event: %q", out)
	}
}

func TestResponsesHeartbeatBeforeFirstUpstreamByte(t *testing.T) {
	withStreamTestAccount(t)
	setTestStreamKeepalive(t, 10*time.Millisecond)

	pr, pw := io.Pipe()
	writer := newLockedResponseWriter()
	req := responsesResilienceRequest()
	resp := &http.Response{StatusCode: http.StatusOK, Body: pr}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if !waitForCondition(time.Second, func() bool {
			return strings.Contains(writer.String(), ": keepalive")
		}) {
			return
		}
		_, _ = pw.Write([]byte(resilienceSSEBody))
		_ = pw.Close()
	}()
	streamResponsesResponse(writer, req, resp, "deepseek-v4.1-flash", 1, testStreamAccount(), &profileCN, time.Now())
	<-done

	out := writer.String()
	if !strings.Contains(out, ": keepalive") {
		t.Fatalf("Responses stream missing heartbeat: %q", out)
	}
	if !strings.Contains(out, "response.created") || !strings.Contains(out, "response.completed") || strings.Contains(out, "response.failed") {
		t.Fatalf("Responses stream did not complete: %q", out)
	}
	keepalive := strings.Index(out, ": keepalive")
	delta := strings.Index(out, `"delta":"ok"`)
	if delta < 0 || keepalive > delta {
		t.Fatalf("heartbeat must precede the first model delta: %q", out)
	}
}

func TestFirstByteReplayPathCoverage(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/responses", "/responses"} {
		if !firstByteReplayPath(path) {
			t.Fatalf("%s must use first-byte replay", path)
		}
	}
	for _, path := range []string{"/v1/chat/completions", "/chat/completions", "/v1/models"} {
		if firstByteReplayPath(path) {
			t.Fatalf("%s must keep its protocol-level behavior", path)
		}
	}
}
