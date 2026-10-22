package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDSHCodeAcrossContentBlocksThroughHTTP(t *testing.T) {
	identity := blockedSystemPromptPrefix + ". Keep this example byte for byte."
	code := identity + "\n" + dshGUIContextOpening
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, role := range []string{"system", "assistant"} {
			for _, delimiter := range []string{"```text\n", "An example `"} {
				t.Run(fmt.Sprintf("%s/%s/%q", path, role, delimiter), func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					close := "`"
					if delimiter == "```text\n" {
						close = "\n```"
					}
					parts := []any{
						map[string]any{"type": "text", "text": delimiter},
						map[string]any{"type": "text", "text": code, "cache_control": map[string]any{"type": "ephemeral"}},
						map[string]any{"type": "text", "text": close},
					}
					messages := []any{map[string]any{"role": "system", "content": "Keep code unchanged."}, map[string]any{"role": "user", "content": "Explain this."}}
					index := 0
					if role == "system" {
						messages[0].(map[string]any)["content"] = parts
					} else {
						messages = append(messages, map[string]any{"role": "assistant", "content": parts}, map[string]any{"role": "user", "content": "Continue."})
						index = 2
					}
					body := map[string]any{"model": compatModel, "max_tokens": 64, "messages": messages}
					if path == "/v1/responses" {
						body["input"], body["store"] = messages, false
						delete(body, "messages")
					} else if path == "/v1/messages" {
						body["system"] = messages[0].(map[string]any)["content"]
						body["messages"] = messages[1:]
					}
					status, _ := h.send(t, path, body, nil, 0)
					if status != 200 {
						t.Fatalf("HTTP %d", status)
					}
					got := h.take(t).body["messages"].([]any)[index].(map[string]any)["content"]
					if path == "/v1/messages" {
						separator := "\n"
						if role == "assistant" {
							separator = ""
						}
						if got != strings.Join([]string{delimiter, code, close}, separator) {
							t.Fatal("Anthropic conversion rewrote Markdown code")
						}
					} else if captured, ok := got.([]any); !ok || captured[1].(map[string]any)["text"] != code {
						t.Fatal("code text in a later content block was rewritten")
					}
				})
			}
		}
	}
}

func TestDSHInlineCodeRespectsAnthropicJoin(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"split delimiter", "An example `", "`" + dshGUIContextOpening + "``"},
		{"unmatched marker before fence", "An unmatched marker ```\n", "```text\n" + dshGUIContextOpening + "\n```"},
		{"unmatched shorter marker before fence", "An unmatched `\n", "```text\n` literal code\n" + dshGUIContextOpening + "\n```"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			parts := []any{map[string]any{"type": "text", "text": tc.first}, map[string]any{"type": "text", "text": tc.second}}
			body := map[string]any{"model": compatModel, "max_tokens": 64, "messages": []any{
				map[string]any{"role": "user", "content": "Explain."},
				map[string]any{"role": "assistant", "content": parts},
				map[string]any{"role": "user", "content": "Continue."},
			}}
			status, _ := h.send(t, "/v1/messages", body, nil, 0)
			if status != 200 {
				t.Fatalf("HTTP %d", status)
			}
			got := h.take(t).body["messages"].([]any)[2].(map[string]any)["content"]
			if got != tc.first+tc.second {
				t.Fatal("joining adjacent Anthropic text blocks lost Markdown code protection")
			}
		})
	}
}

func TestDSHMarkdownLiteralsAndMessageBoundaries(t *testing.T) {
	neutral := strings.Replace(dshGUIContextOpening, dshGUIContextSentence, dshGUIContextNeutral, 1)
	for _, prefix := range []string{"The literal backtick is `.\n\n", "Escaped \\` marker. ", "A literal unmatched `` marker. "} {
		t.Run(prefix, func(t *testing.T) {
			got, changes := normalizeDSHGUIContextText(prefix + dshGUIContextOpening)
			if changes != 1 || got != prefix+neutral {
				t.Fatal("a literal backtick suppressed normalization of ordinary prose")
			}
		})
	}
	unicodeLine := "Example `first\n\u3000\n" + dshGUIContextOpening + "\nlast`."
	if got, changes := normalizeDSHGUIContextText(unicodeLine); got != unicodeLine || changes != 0 {
		t.Fatal("a line containing a full-width space was treated as an empty Markdown paragraph")
	}
	interruptedSpan := "Example ````first\n```\n" + dshGUIContextOpening + "\nlast```` " + dshGUIContextOpening
	if got, changes := normalizeDSHGUIContextText(interruptedSpan); got != interruptedSpan || changes != 0 {
		t.Fatal("a real fence failed to interrupt a paragraph containing unmatched inline backticks")
	}
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": "```text\n"},
		map[string]any{"role": "user", "content": dshGUIContextOpening},
		map[string]any{"role": "assistant", "content": dshGUIContextOpening},
	}}
	logSystemPromptPrefixCompatibility(nil, obj)
	messages := obj["messages"].([]any)
	if messages[1].(map[string]any)["content"] != dshGUIContextOpening || messages[2].(map[string]any)["content"] != neutral {
		t.Fatal("Markdown state leaked between messages or user content was changed")
	}
}

func TestDSHCompatibilityPreservesBlockMetadataAndNonTextData(t *testing.T) {
	original := blockedSystemPromptPrefix + ".\n\n" + dshGUIContextOpening
	message := map[string]any{
		"role": "assistant",
		"content": []any{
			map[string]any{"type": "text", "text": "Earlier prose.", "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": original, "custom": map[string]any{"sentinel": true}},
			map[string]any{"type": "image_url", "text": original, "image_url": map[string]any{"url": "private-image-sentinel"}},
			nil,
		},
		"reasoning_content": original,
		"tool_calls":        []any{map[string]any{"id": "call-sentinel", "type": "function", "function": map[string]any{"name": "save", "arguments": original}}},
	}
	obj := map[string]any{"messages": []any{message, map[string]any{"role": "user", "content": original}, map[string]any{"role": "tool", "content": original, "tool_call_id": "call-sentinel"}}}
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	parts := expected["messages"].([]any)[0].(map[string]any)["content"].([]any)
	parts[1].(map[string]any)["text"] = replacementSystemPromptPrefix + ".\n\n" + strings.Replace(dshGUIContextOpening, dshGUIContextSentence, dshGUIContextNeutral, 1)
	logSystemPromptPrefixCompatibility(nil, obj)
	if !reflect.DeepEqual(obj, expected) {
		t.Fatal("compatibility processing changed block structure, metadata, user/tool data, reasoning or tool arguments")
	}
}

func TestDSHNormalizedForceIsIdempotent(t *testing.T) {
	for _, force := range []string{dshGUIContextOpening, blockedSystemPromptPrefix + ".\n\n" + dshGUIContextOpening} {
		for _, blocks := range []bool{false, true} {
			t.Run(fmt.Sprintf("identity=%t/blocks=%t", strings.HasPrefix(force, blockedSystemPromptPrefix), blocks), func(t *testing.T) {
				withSystemPrompts(t, "Fallback remains raw.", force)
				var content any = "Client instructions."
				if blocks {
					content = []any{map[string]any{"type": "text", "text": content, "cache_control": map[string]any{"type": "ephemeral"}}}
				}
				obj := map[string]any{"messages": []any{map[string]any{"role": "system", "content": content}, map[string]any{"role": "user", "content": force}}}
				r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
				prepareSystemPromptForUpstream(obj, r, 1, "synthetic")
				logSystemPromptPrefixCompatibility(r, obj)
				first, _ := json.Marshal(obj)
				prepareSystemPromptForUpstream(obj, r, 1, "synthetic")
				logSystemPromptPrefixCompatibility(r, obj)
				second, _ := json.Marshal(obj)
				if string(first) != string(second) {
					t.Fatal("normalizing force caused a repeated pass to append it again")
				}
				if settings := configuredSystemPrompts(); settings.force != force || settings.fallback != "Fallback remains raw." {
					t.Fatal("compatibility processing changed configuration text")
				}
				if obj["messages"].([]any)[1].(map[string]any)["content"] != force {
					t.Fatal("user text was changed")
				}
			})
		}
	}
}

func TestDSHForceInClientCodeContext(t *testing.T) {
	for _, force := range []string{"```\n" + dshGUIContextOpening, dshGUIContextOpening + "\n```\n" + dshGUIContextOpening} {
		for _, blocks := range []bool{false, true} {
			t.Run(fmt.Sprintf("closeFirst=%t/blocks=%t", strings.HasPrefix(force, "```"), blocks), func(t *testing.T) {
				withSystemPrompts(t, "", force)
				var content any = "```text\nClient example."
				if blocks {
					content = []any{map[string]any{"type": "text", "text": content}}
				}
				obj := map[string]any{"messages": []any{map[string]any{"role": "system", "content": content}}}
				r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
				prepareSystemPromptForUpstream(obj, r, 1, "synthetic")
				logSystemPromptPrefixCompatibility(r, obj)
				first, _ := json.Marshal(obj)
				prepareSystemPromptForUpstream(obj, r, 1, "synthetic")
				logSystemPromptPrefixCompatibility(r, obj)
				second, _ := json.Marshal(obj)
				if string(first) != string(second) {
					t.Fatal("force was duplicated after closing a code fence from the client")
				}
			})
		}
	}
	// A neutral GUI literal inside code is not a valid normalization of raw GUI
	// code in force. It must not cause the distinct configured force to be skipped.
	force := "```text\n" + dshGUIContextOpening + "\n```"
	neutralCode := strings.Replace(force, dshGUIContextSentence, dshGUIContextNeutral, 1)
	withSystemPrompts(t, "", force)
	obj := map[string]any{"messages": []any{map[string]any{"role": "system", "content": neutralCode}}}
	prepareSystemPromptForUpstream(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 1, "synthetic")
	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got != neutralCode+"\n\n"+force {
		t.Fatal("distinct code in configured force was mistaken for a normalized copy")
	}
}

func TestDSHExistingNormalizedForceThroughHTTP(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, blocks := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/blocks=%t", path, blocks), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				force := blockedSystemPromptPrefix + ".\n\n" + dshGUIContextOpening
				withSystemPrompts(t, "", force)
				var content any = force
				if blocks {
					content = []any{map[string]any{"type": "text", "text": force}}
				}
				messages := []any{map[string]any{"role": "system", "content": content}, map[string]any{"role": "user", "content": force}}
				body := map[string]any{"model": compatModel, "max_tokens": 64, "messages": messages}
				if path == "/v1/responses" {
					body["input"], body["store"] = messages, false
					delete(body, "messages")
				} else if path == "/v1/messages" {
					body["system"], body["messages"] = content, messages[1:]
				}
				status, _ := h.send(t, path, body, nil, 0)
				if status != 200 {
					t.Fatalf("HTTP %d", status)
				}
				captured := h.take(t).body["messages"].([]any)
				want := replacementSystemPromptPrefix + ".\n\n" + strings.Replace(dshGUIContextOpening, dshGUIContextSentence, dshGUIContextNeutral, 1)
				if compatText(captured[0].(map[string]any)["content"]) != want || captured[1].(map[string]any)["content"] != force {
					t.Fatal("the existing force was duplicated or unrelated user text changed")
				}
				if !reflect.DeepEqual(configuredSystemPrompts(), promptSettings{force: force}) {
					t.Fatal("processing rewrote configured force")
				}
			})
		}
	}
}
