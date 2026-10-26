package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func preserveIdentitySettings(t testing.TB) {
	t.Helper()
	identityConfigMu.RLock()
	old := currentIdentity
	identityConfigMu.RUnlock()
	t.Cleanup(func() {
		identityConfigMu.Lock()
		currentIdentity = old
		identityConfigMu.Unlock()
	})
}

func TestIdentityNoMatchAvoidsTextAllocation(t *testing.T) {
	preserveIdentitySettings(t)
	disabled := false
	text := strings.Repeat("Synthetic guidance with `unchanged_code` and ordinary prose.\n", 2048)
	for name, config := range map[string]runtimeIdentityConfig{
		"default":  {},
		"disabled": {Enabled: &disabled, Target: "Synthetic helper"},
		"names":    {Target: "Synthetic helper"},
	} {
		t.Run(name, func(t *testing.T) {
			setIdentityConfig(config)
			var got string
			var changed bool
			allocs := testing.AllocsPerRun(5, func() {
				got, changed = normalizeAssistantIdentityText(text)
			})
			if changed || got != text {
				t.Fatal("unmatched text was changed")
			}
			if allocs > 1 {
				t.Fatalf("unmatched text allocated %.0f objects, want at most 1", allocs)
			}
		})
	}
}

func TestIdentityMessageNoMatchSkipsMarkdownAllocation(t *testing.T) {
	preserveIdentitySettings(t)
	setIdentityConfig(runtimeIdentityConfig{})
	text := strings.Repeat("Synthetic history with `unchanged_code`.\n", 2048)
	obj := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": text}}}
	var modified int
	allocs := testing.AllocsPerRun(5, func() {
		modified = sanitizeMessageIdentityPrefixesForProtocol(obj, "assistant", true)
	})
	if modified != 0 || obj["messages"].([]any)[0].(map[string]any)["content"] != text {
		t.Fatal("unmatched message changed")
	}
	if allocs > 2 {
		t.Fatalf("unmatched message allocated %.0f objects, want at most 2", allocs)
	}
}

func TestIdentityConcurrentConfigSnapshots(t *testing.T) {
	preserveIdentitySettings(t)
	config := func(target string) runtimeIdentityConfig {
		return runtimeIdentityConfig{Replacements: map[string]string{"first term": target + "-one", "second term": target + "-two"}}
	}
	setIdentityConfig(config("A"))
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				got, changed := normalizeAssistantIdentityText("first term / second term / `first term`")
				if !changed || (got != "A-one / A-two / `first term`" && got != "B-one / B-two / `first term`") {
					t.Errorf("mixed or stale identity snapshot: %q", got)
					return
				}
			}
		}()
	}
	for i := 0; i < 100; i++ {
		setIdentityConfig(config("B"))
		setIdentityConfig(config("A"))
	}
	wg.Wait()
}

func TestIdentityCompiledRulesKeepPriorityAndReload(t *testing.T) {
	preserveIdentitySettings(t)
	rules := map[string]string{"Alpha Beta": "Gamma", "Alpha": "short", "Gamma": "final", "remove": ""}
	setIdentityConfig(runtimeIdentityConfig{Replacements: rules})
	rules["Alpha Beta"] = "changed after publication"
	got, changed := normalizeAssistantIdentityText("Alpha Beta / Alpha / Gamma / remove / `Alpha Beta`")
	if !changed || got != "Gamma / short / final /  / `Alpha Beta`" {
		t.Fatalf("rule priority, non-cascading replacement or code changed: %q", got)
	}
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{"Alpha": "new"}})
	got, changed = normalizeAssistantIdentityText("Alpha Beta")
	if !changed || got != "new Beta" {
		t.Fatalf("new rules were not immediately visible: %q", got)
	}
	// An unchanged longest match still shadows a shorter changing rule.
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{"Alpha Beta": "Alpha Beta", "Alpha": "new"}})
	if got, changed = normalizeAssistantIdentityText("Alpha Beta"); changed || got != "Alpha Beta" {
		t.Fatalf("identity rule stopped shadowing shorter matches: %q", got)
	}
}

func TestIdentityCandidateKeepsUnmatchedPartsAsCodeContext(t *testing.T) {
	preserveIdentitySettings(t)
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{"Alpha Beta": "Gamma"}})
	first := map[string]any{"type": "text", "text": "`"}
	second := map[string]any{"type": "text", "text": "Alpha Beta` and Alpha Beta"}
	obj := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{first, second}}}}
	if modified := sanitizeMessageIdentityPrefixesForProtocol(obj, "assistant", true); modified != 1 || first["text"] != "`" || second["text"] != "Alpha Beta` and Gamma" {
		t.Fatalf("an unmatched earlier part lost its Markdown scope: %#v", obj)
	}
}

func TestIdentityLargeRuleSetKeepsFullMatching(t *testing.T) {
	preserveIdentitySettings(t)
	rules := make(map[string]string)
	for i := 0; i < 32; i++ {
		rules[fmt.Sprintf("SyntheticTerm%02d", i)] = fmt.Sprintf("Value%02d", i)
	}
	setIdentityConfig(runtimeIdentityConfig{Replacements: rules})
	got, changed := normalizeAssistantIdentityText("SyntheticTerm17 / `SyntheticTerm18` / SyntheticTerm31")
	if !changed || got != "Value17 / `SyntheticTerm18` / Value31" {
		t.Fatalf("large rule set lost matching or code protection: %q", got)
	}
	if got, changed = normalizeAssistantIdentityText("unchanged text"); changed || got != "unchanged text" {
		t.Fatalf("large rule set changed unmatched text: %q", got)
	}
}

var identityBenchmarkText string

func BenchmarkIdentityPreparation(b *testing.B) {
	preserveIdentitySettings(b)
	for _, configured := range []bool{false, true} {
		name := "default"
		if configured {
			name = "configured"
		}
		for _, size := range []int{1024, 128 << 10} {
			b.Run(fmt.Sprintf("%s/no_match_%dKiB", name, size>>10), func(b *testing.B) {
				config := runtimeIdentityConfig{}
				if configured {
					config.Target = "Synthetic helper"
				}
				setIdentityConfig(config)
				line := "Synthetic guidance with `unchanged_code` and ordinary prose.\n"
				text := strings.Repeat(line, size/len(line))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					identityBenchmarkText, _ = normalizeAssistantIdentityText(text)
				}
			})
		}
	}
	b.Run("configured/match", func(b *testing.B) {
		setIdentityConfig(runtimeIdentityConfig{Target: "Synthetic helper"})
		text := "Use Claude Code and Codex, preserving `Claude Code` verbatim."
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			identityBenchmarkText, _ = normalizeAssistantIdentityText(text)
		}
	})
	b.Run("messages/default_no_match_1MiB", func(b *testing.B) {
		setIdentityConfig(runtimeIdentityConfig{})
		text := strings.Repeat("Synthetic history with `unchanged_code`.\n", 8192)
		var messages []any
		for i := 0; i < 3; i++ {
			messages = append(messages, map[string]any{"role": "assistant", "content": text})
		}
		obj := map[string]any{"messages": messages}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			logDSHPromptCompatibility(nil, 0, obj)
		}
	})
}
