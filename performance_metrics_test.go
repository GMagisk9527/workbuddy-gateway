package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"
)

func isolatedPerformance(t *testing.T) *performanceStore {
	t.Helper()
	old := phaseMetrics
	s := &performanceStore{started: time.Now()}
	phaseMetrics = s
	t.Cleanup(func() { phaseMetrics = old })
	return s
}

func phaseSnapshot(t *testing.T, p performanceProtocolSnapshot, phase performancePhase) performancePhaseSnapshot {
	t.Helper()
	for _, s := range p.Phases {
		if s.Name == performancePhaseNames[phase] {
			return s
		}
	}
	t.Fatal("phase missing")
	return performancePhaseSnapshot{}
}

func TestPerformanceWindowQuantilesAndUnknown(t *testing.T) {
	now := time.Unix(1800000000, 0)
	s := &performanceStore{started: now}
	for i := 0; i < 100; i++ {
		d := time.Millisecond
		if i >= 95 {
			d = 200 * time.Millisecond
		}
		if i == 99 {
			d = 2 * time.Second
		}
		p := &requestPerformance{start: now, protocol: i % 3}
		p.observe(phasePrepare, d, i == 99)
		p.finish(s, 200, false, now.Add(time.Second))
	}
	all := s.snapshot(now.Add(time.Second)).Protocols[0]
	prepare := phaseSnapshot(t, all, phasePrepare)
	if all.Requests != 100 || prepare.Samples != 100 || prepare.Failed != 1 || *prepare.P95MS != 1 || *prepare.P99MS != 250 {
		t.Fatalf("incorrect aggregate: %+v %+v", all.performanceCounters, prepare)
	}
	unknown := phaseSnapshot(t, all, phaseDNS)
	if unknown.Samples != 0 || unknown.AverageMS != nil || unknown.P95MS != nil || all.ReuseRate != nil {
		t.Fatal("unobserved stages/reuse must remain unknown")
	}
	expired := s.snapshot(now.Add(5 * time.Minute)).Protocols[0]
	if expired.Requests != 0 || phaseSnapshot(t, expired, phasePrepare).P99MS != nil {
		t.Fatal("old minute remained in rolling window")
	}
	newer := &requestPerformance{start: now}
	newer.finish(s, 200, false, now.Add(5*time.Minute))
	delayed := &requestPerformance{start: now}
	delayed.finish(s, 200, false, now)
	if got := s.snapshot(now.Add(5 * time.Minute)).Protocols[0].Requests; got != 1 {
		t.Fatalf("late observation evicted current bucket: %d", got)
	}
	for _, path := range []string{"/health", "/admin/api/performance", "/v1/messages/count_tokens", "/user-controlled-label"} {
		r := httptest.NewRequest("POST", path, nil)
		if _, p := withRequestPerformance(r, now); p != nil {
			t.Fatalf("unexpected metric dimension: %s", path)
		}
	}
}

func TestPerformanceReplayCompletionAndLateCallbacks(t *testing.T) {
	now := time.Now()
	s := &performanceStore{started: now}
	p := &requestPerformance{start: now}
	p.networkAttempt(false)
	p.networkAttempt(true)
	p.observe(phaseDNS, time.Millisecond, true)
	p.streamCompletion(false)
	p.streamCompletion(true) // recovered before output: final body determines completeness
	p.observe(phaseFirstToken, 5*time.Millisecond, false)
	p.observe(phaseFirstToken, 50*time.Millisecond, false)
	p.finish(s, 200, false, now.Add(time.Second))
	before, _ := json.Marshal(s.snapshot(now.Add(time.Second)))
	p.finish(s, 500, true, now.Add(time.Second))
	p.observe(phaseDNS, time.Hour, true)
	p.networkAttempt(true)
	p.downstreamError()
	after, _ := json.Marshal(s.snapshot(now.Add(time.Second)))
	if !bytes.Equal(before, after) {
		t.Fatal("completion duplicated or late events changed published metrics")
	}
	all := s.snapshot(now.Add(time.Second)).Protocols[0]
	if all.Failed != 0 || all.Incomplete != 0 || all.Attempts != 2 || all.TransportRetries != 1 || *phaseSnapshot(t, all, phaseFirstToken).AverageMS != 5 {
		t.Fatalf("recovered request counted incorrectly: %+v", all)
	}
	q := &requestPerformance{start: now, protocol: 2}
	q.streamCompletion(false)
	q.downstreamError()
	q.finish(s, 200, true, now.Add(time.Second))
	all = s.snapshot(now.Add(time.Second)).Protocols[0]
	if all.Failed != 1 || all.Incomplete != 1 || all.DownstreamErrors != 1 || all.Canceled != 1 {
		t.Fatalf("in-band/canceled failure lost: %+v", all.performanceCounters)
	}
}

func TestPerformanceTraceParallelAndMissingCallbacks(t *testing.T) {
	p := &requestPerformance{start: time.Now()}
	trace, hooks := newPerformanceTrace(p)
	trace.spans[phaseTCP] = performanceTraceSpan{starts: 2, ends: 2, startSum: 40 * time.Millisecond, endSum: 120 * time.Millisecond}
	hooks.DNSStart(httptrace.DNSStartInfo{Host: "private-host-marker"})
	hooks.DNSDone(httptrace.DNSDoneInfo{Err: errors.New("private-error-marker")})
	hooks.GotConn(httptrace.GotConnInfo{Reused: true})
	trace.finish(false) // upload callback missing; must not fabricate a sample
	if p.cells[phaseTCP].duration != 80*time.Millisecond || p.cells[phaseUpload].seen || p.counters.DroppedTraceEvents != 1 {
		t.Fatalf("parallel/incomplete observations wrong: %+v", p)
	}
	hooks.WroteRequest(httptrace.WroteRequestInfo{}) // late callback ignored
	if p.cells[phaseUpload].seen {
		t.Fatal("late callback revived an omitted span")
	}
	raw, _ := json.Marshal(p.debugFields())
	if bytes.Contains(raw, []byte("private-")) {
		t.Fatal("network metadata leaked into metrics")
	}
	q := &requestPerformance{start: time.Now()}
	failedTrace, failedHooks := newPerformanceTrace(q)
	failedHooks.ConnectStart("tcp", "private-address")
	failedTrace.finish(true)
	if !q.cells[phaseTCP].seen || !q.cells[phaseTCP].failed {
		t.Fatal("failed unfinished dial not accounted")
	}
}

func TestPerformanceSafeRetryStillHonorsWrittenHeaders(t *testing.T) {
	for _, sent := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-headers", true: "after-headers"}[sent], func(t *testing.T) {
			isolatedGatewayConfig(t)
			withRequestProfile(t, "legacy")
			oldRetries, oldBackoff := upstreamTransientRetries, upstreamRetryBackoff
			upstreamTransientRetries, upstreamRetryBackoff = 1, time.Millisecond
			t.Cleanup(func() { upstreamTransientRetries, upstreamRetryBackoff = oldRetries, oldBackoff })
			r, p := withRequestPerformance(httptest.NewRequest("POST", "/v1/messages", nil), time.Now())
			calls := 0
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				trace := httptrace.ContextClientTrace(req.Context())
				trace.GetConn("private-host")
				trace.GotConn(httptrace.GotConnInfo{})
				if calls == 1 {
					if sent {
						trace.WroteHeaders()
					}
					return nil, io.EOF
				}
				trace.WroteHeaders()
				trace.WroteRequest(httptrace.WroteRequestInfo{})
				trace.GotFirstResponseByte()
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})}
			resp, cancel, err := doUpstreamRequest(r, &Account{Path: "synthetic.json", Auth: &StoredAuth{}}, &profileCN, []byte(`{}`), 1, "synthetic-trace")
			if cancel != nil {
				defer cancel()
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			want := uint64(2)
			if sent {
				want = 1
				if err == nil {
					t.Fatal("unsafe retry accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if p.counters.Attempts != want || p.counters.TransportRetries != want-1 {
				t.Fatalf("retry counters wrong: %+v", p.counters)
			}
		})
	}
}

func TestPerformanceHTTPTraceTLSAndConnectionReuse(t *testing.T) {
	isolatedGatewayConfig(t)
	withRequestProfile(t, "legacy")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	cfg.HttpClient = server.Client()
	prof := profileCN
	prof.Base = server.URL
	s := &performanceStore{started: time.Now()}
	for i := 0; i < 2; i++ {
		r, p := withRequestPerformance(httptest.NewRequest("POST", "/v1/messages", nil), time.Now())
		resp, cancel, err := doUpstreamRequest(r, &Account{Path: "synthetic.json", Auth: &StoredAuth{}}, &prof, []byte(`{}`), 1, "synthetic-trace")
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		p.finish(s, 200, false, time.Now())
	}
	all := s.snapshot(time.Now()).Protocols[0]
	if all.Connections != 2 || all.ReusedConnections != 1 || all.ReuseRate == nil || *all.ReuseRate != 0.5 || phaseSnapshot(t, all, phaseTLS).Samples != 1 || phaseSnapshot(t, all, phaseDNS).Samples != 0 {
		t.Fatalf("real TLS/reuse observations wrong: %+v", all)
	}
}

func TestPerformanceAdminOnlyAndDoesNotMeasureItself(t *testing.T) {
	isolateAdminFeatures(t)
	s := isolatedPerformance(t)
	h := newWebUIHandler()
	for _, key := range []string{"", "wrong"} {
		if w := adminRequest(h, "GET", "/admin/api/performance", key, ""); w.Code != 401 {
			t.Fatalf("metrics exposed: %d", w.Code)
		}
	}
	p := &requestPerformance{start: time.Now(), protocol: 2}
	p.observe(phaseRead, time.Millisecond, false)
	p.finish(s, 200, false, time.Now())
	w := adminRequest(h, "GET", "/admin/api/performance", "feature-admin-key", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"protocols"`) {
		t.Fatalf("metrics unavailable: %d %s", w.Code, w.Body.String())
	}
	var data performanceSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil || len(data.Protocols) != 4 || data.Protocols[0].Requests != 1 || phaseSnapshot(t, data.Protocols[0], phaseRead).Samples != 1 {
		t.Fatalf("metrics JSON lost counters or observations: %v %s", err, w.Body.String())
	}
	if w := adminRequest(h, "POST", "/admin/api/performance", "feature-admin-key", `{}`); w.Code != 405 {
		t.Fatalf("metrics accepted write: %d", w.Code)
	}
	if s.snapshot(time.Now()).Protocols[0].Requests != 1 {
		t.Fatal("metrics endpoint measured itself")
	}
}

func TestPerformanceCollectorConcurrentRequests(t *testing.T) {
	s := &performanceStore{started: time.Now()}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				p := &requestPerformance{start: time.Now(), protocol: i % 3}
				p.observe(phaseRead, time.Millisecond, false)
				p.finish(s, 200, false, time.Now())
				_ = s.snapshot(time.Now())
			}
		}()
	}
	wg.Wait()
	if got := s.snapshot(time.Now()).Protocols[0].Requests; got != 400 {
		t.Fatalf("lost requests: %d", got)
	}
}

func TestPerformanceCanceledAdmissionHasNoUpstreamSamples(t *testing.T) {
	isolatePolicies(t)
	s := isolatedPerformance(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{}"))
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	requestAuditMiddleware(requestAdmissionMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("canceled request reached handler") }))).ServeHTTP(w, r)
	all := s.snapshot(time.Now()).Protocols[0]
	if all.Requests != 1 || all.Canceled != 1 || all.Attempts != 0 || phaseSnapshot(t, all, phaseDNS).Samples != 0 {
		t.Fatalf("rejected request polluted upstream metrics: %+v", all)
	}
}

func TestPerformanceAllProtocolsStreamAndAggregate(t *testing.T) {
	isolatedGatewayConfig(t)
	isolatePolicies(t)
	withRequestProfile(t, "legacy")
	resetStatsState(t)
	s := isolatedPerformance(t)
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ route, body string }{
			{"/v1/chat/completions", `{"model":"synthetic-model-marker","stream":%t,"messages":[{"role":"user","content":"synthetic-prompt-marker"}]}`},
			{"/v1/responses", `{"model":"synthetic-model-marker","stream":%t,"input":"synthetic-prompt-marker"}`},
			{"/v1/messages", `{"model":"synthetic-model-marker","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"synthetic-prompt-marker"}]}`},
		} {
			captureUpstreamMessages(t, tc.route, fmt.Sprintf(tc.body, stream))
		}
	}
	snapshot := s.snapshot(time.Now())
	if snapshot.Protocols[0].Requests != 6 || snapshot.Protocols[0].Failed != 0 {
		t.Fatalf("logical requests miscounted: %+v", snapshot.Protocols[0].performanceCounters)
	}
	for _, protocol := range snapshot.Protocols[1:] {
		for _, phase := range []performancePhase{phaseRead, phaseDecode, phasePrepare, phaseAccount, phaseCredential, phaseEncode, phaseConnection, phaseUpstreamRead, phaseFirstToken, phaseDownstream, phaseSettlement, phaseTotal} {
			if got := phaseSnapshot(t, protocol, phase).Samples; got != 2 {
				t.Errorf("%s phase %s samples=%d", protocol.Protocol, performancePhaseNames[phase], got)
			}
		}
	}
	raw, _ := json.Marshal(snapshot)
	for _, private := range []string{"synthetic-model-marker", "synthetic-prompt-marker", "repair-test.json"} {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatalf("metrics retained private dimension: %s", private)
		}
	}
}

func TestPerformanceCompressedReadAndJSONFailures(t *testing.T) {
	isolatePolicies(t)
	s := isolatedPerformance(t)
	var compressed bytes.Buffer
	z := gzip.NewWriter(&compressed)
	_, _ = z.Write([]byte(`{"synthetic":true}`))
	_ = z.Close()
	h := requestAuditMiddleware(withRequestBodyDecode(func(w http.ResponseWriter, r *http.Request) {
		raw, err := readModelRequestBody(r)
		if err != nil || string(raw) != `{"synthetic":true}` {
			t.Errorf("decoded body changed: %v", err)
		}
		w.WriteHeader(200)
	}))
	r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(compressed.Bytes()))
	r.Header.Set("Content-Encoding", "gzip")
	h.ServeHTTP(httptest.NewRecorder(), r)
	all := s.snapshot(time.Now()).Protocols[0]
	if phaseSnapshot(t, all, phaseAdmission).Samples != 1 || phaseSnapshot(t, all, phaseRead).Samples != 1 || phaseSnapshot(t, all, phaseDecompress).Samples != 1 {
		t.Fatalf("compressed stages missing: %+v", all)
	}
	bad := httptest.NewRequest("POST", "/v1/messages", strings.NewReader("invalid gzip"))
	bad.Header.Set("Content-Encoding", "gzip")
	h.ServeHTTP(httptest.NewRecorder(), bad)
	requestAuditMiddleware(withRequestBodyDecode(handleMessages)).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", strings.NewReader("{")))
	all = s.snapshot(time.Now()).Protocols[0]
	if all.Requests != 3 || all.Failed != 2 || all.Attempts != 0 || phaseSnapshot(t, all, phaseDecompress).Failed != 1 || phaseSnapshot(t, all, phaseDecode).Failed != 1 {
		t.Fatalf("invalid inputs polluted counters: %+v", all)
	}
}

func TestPerformanceIncompleteStreamsWithHTTP200(t *testing.T) {
	s := isolatedPerformance(t)
	for _, route := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"} {
		h := requestAuditMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(respChunk(`{"content":"partial"}`, "")))}
			acc := &Account{Path: "synthetic.json"}
			switch route {
			case "/v1/messages":
				streamMessagesResponse(w, r, resp, "synthetic", 1, acc, &profileCN, time.Now(), false)
			case "/v1/responses":
				streamResponsesResponse(w, r, resp, "synthetic", 1, acc, &profileCN, time.Now())
			default:
				streamChatResponse(w, r, resp, "synthetic", 1, acc, &profileCN, time.Now())
			}
		}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", route, nil))
		if w.Code != 200 {
			t.Fatalf("fixture must retain HTTP 200: %d", w.Code)
		}
	}
	all := s.snapshot(time.Now()).Protocols[0]
	if all.Requests != 3 || all.Failed != 3 || all.Incomplete != 3 {
		t.Fatalf("in-band failures counted as success: %+v", all.performanceCounters)
	}
}

func TestPerformanceHandlerPanicPropagatesAndIsCounted(t *testing.T) {
	s := isolatedPerformance(t)
	h := requestAuditMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("synthetic panic") }))
	func() {
		defer func() {
			if got := recover(); got != "synthetic panic" {
				t.Fatalf("panic propagation changed: %v", got)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", nil))
	}()
	all := s.snapshot(time.Now()).Protocols[0]
	if all.Requests != 1 || all.Failed != 1 || phaseSnapshot(t, all, phaseSettlement).Samples != 0 {
		t.Fatalf("aborted handler miscounted: %+v", all)
	}
}

func TestPerformanceLateCancellationDoesNotInvalidateCompletedReply(t *testing.T) {
	s := &performanceStore{started: time.Now()}
	p := &requestPerformance{start: time.Now()}
	p.streamCompletion(true)
	p.finish(s, 200, true, time.Now())
	all := s.snapshot(time.Now()).Protocols[0]
	if all.Failed != 0 || all.Canceled != 0 {
		t.Fatal("late cancellation invalidated complete reply")
	}
	q := &requestPerformance{start: time.Now()}
	q.streamCompletion(true)
	q.downstreamError()
	q.finish(s, 200, true, time.Now())
	all = s.snapshot(time.Now()).Protocols[0]
	if all.Failed != 1 || all.Canceled != 1 {
		t.Fatal("actual downstream error hidden by completion")
	}
}

func TestPerformanceUpstreamRejectionBodyIsMeasured(t *testing.T) {
	isolatedGatewayConfig(t)
	withStreamTestAccount(t)
	s := isolatedPerformance(t)
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Header: make(http.Header), Request: req,
			Body: io.NopCloser(strings.NewReader(`{"code":11128,"msg":"synthetic rejection"}`))}, nil
	})}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"synthetic","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	requestAuditMiddleware(http.HandlerFunc(handleMessages)).ServeHTTP(w, r)
	all := s.snapshot(time.Now()).Protocols[0]
	if w.Code != 400 || all.Requests != 1 || all.Failed != 1 || all.Attempts != 1 || phaseSnapshot(t, all, phaseUpstreamRead).Samples != 1 || phaseSnapshot(t, all, phaseFirstToken).Samples != 0 {
		t.Fatalf("rejection body not measured separately from tokens: status=%d counters=%+v", w.Code, all.performanceCounters)
	}
}
