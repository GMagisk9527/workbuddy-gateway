package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
)

var errRequestMemoryPressure = errors.New("request admission memory pressure")
var errRequestBodyBudget = errors.New("in-flight request body budget exhausted")

// Byte reservations describe retained input, not the entire Go heap. The live
// memory watermark also accounts for JSON trees, conversion buffers and streams.
type requestMemoryAdmission struct {
	mu           sync.Mutex
	enabled      bool
	maxBodyBytes int64
	high, low    uint64
	used         int64
	pressured    bool
	sample       func() memorySample
}

func newRequestMemoryAdmission() *requestMemoryAdmission {
	sampler := newSystemMemorySampler()
	g := &requestMemoryAdmission{sample: sampler.snapshot}
	g.configure(admissionFileConfig{})
	return g
}

func (g *requestMemoryAdmission) configure(c admissionFileConfig) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.enabled = c.MemoryProtection == nil || *c.MemoryProtection
	g.maxBodyBytes = c.MaxBodyBytes
	g.high, g.low = 85, 75
	if c.MemoryHighWatermarkPercent > 0 {
		g.high = uint64(c.MemoryHighWatermarkPercent)
	}
	if c.MemoryLowWatermarkPercent > 0 {
		g.low = uint64(c.MemoryLowWatermarkPercent)
	}
	if !g.enabled {
		g.pressured = false
	}
}

func (g *requestMemoryAdmission) bodyLimitLocked(sample memorySample) int64 {
	if !g.enabled {
		return 1<<63 - 1
	}
	if g.maxBodyBytes > 0 {
		return g.maxBodyBytes
	}
	limit := uint64(64 << 20)
	if sample.runtime.limit > 0 {
		limit = min(limit, sample.runtime.limit/4)
	}
	for _, scope := range sample.cgroups {
		if scope.limit > 0 {
			limit = min(limit, scope.limit/8)
		}
	}
	return int64(max(limit, 1))
}

func (g *requestMemoryAdmission) checkAdmission() error {
	sample := g.sample()
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.enabled {
		return nil
	}
	high, low := sample.watermarks(g.high, g.low)
	if high {
		g.pressured = true
	} else if low {
		g.pressured = false
	}
	if g.pressured {
		return errRequestMemoryPressure
	}
	if g.used >= g.bodyLimitLocked(sample) {
		return errRequestBodyBudget
	}
	return nil
}

func (g *requestMemoryAdmission) snapshot() (used, limit int64, pressured bool) {
	sample := g.sample()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.used, g.bodyLimitLocked(sample), g.pressured
}

type requestMemoryContextKey struct{}

// A lease outlives Body.Close: decoded JSON and upstream request copies may
// remain reachable until the handler (including its SSE stream) has returned.
type requestMemoryLease struct {
	gate   *requestMemoryAdmission
	bytes  int64 // guarded by gate.mu
	closed bool
}

func memoryLeaseFor(r *http.Request) *requestMemoryLease {
	l, _ := r.Context().Value(requestMemoryContextKey{}).(*requestMemoryLease)
	return l
}

func (l *requestMemoryLease) release() {
	l.gate.mu.Lock()
	defer l.gate.mu.Unlock()
	if !l.closed {
		l.gate.used -= l.bytes
		l.bytes = 0
		l.closed = true
	}
}

func (l *requestMemoryLease) reader(reader io.Reader, length int64) (io.Reader, error) {
	if length < 0 {
		length = 0
	}
	sample := l.gate.sample()
	l.gate.mu.Lock()
	defer l.gate.mu.Unlock()
	limit := l.gate.bodyLimitLocked(sample)
	if length > limit-l.bytes {
		return nil, errRequestBodyTooLarge
	}
	if length > limit-l.gate.used {
		return nil, errRequestBodyBudget
	}
	l.gate.used += length
	l.bytes += length
	return &memoryBudgetReader{reader: reader, lease: l, credit: length}, nil
}

type memoryBudgetReader struct {
	reader io.Reader
	lease  *requestMemoryLease
	credit int64
	err    error
}

func (b *memoryBudgetReader) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	g := b.lease.gate
	sample := g.sample()
	g.mu.Lock()
	if b.lease.closed {
		g.mu.Unlock()
		return 0, context.Canceled
	}
	limit := g.bodyLimitLocked(sample)
	available := max(limit-g.used, 0)
	want := min(int64(len(p)), b.credit+available)
	if want == 0 {
		// At the exact boundary an EOF probe must still succeed. At most one
		// byte is read, never retained or passed to the JSON decoder on overflow.
		budgetErr := errRequestBodyBudget
		if b.lease.bytes >= limit {
			budgetErr = errRequestBodyTooLarge
		}
		g.mu.Unlock()
		var probe [1]byte
		n, err := b.reader.Read(probe[:])
		if n > 0 {
			b.err = budgetErr
			return 0, b.err
		}
		return 0, err
	}
	extra := max(want-b.credit, 0)
	g.used += extra
	b.lease.bytes += extra
	g.mu.Unlock()
	n, err := b.reader.Read(p[:int(want)])
	consumedCredit := min(int64(n), b.credit)
	b.credit -= consumedCredit
	unused := extra - (int64(n) - consumedCredit)
	if unused > 0 {
		g.mu.Lock()
		g.used -= unused
		b.lease.bytes -= unused
		g.mu.Unlock()
	}
	return n, err
}

type memoryBudgetBody struct {
	io.Reader
	io.Closer
}

func wrapMemoryBody(r *http.Request) error {
	l := memoryLeaseFor(r)
	if l == nil {
		return nil
	}
	reader, err := l.reader(r.Body, r.ContentLength)
	if err != nil {
		return err
	}
	r.Body = &memoryBudgetBody{Reader: reader, Closer: r.Body}
	return nil
}

func writeRequestMemoryError(w http.ResponseWriter, r *http.Request, err error) bool {
	code, message := "", ""
	switch {
	case errors.Is(err, errRequestMemoryPressure):
		code, message = "request_memory_pressure", "服务内存压力较高，请稍后重试"
	case errors.Is(err, errRequestBodyBudget):
		code, message = "request_body_budget_exceeded", "在途请求体预算已满，请稍后重试"
	case errors.Is(err, errRequestBodyTooLarge):
		writeUpstreamError(w, r, http.StatusRequestEntityTooLarge, "request_body_too_large", "请求体超过网关大小或内存预算上限")
		return true
	default:
		return false
	}
	w.Header().Set("Retry-After", "1")
	log.Printf("[内存准入] traceId=%s 结果=拒绝 原因=%s 说明=未调用上游", w.Header().Get("X-Trace-ID"), code)
	writeUpstreamError(w, r, http.StatusServiceUnavailable, code, message)
	return true
}
