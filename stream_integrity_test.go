package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const auditUUID = "00000000-0000-4000-8000-000000000123"
const auditJWT = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.sig"

func auditMalformedSSE() string {
	return "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n" +
		"data: {bad json}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
}

func TestChatStreamMalformedChunkIsIncomplete(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		for _, label := range []string{"before-finish", "after-finish"} {
			t.Run(fmt.Sprintf("mode=%d/%s", mode, label), func(t *testing.T) {
				old := fastPathMode.Load()
				fastPathMode.Store(int32(mode))
				defer fastPathMode.Store(old)
				upstream := auditMalformedSSE()
				if label == "after-finish" {
					upstream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"stop\"}]}\n\n" +
						"data: {bad json}\n\ndata: [DONE]\n\n"
				}
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				streamChatResponse(rec, req, &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, "m", 1, &Account{Path: "offline.json"}, &profileCN, time.Now())
				out := rec.Body.String()
				if !strings.Contains(out, "stream_interrupted") || strings.Contains(out, "data: [DONE]") {
					t.Fatalf("malformed mode=%d %s was reported as success: %q", mode, label, out)
				}
			})
		}
	}
}

func TestChatAggregateMalformedChunkKeepsUsage(t *testing.T) {
	upstream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":3}}\n\n" +
		"data: {bad json}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	_, err := aggregateCompletion(strings.NewReader(upstream), "m")
	if err == nil {
		t.Fatal("malformed aggregate chunk was accepted")
	}
	usage, ok := aggregateFailureUsage(err)
	if !ok || usage["prompt_tokens"] != float64(17) || usage["completion_tokens"] != float64(3) {
		t.Fatalf("measured usage was lost: usage=%v err=%v", usage, err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	writeChatAggregate(rec, req, &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, "m", 1, &Account{Path: "offline.json"}, &profileCN, time.Now())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("aggregate status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResponsesMalformedChunkIsIncomplete(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		t.Run(fmt.Sprintf("mode=%d", mode), func(t *testing.T) {
			old := fastPathMode.Load()
			fastPathMode.Store(int32(mode))
			defer fastPathMode.Store(old)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			streamResponsesResponse(rec, req, &http.Response{Body: io.NopCloser(strings.NewReader(auditMalformedSSE()))}, "m", 1, &Account{Path: "offline.json"}, &profileCN, time.Now())
			out := rec.Body.String()
			if !strings.Contains(out, "response.failed") || strings.Contains(out, "response.completed") || strings.Contains(out, "data: [DONE]") {
				t.Fatalf("malformed %d stream was reported as success: %q", mode, out)
			}
		})
	}
}

func TestResponsesAggregateMalformedChunkKeepsUsage(t *testing.T) {
	upstream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":3}}\n\n" +
		"data: {bad json}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	writeResponsesAggregate(rec, req, &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, "m", 1, &Account{Path: "offline.json"}, &profileCN, time.Now())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("aggregate status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNonStreamChatRejectsMultipleChoices(t *testing.T) {
	h := newCompatHarness(t, "cn")
	status, out := h.send(t, "/v1/chat/completions", map[string]any{
		"model": compatModel, "stream": false, "n": 2,
		"messages": []any{map[string]any{"role": "user", "content": "Synthetic input."}},
	}, nil, 0)
	if status != http.StatusBadRequest || h.calls.Load() != 0 || !strings.Contains(string(out), "n=1") {
		t.Fatalf("non-stream n>1 reached upstream: status=%d calls=%d output=%s", status, h.calls.Load(), out)
	}
}

func newReplayProbeBody(first string, readErr error, onReplay func()) *firstByteReplayBody {
	reader := io.NopCloser(&dataThenErrorReader{data: []byte(first), err: readErr})
	return newFirstByteReplayBody(reader, context.Background(), nil, 1, "m", 1, func(context.Context) (*http.Response, error) {
		if onReplay != nil {
			onReplay()
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("data: ok\n\n"))}, nil
	})
}

func TestFastPathRejectsInvalidJSONLiterals(t *testing.T) {
	bodies := map[string]string{
		"invalid-extension": `data: {"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":"stop"}],"extension":INVALID}` + "\n\ndata: [DONE]\n\n",
		"invalid-escape":    `data: {"choices":[{"index":0,"delta":{"content":"bad\q"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n",
	}
	for name, upstream := range bodies {
		for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
			t.Run(fmt.Sprintf("%s/mode=%d", name, mode), func(t *testing.T) {
				old := fastPathMode.Load()
				fastPathMode.Store(int32(mode))
				defer fastPathMode.Store(old)

				chat := httptest.NewRecorder()
				chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				streamChatResponse(chat, chatReq, &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, "m", 1, &Account{Path: "offline.json"}, &profileCN, time.Now())
				chatOut := chat.Body.String()
				if !strings.Contains(chatOut, "stream_interrupted") || strings.Contains(chatOut, "data: [DONE]") {
					t.Fatalf("chat fast path accepted invalid JSON: %q", chatOut)
				}

				responses := httptest.NewRecorder()
				responsesReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				streamResponsesResponse(responses, responsesReq, &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, "m", 1, &Account{Path: "offline.json"}, &profileCN, time.Now())
				responsesOut := responses.Body.String()
				if !strings.Contains(responsesOut, "response.failed") || strings.Contains(responsesOut, "response.completed") || strings.Contains(responsesOut, "data: [DONE]") {
					t.Fatalf("responses fast path accepted invalid JSON: %q", responsesOut)
				}
			})
		}
	}
}

func TestAggregateFailureWithoutUsageSettlesUnknown(t *testing.T) {
	upstream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n" +
		"data: {bad json}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	handlers := []struct {
		name string
		path string
		fn   func(http.ResponseWriter, *http.Request, *http.Response, *Account, time.Time)
	}{
		{"chat", "/v1/chat/completions", func(w http.ResponseWriter, r *http.Request, resp *http.Response, acc *Account, start time.Time) {
			writeChatAggregate(w, r, resp, "aggregate-no-usage-chat", 1, acc, &profileCN, start)
		}},
		{"responses", "/v1/responses", func(w http.ResponseWriter, r *http.Request, resp *http.Response, acc *Account, start time.Time) {
			writeResponsesAggregate(w, r, resp, "aggregate-no-usage-responses", 1, acc, &profileCN, start)
		}},
	}
	for _, handler := range handlers {
		t.Run(handler.name, func(t *testing.T) {
			resetKeyWorld(t)
			if err := ledgerLoad(keyDataDir); err != nil {
				t.Fatal(err)
			}
			_, k := mustCreateKey(t, keyCreateOptions{Name: "aggregate-no-usage"})

			req := httptest.NewRequest(http.MethodPost, handler.path, nil)
			req = withKeyIdentity(req, &keyIdentity{ID: k.ID})
			p := &statsAttempt{id: "acc", attemptID: "aggregate-no-usage-" + handler.name, model: "m", started: time.Now()}
			req = req.WithContext(context.WithValue(req.Context(), statsRequestContextKey{}, p))
			rec := httptest.NewRecorder()
			handler.fn(rec, req, &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, &Account{Path: "acc"}, time.Now())
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("aggregate status=%d body=%s", rec.Code, rec.Body.String())
			}
			statsFinishAttempt(req, rec.Code)

			k.mu.Lock()
			u := k.usage
			k.mu.Unlock()
			if u.Unknown != 1 || u.Requests != 1 {
				t.Fatalf("无 usage 的聚合失败应结算 1 次未知请求，实际 %+v", u)
			}
		})
	}
}

func TestFirstByteReplayHandlesDataAndErrorInSameRead(t *testing.T) {
	rst := errors.New("read: connection reset by peer")
	for _, tc := range []struct {
		name       string
		first      string
		wantReplay bool
	}{
		{"comment-only", ": heartbeat\n\n", true},
		{"empty-data", "data: \n\n", true},
		{"effective-data", "data: {\"a\":1}\n\n", false},
		{"partial-data-prefix", "data: ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			body := newFirstByteReplayBody(&dataWithErrorReader{data: []byte(tc.first), err: rst}, context.Background(), nil, 1, "m", 1,
				func(context.Context) (*http.Response, error) {
					called = true
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("data: ok\n\n"))}, nil
				})
			buf := make([]byte, 1024)
			n, err := body.Read(buf)
			if !tc.wantReplay {
				if n == 0 || err == nil || called {
					t.Fatalf("non-replayable same-read body was replayed: n=%d err=%v called=%t", n, err, called)
				}
				return
			}
			if n == 0 || err != nil || !called {
				t.Fatalf("comment/empty data with same-read error was not replayed: n=%d err=%v called=%t", n, err, called)
			}
			n, err = body.Read(buf)
			if n == 0 || err != nil {
				t.Fatalf("replacement body was not readable after same-read replay: n=%d err=%v", n, err)
			}
		})
	}
}

type dataWithErrorReader struct {
	data []byte
	err  error
	done bool
}

func (r *dataWithErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(p, r.data), r.err
}

func (r *dataWithErrorReader) Close() error { return nil }

func TestFirstByteReplayOnlyUsesEffectiveData(t *testing.T) {
	rst := errors.New("read: connection reset by peer")
	for _, tc := range []struct {
		name       string
		first      string
		wantReplay bool
	}{
		{"comment-only", ": heartbeat\n\n", true},
		{"empty-data", "data: \n\n", true},
		{"comment-without-newline", ": heartbeat", false},
		{"partial-data-prefix", "data: ", false},
		{"effective-data", "data: {\"a\":1}\n\n", false},
		{"effective-data-leading-space", " data: {\"a\":1}\n\n", false},
		{"bare-json", "{\"a\":1}\n\n", false},
		{"bare-json-leading-space", " {\"a\":1}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			body := newReplayProbeBody(tc.first, rst, func() { called = true })
			buf := make([]byte, 1024)
			if n, _ := body.Read(buf); n == 0 {
				t.Fatal("first read returned no bytes")
			}
			n, err := body.Read(buf)
			if tc.wantReplay {
				if !called || n == 0 || err != nil {
					t.Fatalf("comment-only/partial body was not replayed: n=%d err=%v called=%t", n, err, called)
				}
			} else if err == nil || called {
				t.Fatalf("effective data must not be replayed: n=%d err=%v called=%t", n, err, called)
			}
		})
	}
}

func TestUpstreamErrorStateIsSanitized(t *testing.T) {
	oldWriter := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(oldWriter)
	acc := &Account{Path: "synthetic-sanitize.json"}
	raw := "upstream: userID:" + auditUUID + " token: Bearer " + auditJWT
	markCooldown(acc, time.Now().Add(time.Minute), raw)
	markModelCooldown(acc, "model", time.Now().Add(time.Minute), raw)
	markModelQuotaBlocked(acc, "model", raw)
	accountMu.Lock()
	cooldown := acc.CooldownMsg
	modelReason := acc.ModelStates["model"].LastReason
	accountMu.Unlock()
	for label, value := range map[string]string{"cooldown": cooldown, "model": modelReason} {
		if strings.Contains(value, auditUUID) || strings.Contains(value, auditJWT) {
			t.Fatalf("%s state leaked upstream identifiers: %q", label, value)
		}
	}
}

func TestDoJSONContextSanitizesUpstreamMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"code":11128,"msg":"userID:%s Bearer %s"}`, auditUUID, auditJWT)
	}))
	defer srv.Close()
	_, _, err := doJSONContext(context.Background(), srv.Client(), http.MethodGet, srv.URL, nil, nil)
	if err == nil || strings.Contains(err.Error(), auditUUID) || strings.Contains(err.Error(), auditJWT) {
		t.Fatalf("upstream identifier leaked through error: %v", err)
	}
	if !strings.Contains(err.Error(), "11128") {
		t.Fatalf("business code was removed by sanitization: %v", err)
	}
}

func TestUpstreamRejectionClientMessageIsSanitized(t *testing.T) {
	body := fmt.Sprintf(`{"code":11128,"msg":"denied userID:%s token:Bearer %s"}`, auditUUID, auditJWT)
	rejection := detectUpstreamStreamRejection([]byte(body))
	if rejection == nil {
		t.Fatal("rejection was not detected")
	}
	message := rejection.clientMessage()
	if strings.Contains(message, auditUUID) || strings.Contains(message, auditJWT) {
		t.Fatalf("client message leaked upstream identifiers: %q", message)
	}
	if !strings.Contains(message, "11128") || !strings.Contains(message, "denied") {
		t.Fatalf("client message lost business details: %q", message)
	}
}
