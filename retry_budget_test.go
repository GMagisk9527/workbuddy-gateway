package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func intPointer(n int) *int { return &n }
func TestRetryBudgetSharedAcrossDoCallsAndReleases(t *testing.T) {
	isolatedGatewayConfig(t)
	oldBackoff := upstreamRetryBackoff
	upstreamRetryBackoff = time.Microsecond
	t.Cleanup(func() { upstreamRetryBackoff = oldBackoff })
	upstreamRetries.configure(retryFileConfig{MaxExtraAttempts: intPointer(1), MaxConcurrent: intPointer(1)})
	r, scope := withRetryScope(httptest.NewRequest("POST", "/v1/messages", nil))
	defer scope.close()
	calls := 0
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	acc := &Account{Path: "synthetic.json", Auth: &StoredAuth{}}
	for i := range 3 {
		resp, cancel, err := doUpstreamRequest(r, acc, &profileCN, []byte(`{}`), 1, "synthetic")
		if i < 2 {
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			cancel()
		} else if !errors.Is(err, errRetryBudget) {
			t.Fatalf("extra attempt escaped logical budget: %v", err)
		}
	}
	if calls != 2 || upstreamRetries.active != 0 {
		t.Fatalf("attempt count or permit leak: calls=%d active=%d", calls, upstreamRetries.active)
	}
	scope.close()
	if upstreamRetries.originals != 0 {
		t.Fatal("initial request gauge leaked")
	}
}
func TestRetryBudgetPermitHeldUntilBodyClose(t *testing.T) {
	isolatedGatewayConfig(t)
	oldBackoff := upstreamRetryBackoff
	upstreamRetryBackoff = time.Microsecond
	t.Cleanup(func() { upstreamRetryBackoff = oldBackoff })
	upstreamRetries.configure(retryFileConfig{MaxConcurrent: intPointer(1)})
	a, b := upstreamRetries.scope(), upstreamRetries.scope()
	defer a.close()
	defer b.close()
	_, _ = a.begin(context.Background())
	_, _ = b.begin(context.Background())
	release, err := a.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.begin(context.Background()); !errors.Is(err, errRetryBudget) {
		t.Fatal("concurrent retries exceeded cap")
	}
	release()
	release()
	r, err := b.begin(context.Background())
	if err != nil {
		t.Fatal("retry slot never released")
	}
	r()
}
func TestRetryBackoffBoundsCancellationAndConcurrentScopes(t *testing.T) {
	for _, n := range []int{1, 2, 4, 64} {
		for _, random := range []float64{0, 0.5, 1} {
			d := retryBackoffDelay(100*time.Millisecond, time.Second, n, random)
			if d < 50*time.Millisecond || d > time.Second {
				t.Fatalf("backoff escaped cap: %v", d)
			}
		}
	}
	g := newRetryAdmission()
	g.configure(retryFileConfig{MaxExtraAttempts: intPointer(2), MaxConcurrent: intPointer(3)})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 30 {
				s := g.scope()
				_, _ = s.begin(context.Background())
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if release, err := s.begin(ctx); err == nil {
					if release != nil {
						release()
					}
					t.Error("cancelled retry admitted")
				}
				s.close()
			}
		})
	}
	wg.Wait()
	if g.originals != 0 || g.active != 0 {
		t.Fatalf("retry state leak: %d/%d", g.originals, g.active)
	}
}
func TestRetryBudgetStopsFirstByteReplayWithoutAnotherSend(t *testing.T) {
	for _, request := range []func() *http.Request{streamTestRequest, messagesResilienceRequest, responsesResilienceRequest} {
		t.Run(request().URL.Path, func(t *testing.T) {
			withStreamTestAccount(t)
			upstreamRetries.configure(retryFileConfig{MaxExtraAttempts: intPointer(0)})
			calls := 0
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &readErrorBody{err: errors.New("read: connection reset by peer")}, Request: req}, nil
			})}
			w := httptest.NewRecorder()
			newAPIHandler().ServeHTTP(w, request())
			if calls != 1 || strings.Contains(w.Body.String(), "data: [DONE]") || strings.Contains(w.Body.String(), "message_stop") || strings.Contains(w.Body.String(), "response.completed") || !strings.Contains(w.Body.String(), "error") {
				t.Fatalf("replay escaped unified budget: calls=%d %s", calls, w.Body.String())
			}
			if upstreamRetries.active != 0 || upstreamRetries.originals != 0 {
				t.Fatal("rejected replay leaked state")
			}
		})
	}
}

func TestRetryBudgetCancellationDuringBackoffReleasesPermit(t *testing.T) {
	isolatedGatewayConfig(t)
	old := upstreamRetryBackoff
	upstreamRetryBackoff = time.Second
	t.Cleanup(func() { upstreamRetryBackoff = old })
	s := upstreamRetries.scope()
	defer s.close()
	_, _ = s.begin(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if release, err := s.begin(ctx); !errors.Is(err, context.DeadlineExceeded) || release != nil {
		t.Fatalf("canceled backoff allowed another send: %v", err)
	}
	if upstreamRetries.active != 0 {
		t.Fatal("canceled backoff retained extra-attempt permit")
	}
}
