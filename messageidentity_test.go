package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestIdentityPrefixesAllTextPartsAndAssistant(t *testing.T) {
	original := blockedSystemPromptPrefix + ". Keep private-history-sentinel."
	normalized := replacementSystemPromptPrefix + ". Keep private-history-sentinel."
	for _, role := range []string{"system", "assistant", "user", "tool"} {
		t.Run(role, func(t *testing.T) {
			parts := []any{
				map[string]any{"type": "text", "text": "An ordinary first part."},
				map[string]any{"type": "text", "text": original},
				map[string]any{"type": "image_url", "text": original, "image_url": map[string]any{"url": "private-image-sentinel"}},
				map[string]any{"type": "text", "text": original},
				map[string]any{"type": "text", "text": "Example in the middle: " + original},
				nil,
			}
			calls := []any{map[string]any{"id": "private-call-sentinel", "type": "function", "function": map[string]any{"name": "save", "arguments": `{"text":"DeepSeek Harness"}`}}}
			message := map[string]any{"role": role, "content": parts, "reasoning_content": original, "tool_calls": calls}
			obj := map[string]any{"messages": []any{message}}
			logSystemPromptPrefixCompatibility(nil, obj)
			for _, index := range []int{1, 3} {
				want := original
				if role == "system" || role == "assistant" {
					want = normalized
				}
				if parts[index].(map[string]any)["text"] != want {
					t.Fatalf("part %d was not normalized within its role scope", index)
				}
			}
			if parts[0].(map[string]any)["text"] != "An ordinary first part." || parts[2].(map[string]any)["text"] != original || parts[4].(map[string]any)["text"] != "Example in the middle: "+original {
				t.Fatal("ordinary or nontext content changed")
			}
			call := message["tool_calls"].([]any)[0].(map[string]any)
			fn := call["function"].(map[string]any)
			if message["reasoning_content"] != original || call["id"] != "private-call-sentinel" || fn["name"] != "save" || fn["arguments"] != `{"text":"DeepSeek Harness"}` {
				t.Fatal("reasoning or tool arguments changed")
			}
			logSystemPromptPrefixCompatibility(nil, obj)
			if parts[1].(map[string]any)["text"] == normalized && strings.Count(parts[1].(map[string]any)["text"].(string), replacementSystemPromptPrefix) != 1 {
				t.Fatal("repeated normalization changed history again")
			}
		})
	}
}

func TestAssistantIdentityAndGUIHistoryThroughHTTP(t *testing.T) {
	identity := blockedSystemPromptPrefix + ". Preserve private-history-sentinel."
	wantIdentity := replacementSystemPromptPrefix + ". Preserve private-history-sentinel."
	// The first two sentences remain stable while the rest changes across DSH
	// versions. Neither an exact whole-paragraph hash nor a DSH system header
	// should be necessary to repair this history replay.
	gui := "Previous context: " + dshGUIContextOpening + " Keep custom GUI instructions."
	wantGUI := strings.Replace(gui, dshGUIContextSentence, dshGUIContextNeutral, 1)
	user := dshGUIContextOpening + " private-user-sentinel"
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			for _, blocks := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/blocks=%t", path, stream, blocks), func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					var content any = identity
					var systemContent any = "Answer briefly."
					wantSystem := "Answer briefly."
					want := wantIdentity
					if blocks {
						content = []any{map[string]any{"type": "text", "text": "Earlier part."}, map[string]any{"type": "text", "text": identity}}
						want = "Earlier part.\n" + wantIdentity
						systemContent = []any{map[string]any{"type": "text", "text": "Answer briefly."}, map[string]any{"type": "text", "text": identity}}
						wantSystem += "\n" + wantIdentity
					}
					messages := []any{
						map[string]any{"role": "system", "content": systemContent},
						map[string]any{"role": "user", "content": "Earlier question."},
						map[string]any{"role": "assistant", "content": content},
						map[string]any{"role": "user", "content": "Continue."},
						map[string]any{"role": "assistant", "content": gui},
						map[string]any{"role": "user", "content": user},
					}
					body := map[string]any{"model": compatModel, "stream": stream, "max_tokens": 64, "messages": messages}
					if path == "/v1/responses" {
						body["input"], body["store"] = messages, false
						delete(body, "messages")
					} else if path == "/v1/messages" {
						body["system"] = systemContent
						body["messages"] = messages[1:]
						if blocks {
							// Anthropic assistant text blocks are concatenated verbatim;
							// unlike chat text arrays, the converter adds no newline.
							want = "Earlier part." + wantIdentity
						}
					}
					status, _ := h.send(t, path, body, map[string]string{"session_id": "private-history-session"}, 0)
					if status != 200 {
						t.Fatalf("HTTP %d", status)
					}
					assertCompatMessages(t, h.take(t).body, []compatWantMessage{
						{Role: "system", Text: wantSystem}, {Role: "user", Text: "Earlier question."},
						{Role: "assistant", Text: want}, {Role: "user", Text: "Continue."},
						{Role: "assistant", Text: wantGUI}, {Role: "user", Text: user},
					})
					h.reply.Store(&compatReply{400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`})
					status, _ = h.send(t, path, body, map[string]string{"session_id": "private-history-session"}, 0)
					h.take(t)
					if status != 400 {
						t.Fatalf("rejection changed: HTTP %d", status)
					}
					if !strings.Contains(h.logs.String(), "dsh_assistant_identity_prefix") || !strings.Contains(h.logs.String(), "dsh_gui_context_identity") || h.calls.Load() != 2 {
						t.Fatal("missing rule diagnostics or unexpected upstream calls")
					}
					for _, private := range []string{identity, gui, "private-history-sentinel", "private-user-sentinel", "private-history-session"} {
						if strings.Contains(h.logs.String(), private) {
							t.Fatal("history was exposed in diagnostics")
						}
					}
				})
			}
		}
	}
}
