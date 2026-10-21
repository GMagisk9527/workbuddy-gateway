package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

//go:embed testdata/dsh/requests.json
var dshRequestFixtures []byte

type compatLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *compatLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *compatLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type compatCapture struct {
	body    map[string]any
	headers http.Header
	err     error
}
type compatReply struct {
	status int
	body   string
}
type compatHarness struct {
	server   *httptest.Server
	captured chan compatCapture
	reply    atomic.Pointer[compatReply]
	calls    atomic.Int32
	logs     compatLog
	keys     [2]string
	key      *apiKey
	accounts []*Account
}

const compatModel = "deepseek-v4.1-flash"

// Both hops are real HTTP servers. No external network or production data.
func newCompatHarness(t *testing.T, site string) *compatHarness {
	t.Helper()
	isolatePolicies(t)
	resetStatsState(t)
	oldCN, oldINTL := profileCN, profileINTL
	oldAffinity, oldFast := currentAffinitySettings(), fastPathMode.Load()
	t.Cleanup(func() {
		profileCN, profileINTL = oldCN, oldINTL
		setAffinityConfig(false, affinityModeHybrid, 0, 0, 0)
		setAffinityConfig(oldAffinity.Enabled, oldAffinity.Mode, int(oldAffinity.TTL/time.Second), oldAffinity.Capacity, oldAffinity.PrefixHeadByte)
		fastPathMode.Store(oldFast)
	})
	enableAffinity(t, true, affinityModeHybrid)
	h := &compatHarness{captured: make(chan compatCapture, 128)}
	log.SetOutput(&h.logs)
	h.reply.Store(&compatReply{200, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"Synthetic reasoning.\"}}]}\n\n" + conversationFixtureSSE(true)})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		c := compatCapture{headers: r.Header.Clone()}
		c.err = json.NewDecoder(r.Body).Decode(&c.body)
		h.captured <- c
		reply := h.reply.Load()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	t.Cleanup(up.Close)
	profileCN.Base, profileCN.Origin = up.URL, up.URL
	profileINTL.Base, profileINTL.Origin = up.URL, up.URL
	cfg.HttpClient = up.Client()
	for i := 0; i < 2; i++ {
		acc := mkAccount(filepath.Join(t.TempDir(), fmt.Sprintf("synthetic-%d.json", i)))
		acc.Auth.Edition = site
		acc.Auth.Auth.AccessToken = fmt.Sprintf("synthetic-upstream-%d", i)
		acc.QuotaKnown, acc.QuotaRemaining = true, 1000
		acc.ModelStates = map[string]*modelRuntimeState{compatModel: {CostClass: modelCostFree}}
		if err := os.WriteFile(acc.Path, []byte("synthetic credential sentinel"), 0600); err != nil {
			t.Fatal(err)
		}
		h.accounts = append(h.accounts, acc)
	}
	withAccounts(t, h.accounts...)
	resetKeyWorld(t)
	if err := ledgerLoad(keyDataDir); err != nil {
		t.Fatal(err)
	}
	h.keys[0], h.key = mustCreateKey(t, keyCreateOptions{Name: "protocol-a", MaxConcurrent: 4})
	h.keys[1], _ = mustCreateKey(t, keyCreateOptions{Name: "protocol-b", MaxConcurrent: 4})
	h.server = httptest.NewServer(newAPIHandler())
	t.Cleanup(h.server.Close)
	return h
}
func (h *compatHarness) send(t *testing.T, path string, body map[string]any, headers map[string]string, caller int) (int, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", h.server.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.keys[caller])
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}
func (h *compatHarness) take(t *testing.T) compatCapture {
	t.Helper()
	select {
	case c := <-h.captured:
		if c.err != nil {
			t.Fatal(c.err)
		}
		for _, k := range []string{conversationIDHeader, conversationRequestIDHeader, conversationMessageIDHeader} {
			if c.headers.Get(k) == "" {
				t.Fatalf("missing upstream %s", k)
			}
		}
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("no upstream request")
		return compatCapture{}
	}
}

type compatWantMessage struct {
	Role       string `json:"role"`
	Text       string `json:"text"`
	Reasoning  string `json:"reasoning"`
	ToolCallID string `json:"tool_call_id"`
	Calls      []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"calls"`
}

func compatText(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	var texts []string
	parts, _ := content.([]any)
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if text, ok := part["text"].(string); ok {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}
func assertCompatMessages(t *testing.T, body map[string]any, want []compatWantMessage) {
	t.Helper()
	messages, _ := body["messages"].([]any)
	if len(messages) != len(want) {
		t.Fatalf("message count=%d want=%d", len(messages), len(want))
	}
	for i, expected := range want {
		msg, _ := messages[i].(map[string]any)
		if msg["role"] != expected.Role || compatText(msg["content"]) != expected.Text {
			t.Fatalf("message %d role/text changed", i)
		}
		if expected.Reasoning != "" && msg["reasoning_content"] != expected.Reasoning {
			t.Fatalf("message %d lost reasoning", i)
		}
		if expected.ToolCallID != "" && msg["tool_call_id"] != expected.ToolCallID {
			t.Fatalf("message %d lost output call_id", i)
		}
		calls, _ := msg["tool_calls"].([]any)
		if len(calls) != len(expected.Calls) {
			t.Fatalf("message %d tool count=%d want=%d", i, len(calls), len(expected.Calls))
		}
		for j, expectedCall := range expected.Calls {
			call := calls[j].(map[string]any)
			fn := call["function"].(map[string]any)
			if call["id"] != expectedCall.ID || fn["name"] != expectedCall.Name || fn["arguments"] != expectedCall.Arguments {
				t.Fatalf("message %d tool %d changed ID/name/arguments", i, j)
			}
		}
	}
}
func assertCompatSuccess(t *testing.T, path string, stream bool, out []byte) {
	t.Helper()
	s := string(out)
	if strings.Contains(s, "response.failed") || strings.Contains(s, "\"error\":{") {
		t.Fatalf("unexpected failure: %s", out)
	}
	if stream {
		end := "[DONE]"
		if path == "/v1/responses" {
			end = "event: response.completed"
		} else if path == "/v1/messages" {
			end = "event: message_stop"
		}
		if !strings.Contains(s, end) || !strings.Contains(s, "call-test") || !strings.Contains(s, "Synthetic reasoning.") {
			t.Fatalf("missing completion/tool/reasoning events: %s", out)
		}
	} else {
		var result map[string]any
		if json.Unmarshal(out, &result) != nil || !strings.Contains(s, "call-test") || !strings.Contains(s, "Synthetic reasoning.") {
			t.Fatalf("invalid aggregate: %s", out)
		}
	}
}

func TestDSHSourceFixturesThroughHTTP(t *testing.T) {
	for _, site := range []string{"cn", "intl"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", site, stream), func(t *testing.T) {
				h := newCompatHarness(t, site)
				var fixtures struct {
					Cases []struct {
						Name         string
						Path         string
						Headers      map[string]string
						Body         map[string]any
						WantMessages []compatWantMessage `json:"want_messages"`
					}
				}
				if err := json.Unmarshal(dshRequestFixtures, &fixtures); err != nil {
					t.Fatal(err)
				}
				var parentID, parentAccount string
				var first compatCapture
				for _, fixture := range fixtures.Cases {
					t.Run(fixture.Name, func(t *testing.T) {
						fixture.Body["stream"] = stream
						status, out := h.send(t, fixture.Path, fixture.Body, fixture.Headers, 0)
						if status != 200 {
							t.Fatalf("HTTP %d: %s", status, out)
						}
						assertCompatSuccess(t, fixture.Path, stream, out)
						c := h.take(t)
						assertCompatMessages(t, c.body, fixture.WantMessages)
						if c.body["reasoning_effort"] != "high" || c.body["stream"] != true {
							t.Fatal("thinking/upstream stream lost")
						}
						if fixture.Path == "/v1/responses" {
							for _, key := range []string{"prompt_cache_key", "service_tier", "parallel_tool_calls"} {
								if !reflect.DeepEqual(c.body[key], fixture.Body[key]) {
									t.Fatalf("lost %s", key)
								}
							}
							fn := c.body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
							if fn["strict"] != true || c.body["max_tokens"] != float64(512) {
								t.Fatal("strict/output budget lost")
							}
						}
						id, account := c.headers.Get(conversationIDHeader), c.headers.Get("Authorization")
						switch fixture.Headers["session_id"] {
						case "synthetic-dsh-parent":
							if parentID == "" {
								parentID, parentAccount, first = id, account, c
							} else if id != parentID || account != parentAccount {
								t.Fatal("continuation/compaction lost session or affinity")
							}
							if fixture.Name != "responses_single" && c.headers.Get(conversationRequestIDHeader) == first.headers.Get(conversationRequestIDHeader) {
								t.Fatal("independent calls reused turn ID")
							}
						case "synthetic-dsh-child":
							if id == parentID {
								t.Fatal("child task shares parent identity")
							}
						}
					})
				}
				fixture := fixtures.Cases[0]
				_, _ = h.send(t, fixture.Path, fixture.Body, fixture.Headers, 0)
				retry := h.take(t)
				if retry.headers.Get(conversationIDHeader) != parentID || retry.headers.Get("Authorization") != parentAccount || retry.headers.Get(conversationRequestIDHeader) == first.headers.Get(conversationRequestIDHeader) {
					t.Fatal("retry changed session/affinity or reused turn")
				}
				_, _ = h.send(t, fixture.Path, fixture.Body, fixture.Headers, 1)
				if h.take(t).headers.Get(conversationIDHeader) == parentID {
					t.Fatal("different API keys share identity")
				}
			})
		}
	}
}

func TestCodexResponsesAndMessagesThroughHTTP(t *testing.T) {
	h := newCompatHarness(t, "cn")
	tool := map[string]any{"type": "function", "name": "inspect", "strict": true, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	body := map[string]any{
		"model": compatModel, "store": false, "stream": true, "instructions": "Keep all source literals unchanged.",
		"input": []any{map[string]any{"role": "developer", "content": "Follow project conventions."}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Review."}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AA==", "detail": "low"}}}},
		"tools": []any{tool}, "tool_choice": map[string]any{"type": "function", "name": "inspect"}, "reasoning": map[string]any{"effort": "high"}, "max_output_tokens": 768,
		"temperature": 0.2, "top_p": 0.8, "parallel_tool_calls": false, "service_tier": "auto", "prompt_cache_key": "synthetic-cache", "prompt_cache_retention": "in_memory", "safety_identifier": "synthetic-safety", "user": "synthetic-user",
		"text": map[string]any{"verbosity": "low", "format": map[string]any{"type": "json_schema", "name": "answer", "strict": true, "schema": map[string]any{"type": "object"}}},
	}
	headers := map[string]string{"thread-id": "synthetic-codex-parent", "session-id": "synthetic-shared-process"}
	var firstID string
	for _, stream := range []bool{true, false} {
		body["stream"] = stream
		status, out := h.send(t, "/v1/responses", body, headers, 0)
		if status != 200 {
			t.Fatalf("HTTP %d: %s", status, out)
		}
		assertCompatSuccess(t, "/v1/responses", stream, out)
		c := h.take(t)
		if firstID == "" {
			firstID = c.headers.Get(conversationIDHeader)
		} else if firstID != c.headers.Get(conversationIDHeader) {
			t.Fatal("same Codex thread changed identity")
		}
		for _, key := range []string{"temperature", "top_p", "parallel_tool_calls", "service_tier", "prompt_cache_key", "prompt_cache_retention", "safety_identifier", "user"} {
			expected, _ := json.Marshal(body[key])
			got, _ := json.Marshal(c.body[key])
			if !bytes.Equal(expected, got) {
				t.Fatalf("parameter %s lost", key)
			}
		}
		if c.body["tool_choice"] != "required" || c.body["max_tokens"] != float64(768) || c.body["verbosity"] != "low" {
			t.Fatal("tool/output/text parameters lost")
		}
		format := c.body["response_format"].(map[string]any)["json_schema"].(map[string]any)
		if format["strict"] != true || format["name"] != "answer" {
			t.Fatal("structured output schema lost")
		}
		messages := c.body["messages"].([]any)
		if messages[1].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["content"] != "Follow project conventions." {
			t.Fatal("developer instruction lost")
		}
		image := messages[2].(map[string]any)["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)
		if image["detail"] != "low" {
			t.Fatal("image detail lost")
		}
	}
	headers["thread-id"] = "synthetic-codex-child"
	_, _ = h.send(t, "/v1/responses", body, headers, 0)
	if h.take(t).headers.Get(conversationIDHeader) == firstID {
		t.Fatal("Codex child shares parent via process session")
	}
	// Replay the gateway's own function and reasoning output, as Codex does.
	headers["thread-id"] = "synthetic-codex-parent"
	status, out := h.send(t, "/v1/responses", body, headers, 0)
	h.take(t)
	var response map[string]any
	if status != 200 || json.Unmarshal(out, &response) != nil {
		t.Fatalf("replay setup failed: %s", out)
	}
	input := append(body["input"].([]any), response["output"].([]any)...)
	body["input"] = append(input, map[string]any{"type": "function_call_output", "call_id": "call-test", "output": "synthetic tool result"})
	status, out = h.send(t, "/v1/responses", body, headers, 0)
	if status != 200 {
		t.Fatalf("replay HTTP %d: %s", status, out)
	}
	messages := h.take(t).body["messages"].([]any)
	if messages[3].(map[string]any)["reasoning_content"] != "Synthetic reasoning." || messages[4].(map[string]any)["tool_call_id"] != "call-test" {
		t.Fatal("round trip lost reasoning/tool relationship")
	}
	for _, stream := range []bool{true, false} {
		anthropic := map[string]any{"model": compatModel, "max_tokens": 512, "stream": stream, "system": "Keep source unchanged.", "thinking": map[string]any{"type": "enabled", "effort": "high"},
			"tools":    []any{map[string]any{"name": "inspect", "input_schema": map[string]any{"type": "object"}}},
			"messages": []any{map[string]any{"role": "user", "content": "Review."}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "Inspect source.", "signature": "synthetic"}, map[string]any{"type": "tool_use", "id": "call-anthropic", "name": "inspect", "input": map[string]any{"path": "sample.go"}}}}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-anthropic", "content": "synthetic result"}}}}}
		status, out := h.send(t, "/v1/messages", anthropic, map[string]string{"X-Session-ID": "synthetic-anthropic"}, 0)
		if status != 200 {
			t.Fatalf("Messages HTTP %d: %s", status, out)
		}
		assertCompatSuccess(t, "/v1/messages", stream, out)
		messages := h.take(t).body["messages"].([]any)
		if messages[0].(map[string]any)["content"] != "Keep source unchanged." || messages[len(messages)-1].(map[string]any)["tool_call_id"] != "call-anthropic" {
			t.Fatal("Messages system/tool relationship lost")
		}
	}
}

func TestUnsupportedResponsesStateRejectedBeforeUpstream(t *testing.T) {
	h := newCompatHarness(t, "cn")
	for _, fields := range []map[string]any{
		{"previous_response_id": "resp_synthetic"}, {"conversation": map[string]any{"id": "conv_synthetic"}}, {"store": true},
		{"input": []any{map[string]any{"type": "item_reference", "id": "msg_synthetic"}}},
		{"input": []any{map[string]any{"type": "compaction", "encrypted_content": "synthetic-opaque"}}},
		{"input": []any{map[string]any{"type": "reasoning", "encrypted_content": "synthetic-opaque", "summary": []any{}}}},
		{"input": []any{map[string]any{"type": "function_call", "name": "inspect", "arguments": "{}"}}},
		{"input": []any{map[string]any{"type": "function_call_output", "output": "result"}}},
		{"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "file_id": "file_synthetic"}}}}},
	} {
		body := map[string]any{"model": compatModel, "input": "Review.", "store": false}
		for k, v := range fields {
			body[k] = v
		}
		if status, _ := h.send(t, "/v1/responses", body, nil, 0); status != 400 {
			t.Fatalf("unsupported state should be 400, got %d", status)
		}
	}
	if h.calls.Load() != 0 {
		t.Fatal("unsupported state reached paid upstream")
	}
}

func TestPrefixAffinityAndUUIDSessionIsolationThroughHTTP(t *testing.T) {
	h := newCompatHarness(t, "cn")
	h.reply.Store(&compatReply{200, conversationFixtureSSE(false)})
	body := map[string]any{"model": compatModel, "stream": false, "messages": []any{
		map[string]any{"role": "system", "content": strings.Repeat("Synthetic project conventions. ", 120)},
		map[string]any{"role": "user", "content": "Synthetic source review."},
	}}
	var first compatCapture
	for i := 0; i < 2; i++ {
		status, out := h.send(t, "/v1/chat/completions", body, nil, 0)
		if status != 200 {
			t.Fatalf("HTTP %d: %s", status, out)
		}
		c := h.take(t)
		if i == 0 {
			first = c
			continue
		}
		if c.headers.Get("Authorization") != first.headers.Get("Authorization") {
			t.Fatal("unchanged prefix lost account affinity")
		}
		if c.headers.Get(conversationIDHeader) == first.headers.Get(conversationIDHeader) {
			t.Fatal("prefix matching must not merge sessions without an explicit ID")
		}
	}
	headers := map[string]string{"session_id": "48b6ae04-d431-4bfa-913b-4937225fa6e3"}
	for caller := 0; caller < 2; caller++ {
		_, _ = h.send(t, "/v1/chat/completions", body, headers, caller)
		c := h.take(t)
		if caller == 0 {
			first = c
			continue
		}
		if c.headers.Get(conversationIDHeader) == first.headers.Get(conversationIDHeader) {
			t.Fatal("valid UUID sessions must also be isolated by API key")
		}
	}
	if affinityMetrics.prefixKey.Load() == 0 {
		t.Fatal("prefix path was not exercised")
	}
}

func Test11128HonestErrorsDiagnosticsAndLedgerThroughHTTP(t *testing.T) {
	for _, site := range []string{"cn", "intl"} {
		t.Run(site, func(t *testing.T) {
			h := newCompatHarness(t, site)
			message := "Illegal API invocation from an unapproved channel"
			rejection := fmt.Sprintf("{\"code\":11128,\"msg\":%q,\"requestId\":\"ac59b5d6-a8cf-40d4-ae15-a8491c83b9b2\",\"private\":\"synthetic-secret-echo\",\"displayTips\":\"channel verification\"}", message)
			totalCalls := int32(0)
			for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
				fastPathMode.Store(int32(mode))
				for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
					for _, stream := range []bool{false, true} {
						for _, upstreamStatus := range []int{400, 401, 429, 200} {
							body := map[string]any{"model": compatModel, "stream": stream, "max_tokens": 64, "messages": []any{map[string]any{"role": "developer", "content": "synthetic-private-prompt"}, map[string]any{"role": "user", "content": "synthetic-private-user"}}}
							if path == "/v1/responses" {
								body["input"], body["store"] = body["messages"], false
								delete(body, "messages")
							} else if path == "/v1/messages" {
								body["system"] = "synthetic-private-prompt"
								body["messages"] = []any{map[string]any{"role": "user", "content": "synthetic-private-user"}}
							}
							reply := rejection
							if upstreamStatus == 200 {
								reply = "data: " + rejection + "\n\ndata: [DONE]\n\n"
							}
							h.reply.Store(&compatReply{upstreamStatus, reply})
							status, out := h.send(t, path, body, map[string]string{"session_id": "synthetic-private-session"}, 0)
							h.take(t)
							totalCalls++
							expectedStatus := upstreamStatus
							if upstreamStatus == 200 && !stream {
								expectedStatus = 502
							}
							if status != expectedStatus || !bytes.Contains(out, []byte("11128")) || !bytes.Contains(out, []byte(message)) {
								t.Fatalf("%s stream=%t upstream=%d got HTTP %d: %s", path, stream, upstreamStatus, status, out)
							}
							for _, falseSuccess := range []string{"[DONE]", "event: response.completed", "event: message_stop", "\"status\":\"completed\""} {
								if bytes.Contains(out, []byte(falseSuccess)) {
									t.Fatalf("rejection pretends success: %s", out)
								}
							}
							if h.calls.Load() != totalCalls {
								t.Fatal("11128 retried/switched accounts")
							}
						}
					}
				}
			}
			for _, acc := range h.accounts {
				data, err := os.ReadFile(acc.Path)
				if err != nil || string(data) != "synthetic credential sentinel" || acc.Disabled {
					t.Fatal("11128 mutated/disabled/deleted credentials")
				}
			}
			logs := h.logs.String()
			for _, expected := range []string{"protocol=responses", "protocol=chat_completions", "protocol=messages", "site=" + site, "http_status=200", "business_code=11128", "category=channel_verification_failed", "has_developer=true", "session_supplied=true", "session_id_normalized", "developer_to_system", "upstream_request_id=ac59b5d6-a8cf-40d4-ae15-a8491c83b9b2"} {
				if !strings.Contains(logs, expected) {
					t.Fatalf("missing diagnostic %q", expected)
				}
			}
			for _, secret := range []string{"synthetic-private-prompt", "synthetic-private-user", "synthetic-private-session", "synthetic-secret-echo", h.keys[0], "synthetic-upstream-"} {
				if strings.Contains(logs, secret) {
					t.Fatalf("diagnostic leaked %q", secret)
				}
			}
			h.key.mu.Lock()
			before := h.key.usage.Total
			h.key.mu.Unlock()
			if before != 0 {
				t.Fatal("rejected requests invented credit charge")
			}
			h.reply.Store(&compatReply{200, conversationFixtureSSE(false)})
			body := map[string]any{"model": compatModel, "store": false, "input": "synthetic successful request"}
			if status, out := h.send(t, "/v1/responses", body, nil, 0); status != 200 {
				t.Fatalf("settlement HTTP %d: %s", status, out)
			}
			h.take(t)
			h.key.mu.Lock()
			usage := h.key.usage
			h.key.mu.Unlock()
			if usage.Total != 0.1 {
				t.Fatalf("wrong credit after success: %v", usage.Total)
			}
			reloadKeysAndLedger(t, keyDataDir)
			replayed := keyByID(h.key.ID)
			if replayed == nil {
				t.Fatal("key lost during WAL replay")
			}
			replayed.mu.Lock()
			got := replayed.usage
			replayed.mu.Unlock()
			if !reflect.DeepEqual(usage, got) {
				t.Fatalf("WAL replay changed usage: %+v -> %+v", usage, got)
			}
		})
	}
}

// Only the exact DSH identity prefix is normalized; the following user instruction is preserved.
func TestDSHIdentityPrefixCompatibilityThroughHTTP(t *testing.T) {
	const identity = "You are an AI agent powered by DeepSeek Harness.\nKeep private-code-sentinel unchanged."
	const normalized = "You are an AI agent powered by a large language model.\nKeep private-code-sentinel unchanged."
	const userText = "You are an AI agent powered by DeepSeek Harness. private-user-sentinel"
	const session = "private-session-sentinel"
	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, blocks := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/blocks=%t", path, blocks), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				var debugLog compatLog
				debugSinkMu.Lock()
				oldSink := debugSink
				debugSink = &debugJSONSink{writer: &debugLog}
				debugSinkMu.Unlock()
				t.Cleanup(func() {
					debugSinkMu.Lock()
					debugSink = oldSink
					debugSinkMu.Unlock()
				})
				var content any = identity
				if blocks {
					content = []any{map[string]any{"type": "text", "text": identity}}
				}
				body := map[string]any{"model": compatModel, "stream": false, "max_tokens": 64,
					"messages": []any{map[string]any{"role": "developer", "content": content}, map[string]any{"role": "user", "content": userText}}}
				if path == "/v1/responses" {
					body["input"], body["store"] = body["messages"], false
					delete(body, "messages")
				} else if path == "/v1/messages" {
					body["system"] = content
					body["messages"] = []any{map[string]any{"role": "user", "content": userText}}
				}
				for _, rejected := range []bool{false, true} {
					if rejected {
						h.reply.Store(&compatReply{400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`})
					}
					status, out := h.send(t, path, body, map[string]string{"session_id": session}, 0)
					assertCompatMessages(t, h.take(t).body, []compatWantMessage{{Role: "system", Text: normalized}, {Role: "user", Text: userText}})
					if rejected {
						if status != 400 || !bytes.Contains(out, []byte("11128")) || !bytes.Contains(out, []byte("unapproved channel")) {
							t.Fatalf("channel rejection changed: HTTP %d %s", status, out)
						}
					} else if status != 200 {
						t.Fatalf("success HTTP %d: %s", status, out)
					}
				}
				if h.calls.Load() != 2 {
					t.Fatal("channel rejection retried")
				}
				if !strings.Contains(h.logs.String(), "dsh_system_identity_prefix") {
					t.Fatal("prefix compatibility rule was not diagnosed")
				}
				if !strings.Contains(debugLog.String(), "request_body_read_completed") {
					t.Fatal("debug logging was not exercised")
				}
				logs := debugLog.String() + h.logs.String()
				for _, secret := range []string{identity, normalized, "private-code-sentinel", "private-user-sentinel", session, h.keys[0], "synthetic-upstream-"} {
					if strings.Contains(logs, secret) {
						t.Fatalf("diagnostics leaked %q", secret)
					}
				}
			})
		}
	}
}

func Test11128ClassificationAndRulesIdempotence(t *testing.T) {
	for _, sample := range []struct{ msg, tips, category string }{
		{"first message is not system prompt", "channel verification", "message_structure"},
		{"developer role is not supported", "", "unsupported_role"},
		{"conversation_id is required", "", "conversation_metadata_missing"},
		{"Illegal API invocation from an unapproved channel", "", "channel_verification_failed"},
		{"prompt blocked by policy", "channel verification", "prompt_policy_rejected"},
		{"invocation rejected", "", "invocation_rejected_unknown"},
	} {
		for _, code := range []any{11128, "11128"} {
			raw, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": sample.msg, "displayTips": sample.tips, "request_id": "sensitive-request-value"}})
			got := classifyUpstreamRejection(raw)
			if got.Category != sample.category || got.Code != "11128" || got.RequestID != "present_redacted" {
				t.Fatalf("classification: %+v", got)
			}
		}
	}
	raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "code 11128"}}}})
	if detectUpstreamStreamRejection(raw) != nil {
		t.Fatal("ordinary output classified as rejection")
	}
	isolatedGatewayConfig(t)
	setSystemPromptConfig("", "Configured synthetic instruction.")
	for _, content := range []any{"Original developer instruction.", []any{map[string]any{"type": "text", "text": "Original developer instruction."}}} {
		body := map[string]any{"messages": []any{map[string]any{"role": "developer", "content": content}, map[string]any{"role": "user", "content": "User source unchanged."}}}
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		prepareConversationContext(r, body)
		sanitizeMessages(body)
		prepareSystemPromptForUpstream(body, r, 1, "synthetic")
		repairToolMessageSequence(body)
		first, _ := json.Marshal(body)
		sanitizeMessages(body)
		prepareSystemPromptForUpstream(body, r, 1, "synthetic")
		repairToolMessageSequence(body)
		second, _ := json.Marshal(body)
		if !bytes.Equal(first, second) {
			t.Fatal("compatibility rules are not idempotent")
		}
	}
}
