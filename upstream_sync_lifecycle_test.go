package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestUpstreamSyncHotSwitchPreservesReadinessAndDrain(t *testing.T) {
	isolatedGatewayConfig(t)
	cfg.Addr, cfg.WebUI, cfg.PortExplicit = "127.0.0.1", true, false
	cfg.Port, cfg.WebPort = freeLoopbackPort(t), freeLoopbackPort(t)
	life := newGatewayLifecycle()
	defer life.cancel()
	servers, listeners, err := prepareGatewayServersWithLifecycle(life)
	if err != nil {
		t.Fatal(err)
	}
	var manager gatewayServerManager
	manager.register(servers, listeners, life)
	manager.start(make(chan error, 4))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		manager.shutdown(ctx)
	})
	apiPort, webPort := freeLoopbackPort(t), freeLoopbackPort(t)
	if _, _, err := manager.apply(gatewayFileConfig{APIPort: apiPort, WebPort: webPort}, apiPort, true); err != nil {
		t.Fatal(err)
	}
	for _, slot := range []*gatewayHTTPSlot{manager.api, manager.web} {
		if slot.server.BaseContext == nil || slot.server.BaseContext(slot.listener) != life.ctx {
			t.Error("new listener lost the shared cancellation context")
		}
	}
	client := &http.Client{Timeout: time.Second}
	for _, draining := range []bool{false, true} {
		if draining {
			life.beginDrain()
		}
		for _, port := range []int{apiPort, webPort} {
			response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/readyz")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			want := http.StatusOK
			if draining {
				want = http.StatusServiceUnavailable
			}
			if response.StatusCode != want {
				t.Errorf("hot-switched port %d lost shared readiness: draining=%t status=%d want=%d", port, draining, response.StatusCode, want)
			}
		}
	}
	nextAPI := freeLoopbackPort(t)
	if _, _, err := manager.apply(gatewayFileConfig{APIPort: nextAPI, WebPort: webPort}, nextAPI, true); err == nil {
		t.Error("hot switch accepted after process entered drain")
	}
}

func TestUpstreamSyncShutdownSettlesRetiredRequest(t *testing.T) {
	isolatedGatewayConfig(t)
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, key := mustCreateKey(t, keyCreateOptions{Name: "retired-listener", QuotaTotal: 10})
	life := newGatewayLifecycle()
	defer life.cancel()
	entered := make(chan struct{})
	handler := requestAuditMiddleware(authMiddleware(keyAdmissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*statsAttemptFor(r) = statsAttempt{id: "synthetic-account", attemptID: "retired-listener-settlement", model: "synthetic", started: time.Now()}
		statsObserveUsage(r, map[string]any{"credit": 3.0}, false)
		close(entered)
		<-r.Context().Done()
	}))))
	cfg.Addr, cfg.WebUI = "127.0.0.1", false
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: life.handler(handler), BaseContext: func(net.Listener) context.Context { return life.ctx }}
	var manager gatewayServerManager
	manager.register([]*http.Server{server}, []net.Listener{listener}, life)
	manager.start(make(chan error, 4))
	t.Cleanup(func() {
		life.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		manager.shutdown(ctx)
		_ = server.Close()
	})
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		req, _ := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		response, _ := http.DefaultClient.Do(req)
		if response != nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("synthetic billable handler did not start")
	}
	nextAPI := freeLoopbackPort(t)
	if _, _, err := manager.apply(gatewayFileConfig{APIPort: nextAPI}, nextAPI, false); err != nil {
		t.Fatal(err)
	}
	all := manager.takeServers()
	defer func() {
		for _, server := range all {
			_ = server.Close()
		}
	}()
	if len(all) != 2 {
		t.Fatalf("shutdown snapshot omitted current/retired servers: %d", len(all))
	}
	ledger.mu.Lock()
	locked := true
	defer func() {
		if locked {
			ledger.mu.Unlock()
		}
	}()
	stopped := make(chan error, 1)
	go func() { stopped <- shutdownGatewayServers(all, life, 30*time.Millisecond, 3*time.Second) }()
	waitSettlementCondition(t, "retired request entered settlement", func() bool {
		settledMu.Lock()
		defer settledMu.Unlock()
		_, exists := settledIDs["retired-listener-settlement"]
		return exists
	})
	select {
	case err := <-stopped:
		t.Fatalf("shutdown escaped pending retired-listener WAL: %v", err)
	default:
	}
	ledger.mu.Unlock()
	locked = false
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	<-requestDone
	key.mu.Lock()
	usage := key.usage
	key.mu.Unlock()
	if usage.Total != 3 || usage.Requests != 1 {
		t.Fatalf("retired-listener settlement lost/duplicated usage: %+v", usage)
	}
	for _, address := range []string{listener.Addr().String(), "127.0.0.1:" + strconv.Itoa(nextAPI)} {
		if conn, err := net.DialTimeout("tcp", address, time.Second); err == nil {
			_ = conn.Close()
			t.Errorf("listener remained open after shutdown: %s", address)
		}
	}
}
