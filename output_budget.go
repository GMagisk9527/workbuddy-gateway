package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
)

var errOutputBudget = errors.New("响应保留内容超过内存预算，本次回复不完整")

type outputFileConfig struct {
	MaxRequestBytes int64 `json:"maxRequestBytes"`
	MaxSharedBytes  int64 `json:"maxSharedBytes"`
}
type outputAdmission struct {
	mu       sync.Mutex
	config   outputFileConfig
	used     int64
	rejected uint64
	sample   func() memorySample
}

func newOutputAdmission() *outputAdmission {
	sampler := newSystemMemorySampler()
	return &outputAdmission{sample: sampler.snapshot}
}

var retainedOutputs = newOutputAdmission()

func (g *outputAdmission) configure(c outputFileConfig) { g.mu.Lock(); g.config = c; g.mu.Unlock() }
func (g *outputAdmission) limitsLocked(sample memorySample) (request, shared int64) {
	request, shared = 16<<20, 32<<20
	if n := sample.runtime.limit; n > 0 {
		request = min(request, int64(n/4))
		shared = min(shared, int64(n/2))
	}
	for _, scope := range sample.cgroups {
		if scope.limit > 0 {
			request = min(request, int64(scope.limit/8))
			shared = min(shared, int64(scope.limit/4))
		}
	}
	if g.config.MaxRequestBytes > 0 {
		request = g.config.MaxRequestBytes
	}
	if g.config.MaxSharedBytes > 0 {
		shared = g.config.MaxSharedBytes
	}
	return max(request, 1), max(shared, 1)
}

type outputLease struct {
	gate                  *outputAdmission
	used, reserved, limit int64 // owned by handler; global reservations guarded by gate.mu
	scratch               int64
	closed                bool
}

func (g *outputAdmission) lease() *outputLease { return &outputLease{gate: g} }
func (l *outputLease) add(n int) error {
	if l == nil || n <= 0 {
		return nil
	}
	if l.closed {
		return errOutputBudget
	}
	if l.limit > 0 && int64(n) <= l.reserved-l.used {
		l.used += int64(n)
		return nil
	}
	sample := l.gate.sample()
	g := l.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	request, shared := g.limitsLocked(sample)
	if l.limit == 0 {
		l.limit = request
	}
	if int64(n) > l.limit-l.used {
		g.rejected++
		return errOutputBudget
	}
	need := l.used + int64(n) - l.reserved
	available := min(l.limit-l.reserved, shared-g.used)
	if need > available {
		g.rejected++
		return errOutputBudget
	}
	block := min(int64(64<<10), max(l.limit/16, 1))
	reserve := min(max(need, block), available)
	g.used += reserve
	l.reserved += reserve
	l.used += int64(n)
	return nil
}
func (l *outputLease) close() {
	if l == nil || l.closed {
		return
	}
	l.closed = true
	l.gate.mu.Lock()
	l.gate.used -= l.reserved
	l.gate.mu.Unlock()
	l.reserved = 0
}

type outputLeaseKey struct{}

func outputFor(r *http.Request) *outputLease {
	if r == nil {
		return nil
	}
	l, _ := r.Context().Value(outputLeaseKey{}).(*outputLease)
	return l
}
func withOutputLease(r *http.Request) (*http.Request, *outputLease) {
	l := retainedOutputs.lease()
	return r.WithContext(context.WithValue(r.Context(), outputLeaseKey{}, l)), l
}
func outputForReader(reader any) *outputLease {
	if t, ok := reader.(*ttftReader); ok {
		return t.output
	}
	return nil
}
func marshalBudgetedOutput(l *outputLease, value any) ([]byte, error) {
	if l != nil {
		if l.closed {
			return nil, errOutputBudget
		}
		// Preflight escaping and envelope size without allocating the full JSON.
		if l.limit == 0 {
			if err := l.add(1); err != nil {
				return nil, err
			}
		}
		limit := int(min((l.limit-l.used+l.scratch)/2, int64(^uint(0)>>1)/2))
		size, err := measureConversionJSON(value, limit)
		if err != nil {
			var tooLarge *conversionSizeError
			if errors.As(err, &tooLarge) {
				l.gate.mu.Lock()
				l.gate.rejected++
				l.gate.mu.Unlock()
				return nil, errOutputBudget
			}
			return nil, err
		}
		if err := l.temp(2 * int64(size.plain)); err != nil {
			return nil, err
		}
	}
	return json.Marshal(value)
}

func (l *outputLease) temp(n int64) error {
	if l == nil {
		return nil
	}
	if l.closed || n < 0 || n > int64(^uint(0)>>1) {
		return errOutputBudget
	}
	if n <= l.scratch {
		return nil
	}
	if err := l.add(int(n - l.scratch)); err != nil {
		return err
	}
	l.scratch = n
	return nil
}
func (l *outputLease) remove(n int) {
	if l != nil {
		l.used -= min(int64(n), l.used-l.scratch)
	}
}

func reserveOutputDelta(l *outputLease, delta map[string]any, text, reasoning, escaped bool, known map[int]*mergedToolCall) error {
	if l == nil {
		return nil
	}
	n := 0
	size := func(v string) int {
		if !escaped {
			return len(v)
		}
		counter := conversionJSONCounter{limit: int(^uint(0) >> 1)}
		_ = counter.string(v)
		return max(counter.plain-2, 0)
	}
	if text {
		n += size(strOf(delta["content"])) + size(strOf(delta["refusal"]))
	}
	if reasoning {
		n += size(strOf(delta["reasoning_content"]))
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range calls {
			if tc, ok := raw.(map[string]any); ok {
				index := int(numOr0(tc["index"]))
				if known == nil || known[index] == nil {
					n += 512
				}
				n += len(strOf(tc["id"]))
				if fn, ok := tc["function"].(map[string]any); ok {
					n += len(strOf(fn["arguments"])) + len(strOf(fn["name"]))
				}
			}
		}
	}
	return l.add(n)
}
