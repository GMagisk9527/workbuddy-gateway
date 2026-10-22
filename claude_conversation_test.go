package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// These tests cover client-supplied session attribution, not upstream channel
// authorization. A stable session does not establish that 11128 is resolved.
func claudeCodeConversation(t *testing.T, caller, session, agent string, extra map[string]string, body map[string]any) *conversationHeaders {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+caller)
	if session != "" {
		req.Header.Set("x-claude-code-session-id", session)
	}
	if agent != "" {
		req.Header.Set("x-claude-code-agent-id", agent)
	}
	for name, value := range extra {
		req.Header.Set(name, value)
	}
	before := req.Header.Clone()
	h := prepareConversationContext(req, body)
	if !reflect.DeepEqual(req.Header, before) {
		t.Fatal("reading Claude Code session headers changed the inbound headers")
	}
	if again := prepareConversationContext(req, body); again != h {
		t.Fatal("the same HTTP request regenerated its conversation identity")
	}
	out := httptest.NewRequest(http.MethodPost, "/v2/chat/completions", nil).WithContext(req.Context())
	setConversationHeaders(out)
	assertConversationTuple(t, conversationTuple(out))
	return h
}

func TestClaudeCodeConversationSessionLifecycle(t *testing.T) {
	isolatedGatewayConfig(t)
	const session = "synthetic-private-claude-session"
	first := claudeCodeConversation(t, "caller-a", session, "", nil, nil)
	second := claudeCodeConversation(t, "caller-a", session, "", nil, nil)
	if first.ID != second.ID {
		t.Error("requests from one Claude Code session must keep the upstream conversation ID")
	}
	if first.RequestID == second.RequestID || first.MessageID == second.MessageID {
		t.Error("separate HTTP calls need fresh turn and message IDs")
	}
	if other := claudeCodeConversation(t, "caller-b", session, "", nil, nil); other.ID == first.ID {
		t.Error("the same Claude Code session must remain isolated across API keys")
	}
	if other := claudeCodeConversation(t, "caller-a", session+"-other", "", nil, nil); other.ID == first.ID {
		t.Error("different Claude Code sessions must not share a conversation ID")
	}
}

func TestClaudeCodeConversationAgentIsolation(t *testing.T) {
	isolatedGatewayConfig(t)
	const session = "synthetic-private-claude-session"
	main := claudeCodeConversation(t, "caller-a", session, "", nil, nil)
	first := claudeCodeConversation(t, "caller-a", session, "synthetic-agent-a", nil, nil)
	second := claudeCodeConversation(t, "caller-a", session, "synthetic-agent-a", nil, nil)
	if first.ID != second.ID {
		t.Error("one Claude Code subagent must retain its conversation across HTTP calls")
	}
	if first.RequestID == second.RequestID || first.MessageID == second.MessageID {
		t.Error("subagent turns must still have fresh request and message IDs")
	}
	identities := []*conversationHeaders{
		main,
		first,
		claudeCodeConversation(t, "caller-a", session, "synthetic-agent-b", nil, nil),
		claudeCodeConversation(t, "caller-a", session+"-other", "synthetic-agent-a", nil, nil),
		claudeCodeConversation(t, "caller-b", session, "synthetic-agent-a", nil, nil),
	}
	seen := map[string]bool{}
	for i, identity := range identities {
		if seen[identity.ID] {
			t.Errorf("conversation %d was shared by a different session, agent, main thread, or API caller", i)
		}
		seen[identity.ID] = true
	}
}

func TestClaudeCodeConversationAgentTupleBoundaries(t *testing.T) {
	isolatedGatewayConfig(t)
	first := claudeCodeConversation(t, "caller-a", "synthetic:session", "agent", nil, nil)
	second := claudeCodeConversation(t, "caller-a", "synthetic", "session:agent", nil, nil)
	if first.ID == second.ID {
		t.Error("different session/agent pairs collided after concatenation")
	}
	// Each incoming value is valid at the existing 512-byte limit. Combining
	// them must not make an otherwise stable subagent fall back to random IDs.
	session, agent := strings.Repeat("s", 512), strings.Repeat("a", 512)
	first = claudeCodeConversation(t, "caller-a", session, agent, nil, nil)
	second = claudeCodeConversation(t, "caller-a", session, agent, nil, nil)
	if first.ID != second.ID {
		t.Error("individually valid session and agent IDs lost their stable identity")
	}
}

func TestClaudeCodeConversationExistingHeadersTakePriority(t *testing.T) {
	isolatedGatewayConfig(t)
	for _, header := range []string{conversationIDHeader, "thread-id", "X-Session-ID", "session-id", "session_id"} {
		t.Run(header, func(t *testing.T) {
			extra := map[string]string{header: "synthetic-existing-session"}
			before := claudeCodeConversation(t, "caller-a", "", "", extra, nil)
			after := claudeCodeConversation(t, "caller-a", "synthetic-claude-session", "synthetic-agent", extra, nil)
			if before.ID != after.ID {
				t.Fatal("Claude Code headers changed an existing explicit session's conversation ID")
			}
		})
	}
	const requestID = "2c377d1a-836f-4de9-aefd-c9ca8e01ab5d"
	const messageID = "856296a51e064305ab603ed474de1815"
	h := claudeCodeConversation(t, "caller-a", "synthetic-claude-session", "synthetic-agent", map[string]string{
		conversationRequestIDHeader: requestID,
		conversationMessageIDHeader: messageID,
	}, nil)
	if h.RequestID != requestID || h.MessageID != messageID {
		t.Fatal("Claude Code session support overrode explicit turn or message IDs")
	}
}

func TestClaudeCodeConversationHeaderPrecedesBodySession(t *testing.T) {
	isolatedGatewayConfig(t)
	const session = "synthetic-private-claude-session"
	for _, body := range []map[string]any{
		{"session_id": "synthetic-body-session"},
		{"metadata": map[string]any{"session_id": "synthetic-metadata-session"}},
	} {
		before, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		headerOnly := claudeCodeConversation(t, "caller-a", session, "", nil, nil)
		withBody := claudeCodeConversation(t, "caller-a", session, "", nil, body)
		if withBody.ID != headerOnly.ID {
			t.Error("an explicit Claude Code session header must take priority over body metadata")
		}
		after, err := json.Marshal(body)
		if err != nil || string(before) != string(after) {
			t.Fatal("session attribution changed the request body")
		}
	}
}

func TestClaudeCodeConversationWithoutValidSessionStaysIndependent(t *testing.T) {
	isolatedGatewayConfig(t)
	for _, session := range []string{"", "   ", strings.Repeat("s", 513), "invalid\nsession", "invalid\x00session", string([]byte{0xff})} {
		first := claudeCodeConversation(t, "caller-a", session, "synthetic-agent", nil, nil)
		second := claudeCodeConversation(t, "caller-a", session, "synthetic-agent", nil, nil)
		if first.ID == second.ID || first.RequestID == second.RequestID || first.MessageID == second.MessageID {
			t.Error("an agent without a valid session must not create a shared conversation identity")
		}
	}
}

func TestClaudeCodeConversationInvalidAgentUsesMainSession(t *testing.T) {
	isolatedGatewayConfig(t)
	const session = "synthetic-private-claude-session"
	main := claudeCodeConversation(t, "caller-a", session, "", nil, nil)
	for _, agent := range []string{"   ", strings.Repeat("a", 513), "invalid\nagent", "invalid\x00agent", string([]byte{0xff})} {
		if got := claudeCodeConversation(t, "caller-a", session, agent, nil, nil); got.ID != main.ID {
			t.Error("an invalid agent ID must not discard the valid parent session")
		}
	}
}

func TestClaudeCodeConversationMessagesHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			withRequestProfile(t, requestProfileLegacy)
			h.reply.Store(&compatReply{200, conversationFixtureSSE(false)})
			const session = "synthetic-private-claude-session"
			const agent = "synthetic-private-claude-agent"
			const user = "synthetic-private-claude-user-text"
			body := map[string]any{
				"model": compatModel, "stream": stream, "max_tokens": 64,
				"system":   "Synthetic coding rules.",
				"messages": []any{map[string]any{"role": "user", "content": user}},
			}
			send := func(sessionID, agentID string, caller int) [3]string {
				headers := map[string]string{"x-claude-code-session-id": sessionID}
				if agentID != "" {
					headers["x-claude-code-agent-id"] = agentID
				}
				status, raw := h.send(t, "/v1/messages", body, headers, caller)
				if status != http.StatusOK {
					t.Fatalf("Messages HTTP status=%d", status)
				}
				if stream {
					for _, event := range []string{"event: message_start", `"type":"text_delta"`, "event: message_delta", "event: message_stop"} {
						if !strings.Contains(string(raw), event) {
							t.Fatalf("Messages stream missing %s", event)
						}
					}
				} else {
					var reply map[string]any
					if json.Unmarshal(raw, &reply) != nil || reply["type"] != "message" || reply["stop_reason"] != "end_turn" {
						t.Fatal("Messages aggregate was not a complete successful reply")
					}
				}
				captured := h.take(t)
				assertCompatMessages(t, captured.body, []compatWantMessage{
					{Role: "system", Text: "Synthetic coding rules."},
					{Role: "user", Text: user},
				})
				tuple := [3]string{captured.headers.Get(conversationIDHeader), captured.headers.Get(conversationRequestIDHeader), captured.headers.Get(conversationMessageIDHeader)}
				assertConversationTuple(t, tuple)
				if captured.headers.Get("x-claude-code-session-id") != "" || captured.headers.Get("x-claude-code-agent-id") != "" {
					t.Error("raw client attribution headers were forwarded to the non-Anthropic upstream")
				}
				return tuple
			}
			main, next := send(session, "", 0), send(session, "", 0)
			subagent, resumed := send(session, agent, 0), send(session, agent, 0)
			if main[0] != next[0] || subagent[0] != resumed[0] {
				t.Error("Claude Code session or agent attribution was lost through HTTP Messages conversion")
			}
			all := [][3]string{main, next, subagent, resumed, send(session, agent+"-other", 0), send(session+"-other", agent, 0), send(session, agent, 1)}
			seenTurns, seenMessages := map[string]bool{}, map[string]bool{}
			for _, tuple := range all {
				if seenTurns[tuple[1]] || seenMessages[tuple[2]] {
					t.Error("independent HTTP calls reused turn or message IDs")
				}
				seenTurns[tuple[1]], seenMessages[tuple[2]] = true, true
			}
			seenConversations := map[string]bool{}
			for _, tuple := range [][3]string{main, subagent, all[4], all[5], all[6]} {
				if seenConversations[tuple[0]] {
					t.Error("main thread, agents, sessions, or API callers shared a conversation")
				}
				seenConversations[tuple[0]] = true
			}
			for _, private := range []string{session, agent, user, h.keys[0], h.keys[1]} {
				if strings.Contains(h.logs.String(), private) {
					t.Fatal("Claude Code attribution or private request data leaked into logs")
				}
			}
		})
	}
}
