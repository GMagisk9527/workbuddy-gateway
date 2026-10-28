package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCacheAffinityRequiresRecentRepeatedUpstreamEvidence(t *testing.T) {
	isolatePolicies(t)
	old := currentAffinitySettings()
	t.Cleanup(func() {
		setAffinityConfig(old.Enabled, old.Mode, int(old.TTL/time.Second), old.Capacity, old.PrefixHeadByte)
	})
	setAffinityConfig(true, "session", 1800, 8, 65536)
	now := time.Now()
	base := 250 * time.Millisecond
	cfg := schedulingFileConfig{CacheAffinityWaitMs: 1500}
	hint := cacheAffinityHint{key: "synthetic-cache-binding", inputBytes: 1 << 20}
	withAccounts(t, mkAccount("a"), mkAccount("b"))
	identity := affinityAccountIdentity("a")
	affinityRemember(hint.key, "a")
	check := func(want time.Duration, at time.Time) {
		t.Helper()
		if got := cacheAffinityGrace(hint, "a", base, cfg, at); got != want {
			t.Fatalf("grace=%v want=%v", got, want)
		}
	}
	check(base, now)
	affinityObserveCache(hint.key, "a", identity, 790016, true, true, now)
	check(base, now)
	affinityObserveCache(hint.key, "a", identity, 790016, true, true, now)
	check(1500*time.Millisecond, now)
	if got := cacheAffinityGrace(cacheAffinityHint{key: hint.key, inputBytes: 1024}, "a", base, cfg, now); got != base {
		t.Fatal("short request waited for cache")
	}
	if got := cacheAffinityGrace(hint, "b", base, cfg, now); got != base {
		t.Fatal("evidence crossed accounts")
	}
	check(base, now.Add(cacheAffinityEvidenceTTL))
	affinityObserveCache(hint.key, "a", identity, 790016, false, true, now.Add(time.Minute))
	affinityObserveCache(hint.key, "a", identity, 790016, true, false, now.Add(time.Minute))
	check(base, now.Add(cacheAffinityEvidenceTTL))
	affinityObserveCache(hint.key, "a", identity, 0, true, true, now)
	check(base, now)
	affinityObserveCache(hint.key, "a", identity, 790016, true, true, now)
	affinityObserveCache(hint.key, "a", identity, 790016, true, true, now)
	affinityRemember(hint.key, "b")
	affinityRemember(hint.key, "a")
	check(base, now)
}

func TestCacheAffinitySchedulerWaitsForProvenWarmAccount(t *testing.T) {
	isolatePolicies(t)
	old := currentAffinitySettings()
	t.Cleanup(func() {
		setAffinityConfig(old.Enabled, old.Mode, int(old.TTL/time.Second), old.Capacity, old.PrefixHeadByte)
	})
	setAffinityConfig(true, "session", 1800, 8, 65536)
	a, b := mkAccount("synthetic-cache-a.json"), mkAccount("synthetic-cache-b.json")
	withAccounts(t, a, b)
	modelConcurrency.configure(concurrentFileConfig{MaxAcc: 1})
	held, _, _ := modelConcurrency.tryAcquire(context.Background(), modelConcurrencyKey(a))
	defer held()
	now := time.Now()
	hint := cacheAffinityHint{key: "synthetic-warm-session", inputBytes: 1 << 20}
	affinityRemember(hint.key, a.Path)
	identity := affinityAccountIdentity(a.Path)
	affinityObserveCache(hint.key, a.Path, identity, 790016, true, true, now)
	affinityObserveCache(hint.key, a.Path, identity, 790016, true, true, now)
	setSchedulingConfig(schedulingFileConfig{AffinityWaitMs: 5, CacheAffinityWaitMs: 1000, WaitTimeoutMs: 2000})
	releaseTimer := time.AfterFunc(50*time.Millisecond, held)
	defer releaseTimer.Stop()
	selected, _, release, err := acquireScheduledAccount(httptest.NewRequest("POST", "/v1/messages", nil), "m", nil, a.Path, 1, "", hint)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if selected != a {
		t.Fatal("large cached session switched to the cold account")
	}
}

func TestCacheAffinityLearnsOnlyFromCompletedAttemptUsage(t *testing.T) {
	isolatePolicies(t)
	old := currentAffinitySettings()
	t.Cleanup(func() {
		setAffinityConfig(old.Enabled, old.Mode, int(old.TTL/time.Second), old.Capacity, old.PrefixHeadByte)
	})
	setAffinityConfig(true, "session", 1800, 8, 65536)
	account := mkAccount("synthetic-usage-cache.json")
	withAccounts(t, account)
	key := "synthetic-usage-binding"
	affinityRemember(key, account.Path)
	cfg := schedulingFileConfig{CacheAffinityWaitMs: 1500}
	base := 250 * time.Millisecond
	hint := cacheAffinityHint{key: key, inputBytes: 1 << 20}
	for i, complete := range []bool{false, true, true} {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		r = r.WithContext(context.WithValue(r.Context(), statsRequestContextKey{}, &statsAttempt{}))
		statsSelectAccount(r, account, "synthetic-cache-model")
		statsAttemptFor(r).cacheAffinityKey = key
		statsObserveUsage(r, map[string]any{"prompt_tokens": float64(800000), "prompt_cache_hit_tokens": float64(790016)}, complete)
		statsFinishAttempt(r, 200)
		want := base
		if i == 2 {
			want = 1500 * time.Millisecond
		}
		if got := cacheAffinityGrace(hint, account.Path, base, cfg, time.Now()); got != want {
			t.Fatalf("attempt %d grace=%v want=%v", i, got, want)
		}
	}
}
