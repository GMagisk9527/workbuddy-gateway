package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestResponsesEncryptedReasoningRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			h.reply.Store(&compatReply{200, respChunk(`{"reasoning_content":"`+hiddenReasoningSentinel+`"}`, "") + respChunk(`{"content":"Synthetic answer."}`, "stop") + "data: [DONE]\n\n"})
			body := map[string]any{"model": compatModel, "stream": stream, "store": false, "include": []any{"reasoning.encrypted_content"}, "input": "Synthetic input."}
			status, raw := h.send(t, "/v1/responses", body, nil, 0)
			if status != http.StatusOK {
				t.Fatalf("HTTP %d: %s", status, raw)
			}
			h.take(t)
			var envelope map[string]any
			var completedItem map[string]any
			if stream {
				for _, ev := range splitSSE(t, string(raw)) {
					var event map[string]any
					if json.Unmarshal([]byte(ev.data), &event) != nil {
						continue
					}
					if ev.name == "response.output_item.done" {
						item := event["item"].(map[string]any)
						if item["type"] == "reasoning" {
							completedItem = item
						}
					}
					if ev.name == "response.completed" {
						envelope = event["response"].(map[string]any)
					}
				}
			} else if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope == nil {
				t.Fatalf("missing response: %s", raw)
			}
			item := envelope["output"].([]any)[0].(map[string]any)
			ciphertext := strOf(item["encrypted_content"])
			if item["type"] != "reasoning" || ciphertext == "" || strings.Contains(ciphertext, hiddenReasoningSentinel) {
				t.Fatalf("missing encrypted replay state: %#v", item)
			}
			if stream && (completedItem == nil || completedItem["encrypted_content"] != ciphertext) {
				t.Fatal("item.done and completed disagree on encrypted state")
			}
			body["stream"] = false
			body["input"] = []any{
				map[string]any{"role": "user", "content": "Synthetic input."},
				map[string]any{"type": "reasoning", "encrypted_content": ciphertext, "summary": []any{}},
				map[string]any{"role": "assistant", "content": "Synthetic answer."},
				map[string]any{"role": "user", "content": "Continue."},
			}
			status, raw = h.send(t, "/v1/responses", body, nil, 0)
			if status != http.StatusOK {
				t.Fatalf("encrypted-only replay: HTTP %d: %s", status, raw)
			}
			messages := h.take(t).body["messages"].([]any)
			if firstReplayAssistant(t, messages)["reasoning_content"] != hiddenReasoningSentinel {
				t.Fatal("encrypted reasoning not restored")
			}
			calls := h.calls.Load()
			status, raw = h.send(t, "/v1/responses", body, nil, 1)
			if status != http.StatusBadRequest || h.calls.Load() != calls || strings.Contains(string(raw), hiddenReasoningSentinel) {
				t.Fatalf("cross-caller replay accepted: HTTP %d: %s", status, raw)
			}
		})
	}
}
