package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type gatewayLifecycleKey struct{}
type gatewayLifecycle struct {
	mu       sync.Mutex
	draining bool
	active   int
	changed  chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

func newGatewayLifecycle() *gatewayLifecycle {
	l := &gatewayLifecycle{changed: make(chan struct{})}
	l.ctx, l.cancel = context.WithCancel(context.WithValue(context.Background(), gatewayLifecycleKey{}, l))
	return l
}
func lifecycleFor(r *http.Request) *gatewayLifecycle {
	l, _ := r.Context().Value(gatewayLifecycleKey{}).(*gatewayLifecycle)
	return l
}
func (l *gatewayLifecycle) isDraining() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.draining }
func (l *gatewayLifecycle) beginDrain()      { l.mu.Lock(); l.draining = true; l.mu.Unlock() }
func (l *gatewayLifecycle) enter() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.draining {
		return false
	}
	l.active++
	return true
}
func (l *gatewayLifecycle) leave() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	close(l.changed)
	l.changed = make(chan struct{})
}
func (l *gatewayLifecycle) wait(ctx context.Context) error {
	for {
		l.mu.Lock()
		active, changed := l.active, l.changed
		l.mu.Unlock()
		if active == 0 {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (l *gatewayLifecycle) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Real servers inherit l.ctx through BaseContext. Direct handler tests
		// still combine cancellation without discarding the caller's context.
		if lifecycleFor(r) != l {
			ctx, cancel := context.WithCancel(r.Context())
			stop := context.AfterFunc(l.ctx, cancel)
			defer stop()
			defer cancel()
			r = r.WithContext(context.WithValue(ctx, gatewayLifecycleKey{}, l))
		}
		if r.URL.Path == "/readyz" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeJSONError(w, http.StatusMethodNotAllowed, "就绪检查仅支持 GET/HEAD")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			status, body := http.StatusOK, `{"status":"ready"}`
			if l.isDraining() {
				status, body = http.StatusServiceUnavailable, `{"status":"draining"}`
			}
			w.WriteHeader(status)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(body + "\n"))
			}
			return
		}
		if r.URL.Path == "/health" || r.URL.Path == "/ping" {
			next.ServeHTTP(w, r)
			return
		}
		if !l.enter() {
			w.Header().Set("Retry-After", "1")
			writeUpstreamError(w, r, http.StatusServiceUnavailable, "gateway_draining", "网关正在停止接收新请求，请稍后重试")
			return
		}
		defer l.leave() // Includes outer audit and synchronous usage settlement.
		next.ServeHTTP(w, r)
	})
}

func shutdownGatewayServers(servers []*http.Server, life *gatewayLifecycle, grace, cleanup time.Duration) error {
	life.beginDrain()
	ctx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	var group sync.WaitGroup
	var shutdownFailed atomic.Bool
	for _, server := range servers {
		group.Go(func() {
			if err := server.Shutdown(ctx); err != nil {
				shutdownFailed.Store(true)
			}
		})
	}
	group.Wait()
	if shutdownFailed.Load() {
		log.Printf("[停机] 阶段=强制取消 说明=自然完成等待期限已到，取消在途请求并等待清理结算")
		life.cancel()
		for _, server := range servers {
			_ = server.Close()
		}
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanup)
	defer cleanupCancel()
	if err := life.wait(cleanupCtx); err != nil {
		return fmt.Errorf("停机清理等待超时，仍有 handler 未退出，结算收尾未确认: %w", err)
	}
	life.cancel()
	return nil
}
