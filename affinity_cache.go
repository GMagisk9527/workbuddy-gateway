package main

import "time"

type cacheAffinityHint struct {
	key        string
	inputBytes int
}

const cacheAffinityEvidenceTTL = 5 * time.Minute
const cacheAffinityMinBytes = 256 << 10
const cacheAffinityMinTokens = 32768

// Only successful upstream usage is evidence, never a local binding hit or a
// generated request ID. Evidence shares the existing bounded LRU and expiry.
func affinityObserveCache(key, account string, tokens int64, known, complete bool, now time.Time) {
	if key == "" || account == "" || !known || !complete || !currentAffinitySettings().Enabled {
		return
	}
	affinityStoreMu.RLock()
	defer affinityStoreMu.RUnlock()
	store := affinityStore
	store.mu.Lock()
	defer store.mu.Unlock()
	el := store.items[key]
	if el == nil {
		return
	}
	item := el.Value.(*affinityEntry)
	if item.account != account || now.Sub(time.Unix(0, item.at)) >= store.ttl {
		return
	}
	if tokens < cacheAffinityMinTokens {
		item.cacheTokens, item.cacheHits, item.cacheAt = 0, 0, 0
		return
	}
	if now.Sub(time.Unix(0, item.cacheAt)) >= cacheAffinityEvidenceTTL {
		item.cacheHits = 0
	}
	item.cacheHits = min(item.cacheHits+1, 2)
	item.cacheTokens = tokens
	item.cacheAt = now.UnixNano()
}

func cacheAffinityGrace(hint cacheAffinityHint, account string, base time.Duration, cfg schedulingFileConfig, now time.Time) time.Duration {
	extra := time.Duration(cfg.CacheAffinityWaitMs) * time.Millisecond
	if extra <= base || hint.inputBytes < cacheAffinityMinBytes || hint.key == "" || account == "" || !currentAffinitySettings().Enabled {
		return base
	}
	affinityStoreMu.RLock()
	defer affinityStoreMu.RUnlock()
	store := affinityStore
	store.mu.Lock()
	defer store.mu.Unlock()
	el := store.items[hint.key]
	if el == nil {
		return base
	}
	item := el.Value.(*affinityEntry)
	if item.account != account || item.cacheHits < 2 || item.cacheTokens < cacheAffinityMinTokens ||
		now.Sub(time.Unix(0, item.at)) >= store.ttl || now.Sub(time.Unix(0, item.cacheAt)) >= cacheAffinityEvidenceTTL {
		return base
	}
	return extra
}
