package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// This is DSH's MIT-licensed built-in paragraph, not a user conversation.
//
//go:embed testdata/dsh/gui_context.txt
var dshGUIContextFixture string

func TestDSHGUIContextExactTemplateAndBoundaries(t *testing.T) {
	digest := sha256.Sum256([]byte(dshGUIContextFixture))
	if len(dshGUIContextFixture) != dshGUIContextBytes || hex.EncodeToString(digest[:]) != dshGUIContextSHA256 {
		t.Fatal("fixture must match the documented shipped template")
	}
	neutral := dshGUIContextNeutral + dshGUIContextFixture[len(dshGUIContextSentence):]
	for _, tc := range []struct {
		name, input, want string
		count             int
	}{
		{"paragraph", dshGUIContextFixture, neutral, 1},
		{"instruction tails", "Keep instructions.\n\n" + dshGUIContextFixture + "\n\nKeep code.", "Keep instructions.\n\n" + neutral + "\n\nKeep code.", 1},
		{"windows newlines", "Keep.\r\n\r\n" + dshGUIContextFixture + "\r\n\r\nKeep.", "Keep.\r\n\r\n" + neutral + "\r\n\r\nKeep.", 1},
		{"unknown template", strings.Replace(dshGUIContextFixture, "The browser", "Our browser", 1), strings.Replace(dshGUIContextFixture, "The browser", "Our browser", 1), 0},
		{"inline quote", "Example: " + dshGUIContextFixture, "Example: " + dshGUIContextFixture, 0},
		{"appended instructions", dshGUIContextFixture + " Extra instructions.", dshGUIContextFixture + " Extra instructions.", 0},
		{"backtick code", "```text\n\n" + dshGUIContextFixture + "\n\n```", "```text\n\n" + dshGUIContextFixture + "\n\n```", 0},
		{"tilde code", "~~~text\n\n" + dshGUIContextFixture + "\n\n~~~", "~~~text\n\n" + dshGUIContextFixture + "\n\n~~~", 0},
		{"after code", "```text\nexample\n```\n\n" + dshGUIContextFixture, "```text\nexample\n```\n\n" + neutral, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := normalizeDSHGUIContextText(tc.input)
			if got != tc.want || n != tc.count {
				t.Fatalf("normalized content differs from exact expected result; changes=%d want=%d", n, tc.count)
			}
			if again, n := normalizeDSHGUIContextText(got); n != 0 || again != got {
				t.Fatal("normalization is not idempotent")
			}
		})
	}
}

func TestDSHGUIContextRoleAndProvenanceScope(t *testing.T) {
	identity := blockedSystemPromptPrefix + ".\n\n"
	for _, role := range []string{"system", "user", "assistant", "tool"} {
		for _, identified := range []bool{false, true} {
			for _, blocks := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/identified=%t/blocks=%t", role, identified, blocks), func(t *testing.T) {
					prefix := "Ordinary instructions.\n\n"
					if identified {
						prefix = identity
					}
					var content any = prefix + dshGUIContextFixture
					if blocks {
						content = []any{map[string]any{"type": "text", "text": prefix}, map[string]any{"type": "text", "text": dshGUIContextFixture}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "private-image-sentinel"}}}
					}
					message := map[string]any{"role": role, "content": content}
					obj := map[string]any{"messages": []any{message}}
					want := 0
					if role == "system" && identified {
						want = 1
					}
					if n := normalizeDSHGUIContexts(obj); n != want {
						t.Fatalf("changed=%d want=%d", n, want)
					}
					text := prefix + dshGUIContextFixture
					if blocks {
						parts := message["content"].([]any)
						text = parts[0].(map[string]any)["text"].(string) + parts[1].(map[string]any)["text"].(string)
						if parts[2].(map[string]any)["image_url"].(map[string]any)["url"] != "private-image-sentinel" {
							t.Fatal("nontext block changed")
						}
					} else {
						text = message["content"].(string)
					}
					if want > 0 {
						if text != prefix+dshGUIContextNeutral+dshGUIContextFixture[len(dshGUIContextSentence):] {
							t.Fatal("functional GUI instructions changed")
						}
					} else if text != prefix+dshGUIContextFixture {
						t.Fatal("unrelated content changed")
					}
					if n := normalizeDSHGUIContexts(obj); n != 0 {
						t.Fatal("second pass changed content")
					}
				})
			}
		}
	}
}

func TestDSHGUIContextCompatibilityThroughHTTP(t *testing.T) {
	system := blockedSystemPromptPrefix + ".\n\nKeep private-instruction-sentinel.\n\n" + dshGUIContextFixture + "\n\nKeep private-code-sentinel."
	want := strings.Replace(system, blockedSystemPromptPrefix, replacementSystemPromptPrefix, 1)
	want = strings.Replace(want, dshGUIContextSentence, dshGUIContextNeutral, 1)
	user := dshGUIContextFixture + " private-user-sentinel"
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, blocks := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/blocks=%t/stream=%t", path, blocks, stream), func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					var content any = system
					if blocks {
						content = []any{map[string]any{"type": "text", "text": system}}
					}
					body := map[string]any{"model": compatModel, "stream": stream, "max_tokens": 64, "messages": []any{map[string]any{"role": "developer", "content": content}, map[string]any{"role": "user", "content": user}}}
					if path == "/v1/responses" {
						body["input"], body["store"] = body["messages"], false
						delete(body, "messages")
					} else if path == "/v1/messages" {
						body["system"] = content
						body["messages"] = []any{map[string]any{"role": "user", "content": user}}
					}
					for _, rejected := range []bool{false, true} {
						if rejected {
							h.reply.Store(&compatReply{400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`})
						}
						status, out := h.send(t, path, body, map[string]string{"session_id": "private-session-sentinel"}, 0)
						assertCompatMessages(t, h.take(t).body, []compatWantMessage{{Role: "system", Text: want}, {Role: "user", Text: user}})
						if rejected {
							if status != 400 || !bytes.Contains(out, []byte("11128")) {
								t.Fatalf("upstream rejection was not preserved: HTTP %d", status)
							}
						} else if status != 200 {
							t.Fatalf("success HTTP %d", status)
						}
					}
					if h.calls.Load() != 2 || !strings.Contains(h.logs.String(), "dsh_gui_context_identity") {
						t.Fatal("missing rule diagnostic or unexpected retries")
					}
					for _, secret := range []string{dshGUIContextFixture, "private-instruction-sentinel", "private-code-sentinel", "private-user-sentinel", "private-session-sentinel", h.keys[0]} {
						if strings.Contains(h.logs.String(), secret) {
							t.Fatal("diagnostics retained private content")
						}
					}
				})
			}
		}
	}
}
