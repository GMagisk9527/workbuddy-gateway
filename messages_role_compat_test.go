package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// These are existing gateway compatibility roles, rather than native Messages
// roles. Keep them local to instruction text; user/tool history must retain its
// role, text, call identifiers and exact arguments through the HTTP conversion.
func TestMessagesInstructionRolesRemainCompatibleThroughHTTP(t *testing.T) {
	for _, role := range []string{"system", "developer"} {
		for _, blocks := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/blocks=%t/stream=%t", role, blocks, stream), func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					withRequestProfile(t, requestProfileLegacy)
					withSystemPrompts(t, "", "")
					instruction := "Synthetic instruction remains in the instruction role."
					var content any = instruction
					if blocks {
						content = []any{map[string]any{"type": "text", "text": instruction}}
					}
					user := "Explain the Claude Code literal; private-role-user-sentinel."
					result := "Claude Code literal; private-role-result-sentinel."
					arguments := map[string]any{
						"text": "Claude Code literal; private-role-argument-sentinel.",
						"id":   json.Number("9007199254740993"),
					}
					wantArguments, err := json.Marshal(arguments)
					if err != nil {
						t.Fatal(err)
					}
					tool := map[string]any{"name": "inspect", "description": "Synthetic inspection.", "input_schema": map[string]any{"type": "object"}}
					body := map[string]any{
						"model": compatModel, "stream": stream, "max_tokens": 64,
						"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
						"messages": []any{
							map[string]any{"role": "user", "content": user},
							map[string]any{"role": role, "content": content},
							map[string]any{"role": "assistant", "content": []any{
								map[string]any{"type": "tool_use", "id": "call_role_compat", "name": "inspect", "input": arguments},
							}},
							map[string]any{"role": "user", "content": []any{
								map[string]any{"type": "tool_result", "tool_use_id": "call_role_compat", "content": result},
								map[string]any{"type": "text", "text": "Continue with the result."},
							}},
						},
						"tools": []any{tool},
					}
					before, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					status, out := h.send(t, "/v1/messages", body, nil, 0)
					if status != 200 || h.calls.Load() != 1 {
						t.Fatalf("compatible instruction role rejected: HTTP %d calls=%d body=%s", status, h.calls.Load(), out)
					}
					assertCompatSuccess(t, "/v1/messages", stream, out)
					wire := h.take(t).body
					messages := wire["messages"].([]any)
					if len(messages) != 5 {
						t.Fatalf("instruction or history lost: got %d messages", len(messages))
					}
					for i, want := range []struct{ role, content string }{
						{"system", instruction}, {"user", user}, {"assistant", ""}, {"tool", result}, {"user", "Continue with the result."},
					} {
						message := messages[i].(map[string]any)
						if message["role"] != want.role || message["content"] != want.content {
							t.Fatalf("message %d role/text changed", i)
						}
					}
					assistant := messages[2].(map[string]any)
					calls, _ := assistant["tool_calls"].([]any)
					if len(calls) != 1 {
						t.Fatalf("tool call count=%d, want 1", len(calls))
					}
					call := calls[0].(map[string]any)
					function := call["function"].(map[string]any)
					if call["id"] != "call_role_compat" || function["name"] != "inspect" || function["arguments"] != string(wantArguments) || messages[3].(map[string]any)["tool_call_id"] != "call_role_compat" {
						t.Fatal("tool call/result identifiers or arguments changed")
					}
					wireTool := wire["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
					if wireTool["name"] != tool["name"] || wireTool["description"] != tool["description"] || !reflect.DeepEqual(wireTool["parameters"], tool["input_schema"]) {
						t.Fatal("tool definition changed")
					}
					after, _ := json.Marshal(body)
					if string(after) != string(before) {
						t.Fatal("HTTP request conversion changed the caller's request")
					}
					for _, private := range []string{"private-role-user-sentinel", "private-role-result-sentinel", "private-role-argument-sentinel"} {
						if strings.Contains(h.logs.String(), private) {
							t.Fatal("request content leaked to diagnostics")
						}
					}
				})
			}
		}
	}
}

// The user's installed Claude Code, with their normal configuration and a fake
// local API, sent top-level system blocks plus messages with roles [user, system].
func TestMessagesClaudeCodeFreshSessionInstructionShape(t *testing.T) {
	for _, cfg := range []struct{ edition, profile string }{
		{"cn", requestProfileLegacy}, {"cn", requestProfileOfficial},
		{"intl", requestProfileLegacy}, {"intl", requestProfileOfficial},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%s/stream=%t", cfg.edition, cfg.profile, stream), func(t *testing.T) {
				h := newCompatHarness(t, cfg.edition)
				withRequestProfile(t, cfg.profile)
				withSystemPrompts(t, "", "")
				h.reply.Store(&compatReply{200, respChunk(`{"content":"Synthetic OK."}`, "stop") + "data: [DONE]\n\n"})
				text := func(s string) any { return map[string]any{"type": "text", "text": s} }
				body := map[string]any{
					"model": compatModel, "stream": stream, "max_tokens": 64,
					"system": []any{text("Synthetic top-level instructions.")},
					"messages": []any{
						map[string]any{"role": "user", "content": []any{text("Synthetic context."), text("Synthetic hello.")}},
						map[string]any{"role": "system", "content": []any{text("Synthetic appended instructions.")}},
					},
				}
				status, out := h.send(t, "/v1/messages?beta=true", body, nil, 0)
				if status != 200 || h.calls.Load() != 1 {
					t.Fatalf("Claude Code fresh-session shape rejected: status=%d calls=%d", status, h.calls.Load())
				}
				messages := h.take(t).body["messages"].([]any)
				if len(messages) != 3 || messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["role"] != "user" || messages[2].(map[string]any)["role"] != "system" {
					t.Fatal("fresh-session instruction or user roles were lost or promoted")
				}
				if !strings.Contains(string(out), "Synthetic OK.") || (stream && !strings.Contains(string(out), "event: message_stop")) {
					t.Fatal("fresh-session response did not complete normally")
				}
			})
		}
	}
}

func TestMessagesInstructionRolesRejectNonTextBlocks(t *testing.T) {
	for _, role := range []string{"system", "developer"} {
		for _, block := range []map[string]any{
			{"type": "tool_use", "id": "call_instruction", "name": "inspect", "input": map[string]any{}},
			{"type": "tool_result", "tool_use_id": "call_instruction", "content": "private-instruction-block-sentinel"},
			{"type": "thinking", "thinking": "private-instruction-block-sentinel"},
			{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.invalid/private-instruction-block-sentinel"}},
		} {
			t.Run(role+"/"+strOf(block["type"]), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				body := map[string]any{
					"model": compatModel,
					"messages": []any{
						map[string]any{"role": "user", "content": "Synthetic request."},
						map[string]any{"role": role, "content": []any{block}},
					},
				}
				status, out := h.send(t, "/v1/messages", body, nil, 0)
				if status != 400 || h.calls.Load() != 0 || !strings.Contains(string(out), "invalid_request_error") {
					t.Fatalf("unsupported instruction block was accepted: HTTP %d calls=%d", status, h.calls.Load())
				}
				if strings.Contains(string(out), "private-instruction-block-sentinel") || strings.Contains(h.logs.String(), "private-instruction-block-sentinel") {
					t.Fatal("rejected instruction block leaked into diagnostics")
				}
			})
		}
	}
}

// Native Chat tool/function histories need a separate converter to retain their
// linkage fields. Do not accept them through the text-only role compatibility.
func TestMessagesInstructionRoleCompatibilityStillRejectsLossyRoles(t *testing.T) {
	for _, role := range []string{"unknown-role", "tool", "function"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", role, stream), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				body := map[string]any{
					"model": compatModel, "stream": stream, "max_tokens": 64,
					"messages": []any{
						map[string]any{"role": "user", "content": "Synthetic user request."},
						map[string]any{"role": role, "content": "private-lossy-role-sentinel", "tool_call_id": "call_role_compat", "name": "inspect"},
					},
				}
				status, out := h.send(t, "/v1/messages", body, nil, 0)
				var response struct {
					Type  string `json:"type"`
					Error struct {
						Type string `json:"type"`
					} `json:"error"`
				}
				if json.Unmarshal(out, &response) != nil || status != 400 || response.Type != "error" || response.Error.Type != "invalid_request_error" || h.calls.Load() != 0 {
					t.Fatalf("lossy role reached upstream or lacked Anthropic 400: HTTP %d calls=%d", status, h.calls.Load())
				}
				if strings.Contains(string(out), "private-lossy-role-sentinel") || strings.Contains(h.logs.String(), "private-lossy-role-sentinel") {
					t.Fatal("rejected role content leaked into errors or diagnostics")
				}
			})
		}
	}
}
