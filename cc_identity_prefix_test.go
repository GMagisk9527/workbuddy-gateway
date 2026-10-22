package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

var ccIdentityTestOpenings = []string{
	"You are Claude Code.",
	"You are Claude Code, Anthropic's official CLI tool for Claude.",
	"You are Claude Code, Anthropic's official CLI for Claude.",
	"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
	"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
	"You are Claude Code, an AI assistant that orchestrates software engineering tasks across multiple workers.",
}

func withDefaultCCIdentity(t *testing.T) {
	t.Helper()
	setIdentityConfig(runtimeIdentityConfig{})
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
}

func TestCCIdentityKnownDefaultOpenings(t *testing.T) {
	withDefaultCCIdentity(t)
	for i, opening := range ccIdentityTestOpenings {
		t.Run(fmt.Sprintf("opening=%d", i), func(t *testing.T) {
			const suffix = "\nKeep custom instructions and discuss Claude Code normally."
			got, changed := normalizeAssistantIdentityText(opening + suffix)
			if !changed || got != "You are an AI coding assistant."+suffix {
				t.Fatal("the known identity opening was not neutralized without changing the suffix")
			}
			again, changed := normalizeAssistantIdentityText(got)
			if changed || again != got {
				t.Fatal("neutralizing a known identity opening was not idempotent")
			}
		})
	}
}

func TestCCIdentityPreservesOtherProseAndCode(t *testing.T) {
	withDefaultCCIdentity(t)
	for i, opening := range ccIdentityTestOpenings {
		for name, text := range map[string]string{
			"middle":        "Quoted client identity: " + opening,
			"leading-space": " " + opening,
			"inline-code":   "`" + opening + "`",
			"fenced-code":   "```text\n" + opening + "\n```",
			"ordinary-name": "Use Claude Code to run the project tests.",
		} {
			t.Run(fmt.Sprintf("opening=%d/%s", i, name), func(t *testing.T) {
				if got, changed := normalizeAssistantIdentityText(text); changed || got != text {
					t.Fatal("identity normalization changed ordinary prose or Markdown code")
				}
			})
		}
	}
	for _, role := range []string{"system", "assistant"} {
		for _, fence := range []bool{false, true} {
			t.Run(fmt.Sprintf("role=%s/fence=%t", role, fence), func(t *testing.T) {
				prefix, suffix := "`literal ", " literal`"
				if fence {
					prefix, suffix = "```text\n", "\n```"
				}
				message := map[string]any{"role": role, "content": []any{
					map[string]any{"type": "text", "text": prefix},
					map[string]any{"type": "text", "text": ccIdentityTestOpenings[0]},
					map[string]any{"type": "text", "text": suffix},
				}}
				before, _ := json.Marshal(message)
				obj := map[string]any{"messages": []any{message}}
				logDSHPromptCompatibility(httptest.NewRequest("POST", "/v1/messages", nil), 1, obj)
				after, _ := json.Marshal(message)
				if string(before) != string(after) {
					t.Fatal("a known opening inside code crossing text blocks was changed")
				}
			})
		}
	}
}

func TestCCIdentityRoleAndToolBoundaries(t *testing.T) {
	withDefaultCCIdentity(t)
	opening := ccIdentityTestOpenings[0]
	args, _ := json.Marshal(map[string]any{"text": opening})
	call := map[string]any{"id": "call_identity", "type": "function", "function": map[string]any{"name": "save", "arguments": string(args)}}
	tool := map[string]any{"type": "function", "function": map[string]any{"name": "save", "description": opening, "parameters": map[string]any{"type": "object"}}}
	messages := []any{
		map[string]any{"role": "system", "content": opening},
		map[string]any{"role": "assistant", "content": opening, "reasoning_content": opening, "tool_calls": []any{call}},
		map[string]any{"role": "user", "content": opening},
		map[string]any{"role": "tool", "tool_call_id": "call_identity", "content": opening},
	}
	obj := map[string]any{"messages": messages, "tools": []any{tool}}
	protected, _ := json.Marshal(map[string]any{"call": call, "tool": tool})
	logDSHPromptCompatibility(httptest.NewRequest("POST", "/v1/messages", nil), 1, obj)
	for _, index := range []int{0, 1} {
		if messages[index].(map[string]any)["content"] != "You are an AI coding assistant." {
			t.Error("system or assistant identity was not neutralized")
		}
	}
	for _, index := range []int{2, 3} {
		if messages[index].(map[string]any)["content"] != opening {
			t.Error("user or tool content was changed")
		}
	}
	after, _ := json.Marshal(map[string]any{"call": call, "tool": tool})
	if string(protected) != string(after) || messages[1].(map[string]any)["reasoning_content"] != opening {
		t.Fatal("tool definitions, arguments or reasoning were changed")
	}
}

func TestCCIdentityExplicitConfigurationHasPriority(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	disabled := false
	for _, opening := range ccIdentityTestOpenings {
		setIdentityConfig(runtimeIdentityConfig{Enabled: &disabled})
		if got, changed := normalizeAssistantIdentityText(opening); changed || got != opening {
			t.Fatal("identity.enabled=false no longer preserves the original opening")
		}
	}
	const opening = "You are Claude Code, Anthropic's official CLI for Claude."
	setIdentityConfig(runtimeIdentityConfig{Target: "Configured Agent"})
	if got, _ := normalizeAssistantIdentityText(opening); got != "You are Configured Agent, Anthropic's official CLI for Configured Agent." {
		t.Fatal("the default prefix rule overrode an explicit identity target")
	}
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{"Claude Code": "Configured helper"}})
	if got, _ := normalizeAssistantIdentityText(opening); got != "You are Configured helper, Anthropic's official CLI for Claude." {
		t.Fatal("the default prefix rule overrode custom identity replacements")
	}
}

func TestCCIdentityForceAndFallbackRemainIdempotent(t *testing.T) {
	for _, kind := range []string{"existing-force", "injected-force", "fallback"} {
		for _, blocks := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/blocks=%t", kind, blocks), func(t *testing.T) {
				withDefaultCCIdentity(t)
				withRequestProfile(t, requestProfileLegacy)
				opening := ccIdentityTestOpenings[0] + " force-sentinel"
				withSystemPrompts(t, opening, opening)
				content := any(opening)
				if kind == "injected-force" {
					content = "Client instruction."
				}
				if blocks {
					content = []any{map[string]any{"type": "text", "text": content}}
				}
				messages := []any{map[string]any{"role": "user", "content": "Keep the user's Claude Code text."}}
				if kind != "fallback" {
					messages = append([]any{map[string]any{"role": "system", "content": content}}, messages...)
				}
				obj := map[string]any{"messages": messages}
				r := httptest.NewRequest("POST", "/v1/messages", nil)
				prepareUpstreamChatRequest(obj, r, 1, "synthetic-model")
				first, err := json.Marshal(obj)
				if err != nil {
					t.Fatal(err)
				}
				prepareUpstreamChatRequest(obj, r, 1, "synthetic-model")
				second, err := json.Marshal(obj)
				if err != nil || string(first) != string(second) {
					t.Fatal("normalizing identity made repeated prepare append force again")
				}
				wireMessages := obj["messages"].([]any)
				text := compatText(wireMessages[0].(map[string]any)["content"])
				if strings.Count(text, "force-sentinel") != 1 {
					t.Fatal("the configured force was duplicated")
				}
				if kind != "injected-force" && text != "You are an AI coding assistant. force-sentinel" {
					t.Fatal("a leading configured identity was not neutralized")
				}
				if kind == "injected-force" {
					want := "Client instruction.\n\n" + opening
					if blocks {
						want = "Client instruction.\nYou are an AI coding assistant. force-sentinel"
					}
					if text != want {
						t.Fatal("injected force did not follow the text-part prefix boundary")
					}
				}
				if wireMessages[len(wireMessages)-1].(map[string]any)["content"] != "Keep the user's Claude Code text." || configuredSystemPrompts().force != opening {
					t.Fatal("configured force or user content changed")
				}
			})
		}
	}
}

func TestCCIdentityMessagesAfterAttributionHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			withDefaultCCIdentity(t)
			withRequestProfile(t, requestProfileLegacy)
			withSystemPrompts(t, "", "")
			opening := ccIdentityTestOpenings[4]
			attribution := "x-anthropic-billing-header: cc_version=2.1.308.abc; cc_entrypoint=sdk-cli;"
			tool := map[string]any{"name": "noop", "description": opening, "input_schema": map[string]any{"type": "object"}}
			body := map[string]any{
				"model": compatModel, "stream": stream, "max_tokens": 64,
				"thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": "high"},
				"system": []any{
					ccAttributionTestBlock(attribution),
					ccAttributionTestBlock(opening + "\n```text"),
					ccAttributionTestBlock(opening),
					ccAttributionTestBlock("```\nKeep the remaining instructions."),
				},
				"messages": []any{map[string]any{"role": "user", "content": opening}},
				"tools":    []any{tool},
			}
			status, out := h.send(t, "/v1/messages", body, nil, 0)
			if status != 200 {
				t.Fatalf("Messages HTTP %d", status)
			}
			assertCompatSuccess(t, "/v1/messages", stream, out)
			wire := h.take(t).body
			assertCompatMessages(t, wire, []compatWantMessage{
				{Role: "system", Text: "You are an AI coding assistant.\n```text\n" + opening + "\n```\nKeep the remaining instructions."},
				{Role: "user", Text: opening},
			})
			gotTool := wire["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
			if !reflect.DeepEqual(gotTool["parameters"], tool["input_schema"]) || gotTool["description"] != opening || wire["reasoning_effort"] != "high" {
				t.Fatal("identity normalization changed tools or thinking mode")
			}
			for _, private := range []string{attribution, opening, "Keep the remaining instructions."} {
				if strings.Contains(h.logs.String(), private) {
					t.Fatal("identity or attribution contents leaked into diagnostics")
				}
			}
		})
	}
}
