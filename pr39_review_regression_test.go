package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPR39ReviewAuthSwitchWriteFailureStaysClosedAfterRestart(t *testing.T) {
	isolatedGatewayConfig(t)
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: false}); err != nil {
		t.Fatal(err)
	}
	dir := resetKeyWorld(t)
	if err := os.Mkdir(filepath.Join(dir, authSwitchName+".tmp"), 0700); err != nil {
		t.Fatal(err)
	}
	plain, key, err := keyCreate(keyCreateOptions{Name: "synthetic-failed-first-key"})
	if err == nil || plain != "" || key != nil {
		t.Fatal("failed auth write must not publish a key")
	}
	apiKeys.mu.Lock()
	apiKeys.byID = map[string]*apiKey{}
	apiKeys.byPrefix = map[string]*apiKey{}
	apiKeys.requireAuth = false
	apiKeys.mu.Unlock()
	apiKeys.degraded.Store(false)
	authSwitchMu.Lock()
	authSwitchOn = false
	authSwitchMu.Unlock()
	for restart := 0; restart < 2; restart++ {
		if err := loadAuthSwitch(dir); err != nil {
			t.Fatal(err)
		}
		if err := keyStoreLoad(); err != nil {
			t.Fatal(err)
		}
		if !authRequired() {
			t.Fatal("auth-switch write failure reopened anonymous access after restart")
		}
		if len(apiKeys.byID) != 0 {
			t.Fatal("unreturned managed key was retained")
		}
	}
}

func TestPR39ReviewUncertainBeginIsNotCanceled(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	_, key := mustCreateKey(t, keyCreateOptions{Name: "uncertain", QuotaTotal: 10})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	attempt := &statsAttempt{id: "synthetic-account", attemptID: "unknown-network-result", model: "synthetic", started: time.Now()}
	request = request.WithContext(context.WithValue(request.Context(), statsRequestContextKey{}, attempt))
	request = withKeyIdentity(request, &keyIdentity{ID: key.ID})
	if err := beginKeyLedgerAttempt(request); err != nil {
		t.Fatal(err)
	}
	statsFinishAttempt(request, http.StatusBadGateway)
	if key.usage.Unknown != 1 || key.usage.Requests != 1 || key.usage.ZeroCost != 0 {
		t.Fatalf("possible consumption silently canceled: %+v", key.usage)
	}
	restartRecoveryTest(t, dir)
	if keyByID(key.ID).usage.Unknown != 1 {
		t.Fatal("unknown consumption lost across restart")
	}
}

func TestPR39ReviewTransportCostCertainty(t *testing.T) {
	cases := []struct {
		name                  string
		status                int
		sentHeaders, sentBody bool
		wantUnknown           int64
	}{
		{name: "dial-refused", wantUnknown: 0},
		{name: "headers-then-eof", sentHeaders: true, wantUnknown: 1},
		{name: "body-then-eof", sentBody: true, wantUnknown: 1},
		{name: "eof-without-write-evidence", wantUnknown: 1},
		{name: "dns-failure", wantUnknown: 0},
		{name: "http500", status: 500, wantUnknown: 1},
		{name: "http502", status: 502, wantUnknown: 1},
		{name: "http503", status: 503, wantUnknown: 1},
		{name: "http408", status: 408, wantUnknown: 1},
		{name: "http401", status: 401, wantUnknown: 0},
		{name: "http403", status: 403, wantUnknown: 0},
		{name: "http429", status: 429, wantUnknown: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withStreamTestAccount(t)
			dir := resetKeyWorld(t)
			if err := ledgerLoad(dir); err != nil {
				t.Fatal(err)
			}
			_, key := mustCreateKey(t, keyCreateOptions{Name: tc.name, QuotaTotal: 10})
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			attempt := &statsAttempt{id: "synthetic-account", attemptID: "transport-" + tc.name, model: "synthetic", started: time.Now()}
			request = request.WithContext(context.WithValue(request.Context(), statsRequestContextKey{}, attempt))
			request = withKeyIdentity(request, &keyIdentity{ID: key.ID})
			cfg.HttpClient = &http.Client{Transport: retryTransportFunc(func(r *http.Request) (*http.Response, error) {
				trace := httptrace.ContextClientTrace(r.Context())
				if tc.sentHeaders {
					trace.WroteHeaders()
				}
				if tc.sentBody {
					trace.WroteRequest(httptrace.WroteRequestInfo{})
				}
				if tc.status != 0 {
					return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				if tc.sentHeaders || tc.sentBody || tc.name == "eof-without-write-evidence" {
					return nil, io.EOF
				}
				if tc.name == "dns-failure" {
					return nil, &net.DNSError{Err: "synthetic lookup failure", Name: "synthetic.invalid", IsNotFound: true}
				}
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrNotExist}
			})}
			accountMu.Lock()
			account := accounts[0]
			accountMu.Unlock()
			resp, cancel, err := doUpstreamRequest(request, account, &profileCN, []byte("{}"), 1, "synthetic-trace")
			status := 502
			if err == nil {
				status = resp.StatusCode
				resp.Body.Close()
				cancel()
			}
			statsFinishAttempt(request, status)
			if key.usage.Unknown != tc.wantUnknown || key.usage.Requests != tc.wantUnknown || key.usage.Total != 0 || key.usage.ZeroCost != 0 {
				t.Fatalf("wrong certainty accounting: %+v", key.usage)
			}
			if len(ledger.pendingAttempts) != 0 {
				t.Fatal("settled or provably uncharged attempt left open intent")
			}
			restartRecoveryTest(t, dir)
			if keyByID(key.ID).usage.Unknown != tc.wantUnknown {
				t.Fatal("restart changed unknown charge count")
			}
		})
	}
}

func TestPR39ReviewResponsesTranslationWithoutUsageCountsUnknown(t *testing.T) {
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, key := mustCreateKey(t, keyCreateOptions{Name: "responses-translation", QuotaTotal: 10})
	handler := requestAuditMiddleware(authMiddleware(keyAdmissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lease := outputTestLease(t, 1024)
		r = r.WithContext(context.WithValue(r.Context(), outputLeaseKey{}, lease))
		r = withResponsesMetadata(r, map[string]any{"synthetic": strings.Repeat("x", 2048)})
		*statsAttemptFor(r) = statsAttempt{id: "synthetic-account", attemptID: "response-conversion-no-usage", model: "synthetic", started: time.Now()}
		sse := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		response := &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sse))}
		writeResponsesAggregate(w, r, response, "synthetic", 1, &Account{Path: "synthetic.json"}, &profileCN, time.Now())
	}))))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer "+plain)
	writer := httptest.NewRecorder()
	handler.ServeHTTP(writer, request)
	if writer.Code != 500 || !strings.Contains(writer.Body.String(), "translate_error") {
		t.Fatalf("conversion failure fixture missed: %d %s", writer.Code, writer.Body.String())
	}
	if key.usage.Unknown != 1 || key.usage.Requests != 1 || key.usage.ZeroCost != 0 {
		t.Fatalf("translation lost unknown cost: %+v", key.usage)
	}
	restartRecoveryTest(t, dir)
	if keyByID(key.ID).usage.Unknown != 1 {
		t.Fatal("conversion failure unknown cost lost on restart")
	}
}

func TestPR39ReviewExplicitRejectionKeepsEarlierConsumption(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, partial := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/partial=%t/stream=%t", protocol, partial, stream), func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					body := ""
					if partial {
						body = "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"
					}
					body += "data: {\"code\":11128,\"msg\":\"synthetic rejection\"}\n\n"
					h.reply.Store(&compatReply{status: 200, body: body})
					path, request := aggregateFailureRequest(protocol)
					request["stream"] = stream
					status, _ := h.send(t, path, request, nil, 0)
					if h.calls.Load() != 1 || (!stream && status != 502) {
						t.Fatalf("rejection changed delivery/retry: status=%d calls=%d", status, h.calls.Load())
					}
					want := int64(0)
					if partial {
						want = 1
					}
					h.key.mu.Lock()
					usage := h.key.usage
					h.key.mu.Unlock()
					if usage.Unknown != want || usage.Requests != want || usage.Total != 0 {
						t.Fatalf("wrong explicit rejection accounting: %+v", usage)
					}
				})
			}
		}
	}
}
