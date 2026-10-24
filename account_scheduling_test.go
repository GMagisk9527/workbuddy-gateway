package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestSchedulingConcurrentArrivalsObserveReservedLoad(t *testing.T) {
	isolatePolicies(t)
	a, b := mkAccount("synthetic-burst-a.json"), mkAccount("synthetic-burst-b.json")
	withAccounts(t, a, b)
	modelConcurrency.configure(concurrentFileConfig{MaxAcc: 32})
	type result struct {
		account *Account
		release func()
		err     error
	}
	for round := 0; round < 10; round++ {
		start := make(chan struct{})
		results := make(chan result, 16)
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
				acc, _, release, err := acquireScheduledAccount(r, "m", nil, "", 1, "")
				results <- result{acc, release, err}
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		counts := map[*Account]int{}
		var releases []func()
		for got := range results {
			if got.err != nil {
				t.Error(got.err)
			}
			counts[got.account]++
			if got.release != nil {
				releases = append(releases, got.release)
			}
		}
		for _, release := range releases {
			release()
		}
		if counts[a] != 8 || counts[b] != 8 {
			t.Fatalf("burst selected stale load: a=%d b=%d", counts[a], counts[b])
		}
	}
}

func TestSchedulingBalancesAndKeepsPolicyTiers(t *testing.T) {
	isolatePolicies(t)
	a, b := mkAccount("synthetic-load-a.json"), mkAccount("synthetic-load-b.json")
	withAccounts(t, a, b)
	modelConcurrency.configure(concurrentFileConfig{MaxAcc: 2})
	held, _, err := modelConcurrency.tryAcquire(context.Background(), modelConcurrencyKey(a))
	if err != nil || held == nil {
		t.Fatal("fixture admission failed")
	}
	defer held()
	accountMu.Lock()
	rrIndex = 0
	accountMu.Unlock()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	selected, _, release, err := acquireScheduledAccount(r, "m", nil, "", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if selected != b {
		t.Fatal("new session did not select lower load")
	}
	markCooldown(b, time.Now().Add(time.Minute), "synthetic")
	selected, _, release, err = acquireScheduledAccount(r, "m", nil, "", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if selected != a {
		t.Fatal("load balancing bypassed cooldown")
	}
}
func TestSchedulingDoesNotReserveUnsentProbes(t *testing.T) {
	isolatePolicies(t)
	a := mkAccount("synthetic-probe-wait.json")
	a.QuotaExhausted = true
	a.QuotaKnown = true
	withAccounts(t, a)
	modelConcurrency.configure(concurrentFileConfig{MaxAcc: 1})
	held, _, _ := modelConcurrency.tryAcquire(context.Background(), modelConcurrencyKey(a))
	defer held()
	setSchedulingConfig(schedulingFileConfig{WaitTimeoutMs: 25})
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	_, _, _, err := acquireScheduledAccount(r, "unknown-fixture-model", nil, "", 1, "")
	if !errors.Is(err, errSchedulingTimeout) {
		t.Fatalf("unexpected queue result: %v", err)
	}
	accountMu.Lock()
	next := modelStateLocked(a, "unknown-fixture-model").NextProbeAt
	accountMu.Unlock()
	if !next.IsZero() {
		t.Fatal("waiting request reserved a quota probe without a permit")
	}
	held()
	_, kind, release, err := acquireScheduledAccount(r, "unknown-fixture-model", nil, "", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if kind != selectionProbeExhausted {
		t.Fatalf("probe kind=%s", kind)
	}
	accountMu.Lock()
	next = modelStateLocked(a, "unknown-fixture-model").NextProbeAt
	accountMu.Unlock()
	if next.IsZero() {
		t.Fatal("admitted probe not reserved")
	}
}
func TestSchedulingStickyReturnsBeforeGraceWhenSlotOpens(t *testing.T) {
	isolatePolicies(t)
	a, b := mkAccount("synthetic-grace-a.json"), mkAccount("synthetic-grace-b.json")
	withAccounts(t, a, b)
	modelConcurrency.configure(concurrentFileConfig{MaxAcc: 1})
	held, _, _ := modelConcurrency.tryAcquire(context.Background(), modelConcurrencyKey(a))
	defer held()
	setSchedulingConfig(schedulingFileConfig{AffinityWaitMs: 1000, WaitTimeoutMs: 2000})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx)
	result := make(chan *Account, 1)
	go func() {
		acc, _, release, err := acquireScheduledAccount(r, "m", nil, a.Path, 1, "")
		if err != nil {
			result <- nil
			return
		}
		release()
		result <- acc
	}()
	time.Sleep(20 * time.Millisecond)
	held()
	select {
	case acc := <-result:
		if acc != a {
			t.Fatal("sticky request escaped before grace ended")
		}
	case <-ctx.Done():
		t.Fatal("slot release did not wake sticky waiter")
	}
}
