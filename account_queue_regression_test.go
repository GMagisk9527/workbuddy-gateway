package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Report when the request reaches its context-aware account-lock wait, after
// selection. This lets the test change eligibility without timing sleeps.
type observedAccountQueueContext struct {
	context.Context
	queued chan struct{}
	once   sync.Once
}

func (c *observedAccountQueueContext) Err() error {
	c.once.Do(func() { close(c.queued) })
	return c.Context.Err()
}

func TestAccountQueueRechecksCooldownAndModelQuota(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block func(*Account)
	}{
		{"account_cooldown", func(acc *Account) {
			markCooldown(acc, time.Now().Add(time.Minute), "synthetic queued-account cooldown")
		}},
		{"model_cooldown", func(acc *Account) {
			markModelCooldown(acc, "serial-model", time.Now().Add(time.Minute), "synthetic queued-model cooldown")
		}},
		{"model_quota", func(acc *Account) {
			markModelQuotaBlocked(acc, "serial-model", "synthetic queued-model quota")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			setupAccountStreamTest(t, retryTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			}))
			acc := accounts[0]
			acc.lock.Lock()
			var unlock sync.Once
			release := func() { unlock.Do(acc.lock.Unlock) }
			defer release()
			base, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ctx := &observedAccountQueueContext{Context: base, queued: make(chan struct{})}
			result := startAccountStreamCall(ctx)
			select {
			case <-ctx.queued:
			case <-time.After(time.Second):
				t.Fatal("request did not reach the account queue")
			}
			tc.block(acc)
			release()
			select {
			case resp := <-result:
				if resp != nil {
					_ = resp.Body.Close()
					t.Fatal("queued request sent after its selected account became ineligible")
				}
			case <-time.After(time.Second):
				t.Fatal("ineligible queued request did not finish")
			}
			if calls.Load() != 0 {
				t.Fatalf("ineligible account received %d upstream requests", calls.Load())
			}
		})
	}
}

func TestAccountQueueCancellationWhileTokenNeedsRefresh(t *testing.T) {
	var calls atomic.Int32
	setupAccountStreamTest(t, retryTransportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"synthetic"}`))}, nil
	}))
	acc := accounts[0]
	accountMu.Lock()
	acc.Auth.Auth.ExpiresAt = time.Now().Add(5 * time.Minute).Unix()
	accountMu.Unlock()
	acc.lock.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(acc.lock.Unlock) }
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	result := startAccountStreamCall(ctx)
	blocked := false
	select {
	case resp := <-result:
		if resp != nil {
			_ = resp.Body.Close()
			t.Fatal("canceled request unexpectedly sent a model request")
		}
	case <-time.After(500 * time.Millisecond):
		// Release and join before failing, so the old blocked implementation
		// cannot leak a goroutine into another test's restored global state.
		blocked = true
		release()
		select {
		case resp := <-result:
			if resp != nil {
				_ = resp.Body.Close()
			}
		case <-time.After(time.Second):
			t.Fatal("token-refresh queue did not finish after releasing the account")
		}
	}
	if blocked {
		t.Fatal("canceled request remained blocked on the token-refresh account lock")
	}
	if calls.Load() != 0 {
		t.Fatalf("canceled request made %d upstream model/refresh requests", calls.Load())
	}
	accountMu.Lock()
	cooling, failures := !acc.CooldownUntil.IsZero(), acc.RefreshFailCount
	accountMu.Unlock()
	if cooling || failures != 0 {
		t.Fatalf("cancellation changed account health: cooling=%t refreshFailures=%d", cooling, failures)
	}
}

func TestAccountQueueRechecksTokenExpiryAfterWaiting(t *testing.T) {
	var calls atomic.Int32
	setupAccountStreamTest(t, retryTransportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}))
	acc := accounts[0]
	accountMu.Lock()
	acc.Auth.Auth.ExpiresAt = time.Now().Add(time.Hour).Unix()
	acc.Auth.Auth.RefreshToken = ""
	accountMu.Unlock()
	acc.lock.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(acc.lock.Unlock) }
	defer release()
	base, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ctx := &observedAccountQueueContext{Context: base, queued: make(chan struct{})}
	result := startAccountStreamCall(ctx)
	select {
	case <-ctx.queued:
	case <-time.After(time.Second):
		t.Fatal("request did not reach the account queue")
	}
	// Simulate a long queue outlasting the token that was valid at selection.
	accountMu.Lock()
	acc.Auth.Auth.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	accountMu.Unlock()
	release()
	select {
	case resp := <-result:
		if resp != nil {
			_ = resp.Body.Close()
			t.Fatal("queued request sent with a token that expired while waiting")
		}
	case <-time.After(time.Second):
		t.Fatal("queued request with an unrefreshable expired token did not finish")
	}
	if calls.Load() != 0 {
		t.Fatalf("expired queued token caused %d upstream model/refresh requests", calls.Load())
	}
}
