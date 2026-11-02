package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Upstream, credentials, API keys and ledger are all
// synthetic and isolated by the existing integration harness.
func TestGeneric403DoesNotDisableHealthyPool(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				h := newRestrictedHarness(t, 0)
				h.reply.Store(&compatReply{http.StatusForbidden, "<html><title>Access denied</title>Temporary edge security block</html>"})
				status, _ := h.send(t, path, restrictedRequestBody(path, stream), nil, 0)
				firstCalls := h.calls.Load()
				disabled, markers := 0, 0
				accountMu.Lock()
				for _, acc := range h.accounts {
					if acc.Disabled {
						disabled++
					}
					if _, err := os.Stat(markerPath(acc.Path)); err == nil {
						markers++
					}
				}
				accountMu.Unlock()
				// The read-only account endpoint always returns code=0 in this
				// harness, independently proving both credentials remain usable.
				h.reply.Store(&compatReply{200, conversationFixtureSSE(true)})
				recoveredStatus, _ := h.send(t, path, restrictedRequestBody(path, stream), nil, 0)
				t.Logf("edge rejection: status=%d model_calls=%d disabled=%d markers=%d; edge recovered: status=%d extra_model_calls=%d", status, firstCalls, disabled, markers, recoveredStatus, h.calls.Load()-firstCalls)
				if status != 403 || disabled != 0 || markers != 0 || firstCalls != 1 || recoveredStatus != 200 {
					t.Error("generic edge 403 permanently disabled healthy accounts and cascaded across pool")
				}
				assertRestrictedCleanup(t, h)
			})
		}
	}
}

func TestSSE11140StopsRestrictedAccount(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", path, stream), func(t *testing.T) {
				h := newRestrictedHarness(t, 0)
				withAccounts(t, h.accounts[0])
				h.reply.Store(&compatReply{200, "data: " + restrictedFixture + "\n\n"})
				status, out := h.send(t, path, restrictedRequestBody(path, stream), nil, 0)
				if !strings.Contains(string(out), "11140") {
					t.Fatalf("fixture did not reach business rejection: status=%d body=%s", status, out)
				}
				firstCalls := h.calls.Load()
				accountMu.Lock()
				disabled := h.accounts[0].Disabled
				accountMu.Unlock()
				_, markerErr := os.Stat(markerPath(h.accounts[0].Path))
				secondStatus, _ := h.send(t, path, restrictedRequestBody(path, stream), nil, 0)
				t.Logf("SSE 11140: status=%d disabled=%t marker=%t; next request: status=%d extra_model_calls=%d", status, disabled, markerErr == nil, secondStatus, h.calls.Load()-firstCalls)
				if !disabled || markerErr != nil || secondStatus != 503 || h.calls.Load() != firstCalls {
					t.Error("explicit SSE account restriction leaves account schedulable for later requests")
				}
				assertRestrictedCleanup(t, h)
			})
		}
	}
}

func TestRestrictedSSEPreservesUsageAndStopsOnlyRejectedAccount(t *testing.T) {
	for _, site := range []string{"cn", "intl"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, stream := range []bool{false, true} {
				for _, partial := range []string{"none", "unknown", "measured"} {
					t.Run(fmt.Sprintf("%s%s/stream=%t/%s", site, path, stream, partial), func(t *testing.T) {
						h := newRestrictedHarness(t, 0)
						healthyReply := h.reply.Load()
						for _, acc := range h.accounts {
							acc.Auth.Edition, acc.Edition = site, site
						}
						base := cfg.HttpClient.Transport
						cfg.HttpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
							if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
								t.Error("account restriction must not trigger credential verification/deletion")
							}
							return base.RoundTrip(r)
						})
						prefix := ""
						wantCredit, wantUnknown, wantRequests := 0.0, int64(0), int64(0)
						if partial != "none" {
							usage := ""
							wantUnknown, wantRequests = 1, 1
							if partial == "measured" {
								usage = `,"usage":{"credit":3,"prompt_tokens":5,"completion_tokens":1}`
								wantCredit, wantUnknown = 3, 0
							}
							prefix = `data: {"choices":[{"index":0,"delta":{"content":"synthetic partial output"}}]` + usage + "}\n\n"
						}
						h.reply.Store(&compatReply{200, prefix + "data: " + restrictedFixture + "\n\n"})
						body := restrictedRequestBody(path, stream)
						headers := map[string]string{"X-Session-ID": "restricted-synthetic-session"}
						status, out := h.send(t, path, body, headers, 0)
						wantStatus := 502
						if stream {
							wantStatus = 200
						}
						if status != wantStatus || !strings.Contains(string(out), "11140") || h.calls.Load() != 1 {
							t.Fatalf("restriction replayed or lost: status=%d calls=%d body=%s", status, h.calls.Load(), out)
						}
						for _, terminal := range []string{"[DONE]", "response.completed", "message_stop"} {
							if strings.Contains(string(out), terminal) {
								t.Fatalf("false success: %s", terminal)
							}
						}
						first := h.take(t)
						accountMu.Lock()
						stopped, unaffected := h.accounts[0].Disabled, !h.accounts[1].Disabled
						accountMu.Unlock()
						if !stopped || !unaffected {
							t.Fatal("restriction affected wrong account")
						}
						h.key.mu.Lock()
						before := h.key.usage
						h.key.mu.Unlock()
						if before.Total != wantCredit || before.Unknown != wantUnknown || before.Requests != wantRequests {
							t.Fatalf("wrong rejected-attempt settlement: %+v", before)
						}
						// Reapply persisted markers as at startup, then send another turn.
						cfg.AuthDir, cfg.AuthExplicit = "", true
						cfg.AuthFile = h.accounts[0].Path + "," + h.accounts[1].Path
						accountMu.Lock()
						h.accounts[0].Disabled = false
						loadDisabledMarkers()
						restored := h.accounts[0].Disabled
						accountMu.Unlock()
						if !restored {
							t.Fatal("restriction was not restored from disk")
						}
						h.reply.Store(healthyReply)
						status, out = h.send(t, path, body, headers, 0)
						if status != 200 || h.calls.Load() != 2 {
							t.Fatalf("healthy fallback failed: %d %s", status, out)
						}
						assertCompatSuccess(t, path, stream, out)
						second := h.take(t)
						if first.headers.Get("Authorization") == second.headers.Get("Authorization") ||
							first.headers.Get(conversationIDHeader) != second.headers.Get(conversationIDHeader) ||
							!reflect.DeepEqual(first.body, second.body) {
							t.Fatal("follow-up reused restricted account or changed request/session")
						}
						h.key.mu.Lock()
						after := h.key.usage
						h.key.mu.Unlock()
						if after.Total != wantCredit+0.1 || after.Unknown != wantUnknown || after.Requests != wantRequests+1 {
							t.Fatalf("follow-up settlement mismatch: %+v", after)
						}
						reloadKeysAndLedger(t, keyDataDir)
						key := keyByID(h.key.ID)
						key.mu.Lock()
						replayed := key.usage
						key.mu.Unlock()
						if !reflect.DeepEqual(after, replayed) || apiKeys.degraded.Load() {
							t.Fatal("WAL settlement did not recover cleanly")
						}
						assertRestrictedCleanup(t, h)
					})
				}
			}
		}
	}
}

func TestAggregateBusinessRejectionRetainsMeasuredUsage(t *testing.T) {
	for _, code := range []string{"11128", "11140"} {
		t.Run(code, func(t *testing.T) {
			stream := `data: {"choices":[{"delta":{"content":"partial"}}],"usage":{"credit":3}}` + "\n\n" +
				`data: {"code":` + code + `,"msg":"synthetic rejection"}` + "\n\n"
			_, err := aggregateCompletion(strings.NewReader(stream), compatModel)
			var rejection *upstreamStreamRejection
			if !errors.As(err, &rejection) || rejection.info.Code != code {
				t.Fatalf("business rejection lost: %v", err)
			}
			usage, retained := aggregateFailureUsage(err)
			credit, known := usageCredit(usage)
			if !retained || !known || credit != 3 {
				t.Fatalf("measured usage lost: %v", usage)
			}
		})
	}
}
