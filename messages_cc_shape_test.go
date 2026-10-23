package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Exercise the ordinary Claude Code request shape without external network,
// client files, or user conversations. Extra provider metadata must not cause
// the supported text/image/client-tool conversation to be rejected or changed.
func TestMessagesClaudeCodeShapeCompatibility(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			cache := map[string]any{"type": "ephemeral"}
			schema := map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path"},
			}
			body := map[string]any{
				"model": compatModel, "max_tokens": 64, "stream": stream,
				"system": []any{
					map[string]any{"type": "text", "text": "Synthetic programming assistant.", "cache_control": cache},
					map[string]any{"type": "text", "text": "Respect explicit tool results."},
				},
				"thinking":      map[string]any{"type": "adaptive"},
				"output_config": map[string]any{"effort": "high"},
				"metadata": map[string]any{
					"user_id": "synthetic-user", "session_id": "synthetic-session", "client_counter": 42,
				},
				"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_thinking_20251015", "keep": "all"}}},
				"tools": []any{map[string]any{
					"name": "inspect", "description": "Inspect an explicitly named file.",
					"input_schema": schema, "strict": true, "defer_loading": false,
					"input_examples": []any{map[string]any{"path": "sample.go"}},
					"cache_control":  cache,
				}},
				"tool_choice": map[string]any{"type": "auto", "disable_parallel_tool_use": true},
				"messages": []any{
					map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "text", "text": "Inspect sample.go", "cache_control": cache},
						map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}},
					}},
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "thinking", "thinking": "Inspect the supplied file.", "signature": "opaque-signature-sentinel"},
						map[string]any{"type": "redacted_thinking", "data": "opaque-redacted-sentinel"},
						map[string]any{"type": "text", "text": "Checking file."},
						map[string]any{"type": "tool_use", "id": "call_cc_shape", "name": "inspect", "input": map[string]any{"path": "sample.go"}},
					}},
					map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "tool_result", "tool_use_id": "call_cc_shape", "is_error": false, "content": []any{map[string]any{"type": "text", "text": "Synthetic file contents."}}, "cache_control": cache},
						map[string]any{"type": "text", "text": "Summarize the result."},
					}},
				},
			}
			before, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			status, out := h.send(t, "/v1/messages", body, map[string]string{"x-claude-code-session-id": "synthetic-cc-session"}, 0)
			if status != http.StatusOK {
				t.Fatalf("ordinary CC metadata rejected: HTTP %d: %s", status, out)
			}
			assertCompatSuccess(t, "/v1/messages", stream, out)
			wire := h.take(t).body
			if wire["reasoning_effort"] != "high" || wire["parallel_tool_calls"] != false || wire["tool_choice"] != "auto" {
				t.Fatalf("CC generation/tool-choice options changed: %#v", wire)
			}
			messages := wire["messages"].([]any)
			if len(messages) != 5 {
				t.Fatalf("CC conversation topology changed: %#v", messages)
			}
			if compatText(messages[0].(map[string]any)["content"]) != "Synthetic programming assistant.\nRespect explicit tool results." || compatText(messages[1].(map[string]any)["content"]) != "Inspect sample.go" {
				t.Fatal("system/user text changed")
			}
			parts := messages[1].(map[string]any)["content"].([]any)
			if len(parts) != 2 || parts[1].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,AAAA" {
				t.Fatal("user image changed")
			}
			assistant := messages[2].(map[string]any)
			call := assistant["tool_calls"].([]any)[0].(map[string]any)
			function := call["function"].(map[string]any)
			if assistant["role"] != "assistant" || assistant["content"] != "Checking file." || assistant["reasoning_content"] != "Inspect the supplied file." || call["id"] != "call_cc_shape" || function["name"] != "inspect" || function["arguments"] != `{"path":"sample.go"}` {
				t.Fatal("assistant reasoning/text/tool input changed")
			}
			result := messages[3].(map[string]any)
			if result["role"] != "tool" || result["tool_call_id"] != "call_cc_shape" || result["content"] != "Synthetic file contents." || compatText(messages[4].(map[string]any)["content"]) != "Summarize the result." {
				t.Fatal("tool result or follow-up text changed")
			}
			fn := wire["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
			if fn["strict"] != true || !reflect.DeepEqual(fn["parameters"], schema) {
				t.Fatal("tool schema/strict lost")
			}
			encoded, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			for _, opaque := range []string{"opaque-signature-sentinel", "opaque-redacted-sentinel"} {
				if strings.Contains(string(encoded), opaque) {
					t.Fatal("opaque Anthropic reasoning metadata forwarded as prompt content")
				}
			}
			after, err := json.Marshal(body)
			if err != nil || string(before) != string(after) {
				t.Fatal("HTTP conversion mutated the caller's request")
			}
		})
	}
}
