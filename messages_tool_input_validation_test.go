package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestMessagesRejectsNonObjectToolHistoryInputBeforeUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			present bool
			input   any
		}{
			{"missing", false, nil},
			{"null", true, nil},
			{"array", true, []any{"private-tool-input-sentinel"}},
			{"string", true, "private-tool-input-sentinel"},
			{"number", true, json.Number("9007199254740993")},
			{"boolean", true, true},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				toolUse := map[string]any{"type": "tool_use", "id": "call_input", "name": "inspect"}
				if tc.present {
					toolUse["input"] = tc.input
				}
				body := map[string]any{
					"model": compatModel, "stream": stream, "max_tokens": 64,
					"messages": []any{
						map[string]any{"role": "assistant", "content": []any{toolUse}},
						map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_input", "content": "Synthetic tool result."}}},
					},
				}
				status, out := h.send(t, "/v1/messages", body, nil, 0)
				if status != 400 || h.calls.Load() != 0 || !strings.Contains(string(out), `"type":"invalid_request_error"`) {
					t.Errorf("invalid tool input reached upstream or returned success: HTTP %d calls=%d", status, h.calls.Load())
				}
				if strings.Contains(string(out), "private-tool-input-sentinel") || strings.Contains(h.logs.String(), "private-tool-input-sentinel") {
					t.Error("invalid tool input leaked into errors or diagnostics")
				}
			})
		}
	}
}

func TestMessagesObjectToolHistoryInputRemainsExact(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"empty", map[string]any{}, `{}`},
		{"numbers", map[string]any{"id": json.Number("9007199254740993"), "ratio": json.Number("0.12345678901234567890123456789")}, `{"id":9007199254740993,"ratio":0.12345678901234567890123456789}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			withRequestProfile(t, requestProfileLegacy)
			body := map[string]any{
				"model": compatModel, "max_tokens": 64,
				"messages": []any{
					map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call_input", "name": "inspect", "input": tc.args}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_input", "content": "Synthetic tool result."}}},
				},
			}
			status, _ := h.send(t, "/v1/messages", body, nil, 0)
			if status != 200 || h.calls.Load() != 1 {
				t.Fatalf("valid object tool input failed: HTTP %d calls=%d", status, h.calls.Load())
			}
			messages := h.take(t).body["messages"].([]any)
			var arguments string
			for _, raw := range messages {
				message, _ := raw.(map[string]any)
				if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
					function := calls[0].(map[string]any)["function"].(map[string]any)
					arguments, _ = function["arguments"].(string)
				}
			}
			if arguments != tc.want {
				t.Fatalf("valid object tool arguments changed: got=%s want=%s", arguments, tc.want)
			}
		})
	}
}

func toolInputValidationSSE(t *testing.T, args, finish string) string {
	t.Helper()
	// Split the argument string to verify validation happens after all fragments.
	first, second := args[:len(args)/2], args[len(args)/2:]
	chunk := func(argument string, initial bool, reason string) string {
		call := map[string]any{"index": 0, "function": map[string]any{"arguments": argument}}
		if initial {
			call["id"] = "call_input"
			call["type"] = "function"
			call["function"].(map[string]any)["name"] = "inspect"
		}
		raw, err := json.Marshal(map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{call}}, "finish_reason": reason,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		return "data: " + string(raw) + "\n\n"
	}
	return chunk(first, true, "") + chunk(second, false, finish) + "data: [DONE]\n\n"
}

func TestMessagesRejectsInvalidUpstreamToolArguments(t *testing.T) {
	for _, finish := range []string{"tool_calls", "length"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct{ name, args string }{
				{"truncated-object", `{"private-tool-input-sentinel":`},
				{"trailing-data", `{"private-tool-input-sentinel":true} extra`},
				{"array", `["private-tool-input-sentinel"]`},
				{"null", `null`},
				{"string", `"private-tool-input-sentinel"`},
				{"number", `42`},
				{"boolean", `true`},
			} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.name, finish, stream), func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					h.reply.Store(&compatReply{200, toolInputValidationSSE(t, tc.args, finish)})
					body := map[string]any{"model": compatModel, "stream": stream, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "Synthetic tool request."}}}
					status, out := h.send(t, "/v1/messages", body, nil, 0)
					if stream {
						if !strings.Contains(string(out), "event: error\n") || strings.Contains(string(out), "event: message_stop\n") {
							t.Errorf("invalid tool arguments were reported as a successful stream: HTTP %d", status)
						}
						for _, line := range strings.Split(string(out), "\n") {
							if !strings.HasPrefix(line, "data: ") {
								continue
							}
							var payload map[string]any
							if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload) == nil && payload["type"] == "error" && strings.Contains(line, "private-tool-input-sentinel") {
								t.Error("invalid tool arguments were repeated in the stream error")
							}
						}
					} else if status != 502 || !strings.Contains(string(out), `"type":"error"`) {
						t.Errorf("invalid tool arguments were aggregated as a successful Message: HTTP %d", status)
					} else if strings.Contains(string(out), "private-tool-input-sentinel") {
						t.Error("invalid tool arguments were repeated in the aggregate error")
					}
					if strings.Contains(h.logs.String(), "private-tool-input-sentinel") {
						t.Error("invalid tool arguments leaked into diagnostics")
					}
				})
			}
		}
	}
}

func TestMessagesValidUpstreamToolArgumentsRemainSupported(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ name, args, want string }{
			{"empty-arguments", "", `{}`},
			{"empty-object", `{}`, `{}`},
			{"numbers", `{"id":9007199254740993,"ratio":0.12345678901234567890123456789}`, `{"id":9007199254740993,"ratio":0.12345678901234567890123456789}`},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				h.reply.Store(&compatReply{200, toolInputValidationSSE(t, tc.args, "tool_calls")})
				body := map[string]any{"model": compatModel, "stream": stream, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "Synthetic tool request."}}}
				status, out := h.send(t, "/v1/messages", body, nil, 0)
				if status != 200 || strings.Contains(string(out), "event: error\n") {
					t.Fatalf("valid object/empty tool arguments failed: HTTP %d", status)
				}
				if stream {
					if !strings.Contains(string(out), "event: message_stop\n") {
						t.Error("valid object/empty tool arguments did not finish")
					}
				} else {
					var message struct {
						Content []struct {
							Type  string          `json:"type"`
							Input json.RawMessage `json:"input"`
						} `json:"content"`
					}
					if json.Unmarshal(out, &message) != nil || len(message.Content) != 1 || message.Content[0].Type != "tool_use" || string(message.Content[0].Input) != tc.want {
						t.Error("valid object/empty tool input changed, including numeric precision")
					}
				}
			})
		}
	}
}
