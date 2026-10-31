package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const restrictedFixture = `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity"},"requestId":"2d55ce33-cd39-4381-a779-04142023cf0a"}`

type restrictedHarness struct {
	*compatHarness
	arrivals    chan time.Time
	beforeReply func(*http.Request, int32)
}

// Both the client and upstream use real HTTP. Account/credential verification
// stays on this same synthetic server and is not counted as a model request.
func newRestrictedHarness(t *testing.T, failures int) *restrictedHarness {
	t.Helper()
	h := &restrictedHarness{compatHarness: newCompatHarness(t, "cn"), arrivals: make(chan time.Time, 128)}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"code":0,"data":{}}`)
			return
		}
		n := h.calls.Add(1)
		h.arrivals <- time.Now()
		c := compatCapture{headers: r.Header.Clone()}
		c.err = json.NewDecoder(r.Body).Decode(&c.body)
		h.captured <- c
		if h.beforeReply != nil {
			h.beforeReply(r, n)
		}
		if int(n) <= failures {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, restrictedFixture)
			return
		}
		reply := h.reply.Load()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	t.Cleanup(up.Close)
	profileCN.Base, profileCN.Origin = up.URL, up.URL
	profileINTL.Base, profileINTL.Origin = up.URL, up.URL
	cfg.HttpClient = up.Client()
	return h
}

func shortenRestrictedWait(t *testing.T, delay time.Duration) {
	t.Helper()
	old, oldBackoff := restrictedReplayDelay, upstreamRetryBackoff
	restrictedReplayDelay, upstreamRetryBackoff = delay, time.Microsecond
	upstreamConfigMu.Lock()
	oldHeartbeat := streamKeepalive
	streamKeepalive = 5 * time.Millisecond
	upstreamConfigMu.Unlock()
	t.Cleanup(func() {
		restrictedReplayDelay, upstreamRetryBackoff = old, oldBackoff
		upstreamConfigMu.Lock()
		streamKeepalive = oldHeartbeat
		upstreamConfigMu.Unlock()
	})
}

func restrictedRequestBody(path string, stream bool) map[string]any {
	const instruction = "Keep user-code-sentinel intact."
	const result = "synthetic tool-result-sentinel"
	body := map[string]any{"model": compatModel, "stream": stream, "max_tokens": 64,
		"messages": []any{
			map[string]any{"role": "developer", "content": instruction},
			map[string]any{"role": "user", "content": "synthetic lookup"},
			map[string]any{"role": "assistant", "content": "", "reasoning_content": "synthetic historical reasoning", "tool_calls": []any{map[string]any{"id": "call-history", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"value":"synthetic"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "call-history", "content": result},
			map[string]any{"role": "user", "content": "synthetic continue"},
		}}
	if strings.HasSuffix(path, "/responses") {
		delete(body, "messages")
		body["instructions"], body["store"] = instruction, false
		body["input"] = []any{
			map[string]any{"type": "message", "role": "user", "content": "synthetic lookup"},
			map[string]any{"type": "function_call", "call_id": "call-history", "name": "lookup", "arguments": `{"value":"synthetic"}`},
			map[string]any{"type": "function_call_output", "call_id": "call-history", "output": result},
			map[string]any{"type": "message", "role": "user", "content": "synthetic continue"},
		}
	} else if strings.HasSuffix(path, "/messages") {
		body["system"] = instruction
		body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
		body["max_tokens"] = 2048
		body["messages"] = []any{
			map[string]any{"role": "user", "content": "synthetic lookup"},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-history", "name": "lookup", "input": map[string]any{"value": "synthetic"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-history", "content": result}}},
		}
	}
	return body
}

func assertRestrictedCleanup(t *testing.T, h *restrictedHarness) {
	t.Helper()
	_, active, waiting := ingressAdmission.snapshot()
	_, modelActive, _ := modelConcurrency.snapshot("")
	if active != 0 || waiting != 0 || modelActive != 0 || upstreamRetries.active != 0 || upstreamRetries.originals != 0 {
		t.Fatal("restricted request leaked an admission, model, or retry permit")
	}
	for i, acc := range h.accounts {
		if !acc.lock.TryLock() {
			t.Fatalf("account %d retained a credential lock", i)
		}
		acc.lock.Unlock()
		data, err := os.ReadFile(acc.Path)
		if err != nil || string(data) != "synthetic credential sentinel" {
			t.Fatal("restriction changed or deleted a credential")
		}
	}
}

func TestRestrictedReplayAllProtocolsPreservesRequestAndWAL(t *testing.T) {
	if restrictedReplayDelayDefault != 2*time.Minute || restrictedReplayDelay != restrictedReplayDelayDefault {
		t.Fatal("production cooldown must default to two minutes")
	}
	for _, site := range []string{"cn", "intl"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(site+path+map[bool]string{true: "/stream", false: "/aggregate"}[stream], func(t *testing.T) {
					h := newRestrictedHarness(t, 1)
					const delay = 60 * time.Millisecond
					shortenRestrictedWait(t, delay)
					for _, acc := range h.accounts {
						acc.Auth.Edition = site
						acc.Edition = site
					}
					body := restrictedRequestBody(path, stream)
					headers := map[string]string{"X-Session-ID": "synthetic-isolated-session"}
					status, out := h.send(t, path, body, headers, 0)
					if status != 200 || h.calls.Load() != 2 {
						t.Fatalf("delayed fallback failed: HTTP %d calls=%d body=%s", status, h.calls.Load(), out)
					}
					first, second := h.take(t), h.take(t)
					firstArrival, secondArrival := <-h.arrivals, <-h.arrivals
					if gap := secondArrival.Sub(firstArrival); gap < delay {
						t.Fatalf("fallback sent before cooldown elapsed: %v", gap)
					}
					if first.headers.Get("Authorization") == second.headers.Get("Authorization") || !reflect.DeepEqual(first.body, second.body) {
						t.Fatal("fallback reused restricted account or modified the prepared body")
					}
					for _, name := range []string{conversationIDHeader, conversationRequestIDHeader, conversationMessageIDHeader} {
						if first.headers.Get(name) != second.headers.Get(name) {
							t.Fatalf("fallback changed %s", name)
						}
					}
					prepared, _ := json.Marshal(second.body)
					for _, want := range []string{"user-code-sentinel", "tool-result-sentinel", "call-history"} {
						if !bytes.Contains(prepared, []byte(want)) {
							t.Fatalf("prepared replay lost %s", want)
						}
					}
					assertCompatSuccess(t, path, stream, out)
					if stream && bytes.Count(out, []byte(": keepalive\n\n")) < 2 {
						t.Fatal("cooldown did not keep SSE alive")
					}
					if !h.accounts[0].Disabled || h.accounts[1].Disabled {
						t.Fatal("wrong account restriction state")
					}
					if _, err := os.Stat(markerPath(h.accounts[0].Path)); err != nil {
						t.Fatal("restriction was not persisted")
					}
					// A later turn keeps affinity on the successful account and does
					// not inherit this request's wait or retry identifiers.
					if status, out := h.send(t, path, body, headers, 0); status != 200 {
						t.Fatalf("follow-up failed: %d %s", status, out)
					}
					third := h.take(t)
					if third.headers.Get("Authorization") != second.headers.Get("Authorization") || third.headers.Get(conversationIDHeader) != second.headers.Get(conversationIDHeader) || third.headers.Get(conversationRequestIDHeader) == second.headers.Get(conversationRequestIDHeader) {
						t.Fatal("follow-up lost affinity/session isolation")
					}
					h.key.mu.Lock()
					usage := h.key.usage
					h.key.mu.Unlock()
					if usage.Total != 0.2 || usage.Requests != 2 || usage.Unknown != 0 {
						t.Fatalf("restriction was charged or success lost: %+v", usage)
					}
					reloadKeysAndLedger(t, keyDataDir)
					key := keyByID(h.key.ID)
					key.mu.Lock()
					replayed := key.usage
					key.mu.Unlock()
					if !reflect.DeepEqual(usage, replayed) {
						t.Fatal("WAL replay changed settlements")
					}
					assertRestrictedCleanup(t, h)
				})
			}
		}
	}
}

func TestRestrictedReplayStopsAfterOneOtherAccount(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(path+map[bool]string{true: "/stream", false: "/aggregate"}[stream], func(t *testing.T) {
				h := newRestrictedHarness(t, 3)
				shortenRestrictedWait(t, 20*time.Millisecond)
				third := mkAccount(filepath.Join(t.TempDir(), "synthetic-2.json"))
				third.QuotaKnown, third.QuotaRemaining = true, 1000
				if err := os.WriteFile(third.Path, []byte("synthetic credential sentinel"), 0600); err != nil {
					t.Fatal(err)
				}
				h.accounts = append(h.accounts, third)
				withAccounts(t, h.accounts...)
				status, out := h.send(t, path, restrictedRequestBody(path, stream), nil, 0)
				wantStatus := http.StatusForbidden
				if stream {
					wantStatus = 200 // heartbeats already committed SSE; the event must be an error
				}
				if status != wantStatus || h.calls.Load() != 2 || !bytes.Contains(out, []byte("11140")) {
					t.Fatalf("restriction lost or cascaded: HTTP %d calls=%d body=%s", status, h.calls.Load(), out)
				}
				for _, terminal := range []string{"[DONE]", "response.completed", "message_stop"} {
					if bytes.Contains(out, []byte(terminal)) {
						t.Fatalf("restriction emitted false success %s", terminal)
					}
				}
				if stream && path == "/v1/responses" && !bytes.Contains(out, []byte("event: response.failed")) {
					t.Fatal("Responses lost its failed event")
				}
				if stream && path == "/v1/messages" && !bytes.Contains(out, []byte("event: error")) {
					t.Fatal("Messages lost its error event")
				}
				if !h.accounts[0].Disabled || !h.accounts[1].Disabled || third.Disabled {
					t.Fatal("a third account was touched")
				}
				h.key.mu.Lock()
				usage := h.key.usage
				h.key.mu.Unlock()
				if usage.Total != 0 || usage.Requests != 0 || usage.Unknown != 0 {
					t.Fatalf("SSE/200 invented a settlement: %+v", usage)
				}
				assertRestrictedCleanup(t, h)
			})
		}
	}
}

func TestRestrictedReplayRequiresEligibleAccountAndBudget(t *testing.T) {
	for _, tc := range []string{"one_account", "disabled_other", "no_retry_budget", "blocked_other"} {
		t.Run(tc, func(t *testing.T) {
			h := newRestrictedHarness(t, 1)
			shortenRestrictedWait(t, time.Hour)
			switch tc {
			case "one_account":
				withAccounts(t, h.accounts[0])
			case "disabled_other":
				h.accounts[1].Disabled = true
			case "no_retry_budget":
				upstreamRetries.configure(retryFileConfig{MaxExtraAttempts: intPointer(0)})
			case "blocked_other":
				if err := setModelAccountFilter(map[string]modelAccountFileConfig{"all": {Blocklist: []string{filepath.Base(h.accounts[1].Path)}}}); err != nil {
					t.Fatal(err)
				}
			}
			h.server.Client().Timeout = time.Second
			status, out := h.send(t, "/v1/responses", restrictedRequestBody("/v1/responses", true), nil, 0)
			if status != 403 || h.calls.Load() != 1 || !bytes.Contains(out, []byte("11140")) || bytes.Contains(out, []byte(": keepalive")) {
				t.Fatalf("ineligible fallback waited or hid the restriction: %d %s", status, out)
			}
			assertRestrictedCleanup(t, h)
		})
	}
}

func TestRestrictedReplayDoesNotReplayInStreamRestriction(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			h := newRestrictedHarness(t, 0)
			shortenRestrictedWait(t, time.Hour)
			h.reply.Store(&compatReply{200, "data: {\"choices\":[{\"delta\":{\"content\":\"synthetic partial output\"}}]}\n\ndata: " + restrictedFixture + "\n\n"})
			status, out := h.send(t, path, restrictedRequestBody(path, true), nil, 0)
			if status != 200 || h.calls.Load() != 1 || !bytes.Contains(out, []byte("11140")) {
				t.Fatalf("stream restriction was replayed or swallowed: %d %s", status, out)
			}
			for _, terminal := range []string{"[DONE]", "response.completed", "message_stop"} {
				if bytes.Contains(out, []byte(terminal)) {
					t.Fatalf("partial output ended in false success %s", terminal)
				}
			}
			assertRestrictedCleanup(t, h)
		})
	}
}

func TestRestrictedReplayDoesNotSendBeforeCooldown(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(path+map[bool]string{true: "/stream", false: "/aggregate"}[stream], func(t *testing.T) {
				h := newRestrictedHarness(t, 1)
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				defer cancel()
				raw, _ := json.Marshal(restrictedRequestBody(path, stream))
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+path, bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+h.keys[0])
				resp, err := h.server.Client().Do(req)
				if err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				// Join handlers before reading counters or restoring global test settings.
				h.server.Close()
				if h.calls.Load() != 1 {
					t.Fatalf("11140 crossed accounts before the two-minute cooldown: model calls=%d", h.calls.Load())
				}
				assertRestrictedCleanup(t, h)
				h.key.mu.Lock()
				usage := h.key.usage
				h.key.mu.Unlock()
				if usage.Total != 0 || usage.Unknown != 0 || usage.Requests != 0 {
					t.Fatalf("canceled cooldown created usage: %+v", usage)
				}
			})
		}
	}
}

func beginRestrictedStream(t *testing.T, h *restrictedHarness) (*http.Response, *bufio.Reader) {
	t.Helper()
	raw, _ := json.Marshal(restrictedRequestBody("/v1/responses", true))
	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/responses", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.keys[0])
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || line != ": keepalive\n" {
		t.Fatalf("cooldown did not begin SSE safely: %q %v", line, err)
	}
	return resp, reader
}

func TestRestrictedReplayWaitReleasesAccountAndRetryPermits(t *testing.T) {
	h := newRestrictedHarness(t, 1)
	shortenRestrictedWait(t, 200*time.Millisecond)
	modelConcurrency.configure(concurrentFileConfig{MaxGlobal: 1, MaxAcc: 1})
	resp, reader := beginRestrictedStream(t, h)
	first := h.take(t)
	_, modelActive, _ := modelConcurrency.snapshot("")
	if modelActive != 0 || upstreamRetries.active != 0 {
		t.Fatal("cooldown retained a model or extra-attempt permit")
	}
	for _, acc := range h.accounts {
		if !acc.lock.TryLock() {
			t.Fatal("cooldown retained the credential lock")
		}
		acc.lock.Unlock()
	}
	// Another logical request is served normally during the first one's wait.
	status, out := h.send(t, "/v1/chat/completions", restrictedRequestBody("/v1/chat/completions", false), map[string]string{"X-Session-ID": "different-synthetic-session"}, 1)
	if status != 200 || h.calls.Load() != 2 {
		t.Fatalf("unrelated request was blocked by the cooldown: %d %s", status, out)
	}
	other := h.take(t)
	if other.headers.Get(conversationIDHeader) == first.headers.Get(conversationIDHeader) {
		t.Fatal("independent request inherited the waiting request's session")
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if h.calls.Load() != 3 {
		t.Fatal("waiting request failed to continue after its cooldown")
	}
	assertRestrictedCleanup(t, h)
}

func TestRestrictedReplayRechecksEligibilityAfterWaiting(t *testing.T) {
	h := newRestrictedHarness(t, 1)
	shortenRestrictedWait(t, 80*time.Millisecond)
	resp, reader := beginRestrictedStream(t, h)
	accountMu.Lock()
	h.accounts[1].Disabled = true
	accountMu.Unlock()
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if h.calls.Load() != 1 || !bytes.Contains(out, []byte("11140")) || !bytes.Contains(out, []byte("response.failed")) {
		t.Fatalf("recheck used a newly disabled account or lost the original rejection: %s", out)
	}
	assertRestrictedCleanup(t, h)
}

func TestRestrictedReplayCancelDuringNextAccountHeaderWait(t *testing.T) {
	h := newRestrictedHarness(t, 1)
	shortenRestrictedWait(t, 15*time.Millisecond)
	started, canceled := make(chan struct{}), make(chan struct{})
	h.beforeReply = func(r *http.Request, n int32) {
		if n == 2 {
			close(started)
			<-r.Context().Done()
			close(canceled)
		}
	}
	resp, reader := beginRestrictedStream(t, h)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("delayed account was not reached")
	}
	// Its response headers are blocked, but the downstream still gets heartbeats.
	for i := 0; i < 4; i++ {
		if _, err := reader.ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	}
	_ = resp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("client disconnect did not cancel the following account")
	}
	h.server.Close()
	if h.calls.Load() != 2 {
		t.Fatal("canceled fallback attempted another account")
	}
	assertRestrictedCleanup(t, h)
}

func TestRestrictedReplayDoesNotCascadeOnDifferentFallbackRejection(t *testing.T) {
	for _, status := range []int{401, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newRestrictedHarness(t, 1)
			shortenRestrictedWait(t, 10*time.Millisecond)
			h.reply.Store(&compatReply{status, `{"msg":"synthetic fallback rejection"}`})
			third := mkAccount(filepath.Join(t.TempDir(), "synthetic-2.json"))
			third.QuotaKnown, third.QuotaRemaining = true, 1000
			if err := os.WriteFile(third.Path, []byte("synthetic credential sentinel"), 0600); err != nil {
				t.Fatal(err)
			}
			h.accounts = append(h.accounts, third)
			withAccounts(t, h.accounts...)
			gotStatus, out := h.send(t, "/v1/responses", restrictedRequestBody("/v1/responses", false), nil, 0)
			if gotStatus != status || h.calls.Load() != 2 || !bytes.Contains(out, []byte("synthetic fallback rejection")) || third.Disabled {
				t.Fatalf("different fallback rejection cascaded or was hidden: HTTP %d %s", gotStatus, out)
			}
			assertRestrictedCleanup(t, h)
		})
	}
}
