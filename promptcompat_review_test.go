package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestConfiguredSystemPromptCompatibilityThroughHTTP(t *testing.T) {
	identity := blockedSystemPromptPrefix + ". config-identity-sentinel"
	neutralIdentity := replacementSystemPromptPrefix + ". config-identity-sentinel"
	gui := dshGUIContextOpening + " Keep config-tail-sentinel."
	neutralGUI := strings.Replace(gui, dshGUIContextSentence, dshGUIContextNeutral, 1)
	user := identity + "\n\n" + gui + " private-user-sentinel"
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				name, fallback, force string
				clientSystem          any
				want                  string
			}{
				{"fallback", identity + "\n\n" + gui, "", nil, neutralIdentity + "\n\n" + neutralGUI},
				{"force_string", "", gui, "Client instruction.", "Client instruction.\n\n" + neutralGUI},
				{"force_block", "", identity + "\n\n" + gui, []any{map[string]any{"type": "text", "text": "Client instruction."}}, "Client instruction.\n" + neutralIdentity + "\n\n" + neutralGUI},
				{"fallback_and_force", identity, gui, nil, neutralIdentity + "\n\n" + neutralGUI},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", path, stream, tc.name), func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					withSystemPrompts(t, tc.fallback, tc.force)
					messages := []any{map[string]any{"role": "user", "content": user}}
					if tc.clientSystem != nil {
						messages = append([]any{map[string]any{"role": "system", "content": tc.clientSystem}}, messages...)
					}
					body := map[string]any{"model": compatModel, "stream": stream, "max_tokens": 64, "messages": messages}
					want := tc.want
					if path == "/v1/responses" {
						body["input"], body["store"] = messages, false
						delete(body, "messages")
					} else if path == "/v1/messages" {
						body["messages"] = []any{map[string]any{"role": "user", "content": user}}
						if tc.clientSystem != nil {
							body["system"] = tc.clientSystem
						}
						if tc.name == "force_block" {
							// Messages joins client system blocks before appending force
							// as string text; this identity is mid-string, not an opening.
							want = "Client instruction.\n\n" + identity + "\n\n" + neutralGUI
						}
					}
					status, _ := h.send(t, path, body, nil, 0)
					if status != 200 {
						t.Fatalf("HTTP %d", status)
					}
					assertCompatMessages(t, h.take(t).body, []compatWantMessage{{Role: "system", Text: want}, {Role: "user", Text: user}})
					if h.calls.Load() != 1 {
						t.Fatal("configured prompt caused extra upstream calls")
					}
				})
			}
		}
	}
}

func TestDSHGUIContextMultilineCodeSpans(t *testing.T) {
	neutral := strings.Replace(dshGUIContextOpening, dshGUIContextSentence, dshGUIContextNeutral, 1)
	for _, tc := range []struct {
		name, input, want string
		changes           int
	}{
		{"single_tick", "Example `first line\n" + dshGUIContextOpening + "\nlast line`.", "Example `first line\n" + dshGUIContextOpening + "\nlast line`.", 0},
		{"double_tick_with_single_tick", "Example ``first line\n` " + dshGUIContextOpening + "\nlast line``.", "Example ``first line\n` " + dshGUIContextOpening + "\nlast line``.", 0},
		{"windows_newlines", "Example `first\r\n" + dshGUIContextOpening + "\r\nlast`.", "Example `first\r\n" + dshGUIContextOpening + "\r\nlast`.", 0},
		{"normalize_after_close", "Example `first\n" + dshGUIContextOpening + "\nlast` " + dshGUIContextOpening, "Example `first\n" + dshGUIContextOpening + "\nlast` " + neutral, 1},
		{"fence_like_line_inside_span", "Example ````first\n```\n" + dshGUIContextOpening + "\nlast```` " + dshGUIContextOpening, "Example ````first\n```\n" + dshGUIContextOpening + "\nlast```` " + neutral, 1},
		{"close_on_indented_line", "Example `first\n" + dshGUIContextOpening + "\n    last`\n" + dshGUIContextOpening, "Example `first\n" + dshGUIContextOpening + "\n    last`\n" + neutral, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := normalizeDSHGUIContextText(tc.input)
			if got != tc.want || n != tc.changes {
				t.Fatalf("multiline span or following prose changed incorrectly: changes=%d want=%d", n, tc.changes)
			}
			if again, n := normalizeDSHGUIContextText(got); again != got || n != 0 {
				t.Fatal("second normalization changed protected code or normalized prose")
			}
		})
	}
}
