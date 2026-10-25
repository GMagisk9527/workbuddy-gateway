package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type tokenWaitBody struct {
	ctx    context.Context
	prefix *strings.Reader
	closed atomic.Bool
}

func (b *tokenWaitBody) Read(p []byte) (int, error) {
	if n, err := b.prefix.Read(p); n > 0 || err != io.EOF {
		return n, err
	}
	timer := time.NewTimer(5 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-timer.C:
		return copy(p, ": upstream heartbeat\n\n"), nil
	}
}
func (b *tokenWaitBody) Close() error { b.closed.Store(true); return nil }

func TestFirstTokenDeadlineIgnoresHeartbeatsAndPreservesUsage(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		for _, fee := range []struct {
			name, usage   string
			total         float64
			unknown, zero int64
		}{{"known", `{"credit":3}`, 3, 0, 0}, {"zero", `{"credit":0}`, 0, 0, 1}, {"unknown", `{}`, 0, 1, 0}} {
			for _, route := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
				for _, stream := range []bool{false, true} {
					t.Run(route+map[bool]string{false: "/aggregate", true: "/stream"}[stream]+"/"+fee.name+map[fastPathModeT]string{fastPathOff: "/slow", fastPathOn: "/fast"}[mode], func(t *testing.T) {
						withStreamTestAccount(t)
						metrics := isolatedPerformance(t)
						oldMode := fastPathMode.Load()
						fastPathMode.Store(int32(mode))
						t.Cleanup(func() { fastPathMode.Store(oldMode) })
						upstreamConfigMu.Lock()
						old := firstTokenTimeout
						firstTokenTimeout = 35 * time.Millisecond
						upstreamConfigMu.Unlock()
						t.Cleanup(func() { upstreamConfigMu.Lock(); firstTokenTimeout = old; upstreamConfigMu.Unlock() })
						dir := resetKeyWorld(t)
						if err := ledgerLoad(dir); err != nil {
							t.Fatal(err)
						}
						plain, key := mustCreateKey(t, keyCreateOptions{Name: "first-token-deadline", QuotaTotal: 20})
						var calls atomic.Int32
						var body *tokenWaitBody
						cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
							calls.Add(1)
							prefix := respChunk(`{"role":"assistant"}`, "") + "data: {\"choices\":[],\"usage\":" + fee.usage + "}\n\n"
							body = &tokenWaitBody{ctx: req.Context(), prefix: strings.NewReader(prefix)}
							return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, Request: req}, nil
						})}
						clientCtx, cancel := context.WithCancel(context.Background())
						r := httptest.NewRequest("POST", route, strings.NewReader(`{"model":"synthetic","stream":`+map[bool]string{false: "false", true: "true"}[stream]+`,"max_tokens":16,"messages":[{"role":"user","content":"hi"}],"input":"hi"}`)).WithContext(clientCtx)
						r.Header.Set("Authorization", "Bearer "+plain)
						w := httptest.NewRecorder()
						done := make(chan struct{})
						go func() { defer close(done); newAPIHandler().ServeHTTP(w, r) }()
						t.Cleanup(func() {
							cancel()
							select {
							case <-done:
							case <-time.After(time.Second):
								t.Error("reader did not cancel")
							}
						})
						select {
						case <-done:
						case <-time.After(500 * time.Millisecond):
							t.Fatal("upstream heartbeats kept a request with no model increment alive")
						}
						if calls.Load() != 1 || !body.closed.Load() || clientCtx.Err() != nil {
							t.Fatalf("deadline retried or did not close only the upstream: calls=%d closed=%t", calls.Load(), body.closed.Load())
						}
						if !strings.Contains(w.Body.String(), "首个有效增量") || strings.Contains(w.Body.String(), "data: [DONE]") || strings.Contains(w.Body.String(), "message_stop") || strings.Contains(w.Body.String(), "response.completed") {
							t.Fatalf("invalid deadline failure: %s", w.Body.String())
						}
						if !stream && w.Code != 504 {
							t.Fatalf("aggregate status=%d", w.Code)
						}
						key.mu.Lock()
						u := key.usage
						key.mu.Unlock()
						if u.Requests != 1 || u.Total != fee.total || u.Unknown != fee.unknown || u.ZeroCost != fee.zero {
							t.Fatalf("deadline discarded observed fee: %+v", u)
						}
						_, active, waiting := ingressAdmission.snapshot()
						_, models, _ := modelConcurrency.snapshot("")
						if active != 0 || waiting != 0 || models != 0 || atomic.LoadInt32(&key.inflight) != 0 {
							t.Fatal("deadline leaked permits")
						}
						observed := metrics.snapshot(time.Now()).Protocols[0]
						if observed.Requests != 1 || observed.Failed != 1 || observed.Canceled != 0 || observed.InFlight.Active != 0 {
							t.Fatalf("internal deadline was attributed to the client or retained live state: %+v", observed.performanceCounters)
						}
					})
				}
			}
		}
	}
}

func setTestFirstTokenTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := currentFirstTokenTimeout()
	upstreamConfigMu.Lock()
	firstTokenTimeout = d
	upstreamConfigMu.Unlock()
	t.Cleanup(func() { upstreamConfigMu.Lock(); firstTokenTimeout = old; upstreamConfigMu.Unlock() })
}

func TestFirstTokenBudgetConfigSnapshotAndDisable(t *testing.T) {
	isolatePolicies(t)
	if currentFirstTokenTimeout() != 300*time.Second {
		t.Fatal("unexpected default")
	}
	for _, bad := range []string{`{"upstream":{"firstTokenTimeoutSeconds":-1}}`, `{"upstream":{"firstTokenTimeoutSeconds":3601}}`, `{"upstream":{"firstTokenTimeoutSeconds":"bad"}}`} {
		if _, err := parseRuntimeConfig([]byte(bad)); err == nil {
			t.Fatalf("invalid first-token config accepted: %s", bad)
		}
	}
	setTestFirstTokenTimeout(t, 35*time.Millisecond)
	r, old := withFirstTokenBudget(httptest.NewRequest("POST", "/v1/messages", nil))
	defer old.close()
	c := runtimeFileConfig{}
	c.Upstream.FirstTokenTimeoutSeconds = intPointer(0)
	if err := applyRuntimeConfig(c, true, "synthetic-timeout-disable"); err != nil {
		t.Fatal(err)
	}
	_, disabled := withFirstTokenBudget(httptest.NewRequest("POST", "/v1/messages", nil))
	defer disabled.close()
	if old.timeout != 35*time.Millisecond || disabled.timeout != 0 {
		t.Fatal("hot reload changed an existing request deadline")
	}
	old.arm()
	disabled.arm()
	select {
	case <-r.Context().Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("existing budget was disabled by reload")
	}
	if !errors.Is(context.Cause(r.Context()), errFirstTokenTimeout) {
		t.Fatal("deadline lost cancellation cause")
	}
	time.Sleep(60 * time.Millisecond)
	if disabled.ctx.Err() != nil {
		t.Fatal("explicit zero did not disable deadline")
	}
	if err := applyRuntimeConfig(runtimeFileConfig{}, true, "synthetic-timeout-default"); err != nil || currentFirstTokenTimeout() != firstTokenTimeoutDefault {
		t.Fatal("omitted config did not restore default")
	}
}

type tokenPauseBody struct {
	ctx            context.Context
	prefix, suffix *strings.Reader
	paused         bool
	delay          time.Duration
	closed         atomic.Bool
}

func (b *tokenPauseBody) Read(p []byte) (int, error) {
	if n, err := b.prefix.Read(p); n > 0 || err != io.EOF {
		return n, err
	}
	if !b.paused {
		timer := time.NewTimer(b.delay)
		defer timer.Stop()
		select {
		case <-b.ctx.Done():
			return 0, b.ctx.Err()
		case <-timer.C:
		}
		b.paused = true
	}
	return b.suffix.Read(p)
}
func (b *tokenPauseBody) Close() error { b.closed.Store(true); return nil }

func TestFirstIncrementEndsDeadlineAndQuietStreamKeepsAlive(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		for _, delta := range []string{`{"content":"first"}`, `{"reasoning_content":"synthetic thought"}`, `{"tool_calls":[{"index":0,"id":"call_synthetic","function":{"name":"synthetic","arguments":"{}"}}]}`} {
			t.Run(route+"/"+delta, func(t *testing.T) {
				withStreamTestAccount(t)
				setTestFirstTokenTimeout(t, 25*time.Millisecond)
				setTestStreamKeepalive(t, 5*time.Millisecond)
				var body *tokenPauseBody
				calls := 0
				cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
					calls++
					body = &tokenPauseBody{ctx: req.Context(), prefix: strings.NewReader(respChunk(delta, "")), suffix: strings.NewReader(respChunk(`{}`, "stop") + "data: [DONE]\n\n"), delay: 90 * time.Millisecond}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, Request: req}, nil
				})}
				r := httptest.NewRequest("POST", route, strings.NewReader(`{"model":"synthetic","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}],"input":"hi"}`))
				w := httptest.NewRecorder()
				newAPIHandler().ServeHTTP(w, r)
				out := w.Body.String()
				if calls != 1 || !body.closed.Load() || strings.Contains(out, "超时") || !strings.Contains(out, ": keepalive\n\n") {
					t.Fatalf("effective increment did not end deadline or pause had no heartbeat: %s", out)
				}
				terminal := map[string]string{"/v1/chat/completions": "data: [DONE]\n\n", "/v1/responses": "data: [DONE]\n\n", "/v1/messages": "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"}[route]
				if !strings.HasSuffix(out, terminal) {
					t.Fatalf("heartbeat escaped the terminal frame: %s", out)
				}
			})
		}
	}
}

func TestFirstTokenDeadlineDuringReplayBackoffDoesNotChargeUnsentAttempt(t *testing.T) {
	for _, tc := range []struct {
		route  string
		stream bool
	}{{"/v1/chat/completions", true}, {"/v1/messages", true}, {"/v1/messages", false}, {"/v1/responses", true}, {"/v1/responses", false}} {
		t.Run(tc.route+map[bool]string{true: "/stream", false: "/aggregate"}[tc.stream], func(t *testing.T) {
			withStreamTestAccount(t)
			setTestFirstTokenTimeout(t, 45*time.Millisecond)
			oldBackoff := upstreamRetryBackoff
			upstreamRetryBackoff = 200 * time.Millisecond
			t.Cleanup(func() { upstreamRetryBackoff = oldBackoff })
			dir := resetKeyWorld(t)
			if err := ledgerLoad(dir); err != nil {
				t.Fatal(err)
			}
			plain, key := mustCreateKey(t, keyCreateOptions{Name: "deadline-before-replay-send", QuotaTotal: 20})
			var calls atomic.Int32
			first := &readErrorBody{err: errors.New("read: connection reset by peer")}
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: first, Request: req}, nil
			})}
			r := httptest.NewRequest("POST", tc.route, strings.NewReader(`{"model":"synthetic","stream":`+map[bool]string{false: "false", true: "true"}[tc.stream]+`,"max_tokens":16,"messages":[{"role":"user","content":"hi"}],"input":"hi"}`))
			r.Header.Set("Authorization", "Bearer "+plain)
			w := httptest.NewRecorder()
			newAPIHandler().ServeHTTP(w, r)
			key.mu.Lock()
			u := key.usage
			key.mu.Unlock()
			if calls.Load() != 1 || u.Requests != 1 || u.Unknown != 1 || u.Total != 0 || !first.closed.Load() {
				t.Fatalf("deadline charged an unsent replay: calls=%d usage=%+v", calls.Load(), u)
			}
			if !strings.Contains(w.Body.String(), "首个有效增量") || strings.Contains(w.Body.String(), "data: [DONE]") || strings.Contains(w.Body.String(), "message_stop") || strings.Contains(w.Body.String(), "response.completed") {
				t.Fatalf("replay deadline missing failure: %s", w.Body.String())
			}
			if upstreamRetries.active != 0 || upstreamRetries.originals != 0 {
				t.Fatal("canceled replay retained retry permits")
			}
		})
	}
}

func TestFirstTokenDeadlineDuringSettlementReportsFailure(t *testing.T) {
	withStreamTestAccount(t)
	setTestFirstTokenTimeout(t, 60*time.Millisecond)
	setTestStreamKeepalive(t, 5*time.Millisecond)
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, key := mustCreateKey(t, keyCreateOptions{Name: "deadline-in-settlement", QuotaTotal: 20})
	budgetReady := make(chan *firstTokenBudget, 1)
	var calls atomic.Int32
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		budgetReady <- firstTokenFor(req)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &readErrorBody{err: errors.New("read: connection reset by peer")}, Request: req}, nil
	})}
	r := streamTestRequest()
	r.Header.Set("Authorization", "Bearer "+plain)
	w := newLockedResponseWriter()
	done := make(chan struct{})
	ledger.mu.Lock()
	locked := true
	defer func() {
		if locked {
			ledger.mu.Unlock()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("settlement did not finish")
		}
	}()
	go func() { defer close(done); newAPIHandler().ServeHTTP(w, r) }()
	var b *firstTokenBudget
	select {
	case b = <-budgetReady:
	case <-time.After(time.Second):
		t.Fatal("first request did not start")
	}
	select {
	case <-b.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("first-token budget never expired")
	}
	ledger.mu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deadline held request after settlement completed")
	}
	key.mu.Lock()
	u := key.usage
	key.mu.Unlock()
	if calls.Load() != 1 || u.Requests != 1 || u.Unknown != 1 || !strings.Contains(w.String(), "upstream_first_token_timeout") {
		t.Fatalf("settlement deadline failed: calls=%d usage=%+v out=%s", calls.Load(), u, w.String())
	}
}

func TestFirstTokenDeadlineCancelsRealHTTPAndKeepsDownstreamConnection(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "http1", true: "http2"}[h2], func(t *testing.T) {
			withStreamTestAccount(t)
			setTestFirstTokenTimeout(t, 50*time.Millisecond)
			setTestStreamKeepalive(t, 5*time.Millisecond)
			upstreamCanceled := make(chan struct{})
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				defer close(upstreamCanceled)
				w.Header().Set("Content-Type", "text/event-stream")
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
						_, _ = io.WriteString(w, ": upstream heartbeat\n\n")
						w.(http.Flusher).Flush()
					}
				}
			}))
			defer up.Close()
			oldProfile := profileCN
			profileCN.Base, profileCN.Origin = up.URL, up.URL
			defer func() { profileCN = oldProfile }()
			cfg.HttpClient = up.Client()
			server := httptest.NewUnstartedServer(newAPIHandler())
			server.EnableHTTP2 = h2
			if h2 {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			client := server.Client()
			client.Timeout = 2 * time.Second
			resp, err := client.Post(server.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"synthetic","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			out, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || !strings.Contains(string(out), "upstream_first_token_timeout") || calls.Load() != 1 {
				t.Fatalf("real stream deadline failed: %v %s", err, out)
			}
			if h2 && resp.ProtoMajor != 2 {
				t.Fatal("fixture did not negotiate HTTP/2")
			}
			select {
			case <-upstreamCanceled:
			case <-time.After(time.Second):
				t.Fatal("upstream HTTP request survived the deadline")
			}
			health, err := client.Get(server.URL + "/health")
			if err != nil {
				t.Fatal(err)
			}
			_ = health.Body.Close()
			if health.StatusCode != 200 {
				t.Fatal("deadline poisoned downstream connection")
			}
		})
	}
}
