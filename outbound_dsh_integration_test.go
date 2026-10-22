package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnifiedDSHCompatibilityThroughHTTP(t *testing.T) {
	identity := blockedSystemPromptPrefix + ". Preserve code."
	code := identity + "\n" + dshGUIContextOpening
	neutral := strings.Replace(dshGUIContextOpening, dshGUIContextSentence, dshGUIContextNeutral, 1)
	for _, profile := range []string{requestProfileLegacy, requestProfileOfficial} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			for _, inline := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s%s/inline=%t", profile, path, inline), func(t *testing.T) {
					// International accounts use the same body profile without gzip;
					// the upstream official gzip contract has separate HTTP coverage.
					h := newCompatHarness(t, "intl")
					withRequestProfile(t, profile)
					withSystemPrompts(t, "", dshGUIContextOpening)
					opening, closing := "```text\n", "\n```"
					if inline {
						opening, closing = "Example `", "`"
					}
					system := []any{map[string]any{"type": "text", "text": opening}, map[string]any{"type": "text", "text": code}, map[string]any{"type": "text", "text": closing}}
					assistant := []any{map[string]any{"type": "text", "text": "Example `"}, map[string]any{"type": "text", "text": dshGUIContextOpening}, map[string]any{"type": "text", "text": "`"}}
					messages := []any{map[string]any{"role": "system", "content": system}, map[string]any{"role": "user", "content": "Explain."}, map[string]any{"role": "assistant", "content": assistant}, map[string]any{"role": "user", "content": code}}
					body := map[string]any{"model": compatModel, "max_tokens": 64, "messages": messages}
					if path == "/v1/responses" {
						body["input"], body["store"] = messages, false
						delete(body, "messages")
					} else if path == "/v1/messages" {
						body["system"], body["messages"] = system, messages[1:]
					}
					status, _ := h.send(t, path, body, nil, 0)
					if status != 200 {
						t.Fatalf("HTTP %d", status)
					}
					got := h.take(t).body["messages"].([]any)
					first := compatText(got[0].(map[string]any)["content"])
					if !strings.Contains(first, code) || strings.Count(first, neutral) != 1 || !strings.HasSuffix(first, neutral) {
						t.Fatal("the unified body profile rewrote code or duplicated configured force")
					}
					if !strings.Contains(compatText(got[2].(map[string]any)["content"]), dshGUIContextOpening) || got[3].(map[string]any)["content"] != code {
						t.Fatal("assistant code or user data changed")
					}
				})
			}
		}
	}
}

func TestUnifiedDSHForceRemainsIdempotent(t *testing.T) {
	for _, profile := range []string{requestProfileLegacy, requestProfileOfficial} {
		for _, blocks := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/blocks=%t", profile, blocks), func(t *testing.T) {
				isolatedGatewayConfig(t)
				withRequestProfile(t, profile)
				withSystemPrompts(t, "", "```\n"+dshGUIContextOpening)
				var content any = "```text\nClient example."
				if blocks {
					content = []any{map[string]any{"type": "text", "text": content}}
				}
				obj := map[string]any{"messages": []any{map[string]any{"role": "system", "content": content}, map[string]any{"role": "user", "content": "hi"}}}
				r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
				prepareUpstreamChatRequest(obj, r, 1, compatModel)
				first, _ := json.Marshal(obj)
				prepareUpstreamChatRequest(obj, r, 1, compatModel)
				second, _ := json.Marshal(obj)
				if string(first) != string(second) {
					t.Fatal("the shared profile chain appended normalized force twice")
				}
			})
		}
	}
}

func TestOfficialMergePreservesMessageCodeBoundaries(t *testing.T) {
	isolatedGatewayConfig(t)
	withRequestProfile(t, requestProfileOfficial)
	withSystemPrompts(t, "", "")
	neutral := strings.Replace(dshGUIContextOpening, dshGUIContextSentence, dshGUIContextNeutral, 1)
	for _, open := range []bool{false, true} {
		t.Run(fmt.Sprint(open), func(t *testing.T) {
			first := "```text\nClient code."
			if !open {
				first += "\n```"
			}
			obj := map[string]any{"messages": []any{map[string]any{"role": "system", "content": first}, map[string]any{"role": "system", "content": dshGUIContextOpening}, map[string]any{"role": "user", "content": "hi"}}}
			prepareUpstreamChatRequest(obj, httptest.NewRequest("POST", "/v1/chat/completions", nil), 1, compatModel)
			messages := obj["messages"].([]any)
			if open {
				if len(messages) != 3 || messages[0].(map[string]any)["content"] != first || messages[1].(map[string]any)["content"] != neutral {
					t.Fatal("merging created a new code context across independent system messages")
				}
			} else if len(messages) != 2 || messages[0].(map[string]any)["content"] != first+"\n\n"+neutral {
				t.Fatal("a safe leading instruction merge was skipped or changed content")
			}
		})
	}
}
