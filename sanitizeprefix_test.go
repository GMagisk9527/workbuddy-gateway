package main

import (
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
