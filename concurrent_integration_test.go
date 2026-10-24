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

func TestConcurrentFirstByteReplayRetainsSlotUntilCanceledSendFinishes(t *testing.T) {
	isolatePolicies(t)
	modelConcurrency.configure(concurrentFileConfig{MaxAcc: 1, MaxGlobal: 1})
	withAccounts(t, mkAccount("synthetic-replay-slot.json"))
	started, canceled, allowReturn := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var allowOnce sync.Once
	unblock := func() { allowOnce.Do(func() { close(allowReturn) }) }
	t.Cleanup(unblock)
	var calls atomic.Int32
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		switch calls.Add(1) {
		case 1:
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &readErrorBody{err: errors.New("read: connection reset by peer")}}, nil
		case 2:
			close(started)
			<-req.Context().Done()
			close(canceled)
			// A transport may take time to finish after cancellation.
			<-allowReturn
			return nil, req.Context().Err()
		default:
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
		}
	})}
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	request := func(ctx context.Context) (*http.Response, bool) {
		r := httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
		resp, _, _, ok := upstreamChat(httptest.NewRecorder(), r, 1, "serial-model", []byte("{}"), time.Now())
		return resp, ok
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, ok := request(ctx)
	if !ok {
		t.Fatal("initial request rejected")
	}
	defer first.Body.Close()
	readDone := make(chan struct{})
	go func() { _, _ = io.ReadAll(first.Body); close(readDone) }()
	defer func() {
		cancel()
		unblock()
		select {
		case <-readDone:
		case <-time.After(time.Second):
			t.Error("replay reader did not finish")
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first-byte retry never started")
	}
	assertOccupied := func() {
		t.Helper()
		_, active, _ := modelConcurrency.snapshot("")
		if active != 1 {
			t.Fatalf("replay released its slot early: active=%d", active)
		}
		waitCtx, stop := context.WithTimeout(context.Background(), 25*time.Millisecond)
		defer stop()
		if resp, ok := request(waitCtx); ok {
			resp.Body.Close()
			t.Fatal("queued request bypassed in-flight replay")
		}
		if calls.Load() != 2 {
			t.Fatalf("unexpected upstream sends: %d", calls.Load())
		}
	}
	assertOccupied()
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("client cancellation did not cancel replay")
	}
	assertOccupied()
	unblock()
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("canceled replay did not finish")
	}
	first.Body.Close()
	_, active, _ := modelConcurrency.snapshot("")
	if active != 0 {
		t.Fatalf("canceled replay leaked %d slots", active)
	}
	next, ok := request(context.Background())
	if !ok {
		t.Fatal("slot was not reusable after replay finished")
	}
	next.Body.Close()
}

func TestConcurrentSlotQueueRechecksAccountEligibility(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block func(*Account)
	}{
		{"cooldown", func(acc *Account) { markCooldown(acc, time.Now().Add(time.Minute), "synthetic") }},
		{"model_quota", func(acc *Account) { markModelQuotaBlocked(acc, "serial-model", "synthetic") }},
		{"removed", func(acc *Account) { accountMu.Lock(); accounts = nil; accountMu.Unlock() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			setupAccountStreamTest(t, retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("held"))}, nil
			}))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			first := awaitAccountStreamCall(t, startAccountStreamCall(ctx))
			waitCtx := &observedAccountQueueContext{Context: ctx, queued: make(chan struct{})}
			second := startAccountStreamCall(waitCtx)
			select {
			case <-waitCtx.queued:
			case <-ctx.Done():
				t.Fatal("request never queued")
			}
			tc.block(accounts[0])
			first.Body.Close()
			select {
			case resp := <-second:
				if resp != nil {
					resp.Body.Close()
					t.Fatal("queued request used an ineligible account")
				}
			case <-ctx.Done():
				t.Fatal("ineligible queued request did not finish")
			}
			_, active, _ := modelConcurrency.snapshot("")
			if calls.Load() != 1 || active != 0 {
				t.Fatalf("calls=%d active=%d", calls.Load(), active)
			}
		})
	}
}
