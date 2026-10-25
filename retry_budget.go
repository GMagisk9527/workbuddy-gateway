package main

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

var errRetryBudget = errors.New("额外上游尝试预算已用尽，请稍后重试")

type retryFileConfig struct {
	MaxExtraAttempts *int `json:"maxExtraAttempts"`
	MaxConcurrent    *int `json:"maxConcurrent"`
	RatioPercent     int  `json:"ratioPercent"`
	MinAllowance     int  `json:"minAllowance"`
	BackoffMaxMs     int  `json:"backoffMaxMs"`
}
type retryLimits struct {
	extra, concurrent, ratio, min int
	maxDelay                      time.Duration
}

func normalizeRetry(c retryFileConfig) (retryLimits, error) {
	l := retryLimits{extra: 4, concurrent: 2, ratio: 20, min: 2, maxDelay: 2 * time.Second}
	if c.MaxExtraAttempts != nil {
		l.extra = *c.MaxExtraAttempts
	}
	if c.MaxConcurrent != nil {
		l.concurrent = *c.MaxConcurrent
	}
	if c.RatioPercent != 0 {
		l.ratio = c.RatioPercent
	}
	if c.MinAllowance != 0 {
		l.min = c.MinAllowance
	}
	if c.BackoffMaxMs != 0 {
		l.maxDelay = time.Duration(c.BackoffMaxMs) * time.Millisecond
	}
	if l.extra < 0 || l.extra > 64 || l.concurrent < 0 || l.concurrent > 1024 || l.ratio < 1 || l.ratio > 100 || l.min < 1 || l.min > 1024 || c.BackoffMaxMs < 0 || c.BackoffMaxMs > 60000 {
		return l, errors.New("retry 配置无效：额外次数0～64，并发0～1024，比例1～100，最小额度1～1024，退避上限0～60000毫秒")
	}
	return l, nil
}

type retryAdmission struct {
	mu                sync.Mutex
	limits            retryLimits
	originals, active int
	denied            uint64
}

func newRetryAdmission() *retryAdmission {
	l, _ := normalizeRetry(retryFileConfig{})
	return &retryAdmission{limits: l}
}

var upstreamRetries = newRetryAdmission()

func (g *retryAdmission) configure(c retryFileConfig) {
	l, _ := normalizeRetry(c)
	g.mu.Lock()
	g.limits = l
	g.mu.Unlock()
}
func (g *retryAdmission) allowanceLocked() int {
	return min(g.limits.concurrent, g.limits.min+g.originals*g.limits.ratio/100)
}

type requestRetryScope struct {
	mu            sync.Mutex
	gate          *retryAdmission
	config        retryLimits
	begun, closed bool
	extras        int
}

func (g *retryAdmission) scope() *requestRetryScope {
	g.mu.Lock()
	defer g.mu.Unlock()
	return &requestRetryScope{gate: g, config: g.limits}
}
func (s *requestRetryScope) begin(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, context.Canceled
	}
	g := s.gate
	g.mu.Lock()
	if !s.begun {
		s.begun = true
		g.originals++
		g.mu.Unlock()
		s.mu.Unlock()
		return nil, nil
	}
	if s.extras >= s.config.extra || g.active >= g.allowanceLocked() {
		g.denied++
		g.mu.Unlock()
		s.mu.Unlock()
		return nil, errRetryBudget
	}
	s.extras++
	ordinal := s.extras
	g.active++
	g.mu.Unlock()
	s.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { g.mu.Lock(); g.active--; g.mu.Unlock() }) }
	delay := retryBackoffDelay(upstreamRetryBackoff, s.config.maxDelay, ordinal, rand.Float64())
	timer := time.NewTimer(delay)
	select {
	case <-timer.C:
	case <-ctx.Done():
		timer.Stop()
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}
func (s *requestRetryScope) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.begun {
		s.gate.mu.Lock()
		s.gate.originals--
		s.gate.mu.Unlock()
	}
}
func retryBackoffDelay(base, maximum time.Duration, ordinal int, random float64) time.Duration {
	d := min(max(base, 0), maximum)
	for i := 1; i < ordinal && d < maximum; i++ {
		if d > maximum/2 {
			d = maximum
		} else {
			d *= 2
		}
	}
	return time.Duration(float64(d) * (0.5 + 0.5*min(max(random, 0), 1)))
}

type requestRetryScopeKey struct{}

func retryScopeFor(r *http.Request) *requestRetryScope {
	s, _ := r.Context().Value(requestRetryScopeKey{}).(*requestRetryScope)
	return s
}
func withRetryScope(r *http.Request) (*http.Request, *requestRetryScope) {
	s := upstreamRetries.scope()
	return r.WithContext(context.WithValue(r.Context(), requestRetryScopeKey{}, s)), s
}
