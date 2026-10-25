package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serveLifecycleTest(t *testing.T, life *gatewayLifecycle, handler http.Handler) (*http.Server, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &http.Server{Handler: life.handler(handler), BaseContext: func(net.Listener) context.Context { return life.ctx }}
	done := make(chan struct{})
	go func() { defer close(done); _ = s.Serve(l) }()
	t.Cleanup(func() { life.cancel(); _ = s.Close(); <-done })
	return s, "http://" + l.Addr().String()
}

func TestGatewayReadinessAndDrainAdmission(t *testing.T) {
	life := newGatewayLifecycle()
	defer life.cancel()
	calls := 0
	h := life.handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
	for _, method := range []string{"GET", "HEAD"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/readyz", nil))
		if w.Code != 200 || method == "HEAD" && w.Body.Len() != 0 {
			t.Fatal("readiness unavailable")
		}
	}
	life.beginDrain()
	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader("do not read")))
		if w.Code != 503 || w.Header().Get("Retry-After") != "1" || path == "/v1/messages" && !strings.Contains(w.Body.String(), `"overloaded_error"`) {
			t.Fatalf("draining request was admitted or wrong envelope: %d %s", w.Code, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "draining") || calls != 0 {
		t.Fatal("readiness or admission failed")
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health", nil))
	if calls != 1 {
		t.Fatal("liveness was disabled during drain")
	}
}

func TestGatewayShutdownLetsActiveReplyFinish(t *testing.T) {
	life := newGatewayLifecycle()
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s, url := serveLifecycleTest(t, life, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "complete")
		close(finished)
	}))
	clientDone := make(chan error, 1)
	go func() {
		resp, err := http.Get(url)
		if err == nil {
			var data []byte
			data, err = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(data) != "complete" {
				err = errors.New("reply cut short")
			}
		}
		clientDone <- err
	}()
	<-entered
	stopped := make(chan error, 1)
	go func() { stopped <- shutdownGatewayServers([]*http.Server{s}, life, 3*time.Second, time.Second) }()
	waitSettlementCondition(t, "drain", life.isDraining)
	select {
	case err := <-stopped:
		t.Fatalf("did not wait for reply: %v", err)
	default:
	}
	close(release)
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	<-finished
	if err := <-clientDone; err != nil {
		t.Fatal(err)
	}
}

func TestGatewayForcedShutdownWaitsForRealSettlement(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, key := mustCreateKey(t, keyCreateOptions{Name: "shutdown", QuotaTotal: 10})
	life := newGatewayLifecycle()
	entered := make(chan struct{})
	h := requestAuditMiddleware(authMiddleware(keyAdmissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*statsAttemptFor(r) = statsAttempt{id: "synthetic-account", attemptID: "shutdown-settlement", model: "synthetic", started: time.Now()}
		statsObserveUsage(r, map[string]any{"credit": 3.0}, false)
		close(entered)
		<-r.Context().Done()
	}))))
	s, url := serveLifecycleTest(t, life, h)
	req, _ := http.NewRequest("POST", url+"/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, _ := http.DefaultClient.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	ledger.mu.Lock()
	locked := true
	defer func() {
		if locked {
			ledger.mu.Unlock()
		}
	}()
	stopped := make(chan error, 1)
	go func() { stopped <- shutdownGatewayServers([]*http.Server{s}, life, 30*time.Millisecond, 3*time.Second) }()
	waitSettlementCondition(t, "forced cancel and settlement", func() bool {
		settledMu.Lock()
		defer settledMu.Unlock()
		_, ok := settledIDs["shutdown-settlement"]
		return ok
	})
	select {
	case err := <-stopped:
		t.Fatalf("shutdown escaped pending WAL: %v", err)
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
		t.Fatalf("shutdown lost or duplicated billing: %+v", usage)
	}
}

func TestGatewayShutdownReportsUnfinishedCleanup(t *testing.T) {
	life := newGatewayLifecycle()
	entered, release := make(chan struct{}), make(chan struct{})
	s, url := serveLifecycleTest(t, life, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release }))
	defer func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = life.wait(ctx)
	}()
	go func() {
		resp, _ := http.Get(url)
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	err := shutdownGatewayServers([]*http.Server{s}, life, 10*time.Millisecond, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stuck handler reported safe exit: %v", err)
	}
}
