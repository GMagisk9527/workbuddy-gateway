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

func TestSyncReplaySettlesEveryUpstreamAttempt(t *testing.T) {
	withStreamTestAccount(t)
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, key := mustCreateKey(t, keyCreateOptions{Name: "replay-accounting", QuotaTotal: 20})
	first := &readErrorBody{err: errors.New("read: connection reset by peer")}
	second := &resilienceTrackingBody{Reader: strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":3}}\n\ndata: [DONE]\n\n")}
	var calls atomic.Int32
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		body := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req, Body: first}
		if calls.Add(1) > 1 {
			body.Body = second
		}
		return body, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	r := streamTestRequest()
	r.Header.Set("Authorization", "Bearer "+plain)
	w := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(w, r)
	key.mu.Lock()
	usage := key.usage
	key.mu.Unlock()
	if calls.Load() != 2 || usage.Requests != 2 || usage.Unknown != 1 || usage.Total != 3 || usage.Lifetime != 3 {
		t.Fatalf("each upstream call must settle once; calls=%d usage=%+v", calls.Load(), usage)
	}
	if !first.closed.Load() || !second.closed.Load() || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Fatal("successful replay must finish and close both upstream bodies")
	}
}

func TestSyncHeartbeatWriteFailureClosesUpstreamWithoutReplay(t *testing.T) {
	withStreamTestAccount(t)
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, key := mustCreateKey(t, keyCreateOptions{Name: "heartbeat-write-failure", QuotaTotal: 20})
	upstreamConfigMu.Lock()
	streamKeepalive = 5 * time.Millisecond
	upstreamConfigMu.Unlock()
	body := &settlementBlockingBody{closed: make(chan struct{}), readStarted: make(chan struct{})}
	defer body.Close()
	var calls atomic.Int32
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	r := streamTestRequest()
	r.Header.Set("Authorization", "Bearer "+plain)
	w := &settlementFailWriter{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		newAPIHandler().ServeHTTP(w, r)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = body.Close()
		<-done
		t.Fatal("failed heartbeat left the handler blocked on the upstream")
	}
	key.mu.Lock()
	usage := key.usage
	key.mu.Unlock()
	if calls.Load() != 1 || usage.Requests != 1 || usage.Unknown != 1 || usage.Total != 0 {
		t.Fatalf("failed downstream must not replay or fabricate credit: calls=%d usage=%+v", calls.Load(), usage)
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("failed heartbeat did not close the upstream body")
	}
}

func TestSyncUpstreamCommentWriteFailureClosesBeforeNextRead(t *testing.T) {
	withStreamTestAccount(t)
	body := &settlementBlockingBody{first: ": heartbeat\n", closed: make(chan struct{}), readStarted: make(chan struct{})}
	defer body.Close()
	w := &settlementFailWriter{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamChatResponse(w, streamTestRequest(), &http.Response{Body: body}, "offline", 1, &Account{Path: "offline"}, &profileCN, time.Now())
	}()
	select {
	case <-done:
	case <-body.readStarted:
		t.Error("upstream comment write failure caused another upstream read")
	case <-time.After(2 * time.Second):
		t.Error("comment write failure did not terminate the stream")
	}
	_ = body.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not return after closing the body")
	}
}

type syncReplayFailWriter struct {
	*lockedResponseWriter
	fail atomic.Bool
}

func (w *syncReplayFailWriter) Write(p []byte) (int, error) {
	if w.fail.Load() {
		return 0, io.ErrClosedPipe
	}
	return w.lockedResponseWriter.Write(p)
}

func TestSyncHeartbeatFailureCancelsReplayBeforeResponseHeaders(t *testing.T) {
	withStreamTestAccount(t)
	upstreamConfigMu.Lock()
	streamKeepalive = 5 * time.Millisecond
	upstreamConfigMu.Unlock()
	w := &syncReplayFailWriter{lockedResponseWriter: newLockedResponseWriter()}
	var calls atomic.Int32
	replayCanceled := make(chan struct{})
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req,
				Body: &readErrorBody{err: errors.New("read: connection reset by peer")}}, nil
		}
		// Only fail heartbeats after the replay is waiting for response headers.
		w.fail.Store(true)
		<-req.Context().Done()
		close(replayCanceled)
		return nil, req.Context().Err()
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	r := streamTestRequest()
	requestContext, cancel := context.WithCancel(r.Context())
	r = r.WithContext(requestContext)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		newAPIHandler().ServeHTTP(w, r)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("failed heartbeat did not cancel the pending replay request")
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want first attempt plus one replay", calls.Load())
	}
	select {
	case <-replayCanceled:
	default:
		t.Fatal("replay was not canceled before receiving its headers")
	}
}
