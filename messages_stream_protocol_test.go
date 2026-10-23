package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMessagesAggregatePreservesToolArgumentNumbers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arguments string
	}{
		{"integer-above-float64-precision", `{"id":9007199254740993}`},
		{"integer-above-uint64", `{"id":18446744073709551617}`},
		{"precise-decimal", `{"amount":0.12345678901234567890123456789}`},
		{"nested-number-values", `{"items":[9007199254740993,{"amount":1234567890.12345678901234567890}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunk, err := json.Marshal(map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": 0, "id": "call_numbers", "type": "function",
						"function": map[string]any{"name": "save_record", "arguments": tc.arguments},
					}}},
					"finish_reason": "tool_calls",
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			upstream := "data: " + string(chunk) + "\n\ndata: [DONE]\n\n"
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
			writeMessagesAggregate(rec, req, resp, "offline", 1, &Account{Path: "offline.json"}, &profileCN, time.Now(), false)
			if rec.Code != http.StatusOK {
				t.Fatalf("HTTP status=%d: %s", rec.Code, rec.Body.String())
			}
			// Decode the envelope only: decoding input through interface{} would
			// round the expected/actual numbers before the assertion could see it.
			var message struct {
				Content []struct {
					Type  string          `json:"type"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
				t.Fatal(err)
			}
			if len(message.Content) != 1 || message.Content[0].Type != "tool_use" {
				t.Fatalf("unexpected content: %s", rec.Body.String())
			}
			var got, want bytes.Buffer
			if err := json.Compact(&got, message.Content[0].Input); err != nil {
				t.Fatal(err)
			}
			if err := json.Compact(&want, []byte(tc.arguments)); err != nil {
				t.Fatal(err)
			}
			if got.String() != want.String() {
				t.Fatalf("tool argument numbers changed during aggregation:\n got: %s\nwant: %s", got.String(), want.String())
			}
		})
	}
}

func TestMessagesMalformedUpstreamJSONIsNotSuccess(t *testing.T) {
	const content = "data: {\"choices\":[{\"delta\":{\"content\":\"visible prefix\"}}]}\n\n"
	const malformed = "data: {\"choices\":[{\"delta\":{\"content\":\"lost fragment\"}}]\n\n"
	const finish = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	for _, stream := range []bool{true, false} {
		for _, tc := range []struct {
			name string
			body string
		}{
			{"malformed-before-finish", content + malformed + finish},
			{"malformed-after-finish", content + finish + malformed},
		} {
			t.Run(tc.name+map[bool]string{true: "/stream", false: "/aggregate"}[stream], func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				resp := &http.Response{Body: io.NopCloser(strings.NewReader(tc.body + "data: [DONE]\n\n"))}
				acc := &Account{Path: "offline.json"}
				if stream {
					streamMessagesResponse(rec, req, resp, "offline", 1, acc, &profileCN, time.Now(), false)
					if !strings.Contains(rec.Body.String(), "event: error\n") || strings.Contains(rec.Body.String(), "event: message_stop\n") {
						t.Fatalf("malformed upstream data was reported as a complete message: %s", rec.Body.String())
					}
				} else {
					writeMessagesAggregate(rec, req, resp, "offline", 1, acc, &profileCN, time.Now(), false)
					if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"type":"error"`) {
						t.Fatalf("malformed upstream data must return a 502 Anthropic error: status=%d body=%s", rec.Code, rec.Body.String())
					}
				}
			})
		}
	}
}

func TestMessagesReadErrorAfterFinishIsNotSuccess(t *testing.T) {
	const finish = "data: {\"choices\":[{\"delta\":{\"content\":\"prefix\"},\"finish_reason\":\"stop\"}]}\n\n"
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "aggregate"}[stream], func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			reader := io.MultiReader(strings.NewReader(finish), messagesFailReader{})
			resp := &http.Response{Body: io.NopCloser(reader)}
			acc := &Account{Path: "offline.json"}
			if stream {
				streamMessagesResponse(rec, req, resp, "offline", 1, acc, &profileCN, time.Now(), false)
				if !strings.Contains(rec.Body.String(), "event: error\n") || strings.Contains(rec.Body.String(), "event: message_stop\n") {
					t.Fatalf("read error after finish was reported as complete: %s", rec.Body.String())
				}
			} else {
				writeMessagesAggregate(rec, req, resp, "offline", 1, acc, &profileCN, time.Now(), false)
				if rec.Code < 500 || !strings.Contains(rec.Body.String(), `"type":"error"`) {
					t.Fatalf("read error after finish was reported as complete: status=%d body=%s", rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestMessagesSSEControlLinesRemainCompatible(t *testing.T) {
	const upstream = ": heartbeat\nevent: message\nid: synthetic-event\nretry: 1000\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"complete answer\"},\"finish_reason\":\"stop\"}]}\n\n" +
		": final heartbeat\n\ndata: [DONE]\n\n"
	for _, stream := range []bool{true, false} {
		t.Run(map[bool]string{true: "stream", false: "aggregate"}[stream], func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
			acc := &Account{Path: "offline.json"}
			if stream {
				streamMessagesResponse(rec, req, resp, "offline", 1, acc, &profileCN, time.Now(), false)
				if !strings.Contains(rec.Body.String(), "event: message_stop\n") || strings.Contains(rec.Body.String(), "event: error\n") {
					t.Fatalf("valid SSE control lines interrupted the response: %s", rec.Body.String())
				}
			} else {
				writeMessagesAggregate(rec, req, resp, "offline", 1, acc, &profileCN, time.Now(), false)
			}
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "complete answer") {
				t.Fatalf("valid SSE response lost its content: status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
