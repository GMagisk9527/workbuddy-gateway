package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSanitizeSystemPromptPrefixBoundaries(t *testing.T) {
	const prefix = "You are an AI agent powered by DeepSeek Harness"
	const replacement = "You are an AI agent powered by a large language model"
	for _, tc := range []struct {
		name, input, want string
		changed           bool
	}{
		{"leading prefix", prefix + ". Keep developer instructions.", replacement + ". Keep developer instructions.", true},
		{"case insensitive", "you are an ai agent powered by deepseek harness!", replacement + "!", true},
		{"leading space", " " + prefix, " " + prefix, false},
		{"middle text", "prefix: " + prefix, "prefix: " + prefix, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := sanitizeSystemPromptPrefix(tc.input)
			if got != tc.want || changed != tc.changed {
				t.Fatalf("got=%q changed=%t want=%q changed=%t", got, changed, tc.want, tc.changed)
			}
		})
	}
}

func TestSanitizeSystemPromptPrefixIsScopedAndIdempotent(t *testing.T) {
	const prefix = "You are an AI agent powered by DeepSeek Harness"
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": prefix + ". Preserve this code and instruction."},
		map[string]any{"role": "user", "content": prefix + " user text"},
		map[string]any{"role": "assistant", "content": prefix + " assistant text"},
	}}
	if got := sanitizeSystemPromptPrefixes(obj); got != 1 {
		t.Fatalf("modified=%d want=1", got)
	}
	if got := sanitizeSystemPromptPrefixes(obj); got != 0 {
		t.Fatalf("second pass modified=%d want=0", got)
	}
	messages := obj["messages"].([]any)
	system := messages[0].(map[string]any)["content"].(string)
	if system != replacementSystemPromptPrefix+". Preserve this code and instruction." {
		t.Fatalf("system content changed beyond identity prefix: %q", system)
	}
	for _, index := range []int{1, 2} {
		msg := messages[index].(map[string]any)
		content := msg["content"].(string)
		if !strings.HasPrefix(content, prefix) {
			t.Fatalf("role %s was changed: %q", msg["role"], content)
		}
	}
}

func TestAssistantIdentityBrandReplacement(t *testing.T) {
	setIdentityConfig(runtimeIdentityConfig{Target: "WorkBuddy"})
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	cases := []struct{ input, want string }{
		{"您是 Codex，一个在 Codex CLI 中运行的编码代理。", "您是 WorkBuddy，一个在 WorkBuddy CLI 中运行的编码代理。"},
		{"You are an AI agent powered by DeepSeek Harness. Keep rules.", "You are WorkBuddy. Keep rules."},
		{"You are Claude Code, Anthropic's official CLI tool for Claude.", "You are WorkBuddy, Anthropic's official CLI tool for WorkBuddy."},
		{"You are ZCode. Keep rules.", "You are WorkBuddy. Keep rules."},
	}
	for _, tc := range cases {
		got, changed := normalizeAssistantIdentityText(tc.input)
		if !changed || got != tc.want {
			t.Fatalf("input=%q got=%q changed=%t want=%q", tc.input, got, changed, tc.want)
		}
	}
}

func TestAssistantIdentityDoesNotTouchUserOrToolText(t *testing.T) {
	setIdentityConfig(runtimeIdentityConfig{Target: "WorkBuddy"})
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	obj := map[string]any{"messages": []any{
		msg("system", "You are Codex. Keep."),
		msg("assistant", "You are Claude Code. Keep."),
		msg("user", "You are Codex. User text."),
		msg("tool", "You are DeepSeek Harness. Tool text."),
	}}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	logDSHPromptCompatibility(r, 1, obj)
	messages := obj["messages"].([]any)
	if messages[0].(map[string]any)["content"] != "You are WorkBuddy. Keep." ||
		messages[1].(map[string]any)["content"] != "You are WorkBuddy. Keep." {
		t.Fatalf("assistant identity not normalized: %#v", messages[:2])
	}
	if messages[2].(map[string]any)["content"] != "You are Codex. User text." ||
		messages[3].(map[string]any)["content"] != "You are DeepSeek Harness. Tool text." {
		t.Fatal("user or tool text was modified")
	}
}

func TestResponsesReasoningSummaryDetailedIsPreserved(t *testing.T) {
	chat, _, err := responsesToChatRequestDetailed(decodeObject(t, []byte(`{
		"input":"hi","reasoning":{"effort":"max","summary":"detailed"}
	}`)), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatal(err)
	}
	if chat["reasoning_summary"] != "detailed" {
		t.Fatalf("summary lost during conversion: %#v", chat)
	}
	applyThinkingRules(chat, "deepseek-v4.1-flash")
	if chat["reasoning_summary"] != "detailed" {
		t.Fatalf("summary downgraded during outbound preparation: %#v", chat)
	}
}

func TestIdentityConfigTargetAndCustomReplacements(t *testing.T) {
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	setIdentityConfig(runtimeIdentityConfig{Target: "WorkBuddy"})
	got, changed := normalizeAssistantIdentityText("You are an AI agent powered by DeepSeek Harness. Codex")
	if !changed || got != "You are WorkBuddy. WorkBuddy" {
		t.Fatalf("target override got=%q changed=%t", got, changed)
	}
	disabled := false
	setIdentityConfig(runtimeIdentityConfig{Enabled: &disabled, Target: "WorkBuddy"})
	got, changed = normalizeAssistantIdentityText("You are Codex. DeepSeek Harness")
	if changed || got != "You are Codex. DeepSeek Harness" {
		t.Fatalf("disabled identity still changed=%q", got)
	}
	setIdentityConfig(runtimeIdentityConfig{Target: "Assistant", Replacements: map[string]string{
		"Codex": "Coder", "DeepSeek Harness": "Buddy",
	}})
	got, changed = normalizeAssistantIdentityText("You are an AI agent powered by DeepSeek Harness. Codex")
	if !changed || got != "You are Buddy. Coder" {
		t.Fatalf("custom replacements got=%q changed=%t", got, changed)
	}
}
