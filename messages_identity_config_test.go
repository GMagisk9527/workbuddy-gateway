package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// This checks the wire request, not whether a real upstream grants channel access.
func TestMessagesClaudeCodeIdentityConfigThroughHTTP(t *testing.T) {
	for _, tc := range []struct{ name, config, prose string }{
		{"default", `{}`, "Claude Code"},
		{"target", `{"target":"WorkBuddy"}`, "WorkBuddy"},
		{"custom", `{"replacements":{"Claude Code":"coding assistant"}}`, "coding assistant"},
	} {
		for _, stream := range []bool{false, true} {
			for _, kind := range []string{"string", "blocks", "fallback"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", tc.name, stream, kind), func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					withRequestProfile(t, requestProfileLegacy)
					config, err := parseRuntimeConfig([]byte(`{"identity":` + tc.config + `}`))
					if err != nil {
						t.Fatal(err)
					}
					setIdentityConfig(config.Identity)
					t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
					withSystemPrompts(t, "You are Claude Code. fallback-sentinel", "Keep Claude Code force-sentinel.")
					user := "Explain Claude Code. private-user-sentinel"
					thought := "Claude Code private-thinking-sentinel"
					toolResult := "Claude Code private-result-sentinel"
					input := map[string]any{"text": "Claude Code private-arguments-sentinel"}
					args, _ := json.Marshal(input)
					body := map[string]any{
						"model": compatModel, "stream": stream, "max_tokens": 64,
						"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
						"messages": []any{
							map[string]any{"role": "user", "content": user},
							map[string]any{"role": "assistant", "content": []any{
								map[string]any{"type": "text", "text": "Use Claude Code; keep `Claude Code` literal."},
								map[string]any{"type": "thinking", "thinking": thought},
								map[string]any{"type": "tool_use", "id": "call_identity", "name": "save", "input": input},
							}},
							map[string]any{"role": "user", "content": []any{
								map[string]any{"type": "tool_result", "tool_use_id": "call_identity", "content": toolResult},
								map[string]any{"type": "text", "text": "Continue."},
							}},
						},
						"tools": []any{map[string]any{"name": "save", "description": "Claude Code schema-sentinel", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string", "description": "Claude Code property-sentinel"}}}}},
					}
					wantSystem := "You are " + tc.prose + ". fallback-sentinel"
					if kind != "fallback" {
						body["system"] = "You are Claude Code.\n```text\nClaude Code\n```\nKeep `Claude Code` literal."
						if kind == "blocks" {
							body["system"] = []any{
								map[string]any{"type": "text", "text": "You are Claude Code.\n```text"},
								map[string]any{"type": "text", "text": "Claude Code", "cache_control": map[string]any{"type": "ephemeral"}},
								map[string]any{"type": "text", "text": "```\nKeep `Claude Code` literal."},
							}
						}
						wantSystem = "You are " + tc.prose + ".\n```text\nClaude Code\n```\nKeep `Claude Code` literal."
					}
					wantSystem += "\n\nKeep " + tc.prose + " force-sentinel."
					status, raw := h.send(t, "/v1/messages", body, nil, 0)
					if status != 200 {
						t.Fatalf("HTTP %d: %s", status, raw)
					}
					assertCompatSuccess(t, "/v1/messages", stream, raw)
					wire := h.take(t).body
					messages := wire["messages"].([]any)
					if len(messages) != 5 || messages[0].(map[string]any)["content"] != wantSystem || messages[1].(map[string]any)["content"] != user {
						t.Fatal("system identity/code or user text was changed incorrectly")
					}
					assistant := messages[2].(map[string]any)
					call := assistant["tool_calls"].([]any)[0].(map[string]any)
					fn := call["function"].(map[string]any)
					if assistant["content"] != "Use "+tc.prose+"; keep `Claude Code` literal." || assistant["reasoning_content"] != thought || fn["arguments"] != string(args) || fn["name"] != "save" || call["id"] != "call_identity" || messages[3].(map[string]any)["content"] != toolResult {
						t.Fatal("history identity, reasoning, tool call or result changed incorrectly")
					}
					tool := wire["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
					originalTool := body["tools"].([]any)[0].(map[string]any)
					if tool["description"] != originalTool["description"] || !reflect.DeepEqual(tool["parameters"], originalTool["input_schema"]) {
						t.Fatal("tool description or schema changed")
					}
					h.reply.Store(&compatReply{400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`})
					status, raw = h.send(t, "/v1/messages", body, nil, 0)
					h.take(t)
					if status != 400 || !strings.Contains(string(raw), "11128") || h.calls.Load() != 2 {
						t.Fatal("channel rejection was hidden or retried")
					}
					for _, private := range []string{"private-user-sentinel", "private-thinking-sentinel", "private-result-sentinel", "private-arguments-sentinel", "schema-sentinel"} {
						if strings.Contains(h.logs.String(), private) {
							t.Fatal("request content leaked into diagnostics")
						}
					}
				})
			}
		}
	}
}

func TestIdentityForceOverlappingParagraphBoundaries(t *testing.T) {
	withClaudeCodeForceIdentity(t, true, "Use Claude Code.")
	obj := identityForceMessage("Client instruction.\n\n\nUse Claude Code.", "Use Claude Code.")
	prepareIdentityForceRequest(obj)
	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != "Client instruction.\n\n\nUse WorkBuddy." {
		t.Fatal("force after overlapping paragraph boundaries was duplicated")
	}
}
