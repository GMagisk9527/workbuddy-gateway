package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func syntheticToolReferenceRequest() map[string]any {
	return map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "search-call", "name": "ToolSearch", "input": map[string]any{"query": "inspect"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "search-call", "content": []any{
					map[string]any{"type": "text", "text": "Search result before."},
					map[string]any{"type": "tool_reference", "tool_name": "Inspect"},
					map[string]any{"type": "text", "text": "Search result after."},
				}},
				map[string]any{"type": "text", "text": "Continue with the available tool."},
			}},
		},
		"tools": []any{
			map[string]any{"name": "ToolSearch", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "Inspect", "description": "Inspect a synthetic file.", "defer_loading": true, "strict": true,
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"large": map[string]any{"const": json.Number("9007199254740993")}}}},
		},
	}
}

func TestMessagesToolReferenceExpandsDeclaredDeferredDefinition(t *testing.T) {
	body := syntheticToolReferenceRequest()
	before, _ := json.Marshal(body)
	chat, err := anthropicToChatRequest(body, compatModel)
	if err != nil {
		t.Fatalf("declared tool reference rejected: %v", err)
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "tool" || msgs[1].(map[string]any)["tool_call_id"] != "search-call" || msgs[2].(map[string]any)["content"] != "Continue with the available tool." {
		t.Fatalf("tool result linkage or ordering changed: %#v", msgs)
	}
	content := msgs[1].(map[string]any)["content"].(string)
	start, end := strings.Index(content, "Search result before."), strings.Index(content, "Search result after.")
	definition := strings.Index(content, `"name":"Inspect"`)
	if start < 0 || definition < start || end < definition || !strings.Contains(content, `"description":"Inspect a synthetic file."`) || !strings.Contains(content, `"parameters":`) || !strings.Contains(content, `9007199254740993`) || !strings.Contains(content, `"strict":true`) {
		t.Fatalf("reference definition, precision or text ordering lost: %s", content)
	}
	tools := chat["tools"].([]any)
	if len(tools) != 2 || tools[1].(map[string]any)["function"].(map[string]any)["name"] != "Inspect" {
		t.Fatal("deferred tool was removed from the Chat tools")
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("tool reference expansion mutated the caller's request")
	}
}

func TestMessagesToolReferenceSupportsChatFormDefinition(t *testing.T) {
	body := syntheticToolReferenceRequest()
	body["tools"].([]any)[1] = map[string]any{"type": "function", "function": map[string]any{"name": "Inspect", "description": "Chat definition.", "parameters": map[string]any{"type": "object"}}}
	chat, err := anthropicToChatRequest(body, compatModel)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(chat["messages"].([]any)[1].(map[string]any)["content"].(string), `"description":"Chat definition."`) {
		t.Fatal("Chat-form tool reference definition was lost")
	}
}

func TestMessagesToolReferenceRejectsUnknownMalformedAndAmbiguous(t *testing.T) {
	for _, name := range []string{"unknown", "missing", "non-string", "duplicate"} {
		t.Run(name, func(t *testing.T) {
			body := syntheticToolReferenceRequest()
			ref := body["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)
			switch name {
			case "unknown":
				ref["tool_name"] = "private-unknown-tool-sentinel"
			case "missing":
				delete(ref, "tool_name")
			case "non-string":
				ref["tool_name"] = 17
			case "duplicate":
				body["tools"] = append(body["tools"].([]any), body["tools"].([]any)[1])
			}
			_, err := anthropicToChatRequest(body, compatModel)
			if err == nil || !strings.Contains(err.Error(), "messages[1].content[0].content[1]") || strings.Contains(err.Error(), "private-unknown-tool-sentinel") {
				t.Fatalf("unsafe or imprecise reference error: %v", err)
			}
		})
	}
}

func TestMessagesToolReferencesRemainScopedToToolResults(t *testing.T) {
	for _, role := range []string{"user", "assistant", "system", "developer"} {
		t.Run(role, func(t *testing.T) {
			body := syntheticToolReferenceRequest()
			body["messages"] = []any{map[string]any{"role": role, "content": []any{map[string]any{"type": "tool_reference", "tool_name": "Inspect"}}}}
			if _, err := anthropicToChatRequest(body, compatModel); err == nil {
				t.Fatal("tool reference accepted outside tool_result.content")
			}
		})
	}
}

func TestMessagesToolReferenceSearchThenCallThroughHTTP(t *testing.T) {
	for _, site := range []string{"cn", "intl"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", site, stream), func(t *testing.T) {
				h := newCompatHarness(t, site)
				body := syntheticToolReferenceRequest()
				body["model"], body["stream"] = compatModel, stream
				body["system"] = "Synthetic tool search instructions."
				body["messages"] = append(body["messages"].([]any),
					map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "inspect-call", "name": "Inspect", "input": map[string]any{"large": json.Number("9007199254740993")}}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "inspect-call", "content": "Synthetic inspected result."}}},
				)
				status, out := h.send(t, "/v1/messages?beta=true", body, nil, 0)
				if status != 200 || h.calls.Load() != 1 {
					t.Fatalf("tool search/result/call chain rejected: status=%d calls=%d out=%s", status, h.calls.Load(), out)
				}
				wire := h.take(t).body
				messages := wire["messages"].([]any)
				if len(messages) != 6 || messages[0].(map[string]any)["content"] != "Synthetic tool search instructions." || messages[2].(map[string]any)["tool_call_id"] != "search-call" || messages[5].(map[string]any)["tool_call_id"] != "inspect-call" {
					t.Fatalf("tool search/result/call topology changed: %#v", messages)
				}
				result := messages[2].(map[string]any)["content"].(string)
				call := messages[4].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
				function := call["function"].(map[string]any)
				if !strings.Contains(result, `"name":"Inspect"`) || !strings.Contains(result, "9007199254740993") || call["id"] != "inspect-call" || function["name"] != "Inspect" || function["arguments"] != `{"large":9007199254740993}` || len(wire["tools"].([]any)) != 2 {
					t.Fatal("referenced definition, exact subsequent arguments or available tools changed")
				}
			})
		}
	}
}

func TestMessagesToolReferenceRepeatedDefinitionsRemainBounded(t *testing.T) {
	body := syntheticToolReferenceRequest()
	body["tools"].([]any)[1].(map[string]any)["description"] = strings.Repeat("a", 16<<10)
	refs := make([]any, 256)
	for i := range refs {
		refs[i] = map[string]any{"type": "tool_reference", "tool_name": "Inspect"}
	}
	result := body["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)
	result["content"] = refs
	before, _ := json.Marshal(body)
	normalized, err := normalizeAnthropicToolReferences(body)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > 4*len(before) {
		t.Fatalf("repeated references expanded %d input bytes to %d bytes", len(before), len(after))
	}
	converted := normalized["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].([]any)
	if len(converted) != len(refs) {
		t.Fatal("repeated references were dropped")
	}
	for _, raw := range converted {
		if text := raw.(map[string]any)["text"].(string); !strings.Contains(text, "Inspect") {
			t.Fatal("reference lost its tool identity")
		}
	}
	unchanged, _ := json.Marshal(body)
	if string(before) != string(unchanged) {
		t.Fatal("bounded normalization changed the caller's request")
	}
}

func TestMessagesToolReferenceRejectsCumulativeExpansionBeforeMarshaling(t *testing.T) {
	body := syntheticToolReferenceRequest()
	tools, refs := []any{}, []any{}
	// HTML escaping makes the definitions much larger on the JSON wire. Each
	// definition fits independently, but their combined expanded text does not.
	description := strings.Repeat("<", 64<<10)
	for i := 0; i < 48; i++ {
		name := fmt.Sprintf("Inspect%d", i)
		tools = append(tools, map[string]any{"name": name, "description": description, "input_schema": map[string]any{"type": "object"}})
		refs = append(refs, map[string]any{"type": "tool_reference", "tool_name": name})
	}
	body["tools"] = tools
	body["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = refs
	if _, err := normalizeAnthropicToolReferences(body); err == nil {
		t.Fatal("cumulative normalized request larger than the request budget was accepted")
	}
}
