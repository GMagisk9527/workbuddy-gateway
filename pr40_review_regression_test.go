package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPR40PreparingRejectionsUseProtocolErrorShape(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/chat/completions", "/v1/responses"} {
		for _, mode := range []string{"full", "timeout"} {
			t.Run(path+"/"+mode, func(t *testing.T) {
				isolatePolicies(t)
				waiting := 0
				if mode == "timeout" {
					waiting = 1
				}
				ingressAdmission.configure(admissionFileConfig{MaxActive: 2, MaxPreparing: 1, MaxWaiting: &waiting, WaitTimeoutMs: 20})
				held, err := ingressAdmission.preparing.acquire(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer held()
				body := &admissionCountBody{Reader: strings.NewReader(`{"model":"synthetic","messages":[]}`)}
				request := httptest.NewRequest("POST", path, nil)
				request.Body = body
				handlerCalls := 0
				handler := requestAdmissionMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handlerCalls++ }))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != 429 || response.Header().Get("Retry-After") != "1" {
					t.Fatalf("status=%d headers=%v", response.Code, response.Header())
				}
				var output struct {
					Type  string `json:"type"`
					Error struct {
						Type string `json:"type"`
					} `json:"error"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &output); err != nil {
					t.Fatal(err)
				}
				want := "request_prepare_queue_full"
				if strings.HasPrefix(path, "/v1/messages") {
					want = "rate_limit_error"
					if output.Type != "error" {
						t.Fatalf("invalid Anthropic envelope: %s", response.Body.String())
					}
				}
				if output.Error.Type != want {
					t.Fatalf("error.type=%q want=%q body=%s", output.Error.Type, want, response.Body.String())
				}
				if body.reads.Load() != 0 || handlerCalls != 0 {
					t.Fatal("rejected request reached the handler or read its body")
				}
				_, active, queued := ingressAdmission.snapshot()
				if active != 0 || queued != 0 {
					t.Fatal("rejection leaked total admission")
				}
			})
		}
	}
}

func TestPR40CacheEvidenceDoesNotSurviveAccountIdentityReplacement(t *testing.T) {
	for _, change := range []string{"uid", "enterprise", "site", "replacement-pointer", "missing-uid", "missing-auth", "removed"} {
		t.Run(change, func(t *testing.T) {
			isolatePolicies(t)
			old := currentAffinitySettings()
			t.Cleanup(func() {
				setAffinityConfig(old.Enabled, old.Mode, int(old.TTL/time.Second), old.Capacity, old.PrefixHeadByte)
			})
			setAffinityConfig(true, "session", 1800, 8, 65536)
			acc := mkAccount("synthetic-reused-path.json")
			withAccounts(t, acc)
			hint := cacheAffinityHint{key: "synthetic-reused-binding", inputBytes: 1 << 20}
			affinityRemember(hint.key, acc.Path)
			now := time.Now()
			base := 250 * time.Millisecond
			cfg := schedulingFileConfig{CacheAffinityWaitMs: 1500}
			identity := affinityAccountIdentity(acc.Path)
			affinityObserveCache(hint.key, acc.Path, identity, 790016, true, true, now)
			affinityObserveCache(hint.key, acc.Path, identity, 790016, true, true, now)
			if got := cacheAffinityGrace(hint, acc.Path, base, cfg, now); got != 1500*time.Millisecond {
				t.Fatalf("warm fixture=%v", got)
			}
			accountMu.Lock()
			auth := *acc.Auth
			switch change {
			case "uid":
				auth.Account.UID = "different-upstream-user"
			case "enterprise":
				auth.Account.EnterpriseID = "different-enterprise"
			case "site":
				auth.Edition = "intl"
			case "missing-uid":
				auth.Account.UID = ""
			case "missing-auth":
				acc.Auth = nil
			case "removed":
				accounts = nil
			case "replacement-pointer":
				replacement := mkAccount(acc.Path)
				replacement.Auth.Account.UID = "replacement-user"
				accounts[0] = replacement
			}
			if change != "replacement-pointer" && change != "missing-auth" {
				acc.Auth = &auth
			}
			accountMu.Unlock()
			if got := cacheAffinityGrace(hint, acc.Path, base, cfg, now); got != base {
				t.Fatalf("reused path retained old %s evidence: grace=%v", change, got)
			}
		})
	}
}

func TestPR40LateUsageCannotReplaceNewIdentityEvidenceAndRenewalKeepsIt(t *testing.T) {
	isolatePolicies(t)
	old := currentAffinitySettings()
	t.Cleanup(func() {
		setAffinityConfig(old.Enabled, old.Mode, int(old.TTL/time.Second), old.Capacity, old.PrefixHeadByte)
	})
	setAffinityConfig(true, "session", 1800, 8, 65536)
	acc := mkAccount("synthetic-late-usage.json")
	withAccounts(t, acc)
	key := "synthetic-late-binding"
	hint := cacheAffinityHint{key: key, inputBytes: 1 << 20}
	affinityRemember(key, acc.Path)
	stale := httptest.NewRequest("POST", "/v1/messages", nil)
	stale = stale.WithContext(context.WithValue(stale.Context(), statsRequestContextKey{}, &statsAttempt{}))
	statsSelectAccount(stale, acc, "synthetic-cache-model")
	statsAttemptFor(stale).cacheAffinityKey = key
	statsObserveUsage(stale, map[string]any{"prompt_tokens": float64(800000), "prompt_cache_hit_tokens": float64(790016)}, true)
	oldIdentity := affinityAccountIdentity(acc.Path)
	accountMu.Lock()
	auth := *acc.Auth
	auth.Account.UID = "new-upstream-account"
	acc.Auth = &auth
	accountMu.Unlock()
	identity := affinityAccountIdentity(acc.Path)
	if identity == oldIdentity {
		t.Fatal("identity did not change on re-login")
	}
	now := time.Now()
	base := 250 * time.Millisecond
	cfg := schedulingFileConfig{CacheAffinityWaitMs: 1500}
	affinityObserveCache(key, acc.Path, identity, 790016, true, true, now)
	if got := cacheAffinityGrace(hint, acc.Path, base, cfg, now); got != base {
		t.Fatal("new identity inherited a previous hit")
	}
	affinityObserveCache(key, acc.Path, identity, 790016, true, true, now)
	statsFinishAttempt(stale, 200)
	if got := cacheAffinityGrace(hint, acc.Path, base, cfg, now); got != 1500*time.Millisecond {
		t.Fatal("late old usage overwrote the new account's evidence")
	}
	accountMu.Lock()
	renewed := *acc.Auth
	renewed.Auth.AccessToken = "synthetic-renewed-token"
	renewed.Auth.RefreshToken = "synthetic-renewed-refresh"
	renewed.Auth.ExpiresAt = time.Now().Add(2 * time.Hour).Unix()
	acc.Auth = &renewed
	accountMu.Unlock()
	if affinityAccountIdentity(acc.Path) != identity {
		t.Fatal("credential renewal changed stable identity")
	}
	if got := cacheAffinityGrace(hint, acc.Path, base, cfg, time.Now()); got != 1500*time.Millisecond {
		t.Fatal("credential renewal discarded valid cache evidence")
	}
}
