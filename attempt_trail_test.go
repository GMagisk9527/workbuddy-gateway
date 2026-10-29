package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAttemptTrailRetainsOrderedNetworkRetryAndRotation(t *testing.T) {
	oldClient, oldRetries, oldBackoff := cfg.HttpClient, upstreamTransientRetries, upstreamRetryBackoff
	t.Cleanup(func() {
		cfg.HttpClient, upstreamTransientRetries, upstreamRetryBackoff = oldClient, oldRetries, oldBackoff
	})
	upstreamTransientRetries, upstreamRetryBackoff = 1, time.Millisecond
	calls := 0
	cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("private-error-marker: %w", io.EOF)
		}
		status := http.StatusOK
		if calls == 2 {
			status = http.StatusTooManyRequests
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("private-body-marker"))}, nil
	})}
	r, p := withRequestPerformance(httptest.NewRequest("POST", "/v1/messages", nil), time.Now())
	p.traceID = "synthetic-attempt-order"
	r, scope := withRetryScope(r)
	defer scope.close()
	for _, path := range []string{"private-account-one", "private-account-two"} {
		acc := &Account{Path: path, Auth: &StoredAuth{Auth: StoredTokens{AccessToken: "private-token-marker"}}}
		resp, cancel, err := doUpstreamRequest(r, acc, &profileCN, []byte(`{"model":"synthetic"}`), 1, p.traceID)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		cancel()
	}
	p.streamCompletion(true)
	store := &performanceStore{}
	p.finish(store, 200, false, time.Now())
	diagnostic, ok := store.requestDiagnostic(p.traceID, time.Now())
	if !ok {
		t.Fatal("missing request diagnostic")
	}
	encoded, err := json.Marshal(diagnostic)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Attempts []struct {
			Attempt int    `json:"attempt"`
			Kind    string `json:"kind"`
			Status  int    `json:"statusCode"`
			Failure string `json:"failureClass"`
			Outcome string `json:"outcome"`
		} `json:"attemptTrail"`
	}
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Attempts) != 3 {
		t.Fatalf("expected three ordered attempts, got %s", encoded)
	}
	for i, a := range data.Attempts {
		if a.Attempt != i+1 {
			t.Fatalf("bad attempt order: %s", encoded)
		}
	}
	if a := data.Attempts[0]; a.Kind != "initial" || a.Status != 0 || a.Failure != "network" {
		t.Fatalf("bad unsent network failure: %s", encoded)
	}
	if a := data.Attempts[1]; a.Kind != "transport_retry" || a.Status != 429 || a.Failure != "rate_limited" {
		t.Fatalf("bad network retry: %s", encoded)
	}
	if a := data.Attempts[2]; a.Kind != "account_fallback" || a.Status != 200 || a.Outcome != "complete" {
		t.Fatalf("bad account fallback: %s", encoded)
	}
	for _, secret := range []string{"private-error-marker", "private-body-marker", "private-account", "private-token-marker"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("private data leaked: %s", secret)
		}
	}
}

func TestAttemptTrailBoundedAndSnapshotsIndependent(t *testing.T) {
	p := &requestPerformance{traceID: "synthetic-bounded-attempts", start: time.Now()}
	acc := &Account{}
	for i := 0; i < requestAttemptCapacity+3; i++ {
		a := p.beginUpstreamAttempt(acc, i > 0)
		a.response(429, nil)
	}
	// An unrecorded later attempt must not overwrite the last retained row.
	p.rejectCurrentAttempt("auth", "401")
	s := &performanceStore{}
	p.finish(s, 429, false, time.Now())
	d, ok := s.requestDiagnostic(p.traceID, time.Now())
	if !ok || len(d.AttemptTrail) != requestAttemptCapacity || d.AttemptsDropped != 3 {
		t.Fatalf("attempt storage not bounded: %+v", d)
	}
	last := d.AttemptTrail[len(d.AttemptTrail)-1]
	if last.FailureClass != "rate_limited" || last.BusinessCode != "" {
		t.Fatalf("later attempt corrupted retained row: %+v", last)
	}
	d.AttemptTrail[0].Kind = "mutated"
	*d.AttemptTrail[0].HeadersMS = -999
	again, _ := s.requestDiagnostic(p.traceID, time.Now())
	if again.AttemptTrail[0].Kind != "initial" || *again.AttemptTrail[0].HeadersMS < 0 {
		t.Fatal("diagnostic snapshot aliases stored attempt")
	}
	if _, ok := s.requestDiagnostic(p.traceID, time.Now().Add(requestDiagnosticTTL+time.Second)); ok {
		t.Fatal("attempt trail outlived diagnostic TTL")
	}
}

func TestAttemptTrailCompletenessAndLateCallbacks(t *testing.T) {
	for _, tc := range []struct {
		name, failure, outcome string
		complete, canceled     bool
	}{
		{"headers alone", "", "response_headers", false, false},
		{"complete stream", "", "complete", true, false},
		{"partial stream", "stream_incomplete", "incomplete", false, false},
		{"canceled stream", "canceled", "error", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &requestPerformance{traceID: "synthetic-stream", start: time.Now()}
			a := p.beginUpstreamAttempt(&Account{}, false)
			a.response(200, nil)
			if tc.name != "headers alone" {
				p.streamCompletion(tc.complete)
			}
			s := &performanceStore{}
			p.finish(s, 200, tc.canceled, time.Now())
			var wg sync.WaitGroup
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); a.response(503, io.EOF); p.rejectCurrentAttempt("auth", "401") }()
			}
			wg.Wait()
			d, _ := s.requestDiagnostic(p.traceID, time.Now())
			if got := d.AttemptTrail[0]; got.Outcome != tc.outcome || got.FailureClass != tc.failure || got.StatusCode != 200 {
				t.Fatalf("incorrect terminal attempt: %+v", got)
			}
		})
	}
}

func TestAttemptTrailReplayKeepsPreviousFailureAndClassifiesErrors(t *testing.T) {
	p := &requestPerformance{traceID: "synthetic-replay", start: time.Now()}
	acc := &Account{}
	first := p.beginUpstreamAttempt(acc, false)
	first.response(200, nil)
	second := p.beginUpstreamAttempt(acc, false)
	second.response(200, nil)
	p.streamCompletion(true)
	s := &performanceStore{}
	p.finish(s, 200, false, time.Now())
	d, _ := s.requestDiagnostic(p.traceID, time.Now())
	if d.AttemptTrail[0].Outcome != "incomplete" || d.AttemptTrail[1].Kind != "stream_replay" || d.AttemptTrail[1].Outcome != "complete" {
		t.Fatalf("replay lost previous failure: %+v", d)
	}
	for _, tc := range []struct {
		err  error
		want string
	}{{context.Canceled, "canceled"}, {context.DeadlineExceeded, "timeout"}, {io.ErrUnexpectedEOF, "network"}} {
		if got := attemptErrorClass(tc.err, "network"); got != tc.want {
			t.Fatalf("error class=%s want=%s", got, tc.want)
		}
	}
}

func TestAttemptTrailProtocolHandlersRetainBusinessClassification(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			withStreamTestAccount(t)
			old := phaseMetrics
			phaseMetrics = &performanceStore{}
			t.Cleanup(func() { phaseMetrics = old })
			previous := cfg.HttpClient
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":11128,"msg":"private-error-body"}`))}, nil
			})}
			t.Cleanup(func() { cfg.HttpClient = previous })
			body := `{"model":"synthetic","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
			if path == "/v1/responses" {
				body = `{"model":"synthetic","stream":true,"input":"hi"}`
			}
			w := httptest.NewRecorder()
			newAPIHandler().ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
			d, ok := phaseMetrics.requestDiagnostic(w.Header().Get("X-Trace-ID"), time.Now())
			if !ok || len(d.AttemptTrail) != 1 || d.AttemptTrail[0].FailureClass != "invocation_rejected_unknown" || d.AttemptTrail[0].BusinessCode != "11128" {
				t.Fatalf("missing classification: %+v", d)
			}
		})
	}
}
