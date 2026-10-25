package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLivePerformanceTracksStagesAndUnlinksEveryExit(t *testing.T) {
	s := isolatedPerformance(t)
	now := time.Now()
	r, p := withRequestPerformance(httptest.NewRequest("POST", "/v1/messages", nil), now.Add(-time.Second))
	_, q := withRequestPerformance(httptest.NewRequest("POST", "/v1/responses", nil), now)
	p.setLivePhase(liveFirstToken)
	p.setLiveStream(true)
	p.beginLiveWrite(now.Add(-20 * time.Millisecond))
	q.setLivePhase(liveAdmission)
	v := s.snapshot(now).Protocols[0].InFlight
	if v.Active != 2 || v.Streams != 1 || v.WaitingFirstToken != 1 || v.Writing != 1 || *v.OldestMS != 1000 || *v.LongestWriteMS != 20 {
		t.Fatalf("live state wrong: %+v", v)
	}
	if performanceFor(r) != p || s.snapshot(now).Protocols[0].Requests != 0 {
		t.Fatal("unfinished requests entered completed window")
	}
	p.observe(phaseFirstToken, time.Second, false)
	p.endLiveWrite()
	v = s.snapshot(time.Now()).Protocols[3].InFlight
	if v.WaitingFirstToken != 0 || v.Stages[liveResponding].Requests != 1 || v.Writing != 0 || v.LongestWriteMS != nil {
		t.Fatalf("first increment did not leave wait: %+v", v)
	}
	p.finish(s, 200, false, now)
	p.finish(s, 500, true, now)
	p.setLivePhase(liveSettlement) // late callbacks must not restore registration
	q.finish(s, 408, true, now)
	v = s.snapshot(now).Protocols[0].InFlight
	if v.Active != 0 || v.OldestMS != nil || v.Streams != 0 {
		t.Fatalf("live request leaked: %+v", v)
	}
	for _, head := range s.live.heads {
		if head != nil {
			t.Fatal("completed request still reachable")
		}
	}
}

func TestLivePerformanceShowsQueuedAndWaitingRequestsBeforeCompletion(t *testing.T) {
	withStreamTestAccount(t)
	s := isolatedPerformance(t)
	maxWaiting := 1
	ingressAdmission.configure(admissionFileConfig{MaxActive: 1, MaxWaiting: &maxWaiting})
	body := &settlementBlockingBody{readStarted: make(chan struct{}), closed: make(chan struct{})}
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		context.AfterFunc(req.Context(), func() { _ = body.Close() })
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body, Request: req}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done1, done2 := make(chan struct{}), make(chan struct{})
	h := newAPIHandler()
	go func() { defer close(done1); h.ServeHTTP(httptest.NewRecorder(), streamTestRequest().WithContext(ctx1)) }()
	select {
	case <-body.readStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first request never reached upstream read")
	}
	go func() { defer close(done2); h.ServeHTTP(httptest.NewRecorder(), streamTestRequest().WithContext(ctx2)) }()
	defer func() { cancel1(); cancel2(); <-done1; <-done2 }()
	waitSettlementCondition(t, "second request admission queue", func() bool { _, _, waiting := ingressAdmission.snapshot(); return waiting == 1 })
	v := s.snapshot(time.Now()).Protocols[0]
	if v.Requests != 0 || v.InFlight.Active != 2 || v.InFlight.Streams != 1 || v.InFlight.WaitingFirstToken != 1 || v.InFlight.Stages[liveAdmission].Requests != 1 {
		t.Fatalf("in-progress queue/stream invisible: completed=%d live=%+v", v.Requests, v.InFlight)
	}
	cancel1()
	cancel2()
	<-done1
	<-done2
	if v := s.snapshot(time.Now()).Protocols[0]; v.InFlight.Active != 0 || v.Requests != 2 {
		t.Fatalf("canceled live state leaked: %+v", v)
	}
}

func TestLivePerformanceConcurrentTransitionsAndSnapshot(t *testing.T) {
	s := isolatedPerformance(t)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			for range 40 {
				paths := [...]string{"/v1/chat/completions", "/v1/responses", "/v1/messages"}
				_, p := withRequestPerformance(httptest.NewRequest("POST", paths[i%3], nil), time.Now())
				p.setLivePhase(liveAccount)
				p.setLivePhase(liveFirstToken)
				p.setLiveStream(true)
				p.beginLiveWrite(time.Now())
				_ = s.snapshot(time.Now())
				p.endLiveWrite()
				p.observe(phaseFirstToken, time.Millisecond, false)
				p.finish(s, 200, false, time.Now())
			}
		})
	}
	wg.Wait()
	v := s.snapshot(time.Now()).Protocols[0]
	if v.Requests != 240 || v.InFlight.Active != 0 {
		t.Fatalf("live/completed accounting wrong: %+v", v)
	}
}

func TestLivePerformanceAdminJSONAndPanicCleanup(t *testing.T) {
	isolateAdminFeatures(t)
	s := isolatedPerformance(t)
	_, p := withRequestPerformance(httptest.NewRequest("POST", "/v1/messages", strings.NewReader("private-prompt-marker")), time.Now())
	defer p.finish(s, 200, false, time.Now())
	p.setLivePhase(liveFirstToken)
	w := adminRequest(newWebUIHandler(), "GET", "/admin/api/performance", "feature-admin-key", "")
	var payload struct {
		performanceSnapshot
		Runtime liveRuntimeSnapshot `json:"runtime"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || w.Code != 200 || payload.Protocols[0].InFlight.Active != 1 || payload.Runtime.AdmissionLimit <= 0 {
		t.Fatalf("live JSON unavailable: %v %s", err, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private-prompt-marker") {
		t.Fatal("body leaked")
	}
	h := requestAuditMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("synthetic") }))
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", nil))
	}()
	if got := s.snapshot(time.Now()).Protocols[0].InFlight.Active; got != 1 {
		t.Fatalf("panic left active registration: %d", got)
	}
}
