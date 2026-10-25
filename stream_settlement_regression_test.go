package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type settlementFailWriter struct {
	*httptest.ResponseRecorder
	successfulWrites int
	failContaining   string
	writes           int
}

func (w *settlementFailWriter) Write(p []byte) (int, error) {
	w.writes++
	if (w.failContaining == "" && w.writes > w.successfulWrites) || (w.failContaining != "" && strings.Contains(string(p), w.failContaining)) {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}

type settlementFlushFailWriter struct {
	*httptest.ResponseRecorder
	flushes, successfulFlushes int
}

func (w *settlementFlushFailWriter) FlushError() error {
	w.flushes++
	if w.flushes > w.successfulFlushes {
		return io.ErrClosedPipe
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestStreamFlushFailurePreservesUsageAcrossProtocols(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		for _, protocol := range []string{"chat", "responses", "messages"} {
			t.Run(map[fastPathModeT]string{fastPathOff: "slow", fastPathOn: "fast"}[mode]+"/"+protocol, func(t *testing.T) {
				oldMode := fastPathMode.Load()
				fastPathMode.Store(int32(mode))
				t.Cleanup(func() { fastPathMode.Store(oldMode) })
				dir := resetKeyWorld(t)
				s := isolatedPerformance(t)
				if err := ledgerLoad(dir); err != nil {
					t.Fatal(err)
				}
				plain, key := mustCreateKey(t, keyCreateOptions{Name: "flush-failure", QuotaTotal: 10})
				body := &trackingBody{r: strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":3}}\n\ndata: [DONE]\n\n")}
				complete := true
				h := requestAuditMiddleware(authMiddleware(keyAdmissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					start := time.Now()
					*statsAttemptFor(r) = statsAttempt{id: "synthetic-account", attemptID: "flush-failure", model: "synthetic", started: start}
					resp := &http.Response{StatusCode: 200, Body: body}
					acc := &Account{Path: "synthetic.json"}
					switch protocol {
					case "chat":
						streamChatResponse(w, r, resp, "synthetic", 1, acc, &profileCN, start)
					case "responses":
						streamResponsesResponse(w, r, resp, "synthetic", 1, acc, &profileCN, start)
					case "messages":
						streamMessagesResponse(w, r, resp, "synthetic", 1, acc, &profileCN, start, false)
					}
					complete = statsAttemptFor(r).complete
				}))))
				route := map[string]string{"chat": "/v1/chat/completions", "responses": "/v1/responses", "messages": "/v1/messages"}[protocol]
				r := httptest.NewRequest("POST", route, nil)
				r.Header.Set("Authorization", "Bearer "+plain)
				writer := &settlementFlushFailWriter{ResponseRecorder: httptest.NewRecorder()}
				if protocol == "responses" {
					writer.successfulFlushes = 2
				}
				h.ServeHTTP(writer, r)
				key.mu.Lock()
				usage := key.usage
				key.mu.Unlock()
				metrics := s.snapshot(time.Now()).Protocols[0]
				if usage.Total != 3 || usage.Requests != 1 || usage.Unknown != 0 || complete || !body.closed || metrics.DownstreamErrors != 1 || metrics.Failed != 1 {
					t.Fatalf("flush failure lost usage/status/cleanup: usage=%+v complete=%t closed=%t counters=%+v", usage, complete, body.closed, metrics.performanceCounters)
				}
			})
		}
	}
}

func TestStreamWriteFailurePreservesParsedCredit(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		for _, protocol := range []string{"chat", "responses", "messages"} {
			for _, credit := range []struct {
				name, field string
				want        float64
				unknown     int64
			}{
				{name: "known", field: `,"credit":3`, want: 3},
				{name: "zero", field: `,"credit":0`},
				{name: "missing", unknown: 1},
			} {
				name := map[fastPathModeT]string{fastPathOff: "slow", fastPathOn: "fast"}[mode]
				t.Run(name+"/"+protocol+"/"+credit.name, func(t *testing.T) {
					previousMode := fastPathMode.Load()
					fastPathMode.Store(int32(mode))
					t.Cleanup(func() { fastPathMode.Store(previousMode) })
					dir := resetKeyWorld(t)
					if err := ledgerLoad(dir); err != nil {
						t.Fatal(err)
					}
					plain, key := mustCreateKey(t, keyCreateOptions{Name: "write-failure", QuotaTotal: 10})
					sse := `data: {"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1` + credit.field + "}}\n\ndata: [DONE]\n\n"
					body := &trackingBody{r: strings.NewReader(sse)}
					complete := true
					handler := requestAuditMiddleware(authMiddleware(keyAdmissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						start := time.Now()
						*statsAttemptFor(r) = statsAttempt{id: "offline-account", attemptID: "write-failure", model: "offline", started: start}
						response := &http.Response{StatusCode: http.StatusOK, Body: body}
						acc := &Account{Path: "offline.json"}
						switch protocol {
						case "chat":
							streamChatResponse(w, r, response, "offline", 1, acc, &profileCN, start)
						case "responses":
							streamResponsesResponse(w, r, response, "offline", 1, acc, &profileCN, start)
						case "messages":
							streamMessagesResponse(w, r, response, "offline", 1, acc, &profileCN, start, false)
						}
						complete = statsAttemptFor(r).complete
					}))))
					r := httptest.NewRequest(http.MethodPost, "/v1/"+protocol, nil)
					r.Header.Set("Authorization", "Bearer "+plain)
					writer := &settlementFailWriter{ResponseRecorder: httptest.NewRecorder()}
					if protocol == "responses" {
						writer.successfulWrites = 2 // Only the initial envelopes reach the client.
					}
					handler.ServeHTTP(writer, r)
					key.mu.Lock()
					usage := key.usage
					key.mu.Unlock()
					if usage.Total != credit.want || usage.Lifetime != credit.want || usage.Unknown != credit.unknown || usage.Requests != 1 {
						t.Fatalf("parsed credit must settle exactly once after write failure: %+v", usage)
					}
					if !body.closed {
						t.Error("write failure must close the upstream body")
					}
					if complete {
						t.Error("a failed downstream write must not mark the response complete")
					}
					if strings.Contains(writer.Body.String(), "[DONE]") || strings.Contains(writer.Body.String(), "response.completed") || strings.Contains(writer.Body.String(), "message_stop") {
						t.Error("a failed downstream write must not produce a success terminator")
					}
				})
			}
		}
	}
}

func TestStreamTerminalWriteFailurePreservesUsageAndIncompleteStatus(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		for _, protocol := range []string{"chat", "responses", "messages"} {
			name := map[fastPathModeT]string{fastPathOff: "slow", fastPathOn: "fast"}[mode]
			t.Run(name+"/"+protocol, func(t *testing.T) {
				previousMode := fastPathMode.Load()
				fastPathMode.Store(int32(mode))
				t.Cleanup(func() { fastPathMode.Store(previousMode) })
				start := time.Now()
				attempt := &statsAttempt{id: "offline-account", model: "offline", started: start}
				req := httptest.NewRequest(http.MethodPost, "/v1/"+protocol, nil)
				req = req.WithContext(context.WithValue(req.Context(), statsRequestContextKey{}, attempt))
				body := &trackingBody{r: strings.NewReader(`data: {"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}],"usage":{"credit":3}}` + "\n\ndata: [DONE]\n\n")}
				writer := &settlementFailWriter{
					ResponseRecorder: httptest.NewRecorder(),
					failContaining: map[string]string{
						"chat": "data: [DONE]", "responses": "event: response.completed", "messages": "event: message_stop",
					}[protocol],
				}
				resp := &http.Response{Body: body}
				acc := &Account{Path: "offline.json"}
				switch protocol {
				case "chat":
					streamChatResponse(writer, req, resp, "offline", 1, acc, &profileCN, start)
				case "responses":
					streamResponsesResponse(writer, req, resp, "offline", 1, acc, &profileCN, start)
				case "messages":
					streamMessagesResponse(writer, req, resp, "offline", 1, acc, &profileCN, start, false)
				}
				if credit, ok := usageCredit(attempt.usage); !ok || credit != 3 || attempt.complete {
					t.Fatalf("terminal write failure must retain known usage and mark response incomplete: %+v", attempt)
				}
				if !body.closed {
					t.Error("terminal write failure must close the upstream body")
				}
			})
		}
	}
}

// Return one line if configured, then block until Close. This detects a read
// after the failing write even when the upstream has not sent the blank line.
type settlementBlockingBody struct {
	first       string
	readStarted chan struct{}
	closed      chan struct{}
	readOnce    sync.Once
	closeOnce   sync.Once
}

func (b *settlementBlockingBody) Read(p []byte) (int, error) {
	if b.first != "" {
		n := copy(p, b.first)
		b.first = b.first[n:]
		return n, nil
	}
	b.readOnce.Do(func() { close(b.readStarted) })
	<-b.closed
	return 0, io.EOF
}

func (b *settlementBlockingBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestResponsesWriteFailureClosesBeforeNextUpstreamRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first string
		allow int
	}{
		{name: "envelope"},
		{name: "delta", first: `data: {"choices":[{"delta":{"content":"answer"}}]}` + "\n", allow: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previousMode := fastPathMode.Load()
			fastPathMode.Store(int32(fastPathOn))
			t.Cleanup(func() { fastPathMode.Store(previousMode) })
			body := &settlementBlockingBody{first: tc.first, readStarted: make(chan struct{}), closed: make(chan struct{})}
			defer body.Close()
			done := make(chan struct{})
			writer := &settlementFailWriter{ResponseRecorder: httptest.NewRecorder(), successfulWrites: tc.allow}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			go func() {
				defer close(done)
				streamResponsesResponse(writer, req, &http.Response{Body: body}, "offline", 1, &Account{Path: "offline.json"}, &profileCN, time.Now())
			}()
			select {
			case <-done:
			case <-body.readStarted:
				t.Error("handler started another upstream read after the downstream write failed")
			case <-time.After(2 * time.Second):
				t.Error("handler did not return after the downstream write failed")
			}
			_ = body.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("handler did not return after upstream body was released")
			}
		})
	}
}
