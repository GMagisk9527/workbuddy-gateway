package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type replayWindowFailWriter struct {
	*lockedResponseWriter
	fail     atomic.Bool
	failed   chan struct{}
	failOnce sync.Once
}

func (w *replayWindowFailWriter) Write(p []byte) (int, error) {
	if w.fail.Load() {
		w.failOnce.Do(func() { close(w.failed) })
		return 0, io.ErrClosedPipe
	}
	return w.lockedResponseWriter.Write(p)
}

func TestSyncHeartbeatFailureDuringSettlementPreventsReplay(t *testing.T) {
	withStreamTestAccount(t)
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, key := mustCreateKey(t, keyCreateOptions{Name: "replay-window", QuotaTotal: 20})
	upstreamConfigMu.Lock()
	streamKeepalive = 5 * time.Millisecond
	upstreamConfigMu.Unlock()
	first := &readErrorBody{err: errors.New("read: connection reset by peer")}
	var calls atomic.Int32
	var settlementLocked atomic.Bool
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		body := io.ReadCloser(first)
		if calls.Add(1) > 1 {
			body = io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"unexpected replay\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		} else {
			// Allow the durable begin record, then hold only settlement.
			ledger.mu.Lock()
			settlementLocked.Store(true)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body, Request: req}, nil
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	r := streamTestRequest()
	r.Header.Set("Authorization", "Bearer "+plain)
	w := &replayWindowFailWriter{lockedResponseWriter: newLockedResponseWriter(), failed: make(chan struct{})}
	done := make(chan struct{})
	defer func() {
		if settlementLocked.Swap(false) {
			ledger.mu.Unlock()
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("handler did not stop after releasing settlement")
		}
	}()
	go func() {
		defer close(done)
		newAPIHandler().ServeHTTP(w, r)
	}()
	// The first attempt has requested replay and is now waiting for the WAL.
	// Delay failure until this boundary, rather than depending on ticker timing.
	waitSettlementCondition(t, "first attempt waiting to settle before replay", func() bool {
		settledMu.Lock()
		defer settledMu.Unlock()
		return len(settledIDs) == 1
	})
	w.fail.Store(true)
	select {
	case <-w.failed:
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat did not encounter the downstream write failure")
	}
	ledger.mu.Unlock()
	settlementLocked.Store(false)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("failed downstream did not stop the pending replay")
	}
	key.mu.Lock()
	usage := key.usage
	key.mu.Unlock()
	if calls.Load() != 1 || usage.Requests != 1 || usage.Unknown != 1 || usage.Total != 0 {
		t.Fatalf("a failure while settling must cancel replay and retain the first attempt: calls=%d usage=%+v", calls.Load(), usage)
	}
	if !first.closed.Load() || strings.Contains(w.String(), "[DONE]") || strings.Contains(w.String(), "unexpected replay") {
		t.Fatal("failed downstream must close the upstream without completing or replaying")
	}
}
