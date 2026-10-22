package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func withClaudeCodeForceIdentity(t *testing.T, enabled bool, force string) {
	t.Helper()
	setIdentityConfig(runtimeIdentityConfig{Enabled: &enabled, Replacements: map[string]string{"Claude Code": "WorkBuddy"}})
	t.Cleanup(func() { setIdentityConfig(runtimeIdentityConfig{}) })
	withRequestProfile(t, requestProfileLegacy)
	withSystemPrompts(t, "Fallback sentinel.", force)
}

func identityForceMessage(content any, force string) map[string]any {
	return map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": content},
		map[string]any{"role": "user", "content": force},
	}}
}

func prepareIdentityForceRequest(obj map[string]any) {
	prepareUpstreamChatRequest(obj, httptest.NewRequest("POST", "/v1/messages", nil), 1, "identity-test-model")
}

func TestIdentityForceAlreadyPresentSharedPrepare(t *testing.T) {
	const force = "You are Claude Code."
	for _, enabled := range []bool{true, false} {
		for _, blocks := range []bool{false, true} {
			t.Run(fmt.Sprintf("enabled=%t/blocks=%t", enabled, blocks), func(t *testing.T) {
				withClaudeCodeForceIdentity(t, enabled, force)
				wantForce := force
				if enabled {
					wantForce = "You are WorkBuddy."
				}
				var content any = "Client instruction.\n\n" + force
				var want any = "Client instruction.\n\n" + wantForce
				if blocks {
					content = []any{
						map[string]any{"type": "text", "text": "Client instruction.", "cache_control": map[string]any{"type": "ephemeral"}},
						map[string]any{"type": "text", "text": force, "sentinel": true},
					}
					want = []any{
						map[string]any{"type": "text", "text": "Client instruction.", "cache_control": map[string]any{"type": "ephemeral"}},
						map[string]any{"type": "text", "text": wantForce, "sentinel": true},
					}
				}
				obj := identityForceMessage(content, force)
				prepareIdentityForceRequest(obj)
				messages := obj["messages"].([]any)
				if got := messages[0].(map[string]any)["content"]; !reflect.DeepEqual(got, want) {
					t.Fatalf("existing force was duplicated or changed: got=%#v want=%#v", got, want)
				}
				if messages[1].(map[string]any)["content"] != force || configuredSystemPrompts().force != force {
					t.Fatal("user content or configured force changed")
				}
			})
		}
	}
}

func TestIdentityForceRepeatedSharedPrepare(t *testing.T) {
	const force = "You are Claude Code."
	for _, blocks := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocks=%t", blocks), func(t *testing.T) {
			withClaudeCodeForceIdentity(t, true, force)
			var content any = "Client instruction."
			if blocks {
				content = []any{map[string]any{"type": "text", "text": content, "sentinel": true}}
			}
			obj := identityForceMessage(content, force)
			prepareIdentityForceRequest(obj)
			first, err := json.Marshal(obj)
			if err != nil {
				t.Fatal(err)
			}
			prepareIdentityForceRequest(obj)
			second, err := json.Marshal(obj)
			if err != nil {
				t.Fatal(err)
			}
			if string(first) != string(second) {
				t.Fatal("preparing a normalized request appended configured force again")
			}
		})
	}
}

func TestIdentityForcePreservesCodeContextAcrossBlocks(t *testing.T) {
	// The configured force starts inside the client's fence and closes it.
	// Only its final prose is eligible for the configured name replacement.
	const force = "Claude Code\n```\nUse Claude Code."
	withClaudeCodeForceIdentity(t, true, force)
	content := []any{
		map[string]any{"type": "text", "text": "```text\nClient example.", "sentinel": true},
		map[string]any{"type": "text", "text": force},
	}
	want := []any{
		map[string]any{"type": "text", "text": "```text\nClient example.", "sentinel": true},
		map[string]any{"type": "text", "text": "Claude Code\n```\nUse WorkBuddy."},
	}
	obj := identityForceMessage(content, force)
	for pass := 0; pass < 2; pass++ {
		prepareIdentityForceRequest(obj)
		if got := obj["messages"].([]any)[0].(map[string]any)["content"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("pass %d lost code context or duplicated force: got=%#v want=%#v", pass+1, got, want)
		}
	}
}

func TestIdentityForceDoesNotTreatDifferentCodeAsNormalizedForce(t *testing.T) {
	const force = "```text\nClaude Code\n```"
	const differentCode = "```text\nWorkBuddy\n```"
	for _, blocks := range []bool{false, true} {
		t.Run(fmt.Sprintf("blocks=%t", blocks), func(t *testing.T) {
			withClaudeCodeForceIdentity(t, true, force)
			var content any = differentCode
			var want any = differentCode + "\n\n" + force
			if blocks {
				content = []any{map[string]any{"type": "text", "text": differentCode}}
				want = []any{
					map[string]any{"type": "text", "text": differentCode},
					map[string]any{"type": "text", "text": force},
				}
			}
			obj := identityForceMessage(content, force)
			for pass := 0; pass < 2; pass++ {
				prepareIdentityForceRequest(obj)
				if got := obj["messages"].([]any)[0].(map[string]any)["content"]; !reflect.DeepEqual(got, want) {
					t.Fatalf("pass %d confused distinct code with normalized force: got=%#v want=%#v", pass+1, got, want)
				}
			}
		})
	}
}

func TestIdentityForceStringCandidateAtExpansionWindowBoundary(t *testing.T) {
	for _, tc := range []struct{ name, force, replacement string }{
		{"same_length", "You are Claude Code.", "Other Agent"},
		{"exact_expansion", "Claude Code", strings.Repeat("W", len("Claude Code")*2)},
		{"one_byte_shrink", "Claude Code", "WorkBuddy!"},
		{"deletion", "Claude Code", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withClaudeCodeForceIdentity(t, true, tc.force)
			setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{"Claude Code": tc.replacement}})
			obj := identityForceMessage("Client instruction.\n\n"+tc.force, tc.force)
			prepareIdentityForceRequest(obj)
			want := "Client instruction.\n\n" + strings.ReplaceAll(tc.force, "Claude Code", tc.replacement)
			if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != want {
				t.Fatalf("force at expansion window boundary duplicated: got=%q want=%q", got, want)
			}
		})
	}
}

func TestIdentityForceEmptyReplacementDoesNotAppendEmptyBlocks(t *testing.T) {
	const force = "Claude Code"
	withClaudeCodeForceIdentity(t, true, force)
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{force: ""}})
	obj := identityForceMessage([]any{map[string]any{"type": "text", "text": "Client instruction."}}, force)
	prepareIdentityForceRequest(obj)
	first, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	prepareIdentityForceRequest(obj)
	second, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("a force removed by an explicit empty replacement appended another empty block")
	}
}
