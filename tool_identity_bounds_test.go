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

func toolIdentityFragment(index int, id, name, arguments string) string {
	encoded, _ := json.Marshal(map[string]any{"tool_calls": []any{map[string]any{
		"index": index, "id": id, "function": map[string]any{"name": name, "arguments": arguments},
	}}})
	return respChunk(string(encoded), "")
}

func TestMessagesDelayedToolIdentityPreservesBlockStartOrder(t *testing.T) {
	upstream := toolIdentityFragment(0, "", "", `{"first":`) +
		respChunk(`{"content":"between"}`, "") + toolIdentityFragment(1, "call_second", "second", `{}`) +
		toolIdentityFragment(0, "call_first", "first", `true}`) + respChunk(`{}`, "tool_calls") + "data: [DONE]\n\n"
	recorder := httptest.NewRecorder()
	streamMessagesResponse(recorder, httptest.NewRequest("POST", "/v1/messages", nil), &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, "test", 1, &Account{Path: "offline.json"}, &profileCN, time.Now(), false)
	if strings.Contains(recorder.Body.String(), "event: error") {
		t.Fatal(recorder.Body.String())
	}
	message := assembleAnthropicMessage(t, recorder.Body.Bytes(), true)
	blocks := message["content"].([]any)
	if len(blocks) != 3 || blocks[0].(map[string]any)["name"] != "first" || blocks[1].(map[string]any)["text"] != "between" || blocks[2].(map[string]any)["name"] != "second" {
		t.Fatalf("block order changed: %#v", blocks)
	}
	if blocks[0].(map[string]any)["input"].(map[string]any)["first"] != true {
		t.Fatal("delayed arguments lost or duplicated")
	}
}

func TestMessagesPendingToolOutputIsBoundedOnlyWhenStreaming(t *testing.T) {
	text, _ := json.Marshal(map[string]any{"content": strings.Repeat("x", maxPendingToolEventBytes+1)})
	upstream := toolIdentityFragment(0, "", "", `{}`) + respChunk(string(text), "") +
		toolIdentityFragment(0, "call_first", "first", "") + respChunk(`{}`, "tool_calls") + "data: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/v1/messages", nil)
			response := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
			if stream {
				streamMessagesResponse(recorder, request, response, "test", 1, &Account{Path: "offline.json"}, &profileCN, time.Now(), false)
				if !strings.Contains(recorder.Body.String(), "event: error") || strings.Contains(recorder.Body.String(), "event: message_stop") {
					t.Fatal("unbounded deferred SSE output reported success")
				}
			} else {
				writeMessagesAggregate(recorder, request, response, "test", 1, &Account{Path: "offline.json"}, &profileCN, time.Now(), false)
				if recorder.Code != 200 {
					t.Fatalf("aggregate incorrectly retained unused SSE queue: HTTP %d", recorder.Code)
				}
			}
		})
	}
}

func TestPendingToolArgumentsHaveGlobalLimit(t *testing.T) {
	for _, route := range []string{"messages", "responses"} {
		for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
			t.Run(fmt.Sprintf("%s/mode=%d", route, mode), func(t *testing.T) {
				old := fastPathMode.Load()
				fastPathMode.Store(int32(mode))
				defer fastPathMode.Store(old)
				fragment := `{"value":"` + strings.Repeat("x", maxPendingToolEventBytes/2+1) + `"}`
				upstream := toolIdentityFragment(0, "", "", fragment) + toolIdentityFragment(1, "", "", fragment) +
					toolIdentityFragment(0, "call_first", "first", "") + toolIdentityFragment(1, "call_second", "second", "") + respChunk(`{}`, "tool_calls") + "data: [DONE]\n\n"
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest("POST", "/v1/"+route, nil)
				response := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
				if route == "messages" {
					streamMessagesResponse(recorder, request, response, "test", 1, &Account{Path: "offline.json"}, &profileCN, time.Now(), false)
					if !strings.Contains(recorder.Body.String(), "event: error") || strings.Contains(recorder.Body.String(), "event: message_stop") {
						t.Fatal("pending argument limit did not abort stream")
					}
				} else {
					streamResponsesResponse(recorder, request, response, "test", 1, &Account{Path: "offline.json"}, &profileCN, time.Now())
					if !strings.Contains(recorder.Body.String(), "event: response.failed") || strings.Contains(recorder.Body.String(), "event: response.completed") {
						t.Fatal("unbounded pending arguments reported success")
					}
				}
			})
		}
	}
}

func TestResponsesDelayedIDsAcrossMixedPaths(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		for _, kind := range []string{"custom", "namespace"} {
			t.Run(fmt.Sprintf("mode=%d/kind=%s", mode, kind), func(t *testing.T) {
				old := fastPathMode.Load()
				fastPathMode.Store(int32(mode))
				defer fastPathMode.Store(old)
				name, arguments, wantType := "apply_patch", `{"input":"patch"}`, "custom_tool_call"
				meta := responsesToolMeta{UpstreamName: name, Name: name, Kind: responsesToolCustom}
				if kind == "namespace" {
					name, arguments, wantType = "workspace__read", `{"path":"sample"}`, "function_call"
					meta = responsesToolMeta{UpstreamName: name, Name: "read", Namespace: "workspace", Kind: responsesToolNamespace}
				}
				cut := len(arguments) / 2
				first := toolIdentityFragment(0, "", "", arguments[:cut])
				last := strings.ReplaceAll(toolIdentityFragment(0, "call_real", name, arguments[cut:]), `"function"`, `"\u0066unction"`)
				request := withResponsesToolMap(httptest.NewRequest("POST", "/v1/responses", nil), map[string]responsesToolMeta{name: meta})
				recorder := httptest.NewRecorder()
				streamResponsesResponse(recorder, request, &http.Response{Body: io.NopCloser(strings.NewReader(first + last + respChunk(`{}`, "tool_calls") + "data: [DONE]\n\n"))}, "test", 1, &Account{Path: "offline.json"}, &profileCN, time.Now())
				var added, done map[string]any
				var argumentDeltas strings.Builder
				for _, event := range splitSSE(t, recorder.Body.String()) {
					var payload map[string]any
					if json.Unmarshal([]byte(event.data), &payload) != nil {
						continue
					}
					switch event.name {
					case "response.output_item.added":
						added, _ = payload["item"].(map[string]any)
					case "response.output_item.done":
						done, _ = payload["item"].(map[string]any)
					case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
						argumentDeltas.WriteString(strOf(payload["delta"]))
					}
				}
				if added == nil || done == nil || added["call_id"] != "call_real" || done["call_id"] != "call_real" || added["type"] != wantType || done["type"] != wantType {
					t.Fatalf("tool identity changed between added/done: %#v %#v", added, done)
				}
				if kind == "custom" {
					if done["input"] != "patch" || argumentDeltas.String() != "patch" {
						t.Fatal("custom fragments were lost or duplicated")
					}
				} else if done["namespace"] != "workspace" || done["name"] != "read" || argumentDeltas.String() != arguments {
					t.Fatal("namespace or arguments changed")
				}
			})
		}
	}
}

func TestResponsesMissingToolIDIsNotComplete(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		old := fastPathMode.Load()
		fastPathMode.Store(int32(mode))
		recorder := httptest.NewRecorder()
		upstream := toolIdentityFragment(0, "", "inspect", `{}`) + respChunk(`{}`, "tool_calls") + "data: [DONE]\n\n"
		streamResponsesResponse(recorder, httptest.NewRequest("POST", "/v1/responses", nil), &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, "test", 1, &Account{Path: "offline.json"}, &profileCN, time.Now())
		fastPathMode.Store(old)
		if !strings.Contains(recorder.Body.String(), "event: response.failed") || strings.Contains(recorder.Body.String(), "event: response.completed") || strings.Contains(recorder.Body.String(), "event: response.output_item.done") {
			t.Fatalf("mode=%d reported missing ID as complete: %s", mode, recorder.Body.String())
		}
	}
}

func TestResponsesDelayedParallelIdentityUsesEmittedIndices(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		t.Run(fmt.Sprintf("mode=%d", mode), func(t *testing.T) {
			old := fastPathMode.Load()
			fastPathMode.Store(int32(mode))
			defer fastPathMode.Store(old)
			upstream := toolIdentityFragment(0, "", "", `{"input":"pa`) + respChunk(`{"content":"between"}`, "") +
				toolIdentityFragment(1, "call_namespace", "workspace__read", `{}`) +
				strings.ReplaceAll(toolIdentityFragment(0, "call_custom", "apply_patch", `tch"}`), `"function"`, `"\u0066unction"`) +
				respChunk(`{}`, "tool_calls") + "data: [DONE]\n\n"
			request := withResponsesToolMap(httptest.NewRequest("POST", "/v1/responses", nil), map[string]responsesToolMeta{
				"apply_patch":     {UpstreamName: "apply_patch", Name: "apply_patch", Kind: responsesToolCustom},
				"workspace__read": {UpstreamName: "workspace__read", Name: "read", Namespace: "workspace", Kind: responsesToolNamespace},
			})
			recorder := httptest.NewRecorder()
			streamResponsesResponse(recorder, request, &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}, "test", 1, &Account{Path: "offline.json"}, &profileCN, time.Now())
			indices := map[string]int{}
			calls := map[string]map[string]any{}
			var final []any
			for _, event := range splitSSE(t, recorder.Body.String()) {
				var payload map[string]any
				if json.Unmarshal([]byte(event.data), &payload) != nil {
					continue
				}
				if event.name == "response.output_item.added" {
					item := payload["item"].(map[string]any)
					index := int(numOr0(payload["output_index"]))
					if index != len(indices) {
						t.Fatalf("non-contiguous item starts: index=%d previous=%v", index, indices)
					}
					indices[strOf(item["id"])] = index
				}
				if id := strOf(payload["item_id"]); id != "" {
					index, exists := indices[id]
					if !exists || index != int(numOr0(payload["output_index"])) {
						t.Fatalf("delta/done refers to wrong index: %s", event.data)
					}
				}
				if event.name == "response.output_item.done" {
					item := payload["item"].(map[string]any)
					if indices[strOf(item["id"])] != int(numOr0(payload["output_index"])) {
						t.Fatal("item.done index changed")
					}
					if id := strOf(item["call_id"]); id != "" {
						calls[id] = item
					}
				}
				if event.name == "response.completed" {
					final = payload["response"].(map[string]any)["output"].([]any)
				}
			}
			if len(final) != 3 || len(calls) != 2 || calls["call_custom"]["type"] != "custom_tool_call" || calls["call_custom"]["input"] != "patch" || calls["call_namespace"]["namespace"] != "workspace" || calls["call_namespace"]["name"] != "read" {
				t.Fatalf("parallel tool identity was changed: %#v", calls)
			}
			for index, raw := range final {
				if indices[strOf(raw.(map[string]any)["id"])] != index {
					t.Fatal("completed output does not match emitted indices")
				}
			}
		})
	}
}

func TestInvalidToolIdentityRetainsKnownUsage(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		for _, stream := range []bool{false, true} {
			if route == "/v1/chat/completions" && stream {
				continue
			}
			t.Run(fmt.Sprintf("%s/stream=%t", route, stream), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				h.reply.Store(&compatReply{200, toolIdentityFragment(0, "call_bad", "", `{}`) + respChunk(`{}`, "tool_calls") +
					"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":9,\"credit\":0.25}}\n\ndata: [DONE]\n\n"})
				body := map[string]any{"model": compatModel, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "Synthetic request."}}}
				if route == "/v1/responses" {
					delete(body, "messages")
					body["input"] = "Synthetic request."
				}
				status, out := h.send(t, route, body, nil, 0)
				if !stream && status < 400 || stream && (strings.Contains(string(out), "event: message_stop") || strings.Contains(string(out), "event: response.completed")) {
					t.Fatalf("invalid call reported success: HTTP %d", status)
				}
				h.key.mu.Lock()
				usage := h.key.usage
				h.key.mu.Unlock()
				if usage.Total != 0.25 || usage.Input != 17 || usage.Output != 9 {
					t.Fatalf("reported consumption lost: %#v", usage)
				}
			})
		}
	}
}
