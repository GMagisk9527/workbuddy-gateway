package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This is DSH's MIT-licensed built-in paragraph, not a user conversation.
//
//go:embed testdata/dsh/gui_context.txt
var dshGUIContextFixture string

func TestDSHGUIContextExactTemplateAndBoundaries(t *testing.T) {
	digest := sha256.Sum256([]byte(dshGUIContextFixture))
	if len(dshGUIContextFixture) != 1006 || hex.EncodeToString(digest[:]) != "7496863f7f81a12cad97ff5c643339f8ddf344b6525c191c29ab086bec5a400a" {
		t.Fatal("fixture must match the documented shipped template")
	}
	if len(dshGUIContextOpening) != 221 || !strings.HasPrefix(dshGUIContextFixture, dshGUIContextOpening) {
		t.Fatal("known opening must match the first two shipped sentences")
	}
	neutral := dshGUIContextNeutral + dshGUIContextFixture[len(dshGUIContextSentence):]
	for _, tc := range []struct {
		name, input, want string
		count             int
	}{
		{"paragraph", dshGUIContextFixture, neutral, 1},
		{"instruction tails", "Keep instructions.\n\n" + dshGUIContextFixture + "\n\nKeep code.", "Keep instructions.\n\n" + neutral + "\n\nKeep code.", 1},
		{"windows newlines", "Keep.\r\n\r\n" + dshGUIContextFixture + "\r\n\r\nKeep.", "Keep.\r\n\r\n" + neutral + "\r\n\r\nKeep.", 1},
		{"changed functional tail", strings.Replace(dshGUIContextFixture, "The browser", "Our browser", 1), strings.Replace(neutral, "The browser", "Our browser", 1), 1},
		{"middle of text", "Context: " + dshGUIContextFixture, "Context: " + neutral, 1},
		{"appended instructions", dshGUIContextFixture + " Extra instructions.", neutral + " Extra instructions.", 1},
		{"unknown opening", strings.Replace(dshGUIContextFixture, "they mean this GUI", "they mean the GUI", 1), strings.Replace(dshGUIContextFixture, "they mean this GUI", "they mean the GUI", 1), 0},
		{"single sentence", dshGUIContextSentence, dshGUIContextSentence, 0},
		{"inline code", "``" + dshGUIContextFixture + "``", "``" + dshGUIContextFixture + "``", 0},
		{"indented code", "    " + dshGUIContextFixture, "    " + dshGUIContextFixture, 0},
		{"tabbed code", "\t" + dshGUIContextFixture, "\t" + dshGUIContextFixture, 0},
		{"two occurrences", dshGUIContextOpening + "\n" + dshGUIContextOpening, strings.ReplaceAll(dshGUIContextOpening, dshGUIContextSentence, dshGUIContextNeutral) + "\n" + strings.ReplaceAll(dshGUIContextOpening, dshGUIContextSentence, dshGUIContextNeutral), 2},
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
					if role == "system" || role == "assistant" {
						want = 1
					}
					if n := normalizeDSHGUIContexts(obj); n != want {
						t.Fatalf("changed=%d want=%d", n, want)
					}
					var text string
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

func TestDSHGUIContextBoundaries(t *testing.T) {
	neutral := testDSHNeutralGUI()
	for _, tc := range []struct {
		name, input, want string
		changes           int
	}{
		{"opening", testDSHGUI, neutral, 1},
		{"custom_tail", "Before. " + testDSHGUI + " Keep after.", "Before. " + neutral + " Keep after.", 1},
		{"unknown_opening", strings.Replace(testDSHGUI, "they mean", "they refer to", 1), strings.Replace(testDSHGUI, "they mean", "they refer to", 1), 0},
		{"single_sentence", dshGUIContextSentence, dshGUIContextSentence, 0},
		{"multiple", testDSHGUI + "\n" + testDSHGUI, neutral + "\n" + neutral, 2},
		{"crlf", "Before.\r\n" + testDSHGUI + "\r\nAfter.", "Before.\r\n" + neutral + "\r\nAfter.", 1},
		{"inline", "``" + testDSHGUI + "``", "``" + testDSHGUI + "``", 0},
		{"indented", "    " + testDSHGUI, "    " + testDSHGUI, 0},
		{"tabbed", "\t" + testDSHGUI, "\t" + testDSHGUI, 0},
		{"fenced", "```text\n" + testDSHGUI + "\n```", "```text\n" + testDSHGUI + "\n```", 0},
		{"tilde_fence", "~~~text\n" + testDSHGUI + "\n~~~", "~~~text\n" + testDSHGUI + "\n~~~", 0},
		{"after_fence", "```text\nexample\n```\n" + testDSHGUI, "```text\nexample\n```\n" + neutral, 1},
		{"single_tick_multiline", "Example `first line\n" + testDSHGUI + "\nlast line`.", "Example `first line\n" + testDSHGUI + "\nlast line`.", 0},
		{"double_tick_with_single_tick", "Example ``first line\n` " + testDSHGUI + "\nlast line``.", "Example ``first line\n` " + testDSHGUI + "\nlast line``.", 0},
		{"multiline_crlf", "Example `first\r\n" + testDSHGUI + "\r\nlast`.", "Example `first\r\n" + testDSHGUI + "\r\nlast`.", 0},
		{"normalize_after_close", "Example `first\n" + testDSHGUI + "\nlast` " + testDSHGUI, "Example `first\n" + testDSHGUI + "\nlast` " + neutral, 1},
		{"fence_like_line_inside_span", "Example ````first\n    ```\n" + testDSHGUI + "\nlast```` " + testDSHGUI, "Example ````first\n    ```\n" + testDSHGUI + "\nlast```` " + neutral, 1},
		{"real_fence_interrupts_span", "Example ````first\n```\n" + testDSHGUI + "\nlast```` " + testDSHGUI, "Example ````first\n```\n" + testDSHGUI + "\nlast```` " + testDSHGUI, 0},
		{"close_on_indented_line", "Example `first\n" + testDSHGUI + "\n    last`\n" + testDSHGUI, "Example `first\n" + testDSHGUI + "\n    last`\n" + neutral, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, n := normalizeDSHGUIContextText(tc.input)
			if got != tc.want || n != tc.changes {
				t.Fatalf("incorrect GUI/code boundary: changes=%d want=%d", n, tc.changes)
			}
			if again, n := normalizeDSHGUIContextText(got); again != got || n != 0 {
				t.Fatal("normalization is not idempotent")
			}
		})
	}
}

func TestDSHPromptScopeAndMetadataOnlyLogs(t *testing.T) {
	var debugLog, auditLog compatLog
	debugSinkMu.Lock()
	oldSink := debugSink
	debugSink = &debugJSONSink{writer: &debugLog}
	debugSinkMu.Unlock()
	oldWriter := log.Writer()
	log.SetOutput(&auditLog)
	t.Cleanup(func() {
		debugSinkMu.Lock()
		debugSink = oldSink
		debugSinkMu.Unlock()
		log.SetOutput(oldWriter)
	})
	prefix := testDSHPrefix + ". private-prompt-sentinel"
	gui := testDSHGUI + " private-gui-sentinel"
	newPrefix := blockedSystemPromptPrefixReplacement + ". private-prompt-sentinel"
	newGUI := testDSHNeutralGUI() + " private-gui-sentinel"
	makeMessage := func(role, identity, context string) map[string]any {
		return map[string]any{
			"role": role, "content": []any{
				map[string]any{"type": "image_url", "text": prefix, "image_url": map[string]any{"url": "private-image-sentinel"}},
				map[string]any{"type": "text", "text": "Ordinary instructions."},
				map[string]any{"type": "text", "text": identity},
				map[string]any{"type": "text", "text": context},
				nil,
			},
			"reasoning_content": prefix + gui,
			"tool_calls":        []any{map[string]any{"id": "call-test", "type": "function", "function": map[string]any{"name": "save", "arguments": prefix + gui}}},
		}
	}
	var messages, want []any
	for _, role := range []string{"system", "assistant", "user", "tool"} {
		messages = append(messages, makeMessage(role, prefix, gui))
		if role == "system" || role == "assistant" {
			want = append(want, makeMessage(role, newPrefix, newGUI))
		} else {
			want = append(want, makeMessage(role, prefix, gui))
		}
	}
	obj := map[string]any{"messages": messages}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	ensureDebugRequestContext(r, "dsh-compat-test", time.Now())
	sanitizeMessages(obj)
	logDSHPromptCompatibility(r, 1, obj)
	if !reflect.DeepEqual(messages, want) {
		t.Fatal("wrong role/block scope, lost instructions, or modified reasoning/tool arguments")
	}
	before, _ := json.Marshal(obj)
	logDSHPromptCompatibility(r, 1, obj)
	after, _ := json.Marshal(obj)
	if string(before) != string(after) {
		t.Fatal("second compatibility pass changed content")
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(debugLog.String())), &record); err != nil || record["event"] != "dsh_prompt_compatibility" {
		t.Fatal("missing metadata event or repeated normalization")
	}
	raw := debugLog.String() + auditLog.String()
	for _, secret := range []string{prefix, gui, "private-prompt-sentinel", "private-gui-sentinel", "private-image-sentinel"} {
		if strings.Contains(raw, secret) {
			t.Fatal("compatibility diagnostics leaked private content")
		}
	}
}
