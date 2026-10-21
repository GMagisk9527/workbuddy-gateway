package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func setupAccountStreamTest(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	isolatePolicies(t)
	oldClient := cfg.HttpClient
	cfg.HttpClient = &http.Client{Transport: transport}
	t.Cleanup(func() { cfg.HttpClient = oldClient })
	withAccounts(t, mkAccount("synthetic-serial-account.json"))
}

func startAccountStreamCall(ctx context.Context) <-chan *http.Response {
	result := make(chan *http.Response, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")).WithContext(ctx)
		resp, _, _, ok := upstreamChat(httptest.NewRecorder(), r, requestIDFor(r), "serial-model", []byte("{}"), time.Now())
		if !ok {
			resp = nil
		}
		result <- resp
	}()
	return result
}

func awaitAccountStreamCall(t *testing.T, result <-chan *http.Response) *http.Response {
	t.Helper()
	select {
	case resp := <-result:
		if resp == nil {
			t.Fatal("upstream request unexpectedly rejected")
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	case <-time.After(2 * time.Second):
		t.Fatal("account queue did not progress")
		return nil
	}
}

func TestAccountStreamSerializesUntilCloseOrEOF(t *testing.T) {
	var calls atomic.Int32
	setupAccountStreamTest(t, retryTransportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("SSE data"))}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := awaitAccountStreamCall(t, startAccountStreamCall(ctx))
	secondResult := startAccountStreamCall(ctx)
	select {
	case <-secondResult:
		t.Fatal("second request started while first response body was still open")
	case <-time.After(60 * time.Millisecond):
	}
	if calls.Load() != 1 {
		t.Fatalf("expected one active request, got %d", calls.Load())
	}
	_ = first.Body.Close()
	second := awaitAccountStreamCall(t, secondResult)
	_ = first.Body.Close() // Repeated close must not release the second request's lock.
	thirdResult := startAccountStreamCall(ctx)
	select {
	case <-thirdResult:
		t.Fatal("repeated close released an account still occupied by the second request")
	case <-time.After(60 * time.Millisecond):
	}
	if _, err := io.ReadAll(second.Body); err != nil {
		t.Fatal(err)
	}
	awaitAccountStreamCall(t, thirdResult)
	if calls.Load() != 3 {
		t.Fatalf("expected all requests to finish in order, got %d", calls.Load())
	}
}

func TestAccountQueueCancellationDoesNotSend(t *testing.T) {
	var calls atomic.Int32
	setupAccountStreamTest(t, retryTransportFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("held"))}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := awaitAccountStreamCall(t, startAccountStreamCall(ctx))
	waitCtx, stopWaiting := context.WithTimeout(ctx, 70*time.Millisecond)
	defer stopWaiting()
	select {
	case resp := <-startAccountStreamCall(waitCtx):
		if resp != nil {
			_ = resp.Body.Close()
			t.Fatal("canceled queued request reached the upstream")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled queued request remained blocked")
	}
	if calls.Load() != 1 {
		t.Fatalf("canceled queued request sent a model request: calls=%d", calls.Load())
	}
	_ = first.Body.Close()
	awaitAccountStreamCall(t, startAccountStreamCall(ctx))
}

func TestAccountLockReleasedAfterSendFailure(t *testing.T) {
	var calls atomic.Int32
	setupAccountStreamTest(t, retryTransportFunc(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("synthetic permanent send failure")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if resp := <-startAccountStreamCall(ctx); resp != nil {
		_ = resp.Body.Close()
		t.Fatal("failed send unexpectedly succeeded")
	}
	awaitAccountStreamCall(t, startAccountStreamCall(ctx))
}

type failingAccountBody struct {
	closed int
}

func (b *failingAccountBody) Read(p []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (b *failingAccountBody) Close() error {
	b.closed++
	return errors.New("synthetic close error")
}

func TestAccountStreamReadFailureReleasesOnce(t *testing.T) {
	underlying := &failingAccountBody{}
	released := 0
	body := newAccountStreamBody(underlying, func() { released++ })
	if _, err := body.Read(make([]byte, 1)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error was lost: %v", err)
	}
	_ = body.Close()
	_ = body.Close()
	if released != 1 || underlying.closed != 1 {
		t.Fatalf("cleanup repeated: released=%d closed=%d", released, underlying.closed)
	}
}

func TestChatRejectsNonObjectJSONOverHTTP(t *testing.T) {
	server := httptest.NewServer(requestAuditMiddleware(http.HandlerFunc(handleChatCompletions)))
	defer server.Close()
	for _, input := range []string{"null", "[]", "true", "1", `"text"`} {
		t.Run(input, func(t *testing.T) {
			resp, err := server.Client().Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(input))
			if err != nil {
				t.Fatalf("invalid body broke the connection: %v", err)
			}
			defer resp.Body.Close()
			var payload map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != 400 || payload["error"] == nil {
				t.Fatalf("expected structured HTTP 400, got %d: %v", resp.StatusCode, payload)
			}
		})
	}
}
