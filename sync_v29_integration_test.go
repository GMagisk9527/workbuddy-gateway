package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncV29ResponsesToolsThroughHTTP(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		for _, stream := range []bool{false, true} {
			for _, finish := range []string{"tool_calls", "length"} {
				t.Run(fmt.Sprintf("mode=%d/stream=%t/finish=%s", mode, stream, finish), func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					fastPathMode.Store(int32(mode))
					args := `{"input":"*** Begin Patch\nCodex file content\n*** End Patch"}`
					calls := []any{
						map[string]any{"index": 0, "id": "call_ns", "type": "function", "function": map[string]any{"name": "repo__read", "arguments": `{"path":"Codex.txt"}`}},
						map[string]any{"index": 1, "id": "call_patch", "type": "function", "function": map[string]any{"name": "apply_patch", "arguments": args}},
					}
					chunk, err := json.Marshal(map[string]any{"tool_calls": calls})
					if err != nil {
						t.Fatal(err)
					}
					upstream := respChunk(string(chunk), finish) + "data: {\"choices\":[],\"usage\":{\"credit\":0.1,\"total_tokens\":4}}\n\ndata: [DONE]\n\n"
					h.reply.Store(&compatReply{200, upstream})
					body := map[string]any{
						"model": compatModel, "stream": stream, "store": false,
						"input": "synthetic task", "max_output_tokens": 64,
						"reasoning": map[string]any{"effort": "high", "summary": "detailed"},
						"tools": []any{
							map[string]any{"type": "namespace", "name": "repo", "tools": []any{map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}, "strict": true}}},
							map[string]any{"type": "custom", "name": "apply_patch", "format": map[string]any{"type": "text"}},
						},
					}
					status, raw := h.send(t, "/v1/responses", body, nil, 0)
					if status != 200 {
						t.Fatalf("status=%d body=%s", status, raw)
					}
					capture := h.take(t)
					if capture.body["reasoning_summary"] != "detailed" || capture.body["max_tokens"] != float64(64) {
						t.Fatal("explicit reasoning summary or output budget was lost")
					}
					var response map[string]any
					if stream {
						for _, event := range splitSSE(t, string(raw)) {
							if event.data == "[DONE]" {
								continue
							}
							var data map[string]any
							if err := json.Unmarshal([]byte(event.data), &data); err != nil {
								t.Fatal(err)
							}
							if event.name == "response.completed" || event.name == "response.incomplete" {
								response, _ = data["response"].(map[string]any)
							}
							if event.name == "response.output_item.added" {
								item, _ := data["item"].(map[string]any)
								if item["call_id"] == "call_patch" && (item["type"] != "custom_tool_call" || item["name"] != "apply_patch") {
									t.Fatal("custom tool started with a function item")
								}
								if item["call_id"] == "call_ns" && (item["name"] != "read" || item["namespace"] != "repo") {
									t.Fatal("namespace lost in opening event")
								}
							}
						}
					} else if err := json.Unmarshal(raw, &response); err != nil {
						t.Fatal(err)
					}
					wantStatus := "completed"
					if finish == "length" {
						wantStatus = "incomplete"
					}
					if response["status"] != wantStatus {
						t.Fatalf("response status=%v want=%s", response["status"], wantStatus)
					}
					items, _ := response["output"].([]any)
					if len(items) != 2 {
						t.Fatalf("output count=%d want=2", len(items))
					}
					first, second := items[0].(map[string]any), items[1].(map[string]any)
					if first["name"] != "read" || first["namespace"] != "repo" || first["call_id"] != "call_ns" || first["status"] != wantStatus {
						t.Fatalf("namespace output=%#v", first)
					}
					if second["type"] != "custom_tool_call" || second["input"] != "*** Begin Patch\nCodex file content\n*** End Patch" || second["call_id"] != "call_patch" || second["status"] != wantStatus {
						t.Fatalf("custom output=%#v", second)
					}
					if finish == "tool_calls" {
						// Replay the exact client-visible output and tool results.
						input := []any{map[string]any{"role": "user", "content": "synthetic task"}, first, second,
							map[string]any{"type": "function_call_output", "call_id": "call_ns", "output": "Codex read result"},
							map[string]any{"type": "custom_tool_call_output", "call_id": "call_patch", "output": "patch applied"}}
						body["input"], body["stream"] = input, false
						h.reply.Store(&compatReply{200, conversationFixtureSSE(false)})
						if code, raw := h.send(t, "/v1/responses", body, nil, 0); code != 200 {
							t.Fatalf("tool replay status=%d body=%s", code, raw)
						}
						next := h.take(t)
						encoded, _ := json.Marshal(next.body["messages"])
						for _, want := range []string{"repo__read", "apply_patch", "call_ns", "call_patch", "Codex read result", "Codex file content"} {
							if !strings.Contains(string(encoded), want) {
								t.Fatalf("replay lost %q", want)
							}
						}
					}
				})
			}
		}
	}
}

func TestSyncV29IdentityPreservesMessageWideCode(t *testing.T) {
	setIdentityConfig(runtimeIdentityConfig{Target: "Assistant"})
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": []any{
			map[string]any{"type": "text", "text": "You are Codex.\n```go\n"},
			map[string]any{"type": "text", "text": "const cli = \"Codex\"\n```\nUse `Codex CLI` unchanged."},
		}},
		map[string]any{"role": "user", "content": "Codex user sentinel"},
		map[string]any{"role": "tool", "content": "Codex tool sentinel"},
	}}
	logDSHPromptCompatibility(nil, 0, obj)
	messages := obj["messages"].([]any)
	parts := messages[0].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["text"] != "You are Assistant.\n```go\n" || parts[1].(map[string]any)["text"] != "const cli = \"Codex\"\n```\nUse `Codex CLI` unchanged." {
		t.Fatal("configured identity rules modified code across content parts")
	}
	if messages[1].(map[string]any)["content"] != "Codex user sentinel" || messages[2].(map[string]any)["content"] != "Codex tool sentinel" {
		t.Fatal("user/tool text was modified")
	}
	before, _ := json.Marshal(obj)
	logDSHPromptCompatibility(nil, 0, obj)
	after, _ := json.Marshal(obj)
	if string(before) != string(after) {
		t.Fatal("identity processing was not idempotent")
	}
}

func TestSyncV29ExistingConfigRemainsByteIdentical(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := []byte("{ \"gateway\": {\"apiPort\":18317}, \"systemPrompt\": {\"force\":\"keep\"} }\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeConfigFile(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatal("startup rewrote an existing config")
	}
}
