package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMessagesToolHistoryPreservesJSONNumbers(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			withRequestProfile(t, requestProfileLegacy)
			body := map[string]any{
				"model": compatModel, "stream": stream, "max_tokens": 64,
				"temperature": 0.25, "top_p": 0.9, "top_k": 7,
				"thinking": map[string]any{"type": "enabled", "budget_tokens": 8192},
				"tools":    []any{map[string]any{"name": "save", "input_schema": map[string]any{"type": "object"}}},
				"messages": []any{
					map[string]any{"role": "assistant", "content": []any{map[string]any{
						"type": "tool_use", "id": "call_precision", "name": "save",
						"input": map[string]any{"id": json.Number("9007199254740993"), "ratio": json.Number("0.12345678901234567890123456789")},
					}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_precision", "content": "Synthetic tool result."}}},
				},
			}
			status, _ := h.send(t, "/v1/messages", body, nil, 0)
			if status != 200 {
				t.Fatalf("HTTP %d", status)
			}
			wire := h.take(t).body
			assistant := wire["messages"].([]any)[1].(map[string]any)
			fn := assistant["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
			if fn["arguments"] != `{"id":9007199254740993,"ratio":0.12345678901234567890123456789}` {
				t.Fatal("tool arguments lost numeric precision")
			}
			if wire["max_tokens"] != float64(64) || wire["temperature"] != 0.25 || wire["top_p"] != 0.9 || wire["top_k"] != float64(7) || wire["reasoning_effort"] != "medium" {
				t.Fatal("preserving tool JSON numbers changed generation options")
			}
		})
	}
}

func TestMessagesRejectsUnconvertibleContentBeforeUpstream(t *testing.T) {
	document := map[string]any{"type": "document", "source": map[string]any{"type": "text", "media_type": "text/plain", "data": "private-document-sentinel"}}
	for name, content := range map[string][]any{
		"document":        {map[string]any{"type": "text", "text": "Read this document."}, document},
		"search-result":   {map[string]any{"type": "search_result", "title": "private-document-sentinel", "source": "https://example.invalid", "content": []any{map[string]any{"type": "text", "text": "result"}}}},
		"nested-document": {map[string]any{"type": "tool_result", "tool_use_id": "call_doc", "content": []any{document}}},
		"malformed-image": {map[string]any{"type": "text", "text": "Image follows."}, map[string]any{"type": "image", "source": map[string]any{"type": "file", "file_id": "private-document-sentinel"}}},
		"unknown-block":   {map[string]any{"type": "future-content-block", "data": "private-document-sentinel"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			body := map[string]any{"model": compatModel, "messages": []any{map[string]any{"role": "user", "content": content}}}
			status, out := h.send(t, "/v1/messages", body, nil, 0)
			if status != 400 || h.calls.Load() != 0 || !strings.Contains(string(out), `"type":"invalid_request_error"`) {
				t.Fatalf("unconvertible content was silently removed (HTTP %d, calls=%d)", status, h.calls.Load())
			}
			if strings.Contains(string(out), "private-document-sentinel") || strings.Contains(h.logs.String(), "private-document-sentinel") {
				t.Fatal("unconvertible content leaked into diagnostics")
			}
		})
	}
}
