package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestResponsesFinishReasonsAcrossModes(t *testing.T) {
	for _, tc := range []struct {
		name, finish, status, reason, args string
	}{
		{name: "stop", finish: "stop", status: "completed"},
		{name: "tools", finish: "tool_calls", status: "completed", args: `{"command":"ls"}`},
		{name: "length-text", finish: "length", status: "incomplete", reason: "max_output_tokens"},
		{name: "length-tool", finish: "length", status: "incomplete", reason: "max_output_tokens", args: `{"command":"ls`},
		{name: "filter-text", finish: "content_filter", status: "incomplete", reason: "content_filter"},
		{name: "filter-tool", finish: "content_filter", status: "incomplete", reason: "content_filter", args: `{"command":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := respChunk(`{"reasoning_content":"original reasoning"}`, "") + respChunk(`{"content":"received text"}`, "")
			if tc.args != "" {
				delta, err := json.Marshal(map[string]any{"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_finish", "type": "function",
					"function": map[string]any{"name": "shell", "arguments": tc.args},
				}}})
				if err != nil {
					t.Fatal(err)
				}
				upstream += respChunk(string(delta), "")
			}
			upstream += respChunk(`{}`, tc.finish) + `data: {"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}` + "\n\ndata: [DONE]\n\n"

			checkResponse := func(t *testing.T, response map[string]any) {
				t.Helper()
				if response["status"] != tc.status || response["error"] != nil {
					t.Fatalf("wrong terminal state: %#v", response)
				}
				if tc.reason == "" {
					if response["incomplete_details"] != nil || response["completed_at"] == nil {
						t.Fatalf("completed response lost completion metadata: %#v", response)
					}
				} else {
					details, _ := response["incomplete_details"].(map[string]any)
					if details["reason"] != tc.reason || response["completed_at"] != nil {
						t.Fatalf("truncation metadata wrong: %#v", response)
					}
				}
				output := response["output"].([]any)
				wantItems := 2
				if tc.args != "" {
					wantItems++
				}
				if len(output) != wantItems {
					t.Fatalf("lost received output: %#v", output)
				}
				for _, raw := range output {
					item := raw.(map[string]any)
					if item["status"] != tc.status {
						t.Errorf("item terminal state disagrees with response: %#v", item)
					}
					switch item["type"] {
					case "reasoning":
						if item["summary"].([]any)[0].(map[string]any)["text"] != "original reasoning" {
							t.Errorf("lost received reasoning: %#v", item)
						}
					case "message":
						if item["content"].([]any)[0].(map[string]any)["text"] != "received text" {
							t.Errorf("lost received text: %#v", item)
						}
					case "function_call":
						if item["arguments"] != tc.args || item["call_id"] != "call_finish" || item["name"] != "shell" {
							t.Errorf("tool call changed: %#v", item)
						}
					}
				}
				wantUsage := map[string]any{
					"input_tokens": float64(12), "output_tokens": float64(5), "total_tokens": float64(17),
					"input_tokens_details":  map[string]any{"cached_tokens": float64(3)},
					"output_tokens_details": map[string]any{"reasoning_tokens": float64(2)},
				}
				if !reflect.DeepEqual(response["usage"], wantUsage) {
					t.Errorf("usage lost at terminal event: %#v", response["usage"])
				}
			}

			t.Run("aggregate", func(t *testing.T) {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				resp := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
				writeResponsesAggregate(rec, req, resp, "finish-test", 1, &Account{Path: "finish-test.json"}, &profileCN, time.Now())
				if rec.Code != http.StatusOK {
					t.Fatalf("HTTP status=%d: %s", rec.Code, rec.Body.String())
				}
				var response map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				checkResponse(t, response)
			})
			for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
				name := map[fastPathModeT]string{fastPathOff: "stream-slow", fastPathOn: "stream-fast"}[mode]
				t.Run(name, func(t *testing.T) {
					events := splitSSE(t, captureResponsesStream(t, upstream, mode))
					terminalCount := 0
					for _, event := range events {
						if event.data == "[DONE]" {
							continue
						}
						var data map[string]any
						if err := json.Unmarshal([]byte(event.data), &data); err != nil {
							t.Fatal(err)
						}
						if event.name == "response.output_item.done" && data["item"].(map[string]any)["status"] != tc.status {
							t.Errorf("done item falsely completed: %#v", data)
						}
						if event.name == "response.completed" || event.name == "response.incomplete" || event.name == "response.failed" {
							terminalCount++
							if event.name != "response."+tc.status {
								t.Errorf("wrong terminal event %s", event.name)
							}
							checkResponse(t, data["response"].(map[string]any))
						}
					}
					if terminalCount != 1 || len(events) < 2 || events[len(events)-1].data != "[DONE]" || events[len(events)-2].name != "response."+tc.status {
						t.Fatalf("wrong stream ending: %#v", events)
					}
				})
			}
		})
	}
}

func TestMessagesThinkingReplayAcrossToolTurns(t *testing.T) {
	for _, thinkingType := range []string{"enabled", "adaptive", "disabled"} {
		t.Run(thinkingType, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			body := map[string]any{
				"model": compatModel, "max_tokens": 128,
				"thinking": map[string]any{"type": thinkingType},
				"messages": []any{
					map[string]any{"role": "user", "content": "inspect this"},
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "thinking", "thinking": "first original trace", "signature": "opaque-client-signature"},
						map[string]any{"type": "text", "text": "checking"},
						map[string]any{"type": "tool_use", "id": "call_first", "name": "inspect", "input": map[string]any{"path": "a"}},
					}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_first", "content": "found a"}}},
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "thinking", "thinking": "second original trace"},
						map[string]any{"type": "thinking", "thinking": "more original trace"},
						map[string]any{"type": "tool_use", "id": "call_second", "name": "inspect", "input": map[string]any{"path": "b"}},
					}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_second", "content": "found b"}}},
				},
			}
			status, out := h.send(t, "/v1/messages", body, nil, 0)
			if status != http.StatusOK {
				t.Fatalf("HTTP status=%d: %s", status, out)
			}
			captured := h.take(t)
			messages := captured.body["messages"].([]any)
			if len(messages) != 6 {
				t.Fatalf("tool history changed: %#v", messages)
			}
			for i, want := range map[int]string{2: "first original trace", 4: "second original trace\n\nmore original trace"} {
				assistant := messages[i].(map[string]any)
				if assistant["reasoning_content"] != want || assistant["reasoning"] != want {
					t.Errorf("lost tool-round thinking: %#v", assistant)
				}
				if _, exists := assistant["signature"]; exists {
					t.Errorf("Anthropic signature leaked upstream: %#v", assistant)
				}
			}
			if messages[3].(map[string]any)["tool_call_id"] != "call_first" || messages[5].(map[string]any)["tool_call_id"] != "call_second" {
				t.Fatal("tool result pairing changed")
			}
		})
	}
}

func TestMessagesThinkingReplayDoesNotInterpretOpaqueState(t *testing.T) {
	for _, block := range []map[string]any{
		{"type": "redacted_thinking", "data": "encrypted-content"},
		{"type": "thinking", "signature": "signature-only"},
	} {
		converted := convertAnthropicBlocks("assistant", []any{block})
		message := converted[0].(map[string]any)
		if _, exists := message["reasoning_content"]; exists {
			t.Fatalf("fabricated reasoning from opaque data: %#v", message)
		}
		if _, exists := message["signature"]; exists {
			t.Fatalf("fabricated or forwarded a signature: %#v", message)
		}
	}
}
