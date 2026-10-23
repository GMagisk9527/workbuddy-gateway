package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestMessagesUnsupportedCapabilitiesStopBeforeUpstream(t *testing.T) {
	for name, options := range map[string]map[string]any{
		"thinking-turn-pruning":    {"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_thinking_20251015", "keep": map[string]any{"type": "thinking_turns", "value": 2}}}}},
		"default-thinking-pruning": {"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_thinking_20251015"}}}},
		"tool-result-pruning":      {"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_tool_uses_20250919"}}}},
		"mcp":                      {"mcp_servers": []any{map[string]any{"type": "url", "url": "https://private-mcp-sentinel.invalid", "name": "synthetic"}}},
		"container":                {"container": "private-container-sentinel"},
		"service-tier":             {"service_tier": "standard_only"},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", name, stream), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				body := map[string]any{"model": compatModel, "messages": []any{map[string]any{"role": "user", "content": "Synthetic input."}}, "stream": stream, "max_tokens": 32}
				for key, value := range options {
					body[key] = value
				}
				status, out := h.send(t, "/v1/messages", body, nil, 0)
				if status != 400 || h.calls.Load() != 0 || !strings.Contains(string(out), `"type":"invalid_request_error"`) {
					t.Fatalf("unsupported semantics reached upstream: status=%d calls=%d output=%s", status, h.calls.Load(), out)
				}
				if strings.Contains(string(out), "private-") || strings.Contains(h.logs.String(), "private-") {
					t.Fatal("opaque capability data leaked")
				}
			})
		}
	}
}

func TestMessagesHarmlessCapabilitiesRemainCompatible(t *testing.T) {
	for name, options := range map[string]map[string]any{
		"empty":    {},
		"defaults": {"context_management": map[string]any{"edits": []any{}}, "mcp_servers": []any{}, "container": nil, "service_tier": "auto"},
		"ordinary-cc-options": {
			"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_thinking_20251015", "keep": "all"}}},
			"metadata":           map[string]any{"user_id": "synthetic-user"},
			"thinking":           map[string]any{"type": "adaptive", "display": "updates"},
			"output_config":      map[string]any{"effort": "high"},
			"tools":              []any{map[string]any{"name": "SyntheticTool", "input_schema": map[string]any{"type": "object"}, "defer_loading": true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			options["messages"] = []any{map[string]any{"role": "user", "content": "Synthetic input."}}
			if _, err := anthropicToChatRequest(options, compatModel); err != nil {
				t.Fatalf("compatible option rejected: %v", err)
			}
		})
	}
}

func TestMessagesCapabilityOptionsRequireValidShapes(t *testing.T) {
	for name, options := range map[string]map[string]any{
		"context":      {"context_management": "private-context-sentinel"},
		"edits":        {"context_management": map[string]any{"edits": "all"}},
		"edit":         {"context_management": map[string]any{"edits": []any{"all"}}},
		"unknown-edit": {"context_management": map[string]any{"edits": []any{map[string]any{"type": "unknown"}}}},
		"mcp":          {"mcp_servers": "private-mcp-sentinel"},
		"tier":         {"service_tier": true},
	} {
		t.Run(name, func(t *testing.T) {
			options["messages"] = []any{map[string]any{"role": "user", "content": "Synthetic input."}}
			if _, err := anthropicToChatRequest(options, compatModel); err == nil {
				t.Fatal("invalid option accepted")
			}
		})
	}
}
