package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAdmissionFairPerKeyBoundAndHotDisable(t *testing.T) {
	g := newRequestAdmission()
	g.configure(admissionFileConfig{MaxActive: 1, MaxWaitingPerKey: 1, WaitTimeoutMs: 2000})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	held, _ := g.acquire(fairContext(ctx, "A"))
	defer held()
	done := make(chan error, 3)
	start := func(id string) {
		go func() {
			release, err := g.acquire(fairContext(ctx, id))
			if release != nil {
				release()
			}
			done <- err
		}()
	}
	start("A")
	waitAdmissionQueue(t, g, 1)
	if release, err := g.acquire(fairContext(ctx, "A")); !errors.Is(err, errAdmissionFull) {
		if release != nil {
			release()
		}
		t.Fatalf("per-Key queue unbounded: %v", err)
	}
	start("B")
	waitAdmissionQueue(t, g, 2)
	no := false
	g.configure(admissionFileConfig{MaxActive: 1, FairQueue: &no, MaxWaitingPerKey: 1})
	start("A")
	waitAdmissionQueue(t, g, 3)
	cancel()
	for i := 0; i < 3; i++ {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel=%v", err)
		}
	}
	held()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.flowActive) != 0 || len(g.waiters) != 0 || g.active != 0 {
		t.Fatal("idle caller state retained")
	}
}

func TestAdmissionFairExistingAndNewConfig(t *testing.T) {
	initial := newRequestAdmission()
	if !initial.fair || !initial.preparing.fair || initial.perKeyWaiting != 4 {
		t.Fatal("constructor defaults differ from runtime defaults")
	}
	for _, raw := range []string{`{}`, `{"admission":{"fairQueue":false,"maxWaitingPerKey":2}}`} {
		c, err := parseRuntimeConfig([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		g := newRequestAdmission()
		g.configure(c.Admission)
		if g.fair != (c.Admission.FairQueue == nil || *c.Admission.FairQueue) {
			t.Fatal("fair config ignored")
		}
	}
	for _, raw := range []string{`{"admission":{"maxWaitingPerKey":-1}}`, `{"admission":{"maxWaitingPerKey":10001}}`} {
		if _, err := parseRuntimeConfig([]byte(raw)); err == nil {
			t.Fatal("invalid per-Key bound accepted")
		}
	}
}

func fairContext(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, keyIdentityContextKey{}, &keyIdentity{ID: key})
}

func TestAdmissionFairRotatesKeyQueues(t *testing.T) {
	for _, preparation := range []bool{false, true} {
		t.Run(map[bool]string{false: "total", true: "preparing"}[preparation], func(t *testing.T) {
			g := newRequestAdmission()
			g.configure(admissionFileConfig{MaxActive: 1, MaxPreparing: 1, WaitTimeoutMs: 2000})
			if preparation {
				g = g.preparing
			}
			ctx, cancel := context.WithCancel(context.Background())
			type entry struct {
				id      string
				release func()
				err     error
			}
			entries := make(chan entry, 6)
			var wg sync.WaitGroup
			t.Cleanup(func() {
				cancel()
				wg.Wait()
				close(entries)
				for e := range entries {
					if e.release != nil {
						e.release()
					}
				}
			})
			held, err := g.acquire(fairContext(ctx, "initial"))
			if err != nil {
				t.Fatal(err)
			}
			defer held()
			for i, id := range []string{"A1", "A2", "A3", "B1", "B2", "C1"} {
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					release, err := g.acquire(fairContext(ctx, id[:1]))
					entries <- entry{id, release, err}
				}(id)
				waitAdmissionQueue(t, g, i+1)
			}
			held()
			for _, want := range []string{"A1", "B1", "C1", "A2", "B2", "A3"} {
				select {
				case e := <-entries:
					if e.release != nil {
						e.release()
					}
					if e.err != nil || e.id != want {
						t.Fatalf("got %s (%v), want %s", e.id, e.err, want)
					}
				case <-time.After(time.Second):
					t.Fatal("fair queue stalled")
				}
			}
		})
	}
}

func TestAdmissionFairPrefersCallerWithFewerInflight(t *testing.T) {
	g := newRequestAdmission()
	g.configure(admissionFileConfig{MaxActive: 2, WaitTimeoutMs: 2000})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, _ := g.acquire(fairContext(ctx, "A"))
	defer first()
	second, _ := g.acquire(fairContext(ctx, "A"))
	defer second()
	type entry struct {
		id      string
		release func()
		err     error
	}
	entries := make(chan entry, 2)
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		wg.Wait()
		close(entries)
		for e := range entries {
			if e.release != nil {
				e.release()
			}
		}
	})
	for i, id := range []string{"A", "B"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			release, err := g.acquire(fairContext(ctx, id))
			entries <- entry{id, release, err}
		}(id)
		waitAdmissionQueue(t, g, i+1)
	}
	first()
	e := <-entries
	if e.release != nil {
		e.release()
	}
	if e.err != nil || e.id != "B" {
		t.Fatalf("backlogged caller won over idle caller: %s (%v)", e.id, e.err)
	}
}
