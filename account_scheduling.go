package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
)

type schedulingFileConfig struct {
	LoadAware           *bool `json:"loadAware"`
	AffinityWaitMs      int   `json:"affinityWaitMs"`
	WaitTimeoutMs       int   `json:"waitTimeoutMs"`
	CacheAffinityWaitMs int   `json:"cacheAffinityWaitMs"`
}

var schedulingMu sync.RWMutex

// Serialize candidate lookup and reservation, never network calls or waiting.
// Concurrent arrivals must observe permits claimed by the preceding selection.
var schedulingAcquireMu sync.Mutex
var schedulingConfig schedulingFileConfig

func setSchedulingConfig(c schedulingFileConfig) {
	schedulingMu.Lock()
	schedulingConfig = c
	schedulingMu.Unlock()
}
func currentSchedulingConfig() schedulingFileConfig {
	schedulingMu.RLock()
	defer schedulingMu.RUnlock()
	return schedulingConfig
}

var errSchedulingTimeout = errors.New("model scheduling wait timed out")

// Account eligibility and hard concurrency admission remain independent checks.
// A busy sticky account gets a short grace period, then the same policy tier is
// searched by load. No upstream request is sent while scheduling is waiting.
func acquireScheduledAccount(r *http.Request, model string, attempted map[*Account]bool, prefer string, reqID uint64, trace string, hints ...cacheAffinityHint) (*Account, accountSelectionKind, func(), error) {
	cfgSchedule := currentSchedulingConfig()
	if cfgSchedule.LoadAware != nil && !*cfgSchedule.LoadAware {
		a, k, err := nextAccountForModelPrefer(model, attempted, prefer)
		if err != nil {
			return nil, "", nil, err
		}
		release, err := acquireModelConcurrency(r, a, reqID, trace)
		return a, k, release, err
	}
	grace := time.Duration(cfgSchedule.AffinityWaitMs) * time.Millisecond
	if grace <= 0 {
		grace = 250 * time.Millisecond
	}
	wait := time.Duration(cfgSchedule.WaitTimeoutMs) * time.Millisecond
	if wait <= 0 {
		wait = 15 * time.Second
	}
	if len(hints) > 0 {
		grace = cacheAffinityGrace(hints[0], prefer, grace, cfgSchedule, time.Now())
	}
	grace = min(grace, wait)
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	if attempted == nil {
		attempted = make(map[*Account]bool)
	}
	limiter := modelConcurrency
	started := time.Now()
	stickyUntil := started.Add(grace)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	waitLogged := false
	for {
		if err := r.Context().Err(); err != nil {
			return nil, "", nil, err
		}
		if err := ctx.Err(); err != nil {
			if r.Context().Err() != nil {
				return nil, "", nil, r.Context().Err()
			}
			return nil, "", nil, errSchedulingTimeout
		}
		sticky := prefer != "" && time.Now().Before(stickyUntil)
		preferred := ""
		if sticky {
			preferred = prefer
		}
		schedulingAcquireMu.Lock()
		picked := &accountPickTrace{}
		// A valid binding takes precedence inside the picker; fallback candidates
		// are load-aware within the same site and eligibility policy in one pass.
		a, kind, err := pickAccountForModel(model, attempted, preferred, true, false, picked)
		if err != nil {
			schedulingAcquireMu.Unlock()
			return nil, "", nil, err
		}
		if sticky && a.Path != prefer {
			// Keep the picker's policy-compliant fallback, without a grace period.
			sticky = false
		}
		release, changed, err := limiter.tryAcquire(ctx, modelConcurrencyKey(a))
		if err != nil {
			schedulingAcquireMu.Unlock()
			if r.Context().Err() != nil {
				return nil, "", nil, r.Context().Err()
			}
			return nil, "", nil, errSchedulingTimeout
		}
		if release != nil {
			accountMu.Lock()
			current, usable := queuedAccountUsableLocked(a, model, kind, time.Now())
			if usable && current == selectionProbeExhausted {
				state := modelStateLocked(a, model)
				if time.Now().Before(state.NextProbeAt) {
					usable = false
				} else {
					state.NextProbeAt = time.Now().Add(modelProbeDelay)
				}
			}
			if usable {
				for i, acc := range accounts {
					if acc == a {
						rrIndex = (i + 1) % len(accounts)
						break
					}
				}
			}
			accountMu.Unlock()
			if !usable {
				release()
				// Eligibility may change after lookup. Do not repeatedly reserve
				// the same rejected account for the rest of this request.
				attempted[a] = true
				schedulingAcquireMu.Unlock()
				continue
			}
			if picked.expiring {
				creditPicked.Add(1)
			}
			schedulingAcquireMu.Unlock()
			var once sync.Once
			finish := func() {
				once.Do(func() {
					release()
					log.Printf("[模型并发] traceId=%s requestId=%d 阶段=释放 账号=%s", trace, reqID, a.Path)
				})
			}
			log.Printf("[模型并发] traceId=%s requestId=%d 阶段=准入 账号=%s 等待=%v 亲和回退=%t", trace, reqID, a.Path, time.Since(started), prefer != "" && a.Path != prefer)
			return a, current, finish, nil
		}
		schedulingAcquireMu.Unlock()
		if !waitLogged {
			log.Printf("[模型并发] traceId=%s requestId=%d 阶段=排队 说明=等待名额或亲和窗口后重新选号", trace, reqID)
			waitLogged = true
		}
		var graceC <-chan time.Time
		var timer *time.Timer
		if sticky {
			timer = time.NewTimer(time.Until(stickyUntil))
			graceC = timer.C
		}
		select {
		case <-ctx.Done():
		case <-changed:
		case <-ticker.C:
		case <-graceC:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}
