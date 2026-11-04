package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Synthetic DSH payloads follow upstream f803975/websearch_test.go. Both hops
// are httptest servers; these tests do not claim a live DSH/upstream validation.
func toolFixture(path string) (map[string]any, string) {
	if path == "/v1/search" {
		return map[string]any{"query": "private-query-sentinel", "count": 8, "need_content": false, "ori_question": "private-question-sentinel"}, `{"results":[{"title":"Go","url":"https://go.dev","snippet":"Language"}]}`
	}
	return map[string]any{"url": "https://example.test/private-url-sentinel"}, `{"content":"private-content-sentinel"}`
}

func TestAgentToolIntegrationSessionUsageAndWAL(t *testing.T) {
	for _, site := range []string{"cn", "intl"} {
		for _, path := range []string{"/v1/search", "/v1/fetch"} {
			t.Run(site+path, func(t *testing.T) {
				h := newCompatHarness(t, site)
				body, reply := toolFixture(path)
				reply = strings.TrimSuffix(reply, "}") + `,"usage":{"credit":2,"prompt_tokens":4}}`
				h.reply.Store(&compatReply{200, reply})
				var conversations []string
				var requestIDs []string
				for _, caller := range []int{0, 0, 1} {
					status, out := h.send(t, path, body, map[string]string{"X-Session-ID": "tool-shared-synthetic"}, caller)
					if status != 200 {
						t.Fatalf("status=%d body=%s", status, out)
					}
					capture := h.take(t)
					conversations = append(conversations, capture.headers.Get(conversationIDHeader))
					requestIDs = append(requestIDs, capture.headers.Get(conversationRequestIDHeader))
					for _, name := range []string{"X-IDE-Type", "X-IDE-Version", "X-Private-Data", "X-Codebuddy-Request", "X-Agent-Type"} {
						if capture.headers.Get(name) != "" {
							t.Fatalf("added desktop assertion %s", name)
						}
					}
					if path == "/v1/search" {
						if capture.body["query"] != body["query"] || capture.body["max_results"] != float64(8) {
							t.Fatal("search payload lost")
						}
					} else if capture.body["url"] != body["url"] || capture.body["format"] != "markdown" {
						t.Fatal("fetch payload lost")
					}
				}
				if conversations[0] != conversations[1] || conversations[0] == conversations[2] || requestIDs[0] == requestIDs[1] {
					t.Fatal("session scope/lifecycle incorrect")
				}
				h.key.mu.Lock()
				usage := h.key.usage
				h.key.mu.Unlock()
				if usage.Total != 4 || usage.Unknown != 0 || usage.Requests != 2 {
					t.Fatalf("usage=%+v", usage)
				}
				// Recreate key memory as startup does, then replay the un-compacted WAL.
				ledger.mu.Lock()
				if ledger.file != nil {
					_ = ledger.file.Close()
					ledger.file = nil
				}
				ledger.writable = false
				ledger.mu.Unlock()
				if err := keyStoreLoad(); err != nil {
					t.Fatal(err)
				}
				registerRuntimeKeys()
				if err := ledgerLoad(keyDataDir); err != nil {
					t.Fatal(err)
				}
				h.key = keyByID(h.key.ID)
				h.key.mu.Lock()
				recovered := h.key.usage
				h.key.mu.Unlock()
				if recovered.Total != 4 || recovered.Requests != 2 {
					t.Fatalf("WAL recovery=%+v", recovered)
				}
				for _, secret := range []string{"private-query-sentinel", "private-question-sentinel", "private-url-sentinel", "private-content-sentinel", h.keys[0]} {
					if strings.Contains(h.logs.String(), secret) {
						t.Fatalf("log leaked %s", secret)
					}
				}
				assertToolReleased(t, h)
			})
		}
	}
}

func assertToolReleased(t *testing.T, h *compatHarness) {
	t.Helper()
	_, active, waiting := ingressAdmission.snapshot()
	_, upstreamActive, _ := modelConcurrency.snapshot("")
	if active != 0 || waiting != 0 || upstreamActive != 0 || atomic.LoadInt32(&h.key.inflight) != 0 {
		t.Fatal("request/concurrency lease leaked")
	}
	ledger.mu.Lock()
	pending := len(ledger.pendingAttempts)
	ledger.mu.Unlock()
	if pending != 0 {
		t.Fatal("unsettled WAL intent")
	}
}

func TestAgentToolRejectionNoReplayOrSensitiveLogs(t *testing.T) {
	for _, path := range []string{"/v1/search", "/v1/fetch"} {
		for _, tc := range []struct {
			name   string
			status int
			body   string
			want   int
			stop   bool
		}{
			{"channel", 400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel private-error-sentinel"}`, 400, false},
			{"restriction", 403, `{"code":"11140","msg":"private-error-sentinel"}`, 403, true},
			{"envelope_restriction", 200, `{"code":11140,"msg":"private-error-sentinel"}`, 502, true},
			{"rejection_with_usage", 200, `{"code":11140,"msg":"private-error-sentinel","usage":{"credit":3}}`, 502, true},
			{"generic403", 403, `private-error-sentinel`, 403, false},
			{"server_error", 500, `private-error-sentinel`, 502, false},
		} {
			t.Run(path+tc.name, func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				h.reply.Store(&compatReply{tc.status, tc.body})
				body, _ := toolFixture(path)
				status, out := h.send(t, path, body, nil, 0)
				if status != tc.want || h.calls.Load() != 1 {
					t.Fatalf("status=%d calls=%d body=%s", status, h.calls.Load(), out)
				}
				if strings.Contains(tc.body, "11140") && !bytes.Contains(out, []byte("11140")) {
					t.Fatal("business code lost")
				}
				if strings.Contains(tc.body, "11128") && !bytes.Contains(out, []byte("11128")) {
					t.Fatal("channel code lost")
				}
				accountMu.Lock()
				stopped := h.accounts[0].Disabled
				other := h.accounts[1].Disabled
				accountMu.Unlock()
				if stopped != tc.stop || other {
					t.Fatal("wrong account state")
				}
				if _, err := os.Stat(h.accounts[0].Path); err != nil {
					t.Fatal("credential deleted")
				}
				if strings.Contains(h.logs.String(), "private-error-sentinel") || bytes.Contains(out, []byte("private-error-sentinel")) {
					t.Fatal("upstream body echoed")
				}
				if tc.name == "envelope_restriction" && !strings.Contains(h.logs.String(), "http_status=200 business_code=11140") {
					t.Fatal("diagnostic confused upstream and downstream status")
				}
				if tc.name == "rejection_with_usage" {
					h.key.mu.Lock()
					usage := h.key.usage
					h.key.mu.Unlock()
					if usage.Total != 3 || usage.Requests != 1 || usage.Unknown != 0 {
						t.Fatalf("rejection discarded measured usage: %+v", usage)
					}
				}
				assertToolReleased(t, h)
			})
		}
	}
}

func TestAgentToolBoundsAuthAndCompression(t *testing.T) {
	for _, path := range []string{"/v1/search", "/v1/fetch"} {
		t.Run(path, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			body, reply := toolFixture(path)
			h.reply.Store(&compatReply{200, reply})
			raw, _ := json.Marshal(body)
			for _, tc := range []struct {
				name             string
				raw              []byte
				auth, compressed bool
				want             int
			}{
				{"anonymous", raw, false, false, 401},
				{"oversized", append(append([]byte{}, raw...), bytes.Repeat([]byte(" "), webSearchMaxBodyBytes)...), true, false, 413},
				{"gzip", raw, true, true, 200},
			} {
				data := tc.raw
				if tc.compressed {
					var b bytes.Buffer
					gz := gzip.NewWriter(&b)
					_, _ = gz.Write(data)
					_ = gz.Close()
					data = b.Bytes()
				}
				req, _ := http.NewRequest("POST", h.server.URL+path, bytes.NewReader(data))
				if tc.auth {
					req.Header.Set("Authorization", "Bearer "+h.keys[0])
				}
				if tc.compressed {
					req.Header.Set("Content-Encoding", "gzip")
				}
				resp, err := h.server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != tc.want {
					t.Fatalf("%s status=%d", tc.name, resp.StatusCode)
				}
			}
			if h.calls.Load() != 1 {
				t.Fatalf("rejected requests reached upstream: %d", h.calls.Load())
			}
			h.key.mu.Lock()
			usage := h.key.usage
			h.key.mu.Unlock()
			if usage.Unknown != 1 || usage.Total != 0 || usage.Requests != 1 {
				t.Fatalf("missing usage misbilled: %+v", usage)
			}
			assertToolReleased(t, h)
		})
	}
}

func TestAgentToolRejectsRedirectAndTruncatedResponses(t *testing.T) {
	for _, mode := range []string{"redirect", "oversize", "truncated", "wrong_shape", "string_zero", "fractional_code"} {
		t.Run(mode, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			var leaked atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				leaked.Add(1)
				_, _ = io.WriteString(w, `{"results":[]}`)
			}))
			defer target.Close()
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "redirect":
					w.Header().Set("Location", target.URL)
					w.WriteHeader(307)
				case "oversize":
					_, _ = io.WriteString(w, `{"results":[]}`+strings.Repeat(" ", webSearchMaxBodyBytes))
				case "truncated":
					w.Header().Set("Content-Length", "200")
					_, _ = io.WriteString(w, `{"results":[]}`)
				case "wrong_shape":
					_, _ = io.WriteString(w, `{"results":"not an array"}`)
				case "string_zero":
					_, _ = io.WriteString(w, `{"code":"0","results":[]}`)
				case "fractional_code":
					_, _ = io.WriteString(w, `{"code":0.5,"results":[]}`)
				}
			}))
			defer up.Close()
			profileCN.Base = up.URL
			cfg.HttpClient = up.Client()
			body, _ := toolFixture("/v1/search")
			status, out := h.send(t, "/v1/search", body, nil, 0)
			want := 502
			if mode == "string_zero" {
				want = 200
			}
			if status != want || leaked.Load() != 0 {
				t.Fatalf("status=%d leaks=%d body=%s", status, leaked.Load(), out)
			}
			assertToolReleased(t, h)
		})
	}
}

func TestAgentToolRechecksDisabledAccountAfterConcurrencyWait(t *testing.T) {
	h := newCompatHarness(t, "cn")
	modelConcurrency.configure(concurrentFileConfig{MaxAcc: 1})
	release, err := modelConcurrency.acquire(context.Background(), modelConcurrencyKey(h.accounts[0]), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	body, _ := toolFixture("/v1/search")
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/v1/search", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+h.keys[0])
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { newAPIHandler().ServeHTTP(rec, req); close(done) }()
	// A prepared request is waiting for the held account slot, before any call.
	deadline := time.After(3 * time.Second)
	for !strings.Contains(h.logs.String(), "阶段=排队") {
		select {
		case <-deadline:
			t.Fatal("request did not queue")
		case <-time.After(time.Millisecond):
		}
	}
	accountMu.Lock()
	h.accounts[0].Disabled = true
	accountMu.Unlock()
	release()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("queue did not release")
	}
	if rec.Code < 400 || h.calls.Load() != 0 {
		t.Fatal("disabled queued account sent upstream")
	}
	assertToolReleased(t, h)
}

func TestAgentToolCancellationStopsUpstreamAndSettlesUnknown(t *testing.T) {
	for _, path := range []string{"/v1/search", "/v1/fetch"} {
		t.Run(path, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			arrived, canceled := make(chan struct{}), make(chan struct{})
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(arrived)
				<-r.Context().Done()
				close(canceled)
			}))
			defer up.Close()
			profileCN.Base = up.URL
			cfg.HttpClient = up.Client()
			body, _ := toolFixture(path)
			raw, _ := json.Marshal(body)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("POST", path, bytes.NewReader(raw)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+h.keys[0])
			rec := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { newAPIHandler().ServeHTTP(rec, req); close(done) }()
			select {
			case <-arrived:
			case <-time.After(3 * time.Second):
				t.Fatal("no upstream request")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("request not canceled")
			}
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream not canceled")
			}
			h.key.mu.Lock()
			usage := h.key.usage
			h.key.mu.Unlock()
			if usage.Unknown != 1 || usage.Total != 0 {
				t.Fatalf("ambiguous canceled call lost: %+v", usage)
			}
			assertToolReleased(t, h)
		})
	}
}

func TestAgentToolKeyPolicy(t *testing.T) {
	for _, path := range []string{"/v1/search", "/v1/fetch"} {
		for _, policy := range []string{"disabled", "quota", "model", "ledger"} {
			t.Run(fmt.Sprintf("%s/%s", path, policy), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				h.key.mu.Lock()
				switch policy {
				case "disabled":
					h.key.Enabled = false
				case "quota":
					h.key.QuotaTotal = 1
					h.key.usage.Total = 1
				case "model":
					h.key.Models = []string{compatModel}
				}
				h.key.mu.Unlock()
				if policy == "ledger" {
					ledger.mu.Lock()
					ledger.state.Incomplete = true
					ledger.mu.Unlock()
				}
				body, _ := toolFixture(path)
				status, out := h.send(t, path, body, nil, 0)
				if status < 400 || h.calls.Load() != 0 {
					t.Fatalf("policy bypass status=%d calls=%d body=%s", status, h.calls.Load(), out)
				}
				assertToolReleased(t, h)
			})
		}
	}
}
