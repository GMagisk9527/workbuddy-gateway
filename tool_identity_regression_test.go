package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Tool-call identity is known before completion but arrives after its first
// arguments delta. The same upstream data must not yield an executable
// aggregate call and an unidentified streaming call.
func TestMessagesDelayedToolIdentityRegression(t *testing.T) {
	upstream := respChunk(`{"tool_calls":[{"index":0,"type":"function","function":{"arguments":"{\"path\":"}}]}`, "") +
		respChunk(`{"tool_calls":[{"index":0,"id":"call_delayed","function":{"name":"inspect","arguments":"\"example\"}"}}]}`, "tool_calls") +
		"data: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "aggregate", true: "stream"}[stream], func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			response := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
			account := &Account{Path: "offline.json"}
			if stream {
				streamMessagesResponse(recorder, request, response, "offline", 1, account, &profileCN, time.Now(), false)
			} else {
				writeMessagesAggregate(recorder, request, response, "offline", 1, account, &profileCN, time.Now(), false)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", recorder.Code, recorder.Body.String())
			}
			message := assembleAnthropicMessage(t, recorder.Body.Bytes(), stream)
			blocks, _ := message["content"].([]any)
			if len(blocks) != 1 {
				t.Fatalf("lost tool call: %#v", message)
			}
			block := blocks[0].(map[string]any)
			input, _ := json.Marshal(block["input"])
			if block["id"] != "call_delayed" || block["name"] != "inspect" || string(input) != `{"path":"example"}` {
				t.Fatalf("complete upstream tool identity/arguments lost: %#v", block)
			}
		})
	}
}

// Invalid call identity cannot be repaired by the client or by later tool
// result conversion. Input-object validation alone is insufficient.
func TestMessagesMissingToolIdentityIsNotComplete(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "aggregate", true: "stream"}[stream], func(t *testing.T) {
			upstream := respChunk(`{"tool_calls":[{"index":0,"type":"function","function":{"arguments":"{}"}}]}`, "tool_calls") + "data: [DONE]\n\n"
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			response := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
			account := &Account{Path: "offline.json"}
			if stream {
				streamMessagesResponse(recorder, request, response, "offline", 1, account, &profileCN, time.Now(), false)
				if !strings.Contains(recorder.Body.String(), "event: error\n") || strings.Contains(recorder.Body.String(), "event: message_stop\n") {
					t.Fatalf("unexecutable tool call reported as complete: %s", recorder.Body.String())
				}
			} else {
				writeMessagesAggregate(recorder, request, response, "offline", 1, account, &profileCN, time.Now(), false)
				if recorder.Code != http.StatusBadGateway {
					t.Fatalf("unexecutable tool call reported as HTTP %d: %s", recorder.Code, recorder.Body.String())
				}
			}
		})
	}
}

func TestResponsesDelayedToolIdentityRegression(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		for _, kind := range []string{"custom", "namespace"} {
			t.Run(fmt.Sprintf("mode=%d/kind=%s", mode, kind), func(t *testing.T) {
				isolatePolicies(t)
				resetStatsState(t)
				old := fastPathMode.Load()
				fastPathMode.Store(int32(mode))
				defer fastPathMode.Store(old)
				upstreamName, wantName, wantType, arguments := "apply_patch", "apply_patch", "custom_tool_call", `{"input":"patch"}`
				meta := responsesToolMeta{UpstreamName: upstreamName, Name: wantName, Kind: responsesToolCustom}
				if kind == "namespace" {
					upstreamName, wantName, wantType, arguments = "workspace__read", "read", "function_call", `{}`
					meta = responsesToolMeta{UpstreamName: upstreamName, Name: wantName, Namespace: "workspace", Kind: responsesToolNamespace}
				}
				r := httptest.NewRequest("POST", "/v1/responses", nil)
				r = withResponsesToolMap(r, map[string]responsesToolMeta{upstreamName: meta})
				args, _ := json.Marshal(arguments)
				body := respChunk(`{"tool_calls":[{"index":0,"id":"call_delayed","type":"function","function":{"arguments":""}}]}`, "") +
					respChunk(`{"tool_calls":[{"index":0,"function":{"name":"`+upstreamName+`","arguments":`+string(args)+`}}]}`, "tool_calls") + "data: [DONE]\n\n"
				w := httptest.NewRecorder()
				streamResponsesResponse(w, r, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, "test-model", 1, &Account{Path: "offline-test.json"}, &profileCN, time.Now())
				done := false
				for _, ev := range splitSSE(t, w.Body.String()) {
					if ev.name != "response.output_item.done" {
						continue
					}
					var event map[string]any
					if err := json.Unmarshal([]byte(ev.data), &event); err != nil {
						t.Fatal(err)
					}
					item := event["item"].(map[string]any)
					if item["type"] != wantType || item["name"] != wantName {
						t.Fatalf("tool identity was frozen before the name arrived: got=%v expected type=%s name=%s", item, wantType, wantName)
					}
					if kind == "custom" && item["input"] != "patch" {
						t.Fatalf("custom input lost: %v", item)
					}
					if kind == "namespace" && item["namespace"] != "workspace" {
						t.Fatalf("namespace lost: %v", item)
					}
					done = true
				}
				if !done {
					t.Fatal("missing output_item.done")
				}
			})
		}
	}
}
