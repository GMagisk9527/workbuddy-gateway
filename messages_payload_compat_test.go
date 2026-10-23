package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestMessagesToolStrictSurvivesConversion(t *testing.T) {
	for _, strict := range []bool{true, false} {
		t.Run(fmt.Sprint(strict), func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			schema := map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"path": map[string]any{"type": "string", "description": "Exact filename"}},
				"required":   []any{"path"},
			}
			body := map[string]any{
				"model": compatModel, "max_tokens": 64, "stream": strict,
				"messages": []any{map[string]any{"role": "user", "content": "Inspect sample.go"}},
				"tools": []any{map[string]any{
					"name": "inspect", "description": "Inspect a file", "input_schema": schema, "strict": strict,
				}},
			}
			status, out := h.send(t, "/v1/messages", body, nil, 0)
			if status != http.StatusOK {
				t.Fatalf("HTTP %d: %s", status, out)
			}
			captured := h.take(t)
			tool := captured.body["tools"].([]any)[0].(map[string]any)
			fn := tool["function"].(map[string]any)
			if actual, exists := fn["strict"]; !exists || actual != strict {
				t.Fatalf("tool strict=%v present=%t want=%t", actual, exists, strict)
			}
			if fn["name"] != "inspect" || fn["description"] != "Inspect a file" || !reflect.DeepEqual(fn["parameters"], schema) {
				t.Fatalf("tool definition changed: %#v", fn)
			}
		})
	}
}

func TestMessagesToolErrorSemanticsReachUpstream(t *testing.T) {
	const marker = "[Tool execution error]"
	cases := []struct {
		name    string
		content any
		isError bool
		want    string
		image   bool
	}{
		{"error-string", "permission denied", true, marker + "\npermission denied", false},
		{"error-text-block", []any{map[string]any{"type": "text", "text": "permission denied"}}, true, marker + "\npermission denied", false},
		{"error-image-only", []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}}}, true, marker + "\n[图片]", true},
		{"error-empty", "", true, marker, false},
		{"success-string", "permission denied", false, "permission denied", false},
		{"success-text-block", []any{map[string]any{"type": "text", "text": "first"}, map[string]any{"type": "text", "text": "second"}}, false, "firstsecond", false},
	}
	for _, stream := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				body := map[string]any{
					"model": compatModel, "max_tokens": 64, "stream": stream,
					"messages": []any{
						map[string]any{"role": "user", "content": "Inspect sample.go"},
						map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call_result", "name": "inspect", "input": map[string]any{"path": "sample.go"}}}},
						map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_result", "content": tc.content, "is_error": tc.isError}}},
					},
				}
				status, out := h.send(t, "/v1/messages", body, nil, 0)
				if status != http.StatusOK {
					t.Fatalf("HTTP %d: %s", status, out)
				}
				captured := h.take(t)
				messages := captured.body["messages"].([]any)
				var result map[string]any
				for _, raw := range messages {
					message := raw.(map[string]any)
					if message["role"] == "tool" && message["tool_call_id"] == "call_result" {
						result = message
					}
				}
				if result == nil || result["content"] != tc.want {
					t.Fatalf("tool result semantics lost: got=%#v want=%q", result, tc.want)
				}
				if tc.image {
					last := messages[len(messages)-1].(map[string]any)
					parts, _ := last["content"].([]any)
					if last["role"] != "user" || len(parts) != 1 || parts[0].(map[string]any)["image_url"].(map[string]any)["url"] != "data:image/png;base64,AAAA" {
						t.Fatalf("tool image changed or disappeared: %#v", last)
					}
				}
			})
		}
	}
}

func TestMessagesServerToolsAreRejectedBeforeUpstream(t *testing.T) {
	for _, toolType := range []string{"web_search_20250305", "code_execution_20250825", "tool_search_tool_regex_20251119"} {
		t.Run(toolType, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			body := map[string]any{
				"model": compatModel, "max_tokens": 64,
				"messages": []any{map[string]any{"role": "user", "content": "Search synthetic material"}},
				"tools":    []any{map[string]any{"type": toolType, "name": "hosted_tool"}},
			}
			status, out := h.send(t, "/v1/messages", body, nil, 0)
			if status != http.StatusBadRequest {
				t.Fatalf("unsupported server tool must fail locally, got HTTP %d: %s", status, out)
			}
			var response map[string]any
			if err := json.Unmarshal(out, &response); err != nil {
				t.Fatal(err)
			}
			errBody, _ := response["error"].(map[string]any)
			if response["type"] != "error" || errBody["type"] != "invalid_request_error" || !strings.Contains(strOf(errBody["message"]), "tools[0]") || !strings.Contains(strOf(errBody["message"]), "不支持") {
				t.Fatalf("missing actionable protocol error: %s", out)
			}
			if h.calls.Load() != 0 {
				t.Fatalf("unsupported tool consumed upstream calls: %d", h.calls.Load())
			}
		})
	}
}
