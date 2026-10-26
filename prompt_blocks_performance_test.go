package main

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestPromptBlockParsingViewBoundaries(t *testing.T) {
	for _, anthropic := range []bool{false, true} {
		for _, role := range []string{"system", "assistant"} {
			message := map[string]any{"role": role, "content": []any{
				map[string]any{"type": "text", "text": "中文`"},
				map[string]any{"type": "text", "text": ""},
				map[string]any{"type": "text", "text": 42},
				map[string]any{"type": "image_url", "text": "ignored"},
				nil,
				map[string]any{"type": "text", "text": "🙂`"},
			}}
			separator := "\n"
			if anthropic && role == "assistant" {
				separator = ""
			}
			text, parts := dshMessageText(message, anthropic)
			if text != "中文`"+separator+"🙂`" || len(parts) != 2 || parts[0].start != 0 || parts[1].start != len("中文`"+separator) {
				t.Fatalf("parsing view or byte offsets changed: role=%s anthropic=%t", role, anthropic)
			}
		}
	}
}

// Compare the lazy candidate path with the previous eager whole-message scan.
// The oracle uses the same rewrite rules, so this targets skipped candidates
// and lost cross-block Markdown context rather than duplicating those rules.
func eagerPromptBlockCompatibility(obj map[string]any, role string, anthropic, gui bool) int {
	settings := identitySnapshot()
	changed := 0
	for _, raw := range obj["messages"].([]any) {
		message := raw.(map[string]any)
		text, parts := dshMessageText(message, anthropic)
		code := dshMarkdownCodeRanges(text)
		for _, part := range parts {
			if gui {
				if normalized, n := normalizeDSHGUIContextPart(part.text, part.start, code); n > 0 {
					part.container[part.key] = normalized
					changed += n
				}
			} else if roleOfMessage(message) == role {
				if normalized, ok := normalizeIdentityText(part.text, code, part.start, true, settings); ok {
					part.container[part.key] = normalized
					changed++
				}
			}
		}
	}
	return changed
}

func FuzzPromptBlockCompatibilityParity(f *testing.F) {
	preserveIdentitySettings(f)
	setIdentityConfig(runtimeIdentityConfig{Replacements: map[string]string{"SyntheticName": "Helper"}})
	for _, seed := range [][3]string{
		{"", "", ""},
		{"`", "SyntheticName` and SyntheticName", ""},
		{"```text\n", dshGUIContextOpening, "\n```\n" + dshGUIContextOpening},
		{dshGUIContextOpening[:30], dshGUIContextOpening[30:], ""},
		{"", "You are Claude Code.", "SyntheticName"},
		{"中文\r\n", "``SyntheticName", "`🙂`` SyntheticName"},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}
	f.Fuzz(func(t *testing.T, first, middle, last string) {
		if len(first)+len(middle)+len(last) > 8192 {
			t.Skip()
		}
		for _, anthropic := range []bool{false, true} {
			for _, role := range []string{"system", "assistant"} {
				makeObject := func() map[string]any {
					var blocks []any
					for _, text := range []string{first, middle, last} {
						blocks = append(blocks, map[string]any{"type": "text", "text": text, "cache_control": "synthetic-metadata"})
					}
					return map[string]any{"messages": []any{map[string]any{"role": role, "content": blocks}}}
				}
				for _, gui := range []bool{false, true} {
					got, want := makeObject(), makeObject()
					wantChanged := eagerPromptBlockCompatibility(want, role, anthropic, gui)
					var changed int
					if gui {
						changed = normalizeDSHGUIContextsForProtocol(got, anthropic)
					} else {
						changed = sanitizeMessageIdentityPrefixesForProtocol(got, role, anthropic)
					}
					if changed != wantChanged || !reflect.DeepEqual(got, want) {
						t.Fatalf("lazy/eager mismatch: role=%s anthropic=%t gui=%t", role, anthropic, gui)
					}
				}
			}
		}
	})
}

func syntheticPromptBlocks(count, size int) map[string]any {
	line := "Synthetic guidance with `unchanged_code` and ordinary prose.\n"
	text := strings.Repeat(line, size/len(line))
	blocks := make([]any, count)
	for i := range blocks {
		blocks[i] = map[string]any{"type": "text", "text": text, "cache_control": map[string]any{"type": "ephemeral"}}
	}
	return map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": blocks}}}
}

func TestUnmatchedPromptBlocksAvoidBodyCopies(t *testing.T) {
	preserveIdentitySettings(t)
	setIdentityConfig(runtimeIdentityConfig{})
	for _, anthropic := range []bool{false, true} {
		for _, stage := range []string{"gui", "identity"} {
			t.Run(fmt.Sprintf("%s/anthropic=%t", stage, anthropic), func(t *testing.T) {
				obj := syntheticPromptBlocks(16, 128<<10)
				run := func() int {
					if stage == "gui" {
						return normalizeDSHGUIContextsForProtocol(obj, anthropic)
					}
					return sanitizeMessageIdentityPrefixesForProtocol(obj, "assistant", anthropic)
				}
				if run() != 0 {
					t.Fatal("unmatched content changed")
				}
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				for i := 0; i < 4; i++ {
					if run() != 0 {
						t.Fatal("unmatched content changed")
					}
				}
				runtime.ReadMemStats(&after)
				runtime.KeepAlive(obj)
				// Allow small part descriptors and runtime bookkeeping, but not a
				// full copy of the two-megabyte Markdown parsing view.
				if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 256<<10 {
					t.Fatalf("four unchanged checks allocated %d bytes", allocated)
				}
			})
		}
	}
}

func BenchmarkPromptBlockPreparation(b *testing.B) {
	preserveIdentitySettings(b)
	setIdentityConfig(runtimeIdentityConfig{})
	for _, count := range []int{1, 16} {
		obj := syntheticPromptBlocks(count, 128<<10)
		for _, stage := range []string{"gui", "identity", "combined"} {
			b.Run(fmt.Sprintf("%s/%d_blocks", stage, count), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					switch stage {
					case "gui":
						normalizeDSHGUIContextsForProtocol(obj, true)
					case "identity":
						sanitizeMessageIdentityPrefixesForProtocol(obj, "assistant", true)
					case "combined":
						logDSHPromptCompatibility(nil, 0, obj)
					}
				}
			})
		}
	}
}
