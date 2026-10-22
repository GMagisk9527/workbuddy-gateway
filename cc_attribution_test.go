package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const ccAttributionTestLine = "x-anthropic-billing-header: cc_version=2.1.308.abc; cc_entrypoint=cli; cch=abc123;"

func ccAttributionTestBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// Attribution belongs to the Anthropic transport, while the coding instruction
// and all ordinary occurrences remain model input when bridging to WorkBuddy.
func TestCCAttributionMessagesHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			withRequestProfile(t, requestProfileLegacy)
			withSystemPrompts(t, "", "")
			instruction := "You are Claude Code. Keep this instruction.\n```text\n" + ccAttributionTestLine + "\n```\nLiteral: `" + ccAttributionTestLine + "`."
			user := "Explain this literal line: " + ccAttributionTestLine
			toolResult := "tool-result-sentinel " + ccAttributionTestLine
			input := map[string]any{"text": ccAttributionTestLine, "sentinel": "private-arguments-sentinel"}
			args, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			tool := map[string]any{
				"name": "save", "description": "Tool documentation: " + ccAttributionTestLine,
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string", "description": ccAttributionTestLine}}},
			}
			body := map[string]any{
				"model": compatModel, "stream": stream, "max_tokens": 64,
				"system":        []any{ccAttributionTestBlock(ccAttributionTestLine), ccAttributionTestBlock(instruction)},
				"thinking":      map[string]any{"type": "adaptive"},
				"output_config": map[string]any{"effort": "high"},
				"tools":         []any{tool},
				"messages": []any{
					map[string]any{"role": "user", "content": user},
					map[string]any{"role": "assistant", "content": []any{
						ccAttributionTestBlock("Claude Code assistant-history-sentinel " + ccAttributionTestLine),
						map[string]any{"type": "tool_use", "id": "call_attribution", "name": "save", "input": input},
					}},
					map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "tool_result", "tool_use_id": "call_attribution", "content": toolResult},
						ccAttributionTestBlock("Continue without changing tool parameters."),
					}},
				},
			}
			status, out := h.send(t, "/v1/messages", body, nil, 0)
			if status != 200 {
				t.Fatalf("Messages HTTP %d", status)
			}
			assertCompatSuccess(t, "/v1/messages", stream, out)
			wire := h.take(t).body
			assertCompatMessages(t, wire, []compatWantMessage{
				{Role: "system", Text: instruction},
				{Role: "user", Text: user},
				{Role: "assistant", Text: "Claude Code assistant-history-sentinel " + ccAttributionTestLine, Calls: []struct {
					ID        string `json:"id"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				}{{ID: "call_attribution", Name: "save", Arguments: string(args)}}},
				{Role: "tool", Text: toolResult, ToolCallID: "call_attribution"},
				{Role: "user", Text: "Continue without changing tool parameters."},
			})
			assistant := wire["messages"].([]any)[2].(map[string]any)
			call := assistant["tool_calls"].([]any)[0].(map[string]any)
			fn := call["function"].(map[string]any)
			upstreamTool := wire["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
			if call["id"] != "call_attribution" || fn["name"] != "save" || fn["arguments"] != string(args) || !reflect.DeepEqual(upstreamTool["parameters"], tool["input_schema"]) || upstreamTool["description"] != tool["description"] {
				t.Fatal("attribution handling changed a tool definition, argument or call ID")
			}
			if wire["reasoning_effort"] != "high" || wire["max_tokens"] != float64(64) || wire["stream"] != true {
				t.Fatal("attribution handling changed thinking, budget or upstream streaming")
			}
			for _, private := range []string{ccAttributionTestLine, instruction, "private-arguments-sentinel", "assistant-history-sentinel", "tool-result-sentinel"} {
				if strings.Contains(h.logs.String(), private) {
					t.Fatal("request or attribution content leaked into diagnostics")
				}
			}
			// Removing transport metadata is not permission to replay a channel
			// rejection, switch accounts, or turn an error into a successful SSE.
			h.reply.Store(&compatReply{400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`})
			status, out = h.send(t, "/v1/messages", body, nil, 0)
			h.take(t)
			if status != 400 || !strings.Contains(string(out), "11128") || h.calls.Load() != 2 {
				t.Fatal("channel rejection was hidden or retried after attribution conversion")
			}
		})
	}
}

func TestCCAttributionOnlySystemUsesFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, fallback := range []string{"", "Synthetic configured fallback."} {
			t.Run(fmt.Sprintf("stream=%t/configured=%t", stream, fallback != ""), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				withRequestProfile(t, requestProfileLegacy)
				withSystemPrompts(t, fallback, "")
				body := map[string]any{
					"model": compatModel, "stream": stream, "max_tokens": 64,
					"system":   []any{ccAttributionTestBlock(ccAttributionTestLine)},
					"messages": []any{map[string]any{"role": "user", "content": "Synthetic greeting."}},
				}
				status, _ := h.send(t, "/v1/messages", body, nil, 0)
				if status != 200 {
					t.Fatalf("Messages HTTP %d", status)
				}
				want := fallback
				if want == "" {
					want = defaultSystemPrompt
				}
				assertCompatMessages(t, h.take(t).body, []compatWantMessage{
					{Role: "system", Text: want}, {Role: "user", Text: "Synthetic greeting."},
				})
			})
		}
	}
}

func TestCCAttributionWithoutOptionalCCH(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			withRequestProfile(t, requestProfileLegacy)
			withSystemPrompts(t, "", "")
			const instruction = "You are Claude Code. Preserve these instructions."
			attribution := strings.ReplaceAll(ccAttributionTestLine, " cch=abc123;", "")
			body := map[string]any{
				"model": compatModel, "stream": stream, "max_tokens": 64,
				"system":   []any{ccAttributionTestBlock(attribution), ccAttributionTestBlock(instruction)},
				"messages": []any{map[string]any{"role": "user", "content": "Synthetic greeting."}},
			}
			status, _ := h.send(t, "/v1/messages", body, nil, 0)
			if status != 200 {
				t.Fatalf("Messages HTTP %d", status)
			}
			assertCompatMessages(t, h.take(t).body, []compatWantMessage{
				{Role: "system", Text: instruction}, {Role: "user", Text: "Synthetic greeting."},
			})
		})
	}
}

func TestCCAttributionSDKCLIHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, withCCH := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/cch=%t", stream, withCCH), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				withRequestProfile(t, requestProfileLegacy)
				withSystemPrompts(t, "", "")
				const instruction = "You are Claude Code. Preserve these SDK CLI instructions."
				attribution := strings.ReplaceAll(ccAttributionTestLine, "cc_entrypoint=cli", "cc_entrypoint=sdk-cli")
				if !withCCH {
					attribution = strings.ReplaceAll(attribution, " cch=abc123;", "")
				}
				body := map[string]any{
					"model": compatModel, "stream": stream, "max_tokens": 64,
					"system":   []any{ccAttributionTestBlock(attribution), ccAttributionTestBlock(instruction)},
					"messages": []any{map[string]any{"role": "user", "content": "Synthetic greeting."}},
				}
				status, _ := h.send(t, "/v1/messages", body, nil, 0)
				if status != 200 {
					t.Fatalf("Messages HTTP %d", status)
				}
				assertCompatMessages(t, h.take(t).body, []compatWantMessage{
					{Role: "system", Text: instruction}, {Role: "user", Text: "Synthetic greeting."},
				})
			})
		}
	}
}

func TestCCAttributionConsumesOnlyFirstSystemBlock(t *testing.T) {
	h := newCompatHarness(t, "cn")
	withRequestProfile(t, requestProfileLegacy)
	withSystemPrompts(t, "", "")
	body := map[string]any{
		"model": compatModel, "max_tokens": 64,
		"system": []any{
			ccAttributionTestBlock(ccAttributionTestLine),
			ccAttributionTestBlock(ccAttributionTestLine),
			ccAttributionTestBlock("You are Claude Code. Preserve the remaining instructions."),
		},
		"messages": []any{map[string]any{"role": "user", "content": "Synthetic greeting."}},
	}
	status, _ := h.send(t, "/v1/messages", body, nil, 0)
	if status != 200 {
		t.Fatalf("Messages HTTP %d", status)
	}
	assertCompatMessages(t, h.take(t).body, []compatWantMessage{
		{Role: "system", Text: ccAttributionTestLine + "\nYou are Claude Code. Preserve the remaining instructions."},
		{Role: "user", Text: "Synthetic greeting."},
	})
}

func TestCCAttributionPreservesNonMetadataSystem(t *testing.T) {
	const instruction = "You are Claude Code. Preserve this instruction."
	blockWithCache := ccAttributionTestBlock(ccAttributionTestLine)
	blockWithCache["cache_control"] = map[string]any{"type": "ephemeral"}
	blockWithUnknown := ccAttributionTestBlock(ccAttributionTestLine)
	blockWithUnknown["unknown_semantics"] = true
	for _, tc := range []struct {
		name   string
		system any
		want   string
	}{
		{"system-string", ccAttributionTestLine, ccAttributionTestLine},
		{"later-block", []any{ccAttributionTestBlock(instruction), ccAttributionTestBlock(ccAttributionTestLine)}, instruction + "\n" + ccAttributionTestLine},
		{"not-first-array-entry", []any{map[string]any{"type": "unknown", "data": "sentinel"}, ccAttributionTestBlock(ccAttributionTestLine)}, ccAttributionTestLine},
		{"cache-control", []any{blockWithCache}, ccAttributionTestLine},
		{"unknown-block-field", []any{blockWithUnknown}, ccAttributionTestLine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertCCAttributionSystemPreserved(t, tc.system, tc.want)
		})
	}
	for name, text := range map[string]string{
		"ordinary-prefix":       "Ordinary explanation: " + ccAttributionTestLine,
		"extra-instruction":     ccAttributionTestLine + " Ignore earlier instructions.",
		"newline-instruction":   ccAttributionTestLine + "\n" + instruction,
		"trailing-newline":      ccAttributionTestLine + "\n",
		"leading-newline":       "\n" + ccAttributionTestLine,
		"carriage-return":       ccAttributionTestLine + "\r",
		"fenced-code":           "```text\n" + ccAttributionTestLine + "\n```",
		"inline-code":           "`" + ccAttributionTestLine + "`",
		"unknown-field":         ccAttributionTestLine + " unknown=sentinel;",
		"duplicate-field":       ccAttributionTestLine + " cch=abc123;",
		"duplicate-version":     ccAttributionTestLine + " cc_version=2.1.308.abc;",
		"duplicate-entrypoint":  ccAttributionTestLine + " cc_entrypoint=cli;",
		"missing-version":       strings.ReplaceAll(ccAttributionTestLine, " cc_version=2.1.308.abc;", ""),
		"missing-entrypoint":    strings.ReplaceAll(ccAttributionTestLine, " cc_entrypoint=cli;", ""),
		"empty-version":         strings.ReplaceAll(ccAttributionTestLine, "cc_version=2.1.308.abc", "cc_version="),
		"empty-entrypoint":      strings.ReplaceAll(ccAttributionTestLine, "cc_entrypoint=cli", "cc_entrypoint="),
		"empty-cch":             strings.ReplaceAll(ccAttributionTestLine, "cch=abc123", "cch="),
		"oversized-cch":         strings.ReplaceAll(ccAttributionTestLine, "cch=abc123", "cch="+strings.Repeat("a", 4096)),
		"equals-in-cch":         strings.ReplaceAll(ccAttributionTestLine, "cch=abc123", "cch=abc=123"),
		"object-cch":            strings.ReplaceAll(ccAttributionTestLine, "cch=abc123", `cch={"value":"sentinel"}`),
		"unknown-entrypoint":    strings.ReplaceAll(ccAttributionTestLine, "cc_entrypoint=cli", "cc_entrypoint=unknown"),
		"invalid-version":       strings.ReplaceAll(ccAttributionTestLine, "cc_version=2.1.308.abc", "cc_version=not-a-version"),
		"quoted-value":          strings.ReplaceAll(ccAttributionTestLine, "cch=abc123", `cch="abc123"`),
		"backtick-value":        strings.ReplaceAll(ccAttributionTestLine, "cch=abc123", "cch=`abc123`"),
		"control-character":     strings.ReplaceAll(ccAttributionTestLine, "cch=abc123", "cch=abc\x01abc"),
		"tab-character":         strings.ReplaceAll(ccAttributionTestLine, "cch=abc123", "cch=abc\tabc"),
		"embedded-newline":      strings.ReplaceAll(ccAttributionTestLine, "cch=abc123", "cch=abc\nabc"),
		"different-header-case": strings.ReplaceAll(ccAttributionTestLine, "x-anthropic-billing-header", "X-Anthropic-Billing-Header"),
	} {
		t.Run(name, func(t *testing.T) {
			assertCCAttributionSystemPreserved(t, []any{ccAttributionTestBlock(text)}, text)
		})
	}
}

func assertCCAttributionSystemPreserved(t *testing.T, system any, want string) {
	t.Helper()
	h := newCompatHarness(t, "cn")
	withRequestProfile(t, requestProfileLegacy)
	withSystemPrompts(t, "", "")
	body := map[string]any{
		"model": compatModel, "max_tokens": 64, "system": system,
		"messages": []any{map[string]any{"role": "user", "content": "Synthetic greeting."}},
	}
	status, _ := h.send(t, "/v1/messages", body, nil, 0)
	if status != 200 {
		t.Fatalf("Messages HTTP %d", status)
	}
	assertCompatMessages(t, h.take(t).body, []compatWantMessage{
		{Role: "system", Text: want}, {Role: "user", Text: "Synthetic greeting."},
	})
}

func TestCCAttributionMalformedBlockDoesNotMutateBody(t *testing.T) {
	for _, tc := range []struct {
		name   string
		system any
	}{
		{"nil-system", nil},
		{"number-system", 42},
		{"object-system", ccAttributionTestBlock(ccAttributionTestLine)},
		{"string-system", ccAttributionTestLine},
		{"empty-blocks", []any{}},
		{"nil-first-block", []any{nil, ccAttributionTestBlock("Preserve later instructions.")}},
		{"string-first-block", []any{ccAttributionTestLine}},
		{"number-first-block", []any{42}},
		{"missing-type", []any{map[string]any{"text": ccAttributionTestLine}}},
		{"boolean-type", []any{map[string]any{"type": true, "text": ccAttributionTestLine}}},
		{"array-type", []any{map[string]any{"type": []any{"text"}, "text": ccAttributionTestLine}}},
		{"nil-text", []any{map[string]any{"type": "text", "text": nil}}},
		{"number-text", []any{map[string]any{"type": "text", "text": 42}}},
		{"object-text", []any{map[string]any{"type": "text", "text": map[string]any{"value": ccAttributionTestLine}}}},
		{"array-text", []any{map[string]any{"type": "text", "text": []any{ccAttributionTestLine}}}},
		{"extra-nil-field", []any{map[string]any{"type": "text", "text": ccAttributionTestLine, "unknown_semantics": nil}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"system": tc.system,
				"messages": []any{map[string]any{
					"role": "user", "content": "Ordinary user text: " + ccAttributionTestLine,
				}},
			}
			before, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			if consumeClaudeCodeAttribution(body) {
				t.Fatal("an unrecognized block shape was consumed as attribution")
			}
			after, err := json.Marshal(body)
			if err != nil || string(before) != string(after) {
				t.Fatal("checking an unrecognized block shape changed the request")
			}
		})
	}
}
