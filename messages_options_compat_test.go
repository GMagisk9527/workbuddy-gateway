package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMessagesStructuredOutputIsExplicitlyUnsupported(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			withRequestProfile(t, requestProfileLegacy)
			schema := map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{"answer": map[string]any{"type": "string", "enum": []any{"OK"}}},
				"required":   []any{"answer"},
			}
			body := map[string]any{
				"model": compatModel, "stream": stream, "max_tokens": 64,
				"messages": []any{map[string]any{"role": "user", "content": "Synthetic JSON request."}},
				"thinking": map[string]any{"type": "disabled"},
				"output_config": map[string]any{
					"effort": "high", "format": map[string]any{"type": "json_schema", "schema": schema},
				},
			}
			before, _ := json.Marshal(body)
			status, out := h.send(t, "/v1/messages", body, nil, 0)
			if status != 400 || h.calls.Load() != 0 || !strings.Contains(string(out), "output_config.format") || !strings.Contains(string(out), `"type":"invalid_request_error"`) {
				t.Fatalf("unverified upstream schema enforcement was reported as supported (HTTP %d calls=%d)", status, h.calls.Load())
			}
			after, _ := json.Marshal(body)
			if string(before) != string(after) {
				t.Fatal("conversion mutated the caller's request")
			}
		})
	}
}

func TestMessagesRejectsMissingHistoryAndMalformedOutputFormat(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"missing-history":      {"system": "A system prompt alone is not conversation input."},
		"empty-history":        {"system": "Synthetic system.", "messages": []any{}},
		"invalid-history-item": {"system": "Synthetic system.", "messages": []any{42}},
		"invalid-content":      {"messages": []any{map[string]any{"role": "user", "content": 42}}},
		"format-string":        {"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "output_config": map[string]any{"format": "json_schema"}},
		"format-type":          {"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "output_config": map[string]any{"format": map[string]any{"type": "unknown", "schema": map[string]any{"type": "object"}}}},
		"format-schema":        {"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": "private-schema-sentinel"}}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			status, out := h.send(t, "/v1/messages", body, nil, 0)
			var result struct {
				Type  string `json:"type"`
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if json.Unmarshal(out, &result) != nil || status != 400 || result.Type != "error" || result.Error.Type != "invalid_request_error" || h.calls.Load() != 0 {
				t.Fatalf("invalid request reached upstream or was not rejected as Anthropic 400 (status=%d calls=%d)", status, h.calls.Load())
			}
			if strings.Contains(string(out), "private-schema-sentinel") || strings.Contains(h.logs.String(), "private-schema-sentinel") {
				t.Fatal("invalid schema contents leaked")
			}
		})
	}
}
