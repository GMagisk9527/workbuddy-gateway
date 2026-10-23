package main

import (
	"fmt"
	"testing"
)

func collisionFunction(name, marker string) map[string]any {
	return map[string]any{
		"type": "function", "name": name,
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			marker: map[string]any{"type": "string"},
		}},
	}
}

func collisionNamespace(name string, children ...any) map[string]any {
	return map[string]any{"type": "namespace", "name": name, "tools": children}
}

func TestResponsesRejectsAmbiguousFlattenedToolNames(t *testing.T) {
	plain := collisionFunction("mcp__run", "plain_only")
	namespaced := collisionNamespace("mcp", collisionFunction("run", "namespaced_only"))
	for name, tools := range map[string][]any{
		"plain-first":     {plain, namespaced},
		"namespace-first": {namespaced, plain},
		"duplicate-functions": {
			collisionFunction("run", "first_only"), collisionFunction("run", "second_only"),
		},
		"namespace-separator-in-name": {
			collisionNamespace("a__b", collisionFunction("c", "first_only")),
			collisionNamespace("a", collisionFunction("b__c", "second_only")),
		},
		"duplicate-namespace-children": {
			collisionNamespace("mcp", collisionFunction("run", "first_only"), collisionFunction("run", "second_only")),
		},
		"function-custom": {
			collisionFunction("run", "function_only"), map[string]any{"type": "custom", "name": "run"},
		},
		"custom-function": {
			map[string]any{"type": "custom", "name": "run"}, collisionFunction("run", "function_only"),
		},
		"namespace-custom": {
			collisionFunction("mcp__run", "plain_only"),
			collisionNamespace("mcp", map[string]any{"type": "custom", "name": "run"}),
		},
		"function-search": {
			collisionFunction("web_search", "local_only"), map[string]any{"type": "web_search"},
		},
		"chat-form-function": {
			map[string]any{"type": "function", "function": map[string]any{"name": "run", "parameters": map[string]any{"type": "object"}}},
			collisionFunction("run", "second_only"),
		},
		"trimmed-function-names": {
			collisionFunction("run", "first_only"), collisionFunction(" run ", "second_only"),
		},
		"custom-default-name": {
			map[string]any{"type": "custom"}, collisionFunction("custom_tool", "function_only"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := responsesToChatRequestDetailed(map[string]any{"input": "Synthetic tool-name collision probe.", "tools": tools}, "m")
			if err == nil {
				t.Fatal("ambiguous flattened names silently discarded a tool and reused another tool's identity/schema")
			}
		})
	}
}

func TestResponsesRejectsToolNameCollisionBeforeUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			body := map[string]any{
				"model": compatModel, "stream": stream,
				"input": "Synthetic tool-name collision probe.",
				"tools": []any{
					collisionFunction("mcp__run", "plain_only"),
					collisionNamespace("mcp", collisionFunction("run", "namespaced_only")),
				},
				"tool_choice": map[string]any{"type": "function", "namespace": "mcp", "name": "run"},
			}
			status, _ := h.send(t, "/v1/responses", body, nil, 0)
			if status != 400 || h.calls.Load() != 0 {
				t.Fatalf("stream=%v: ambiguous tool selection reached upstream: status=%d calls=%d", stream, status, h.calls.Load())
			}
		})
	}
}

func TestResponsesDistinctToolNamesKeepMappingAndSelection(t *testing.T) {
	chat, mapping, err := responsesToChatRequestDetailed(map[string]any{
		"input": "Synthetic non-colliding tool probe.",
		"tools": []any{
			collisionFunction("plain", "plain_only"),
			collisionNamespace("mcp", collisionFunction("run", "namespaced_only")),
			map[string]any{"type": "custom", "name": "patch"},
		},
		"tool_choice": map[string]any{"type": "function", "namespace": "mcp", "name": "run"},
	}, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(mapping) != 3 || mapping["mcp__run"].Name != "run" || mapping["mcp__run"].Namespace != "mcp" || mapping["patch"].Kind != responsesToolCustom {
		t.Fatalf("valid reverse mapping changed: %#v", mapping)
	}
	if chat["tool_choice"] != "required" {
		t.Fatalf("tool_choice=%#v, want required", chat["tool_choice"])
	}
	tools, _ := chat["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("selected tools=%d, want 1", len(tools))
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "mcp__run" {
		t.Fatalf("wrong selected function name: %v", fn["name"])
	}
	properties := fn["parameters"].(map[string]any)["properties"].(map[string]any)
	if properties["namespaced_only"] == nil || properties["plain_only"] != nil {
		t.Fatal("selected function schema changed")
	}
}
