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

type stalledErrorBody struct {
	ctx       context.Context
	prefix    *strings.Reader
	trickle   bool
	readErr   error
	reading   chan struct{}
	closed    chan struct{}
	readOnce  sync.Once
	closeOnce sync.Once
	closes    atomic.Int32
}

func (b *stalledErrorBody) Read(p []byte) (int, error) {
	if n, err := b.prefix.Read(p); n > 0 || err != io.EOF {
		return n, err
	}
	b.readOnce.Do(func() { close(b.reading) })
	if b.readErr != nil {
		return 0, b.readErr
	}
	var tick <-chan time.Time
	if b.trickle {
		timer := time.NewTimer(5 * time.Millisecond)
		defer timer.Stop()
		tick = timer.C
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.ErrClosedPipe
	case <-tick:
		p[0] = ' '
		return 1, nil
	}
}

func (b *stalledErrorBody) Close() error {
	b.closes.Add(1)
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestUpstreamErrorBodyDeadlineReleasesResources(t *testing.T) {
	for _, replay := range []bool{false, true} {
		for _, request := range []func() *http.Request{streamTestRequest, messagesResilienceRequest, responsesResilienceRequest} {
			for _, tc := range []struct {
				name    string
				status  int
				prefix  string
				trickle bool
				readErr error
			}{
				{"stalled", 400, "", false, nil},
				{"partial_auth", 401, `{"error":"invalid token`, false, nil},
				{"partial_quota", 429, `{"code":14018,"msg":"credits exhausted`, false, nil},
				{"trickle", 503, "", true, nil},
				{"read_error", 401, `{"error":"invalid token`, false, io.ErrUnexpectedEOF},
			} {
				name := request().URL.Path + "/" + tc.name
				if replay {
					name += "/replay"
				}
				t.Run(name, func(t *testing.T) {
					withStreamTestAccount(t)
					oldTimeout, oldBackoff := upstreamErrorBodyReadTimeout, upstreamRetryBackoff
					upstreamErrorBodyReadTimeout, upstreamRetryBackoff = 25*time.Millisecond, time.Microsecond
					t.Cleanup(func() { upstreamErrorBodyReadTimeout, upstreamRetryBackoff = oldTimeout, oldBackoff })
					dir := resetKeyWorld(t)
					if err := ledgerLoad(dir); err != nil {
						t.Fatal(err)
					}
					plain, key := mustCreateKey(t, keyCreateOptions{Name: "error-body-timeout", QuotaTotal: 20})
					ctx, cancel := context.WithCancel(context.Background())
					reading := make(chan struct{})
					var body *stalledErrorBody
					var calls, controlCalls atomic.Int32
					cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
						if !strings.HasSuffix(req.URL.Path, "/chat/completions") {
							controlCalls.Add(1)
							return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"synthetic verification unavailable"}`)), Request: req}, nil
						}
						if calls.Add(1) == 1 && replay {
							return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &readErrorBody{err: errors.New("read: connection reset by peer")}, Request: req}, nil
						}
						body = &stalledErrorBody{ctx: req.Context(), prefix: strings.NewReader(tc.prefix), trickle: tc.trickle, readErr: tc.readErr, reading: reading, closed: make(chan struct{})}
						return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body, Request: req}, nil
					})}
					r := request().WithContext(ctx)
					r.Header.Set("Authorization", "Bearer "+plain)
					w := httptest.NewRecorder()
					done := make(chan struct{})
					go func() { defer close(done); newAPIHandler().ServeHTTP(w, r) }()
					t.Cleanup(func() {
						cancel()
						select {
						case <-done:
						case <-time.After(2 * time.Second):
							t.Error("error-body reader did not stop after client cancellation")
						}
					})
					select {
					case <-reading:
					case <-time.After(2 * time.Second):
						t.Fatal("error-body read was not reached")
					}
					select {
					case <-done:
					case <-time.After(500 * time.Millisecond):
						t.Fatal("upstream error body exceeded its fixed read budget and retained the request")
					}
					wantCalls, wantSettlements := int32(1), int64(0)
					if replay {
						wantCalls, wantSettlements = 2, 1
					} else {
						wantStatus, wantMessage := http.StatusGatewayTimeout, errUpstreamErrorBodyTimeout.Error()
						if tc.readErr != nil {
							wantStatus, wantMessage = http.StatusBadGateway, "上游错误响应读取失败"
						}
						if w.Code != wantStatus || !strings.Contains(w.Body.String(), wantMessage) {
							t.Fatalf("missing bounded error-body failure: status=%d body=%s", w.Code, w.Body.String())
						}
					}
					if tc.status >= 500 {
						wantSettlements++
					} // The 5xx attempt can have consumed quota even if its body stalls.
					if calls.Load() != wantCalls || controlCalls.Load() != 0 || body.closes.Load() != 1 || ctx.Err() != nil {
						t.Fatalf("timeout retried, failed cleanup, or canceled the client: calls=%d closes=%d clientErr=%v", calls.Load(), body.closes.Load(), ctx.Err())
					}
					if strings.Contains(w.Body.String(), "message_stop") || strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), "data: [DONE]") {
						t.Fatal("error-body timeout emitted a successful SSE terminal")
					}
					_, active, waiting := ingressAdmission.snapshot()
					_, modelActive, _ := modelConcurrency.snapshot("")
					if active != 0 || waiting != 0 || modelActive != 0 || atomic.LoadInt32(&key.inflight) != 0 || upstreamRetries.active != 0 || upstreamRetries.originals != 0 {
						t.Fatal("timeout leaked an admission, model, key or retry permit")
					}
					accountMu.Lock()
					acc := accounts[0]
					unusable := acc.Disabled || !acc.CooldownUntil.IsZero() || acc.QuotaExhausted
					for _, state := range acc.ModelStates {
						unusable = unusable || state.QuotaBlocked || !state.CooldownUntil.IsZero() || state.CostClass == modelCostFree
					}
					accountMu.Unlock()
					if unusable {
						t.Fatal("partial error body changed account eligibility or pricing")
					}
					if !acc.lock.TryLock() {
						t.Fatal("account read lock survived error-body timeout")
					}
					acc.lock.Unlock()
					key.mu.Lock()
					usage := key.usage
					key.mu.Unlock()
					if usage.Requests != wantSettlements || usage.Unknown != wantSettlements || usage.Total != 0 {
						t.Fatalf("timeout changed original/replay settlement semantics: %+v", usage)
					}
				})
			}
		}
	}
}

func TestUpstreamErrorBodyDeadlineWithRealTransport(t *testing.T) {
	for _, trickle := range []bool{false, true} {
		t.Run(map[bool]string{false: "stalled", true: "trickle"}[trickle], func(t *testing.T) {
			old := upstreamErrorBodyReadTimeout
			upstreamErrorBodyReadTimeout = 40 * time.Millisecond
			t.Cleanup(func() { upstreamErrorBodyReadTimeout = old })
			serverCanceled := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(serverCanceled)
				w.WriteHeader(400)
				_, _ = io.WriteString(w, `{"error":`)
				w.(http.Flusher).Flush()
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
						if trickle {
							_, _ = io.WriteString(w, " ")
							w.(http.Flusher).Flush()
						}
					}
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := readUpstreamErrorBody(resp.Body, cancel, 1<<20); !errors.Is(err, errUpstreamErrorBodyTimeout) {
				t.Fatalf("real error body escaped deadline: %v", err)
			}
			select {
			case <-serverCanceled:
			case <-time.After(time.Second):
				t.Fatal("timeout did not cancel the real upstream HTTP request")
			}
		})
	}
}

func TestUpstreamErrorBodyCompleteStopsDeadline(t *testing.T) {
	old := upstreamErrorBodyReadTimeout
	upstreamErrorBodyReadTimeout = 25 * time.Millisecond
	defer func() { upstreamErrorBodyReadTimeout = old }()
	var cancellations atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &stalledErrorBody{ctx: ctx, prefix: strings.NewReader(""), reading: make(chan struct{}), closed: make(chan struct{})}
	complete := struct {
		io.Reader
		io.Closer
	}{strings.NewReader(`{"code":11128}`), body}
	got, err := readUpstreamErrorBody(complete, func() { cancellations.Add(1); cancel() }, 1<<20)
	if err != nil || string(got) != `{"code":11128}` || body.closes.Load() != 1 {
		t.Fatalf("complete error body changed or did not close: %s %v", got, err)
	}
	time.Sleep(3 * upstreamErrorBodyReadTimeout)
	if cancellations.Load() != 1 || body.closes.Load() != 1 {
		t.Fatal("error-body deadline callback outlived a completed read")
	}
}
