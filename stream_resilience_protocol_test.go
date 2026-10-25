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

// 下游在重放请求仍在建立时关闭流：Close 必须取消该请求、等待其结束，
// 并关闭 Close 之后才返回的响应体，不能继续读取或提前释放账号锁。
func TestFirstByteReplayBodyCloseCancelsInflightReplay(t *testing.T) {
	first := &readErrorBody{err: errors.New("read: connection reset by peer")}
	replayStarted := make(chan struct{})
	lateBody := &resilienceTrackingBody{Reader: strings.NewReader("late")}
	body := newFirstByteReplayBody(first, context.Background(), nil, 1, "m", 1, func(ctx context.Context) (*http.Response, error) {
		close(replayStarted)
		<-ctx.Done()
		return &http.Response{StatusCode: http.StatusOK, Body: lateBody}, nil
	})

	readDone := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 16))
		readDone <- err
	}()
	<-replayStarted
	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("read after close must fail")
		}
	case <-time.After(time.Second):
		t.Fatal("Close returned without terminating the in-flight replay")
	}
	if !lateBody.closed.Load() {
		t.Fatal("response returned after Close was not closed")
	}
	if !first.closed.Load() {
		t.Fatal("original upstream body was not closed")
	}
}

// 两次上游调用必须各结算一次：第一次没有 usage，记为未知，第二次按实测 credit 计入。
func TestMessagesReplaySettlesEveryUpstreamAttempt(t *testing.T) {
	withStreamTestAccount(t)
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, key := mustCreateKey(t, keyCreateOptions{Name: "messages-replay-accounting", QuotaTotal: 20})
	first := &readErrorBody{err: errors.New("read: connection reset by peer")}
	second := &resilienceTrackingBody{Reader: strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":3}}\n\ndata: [DONE]\n\n")}
	calls := installReplayTransport(t, first, second)

	r := messagesResilienceRequest()
	r.Header.Set("Authorization", "Bearer "+plain)
	w := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(w, r)

	key.mu.Lock()
	usage := key.usage
	key.mu.Unlock()
	if calls.Load() != 2 || usage.Requests != 2 || usage.Unknown != 1 || usage.Total != 3 || usage.Lifetime != 3 {
		t.Fatalf("each upstream call must settle once; calls=%d usage=%+v", calls.Load(), usage)
	}
	if !first.closed.Load() || !second.closed.Load() || !strings.Contains(w.Body.String(), "event: message_stop") {
		t.Fatal("successful replay must finish and close both upstream bodies")
	}
}

// 重放拿到业务拒绝时，Responses 必须保留业务码，而不是退化成通用 stream_interrupted。
func TestResponsesReplayPreservesBusinessRejection(t *testing.T) {
	withStreamTestAccount(t)
	first := &readErrorBody{err: errors.New("read: connection reset by peer")}
	rejectionBody := &resilienceTrackingBody{Reader: strings.NewReader(`{"error":{"code":11128,"message":"upstream rejected"}}`)}
	var calls atomic.Int32
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: first, Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: rejectionBody, Request: req}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })

	w := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(w, responsesResilienceRequest())

	out := w.Body.String()
	if !strings.Contains(out, "response.failed") || !strings.Contains(out, `"code":"11128"`) {
		t.Fatalf("business rejection was replaced by a generic error: %q", out)
	}
	if !first.closed.Load() || !rejectionBody.closed.Load() {
		t.Fatal("replay rejection bodies must be closed")
	}
}

// 下游写失败触发关闭时，若重放还在建立连接，账号锁必须保持到重放真正结束，
// 且 Close 之后返回的响应体必须关闭。
type replayAwareFailWriter struct {
	*httptest.ResponseRecorder
	ready <-chan struct{}
}

func (w *replayAwareFailWriter) Write(p []byte) (int, error) {
	select {
	case <-w.ready:
		return 0, io.ErrClosedPipe
	default:
		return w.ResponseRecorder.Write(p)
	}
}

func TestMessagesCloseDuringReplayKeepsAccountLock(t *testing.T) {
	withStreamTestAccount(t)
	setTestStreamKeepalive(t, 5*time.Millisecond)
	accountMu.Lock()
	acc := accounts[0]
	accountMu.Unlock()

	replayStarted := make(chan struct{})
	lateBody := &resilienceTrackingBody{Reader: strings.NewReader(resilienceSSEBody)}
	var lockWasFree atomic.Bool
	var calls atomic.Int32
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			first := &readErrorBody{err: errors.New("read: connection reset by peer")}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: first, Request: req}, nil
		}
		close(replayStarted)
		<-req.Context().Done()
		lockCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		if lockAccountWithContext(lockCtx, &acc.lock) {
			lockWasFree.Store(true)
			acc.lock.Unlock()
		}
		cancel()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: lateBody, Request: req}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })

	writer := &replayAwareFailWriter{ResponseRecorder: httptest.NewRecorder(), ready: replayStarted}
	done := make(chan struct{})
	go func() {
		defer close(done)
		newAPIHandler().ServeHTTP(writer, messagesResilienceRequest())
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("closing the downstream did not terminate the in-flight replay")
	}
	select {
	case <-replayStarted:
	default:
		t.Fatal("replay was never attempted")
	}
	if lockWasFree.Load() {
		t.Fatal("account lock was released while the replay request was still in flight")
	}
	if !lateBody.closed.Load() {
		t.Fatal("response returned after Close was not closed")
	}
}

// 重放返回的 400 错误体可能还在阻塞读取：Close 必须能取消它并等待处理结束。
type replayCtxBlockingBody struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
	closed  atomic.Bool
}

func (b *replayCtxBlockingBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *replayCtxBlockingBody) Close() error {
	b.closed.Store(true)
	return nil
}

func TestFirstByteReplayBodyCloseCancelsReplayErrorBody(t *testing.T) {
	first := &readErrorBody{err: errors.New("read: connection reset by peer")}
	replayReady := make(chan *replayCtxBlockingBody, 1)
	body := newFirstByteReplayBody(first, context.Background(), nil, 1, "m", 1, func(ctx context.Context) (*http.Response, error) {
		respBody := &replayCtxBlockingBody{ctx: ctx, started: make(chan struct{})}
		replayReady <- respBody
		return &http.Response{StatusCode: http.StatusBadRequest, Body: respBody}, nil
	})

	readDone := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 16))
		readDone <- err
	}()
	var respBody *replayCtxBlockingBody
	select {
	case respBody = <-replayReady:
	case <-time.After(time.Second):
		t.Fatal("replay was never attempted")
	}
	select {
	case <-respBody.started:
	case <-time.After(time.Second):
		t.Fatal("replay error body was not read")
	}
	closeDone := make(chan struct{})
	go func() {
		_ = body.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close blocked on the replay error body")
	}
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("read after close must fail")
		}
	case <-time.After(time.Second):
		t.Fatal("Read did not return after Close canceled the replay")
	}
	if !respBody.closed.Load() {
		t.Fatal("replay error body was not closed")
	}
}

// 重放被上游明确拒绝时，第二个 attempt 不能因为 SSE 的 200 被记成 unknown 消费。
func TestMessagesReplayRejectionIsNotSettledAsUnknown(t *testing.T) {
	withStreamTestAccount(t)
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, key := mustCreateKey(t, keyCreateOptions{Name: "messages-replay-rejected", QuotaTotal: 20})
	first := &readErrorBody{err: errors.New("read: connection reset by peer")}
	rejectionBody := &resilienceTrackingBody{Reader: strings.NewReader(`{"error":{"code":11128,"message":"upstream rejected"}}`)}
	var calls atomic.Int32
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: first, Request: req}, nil
		}
		return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: rejectionBody, Request: req}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })

	r := messagesResilienceRequest()
	r.Header.Set("Authorization", "Bearer "+plain)
	w := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(w, r)

	key.mu.Lock()
	usage := key.usage
	key.mu.Unlock()
	if calls.Load() != 2 || usage.Requests != 1 || usage.Unknown != 1 || usage.Total != 0 {
		t.Fatalf("rejected replay must not be settled as unknown consumption; calls=%d usage=%+v", calls.Load(), usage)
	}
	if !strings.Contains(w.Body.String(), "event: error") || !strings.Contains(w.Body.String(), "11128") {
		t.Fatalf("business rejection not surfaced: %q", w.Body.String())
	}
}
