package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"
)

type admissionFileConfig struct {
	MaxActive                  int   `json:"maxActive"`
	MaxPreparing               int   `json:"maxPreparing"`
	MaxWaiting                 *int  `json:"maxWaiting"`
	WaitTimeoutMs              int   `json:"waitTimeoutMs"`
	MemoryProtection           *bool `json:"memoryProtection"`
	MaxBodyBytes               int64 `json:"maxBodyBytes"`
	MemoryHighWatermarkPercent int   `json:"memoryHighWatermarkPercent"`
	MemoryLowWatermarkPercent  int   `json:"memoryLowWatermarkPercent"`
}
type admissionLimits struct {
	active, waiting int
	wait            time.Duration
}

func normalizeAdmission(c admissionFileConfig) admissionLimits {
	l := admissionLimits{active: c.MaxActive, waiting: 8, wait: time.Duration(c.WaitTimeoutMs) * time.Millisecond}
	if l.active <= 0 {
		l.active = 2
	}
	if c.MaxWaiting != nil {
		l.waiting = *c.MaxWaiting
	}
	if l.wait <= 0 {
		l.wait = 15 * time.Second
	}
	return l
}

var errAdmissionFull = errors.New("request admission queue full")
var errAdmissionTimeout = errors.New("request admission wait timed out")

type admissionTicket struct{ id byte }
type requestAdmission struct {
	mu             sync.Mutex
	memory         *requestMemoryAdmission
	limits         admissionLimits
	active         int
	waiters        []*admissionTicket
	changed        chan struct{}
	preparing      *requestAdmission
	preparingLimit int
}

func newRequestAdmission() *requestAdmission {
	g := &requestAdmission{limits: normalizeAdmission(admissionFileConfig{}), changed: make(chan struct{}), memory: newRequestMemoryAdmission()}
	g.preparing = &requestAdmission{limits: admissionLimits{active: 1024, waiting: 8, wait: 15 * time.Second}, changed: make(chan struct{}), memory: g.memory}
	return g
}

var ingressAdmission = newRequestAdmission()

func logGatewayControls(trace string) {
	l, active, waiting := ingressAdmission.snapshot()
	s := currentSchedulingConfig()
	enabled := s.LoadAware == nil || *s.LoadAware
	_, preparing, preparingWaiting := ingressAdmission.preparingSnapshot()
	log.Printf("[请求控制配置] traceId=%s 准入=%d/%d 排队=%d/%d 前处理=%d 前处理排队=%d 排队期限=%v 负载感知=%t 说明=总准入覆盖响应结束；可独立限制前处理；已有请求不因缩容中断", trace, active, l.active, waiting, l.waiting, preparing, preparingWaiting, l.wait, enabled)
	used, limit, pressured := ingressAdmission.memory.snapshot()
	log.Printf("[内存准入配置] traceId=%s 请求体预算=%d/%d字节 内存压力=%t 说明=预算包含压缩与解压后的输入；新请求受内存水位保护", trace, used, limit, pressured)
}

func (g *requestAdmission) notifyLocked() { close(g.changed); g.changed = make(chan struct{}) }
func (g *requestAdmission) configure(c admissionFileConfig) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.limits = normalizeAdmission(c)
	g.memory.configure(c)
	g.preparingLimit = c.MaxPreparing
	g.preparing.mu.Lock()
	g.preparing.limits = g.limits
	if c.MaxPreparing > 0 {
		g.preparing.limits.active = c.MaxPreparing
	} else {
		g.preparing.limits.active = 1024
	}
	g.preparing.notifyLocked()
	g.preparing.mu.Unlock()
	g.notifyLocked()
}

func (g *requestAdmission) preparingSnapshot() (limit, active, waiting int) {
	g.mu.Lock()
	limit = g.preparingLimit
	g.mu.Unlock()
	_, active, waiting = g.preparing.snapshot()
	return
}

type preparationContextKey struct{}
type preparationLease struct {
	once    sync.Once
	release func()
}

func (p *preparationLease) finish() {
	if p != nil {
		p.once.Do(p.release)
	}
}
func finishRequestPreparation(r *http.Request) {
	if p, _ := r.Context().Value(preparationContextKey{}).(*preparationLease); p != nil {
		p.finish()
	}
}
func (g *requestAdmission) snapshot() (admissionLimits, int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.limits, g.active, len(g.waiters)
}
func (g *requestAdmission) reserveLocked() func() {
	g.active++
	var once sync.Once
	return func() { once.Do(func() { g.mu.Lock(); defer g.mu.Unlock(); g.active--; g.notifyLocked() }) }
}

// acquire registers and checks admission under the same lock, so concurrent
// arrivals cannot overfill the queue or bypass a previously queued request.
func (g *requestAdmission) acquire(ctx context.Context) (func(), error) {
	g.mu.Lock()
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	if err := g.memory.checkAdmission(); err != nil {
		g.mu.Unlock()
		return nil, err
	}
	if g.active < g.limits.active && len(g.waiters) == 0 {
		release := g.reserveLocked()
		g.mu.Unlock()
		return release, nil
	}
	if len(g.waiters) >= g.limits.waiting {
		g.mu.Unlock()
		return nil, errAdmissionFull
	}
	// Each allocation carries one byte, avoiding Go's zero-size pointer aliasing.
	ticket := new(admissionTicket)
	g.waiters = append(g.waiters, ticket)
	wait := g.limits.wait
	g.mu.Unlock()
	ctxWait, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	remove := func() {
		for i, t := range g.waiters {
			if t == ticket {
				g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
				g.notifyLocked()
				break
			}
		}
	}
	for {
		g.mu.Lock()
		if err := ctxWait.Err(); err != nil {
			remove()
			g.mu.Unlock()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errAdmissionTimeout
		}
		if g.active < g.limits.active && len(g.waiters) > 0 && g.waiters[0] == ticket {
			if err := g.memory.checkAdmission(); err != nil {
				remove()
				g.mu.Unlock()
				return nil, err
			}
			g.waiters = g.waiters[1:]
			release := g.reserveLocked()
			g.notifyLocked()
			g.mu.Unlock()
			return release, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctxWait.Done():
		case <-changed:
		}
	}
}

func requestAdmissionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		performanceFor(r).setLivePhase(liveAdmission)
		gate := ingressAdmission
		release, err := gate.acquire(r.Context())
		if err != nil {
			recordPerformance(r, phaseAdmission, started, true)
			if writeRequestMemoryError(w, r, err) {
				return
			}
			code := "request_queue_full"
			message := "请求队列已满，请稍后重试"
			if errors.Is(err, errAdmissionTimeout) {
				code = "request_queue_timeout"
				message = "等待请求处理名额超时，请稍后重试"
			}
			status := http.StatusTooManyRequests
			if r.Context().Err() != nil {
				status = http.StatusRequestTimeout
				code = "request_canceled"
				message = "请求已取消，未读取请求体"
			}
			w.Header().Set("Retry-After", "1")
			log.Printf("[请求准入] traceId=%s 结果=拒绝 原因=%s 等待=%v 说明=未读取请求体，未调用上游", w.Header().Get("X-Trace-ID"), code, time.Since(started))
			writeUpstreamError(w, r, status, code, message)
			return
		}
		defer release()
		gLimit, _, _ := gate.preparingSnapshot()
		if gLimit > 0 {
			limits, _, _ := gate.snapshot()
			ctx, cancel := context.WithDeadline(r.Context(), started.Add(limits.wait))
			prepareRelease, prepareErr := gate.preparing.acquire(ctx)
			cancel()
			if prepareErr != nil {
				recordPerformance(r, phaseAdmission, started, true)
				if !writeRequestMemoryError(w, r, prepareErr) {
					w.Header().Set("Retry-After", "1")
					status, code := http.StatusTooManyRequests, "request_prepare_queue_full"
					if r.Context().Err() != nil {
						status, code = http.StatusRequestTimeout, "request_canceled"
					}
					writeUpstreamError(w, r, status, code, "等待前处理名额超时、取消或队列已满，未调用上游")
				}
				return
			}
			lease := &preparationLease{release: prepareRelease}
			defer lease.finish()
			r = r.WithContext(context.WithValue(r.Context(), preparationContextKey{}, lease))
		}
		recordPerformance(r, phaseAdmission, started, false)
		performanceFor(r).setLivePhase(livePreparing)
		lease := &requestMemoryLease{gate: gate.memory}
		defer lease.release()
		r = r.WithContext(context.WithValue(r.Context(), requestMemoryContextKey{}, lease))
		if r.Context().Err() != nil {
			writeUpstreamError(w, r, http.StatusRequestTimeout, "request_canceled", "请求已取消，未读取请求体")
			return
		}
		next.ServeHTTP(w, r)
	})
}
