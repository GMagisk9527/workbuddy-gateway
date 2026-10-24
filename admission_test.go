package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func waitAdmissionQueue(t *testing.T, g *requestAdmission, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		_, _, waiting := g.snapshot()
		if waiting == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting=%d want=%d", waiting, n)
		}
		time.Sleep(time.Millisecond)
	}
}
func TestAdmissionBoundedFIFOAndCancellation(t *testing.T) {
	g := newRequestAdmission()
	one := 1
	g.configure(admissionFileConfig{MaxActive: 1, MaxWaiting: &one, WaitTimeoutMs: 1000})
	first, err := g.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := make(chan error, 1)
	go func() {
		release, err := g.acquire(ctx)
		if release != nil {
			release()
		}
		second <- err
	}()
	waitAdmissionQueue(t, g, 1)
	if release, err := g.acquire(context.Background()); !errors.Is(err, errAdmissionFull) {
		if release != nil {
			release()
		}
		t.Fatalf("unbounded queue: %v", err)
	}
	cancel()
	if err := <-second; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	waitAdmissionQueue(t, g, 0)
	first()
	first()
	_, active, _ := g.snapshot()
	if active != 0 {
		t.Fatalf("release repeated: active=%d", active)
	}
}
func TestAdmissionHotResizePreservesInflight(t *testing.T) {
	g := newRequestAdmission()
	g.configure(admissionFileConfig{MaxActive: 1})
	first, _ := g.acquire(context.Background())
	defer first()
	result := make(chan func(), 1)
	go func() {
		release, err := g.acquire(context.Background())
		if err != nil {
			result <- nil
			return
		}
		result <- release
	}()
	waitAdmissionQueue(t, g, 1)
	g.configure(admissionFileConfig{MaxActive: 2})
	var second func()
	select {
	case second = <-result:
	case <-time.After(time.Second):
		t.Fatal("expansion did not wake queued request")
	}
	if second == nil {
		t.Fatal("acquire failed")
	}
	defer second()
	g.configure(admissionFileConfig{MaxActive: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if release, err := g.acquire(ctx); err == nil {
		release()
		t.Fatal("shrink bypassed existing occupants")
	}
	first()
	second()
	_, active, waiting := g.snapshot()
	if active != 0 || waiting != 0 {
		t.Fatalf("leaked: active=%d waiting=%d", active, waiting)
	}
}
func TestAdmissionAuthenticationPrecedesBodyAndQueue(t *testing.T) {
	isolatePolicies(t)
	if err := applyGatewayConfig(gatewayFileConfig{APIKey: "synthetic-valid-key", APIKeyEnabled: true}); err != nil {
		t.Fatal(err)
	}
	ingressAdmission.configure(admissionFileConfig{MaxActive: 1})
	release, _ := ingressAdmission.acquire(context.Background())
	defer release()
	body := &admissionCountBody{Reader: strings.NewReader(`{}`)}
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Body = body
	r.Header.Set("Authorization", "Bearer synthetic-invalid-key")
	w := httptest.NewRecorder()
	newAPIHandler().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || body.reads.Load() != 0 {
		t.Fatalf("authentication order changed: %d reads=%d", w.Code, body.reads.Load())
	}
	_, _, waiting := ingressAdmission.snapshot()
	if waiting != 0 {
		t.Fatal("unauthorized request entered queue")
	}
}
func TestGatewayControlRuntimeConfigValidation(t *testing.T) {
	isolatePolicies(t)
	for _, raw := range []string{`{"admission":{"maxActive":-1}}`, `{"admission":{"maxWaiting":-1}}`, `{"admission":{"waitTimeoutMs":600001}}`, `{"scheduling":{"affinityWaitMs":-1}}`, `{"scheduling":{"waitTimeoutMs":600001}}`, `{"admission":{"maxWaitng":1}}`} {
		if _, err := parseRuntimeConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid control config accepted: %s", raw)
		}
	}
	c, err := parseRuntimeConfig([]byte(`{"admission":{"maxActive":3,"maxWaiting":0},"scheduling":{"affinityWaitMs":10}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := applyRuntimeConfig(c, true, ""); err != nil {
		t.Fatal(err)
	}
	limits, _, _ := ingressAdmission.snapshot()
	if limits.active != 3 || limits.waiting != 0 || currentSchedulingConfig().AffinityWaitMs != 10 {
		t.Fatal("control settings not hot loaded")
	}
}

func TestAdmissionFIFOOrderAndPanicRelease(t *testing.T) {
	g := newRequestAdmission()
	g.configure(admissionFileConfig{MaxActive: 1})
	first, _ := g.acquire(context.Background())
	defer first()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	order := make(chan int, 2)
	start := func(id int) {
		go func() {
			release, err := g.acquire(ctx)
			if err != nil {
				order <- -1
				return
			}
			order <- id
			release()
		}()
	}
	start(1)
	waitAdmissionQueue(t, g, 1)
	start(2)
	waitAdmissionQueue(t, g, 2)
	first()
	if a, b := <-order, <-order; a != 1 || b != 2 {
		t.Fatalf("FIFO order=%d,%d", a, b)
	}
	isolatePolicies(t)
	h := requestAdmissionMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("synthetic panic") }))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("missing panic fixture")
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/messages", nil))
	}()
	_, active, waiting := ingressAdmission.snapshot()
	if active != 0 || waiting != 0 {
		t.Fatalf("panic leaked permit: %d,%d", active, waiting)
	}
}
