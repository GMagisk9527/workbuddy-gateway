package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type unexpectedEOFAuditBody struct{ io.ReadCloser }

func (b unexpectedEOFAuditBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func aggregateFailureRequest(protocol string) (string, map[string]any) {
	path := "/v1/chat/completions"
	body := map[string]any{"model": compatModel, "stream": false,
		"messages": []any{map[string]any{"role": "user", "content": "Synthetic failure boundary."}}}
	if protocol == "responses" {
		path = "/v1/responses"
		delete(body, "messages")
		body["input"] = "Synthetic failure boundary."
	} else if protocol == "messages" {
		path = "/v1/messages"
		body["max_tokens"] = 64
	}
	return path, body
}

func TestAggregateFailureSettlementMatrix(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, failure := range []string{"missing_finish", "read_error", "invalid_json"} {
			for _, credit := range []string{"missing", "zero", "paid"} {
				t.Run(protocol+"/"+failure+"/"+credit, func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					field := ""
					wantTotal, wantUnknown := 0.0, int64(1)
					if credit == "zero" {
						field, wantUnknown = `,"usage":{"credit":0}`, 0
					} else if credit == "paid" {
						field, wantTotal, wantUnknown = `,"usage":{"credit":3}`, 3, 0
					}
					upstream := `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]` + field + "}\n\n"
					if failure == "invalid_json" {
						upstream += "data: {bad json}\n\n"
					} else if failure == "missing_finish" {
						upstream += "data: [DONE]\n\n"
					} else {
						base := cfg.HttpClient.Transport
						cfg.HttpClient.Transport = retryTransportFunc(func(r *http.Request) (*http.Response, error) {
							response, err := base.RoundTrip(r)
							if response != nil {
								response.Body = unexpectedEOFAuditBody{response.Body}
							}
							return response, err
						})
					}
					h.reply.Store(&compatReply{status: 200, body: upstream})
					path, body := aggregateFailureRequest(protocol)
					status, _ := h.send(t, path, body, nil, 0)
					if status < 400 || h.calls.Load() != 1 {
						t.Fatalf("failure retried or reported success: status=%d calls=%d", status, h.calls.Load())
					}
					h.key.mu.Lock()
					usage := h.key.usage
					h.key.mu.Unlock()
					if usage.Total != wantTotal || usage.Unknown != wantUnknown || usage.Requests != 1 {
						t.Fatalf("failed response did not settle exactly once: %+v", usage)
					}
				})
			}
		}
	}
}

func TestAggregateExplicitRejectionDoesNotSettleUnknown(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		t.Run(protocol, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			h.reply.Store(&compatReply{status: 200, body: "data: {\"code\":11128,\"msg\":\"Synthetic rejection.\"}\n\n"})
			path, body := aggregateFailureRequest(protocol)
			status, _ := h.send(t, path, body, nil, 0)
			if status != http.StatusBadGateway || h.calls.Load() != 1 {
				t.Fatalf("rejection handling changed: status=%d calls=%d", status, h.calls.Load())
			}
			h.key.mu.Lock()
			usage := h.key.usage
			h.key.mu.Unlock()
			if usage.Unknown != 0 || usage.Requests != 0 || usage.Total != 0 {
				t.Fatalf("explicit rejection was billed: %+v", usage)
			}
		})
	}
}

func TestMessagesUnknownFailuresReachAdmissionTolerance(t *testing.T) {
	h := newCompatHarness(t, "cn")
	h.keys[0], h.key = mustCreateKey(t, keyCreateOptions{Name: "synthetic-unknown-limit", QuotaTotal: 10})
	h.reply.Store(&compatReply{status: 200, body: "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"})
	path, body := aggregateFailureRequest("messages")
	for i := 0; i < defaultUnknownTolerance; i++ {
		status, _ := h.send(t, path, body, nil, 0)
		if status != http.StatusBadGateway {
			t.Fatalf("unknown attempt %d status=%d", i, status)
		}
	}
	status, _ := h.send(t, path, body, nil, 0)
	if status != http.StatusTooManyRequests || int(h.calls.Load()) != defaultUnknownTolerance {
		t.Fatalf("unknown usage protection bypassed: status=%d calls=%d", status, h.calls.Load())
	}
}

func TestChatMultiChoiceCompletionBoundaries(t *testing.T) {
	fixtures := []struct {
		name, body string
		complete   bool
	}{
		{"out_of_order", `data: {"choices":[{"index":1,"delta":{"content":"second"},"finish_reason":"stop"}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{"content":"first"},"finish_reason":"length"}]}` + "\n\n" +
			`data: {"choices":[],"usage":{"credit":0}}` + "\n\n", true},
		{"missing_candidate", `data: {"choices":[{"index":0,"delta":{"content":"first"},"finish_reason":"stop"}]}` + "\n\n", false},
		{"duplicate_finish", `data: {"choices":[{"index":0,"finish_reason":"stop"},{"index":0,"finish_reason":"stop"},{"index":1,"delta":{"content":"partial"}}]}` + "\n\n", false},
		{"sparse_indices", `data: {"choices":[{"index":1,"finish_reason":"stop"},{"index":2,"finish_reason":"stop"}]}` + "\n\n", false},
		{"fallback_numeric_indices", `data: {"choices":[{"index":1.0,"finish_reason":"stop"},{"index":0.0,"finish_reason":"stop"}]}` + "\n\n", true},
	}
	for _, mode := range []fastPathModeT{fastPathOff, fastPathShadow, fastPathOn} {
		for _, fixture := range fixtures {
			t.Run(fmt.Sprintf("mode=%d/%s", mode, fixture.name), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				fastPathMode.Store(int32(mode))
				h.reply.Store(&compatReply{status: 200, body: fixture.body + "data: [DONE]\n\n"})
				status, output := h.send(t, "/v1/chat/completions", map[string]any{"model": compatModel, "stream": true, "n": 2,
					"messages": []any{map[string]any{"role": "user", "content": "Synthetic choice completion."}}}, nil, 0)
				if status != 200 || h.calls.Load() != 1 {
					t.Fatalf("unexpected choice request status=%d calls=%d", status, h.calls.Load())
				}
				if done := strings.Contains(string(output), "data: [DONE]"); done != fixture.complete {
					t.Fatalf("candidate completion mismatch: %s", output)
				}
				if incomplete := strings.Contains(string(output), "stream_choices_incomplete"); incomplete == fixture.complete {
					t.Fatalf("candidate error mismatch: %s", output)
				}
			})
		}
	}
}
