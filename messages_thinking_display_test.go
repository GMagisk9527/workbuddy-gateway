package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const hiddenReasoningSentinel = "Private synthetic reasoning for the next tool round."

func TestMessagesThinkingDisplayRoundTrip(t *testing.T) {
	for _, display := range []string{"updates", "omitted"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", display, stream), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				h.reply.Store(&compatReply{http.StatusOK, respChunk(`{"reasoning_content":"`+hiddenReasoningSentinel+`"}`, "") +
					respChunk(`{"tool_calls":[{"index":0,"id":"call_hidden","type":"function","function":{"name":"inspect","arguments":"{}"}}]}`, "tool_calls") + "data: [DONE]\n\n"})
				body := map[string]any{
					"model": compatModel, "max_tokens": 100, "stream": stream,
					"thinking":      map[string]any{"type": "adaptive", "display": display},
					"output_config": map[string]any{"effort": "high"},
					"tools":         []any{map[string]any{"name": "inspect", "input_schema": map[string]any{"type": "object"}}},
					"messages":      []any{map[string]any{"role": "user", "content": "Inspect the synthetic input."}},
				}
				status, raw := h.send(t, "/v1/messages", body, nil, 0)
				if status != http.StatusOK || strings.Contains(string(raw), hiddenReasoningSentinel) {
					t.Fatalf("display=%s leaked reasoning or failed: HTTP %d: %s", display, status, raw)
				}
				if h.take(t).body["reasoning_effort"] != "high" {
					t.Fatal("hiding the display must not disable model reasoning")
				}
				assistant := assembleAnthropicMessage(t, raw, stream)
				blocks := assistant["content"].([]any)
				thought := blocks[0].(map[string]any)
				if thought["type"] != "thinking" || thought["thinking"] != "" || strOf(thought["signature"]) == "" {
					t.Fatalf("hidden thinking needs an opaque replay signature: %#v", thought)
				}
				body["stream"] = false
				body["messages"] = append(body["messages"].([]any), assistant, map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "call_hidden", "content": "Synthetic result."},
				}})
				status, raw = h.send(t, "/v1/messages", body, nil, 0)
				if status != http.StatusOK {
					t.Fatalf("hidden reasoning replay failed: HTTP %d: %s", status, raw)
				}
				upstream := h.take(t).body["messages"].([]any)
				if firstReplayAssistant(t, upstream)["reasoning_content"] != hiddenReasoningSentinel {
					t.Fatalf("hidden reasoning lost on tool follow-up: %#v", upstream)
				}
				calls := h.calls.Load()
				status, raw = h.send(t, "/v1/messages", body, nil, 1)
				if status != http.StatusBadRequest || h.calls.Load() != calls || strings.Contains(string(raw), hiddenReasoningSentinel) {
					t.Fatalf("another caller must not replay opaque reasoning: HTTP %d: %s", status, raw)
				}
			})
		}
	}
}

func firstReplayAssistant(t *testing.T, messages []any) map[string]any {
	t.Helper()
	for _, raw := range messages {
		if message, ok := raw.(map[string]any); ok && message["role"] == "assistant" {
			return message
		}
	}
	t.Fatal("missing upstream assistant history")
	return nil
}

// Rebuild the client-visible message from the public SSE contract, not the
// converter's state. This checks that a real client can replay hidden thinking.
func assembleAnthropicMessage(t *testing.T, raw []byte, stream bool) map[string]any {
	t.Helper()
	if !stream {
		var msg map[string]any
		if err := json.Unmarshal(raw, &msg); err != nil {
			t.Fatal(err)
		}
		return msg
	}
	msg := map[string]any{"role": "assistant"}
	blocks := []any{}
	args := map[int]string{}
	for _, event := range splitSSE(t, string(raw)) {
		var data map[string]any
		if err := json.Unmarshal([]byte(event.data), &data); err != nil {
			continue
		}
		index := int(numOr0(data["index"]))
		switch event.name {
		case "content_block_start":
			if index != len(blocks) {
				t.Fatalf("nonsequential block index: %s", event.data)
			}
			blocks = append(blocks, data["content_block"])
		case "content_block_delta":
			delta := data["delta"].(map[string]any)
			block := blocks[index].(map[string]any)
			switch delta["type"] {
			case "signature_delta":
				block["signature"] = strOf(block["signature"]) + strOf(delta["signature"])
			case "thinking_delta":
				block["thinking"] = strOf(block["thinking"]) + strOf(delta["thinking"])
			case "text_delta":
				block["text"] = strOf(block["text"]) + strOf(delta["text"])
			case "input_json_delta":
				args[index] += strOf(delta["partial_json"])
			}
		}
	}
	for index, encoded := range args {
		var input any
		if err := json.Unmarshal([]byte(encoded), &input); err != nil {
			t.Fatal(err)
		}
		blocks[index].(map[string]any)["input"] = input
	}
	msg["content"] = blocks
	return msg
}
