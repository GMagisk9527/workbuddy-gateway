package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const firstTokenTimeoutDefault = 300 * time.Second

// Protected by upstreamConfigMu. Each logical request snapshots this value.
var firstTokenTimeout = firstTokenTimeoutDefault

var errFirstTokenTimeout = errors.New("等待上游首个有效增量超时，本次回复不完整")

func normalizeFirstTokenTimeout(seconds *int) (time.Duration, error) {
	if seconds == nil {
		return firstTokenTimeoutDefault, nil
	}
	if *seconds < 0 || *seconds > 3600 {
		return 0, errors.New("upstream.firstTokenTimeoutSeconds 必须为0～3600秒，0关闭，省略默认300秒")
	}
	return time.Duration(*seconds) * time.Second, nil
}

func currentFirstTokenTimeout() time.Duration {
	upstreamConfigMu.RLock()
	defer upstreamConfigMu.RUnlock()
	return firstTokenTimeout
}

type firstTokenBudget struct {
	mu              sync.Mutex
	timeout         time.Duration
	ctx             context.Context
	cancel          context.CancelCauseFunc
	timer           *time.Timer
	finished, armed bool
	expired         atomic.Bool
}
type firstTokenBudgetKey struct{}

func withFirstTokenBudget(r *http.Request) (*http.Request, *firstTokenBudget) {
	ctx, cancel := context.WithCancelCause(r.Context())
	b := &firstTokenBudget{ctx: ctx, cancel: cancel, timeout: currentFirstTokenTimeout()}
	return r.WithContext(context.WithValue(ctx, firstTokenBudgetKey{}, b)), b
}
func firstTokenFor(r *http.Request) *firstTokenBudget {
	if r == nil {
		return nil
	}
	b, _ := r.Context().Value(firstTokenBudgetKey{}).(*firstTokenBudget)
	return b
}
func (b *firstTokenBudget) arm() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timeout <= 0 || b.armed || b.finished || b.ctx.Err() != nil {
		return
	}
	b.armed = true
	b.timer = time.AfterFunc(b.timeout, func() {
		b.mu.Lock()
		if !b.finished {
			b.expired.Store(true)
			b.cancel(errFirstTokenTimeout)
		}
		b.mu.Unlock()
	})
}
func (b *firstTokenBudget) err() error {
	if b != nil && b.expired.Load() {
		return errFirstTokenTimeout
	}
	return nil
}

// A real increment or completed upstream response ends this one-shot budget.
func (b *firstTokenBudget) finish() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.err(); err != nil {
		return err
	}
	b.finished = true
	if b.timer != nil {
		b.timer.Stop()
	}
	return nil
}
func (b *firstTokenBudget) close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.finished = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	b.cancel(nil)
}
func firstTokenForReader(reader any) *firstTokenBudget {
	if t, ok := reader.(*ttftReader); ok {
		return t.firstToken
	}
	return nil
}
func writeFirstTokenTimeout(w http.ResponseWriter, r *http.Request, err error) bool {
	if !errors.Is(err, errFirstTokenTimeout) && firstTokenFor(r).err() == nil {
		return false
	}
	writeUpstreamError(w, r, http.StatusGatewayTimeout, "upstream_first_token_timeout", errFirstTokenTimeout.Error())
	return true
}
