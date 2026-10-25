package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMultilineSSEHTTPPreservesToolsAndFees(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		for _, route := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
			for _, stream := range []bool{false, true} {
				t.Run(route+map[bool]string{false: "/aggregate", true: "/stream"}[stream]+map[fastPathModeT]string{fastPathOff: "/slow", fastPathOn: "/fast"}[mode], func(t *testing.T) {
					h := newCompatHarness(t, "cn")
					fastPathMode.Store(int32(mode))
					chunk := map[string]any{"model": compatModel, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_synthetic", "type": "function", "function": map[string]any{"name": "synthetic", "arguments": `{"n":9007199254740993,"text":"synthetic"}`}}}}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"credit": 3}}
					raw, _ := json.MarshalIndent(chunk, "", "  ")
					wire := "\xef\xbb\xbfevent: message\r\nid: synthetic\r\n"
					for _, line := range strings.Split(string(raw), "\n") {
						wire += "data: " + line + "\r\n"
					}
					wire += "\r\ndata: [DONE]\r\n\r\n"
					h.reply.Store(&compatReply{status: 200, body: wire})
					request := map[string]any{"model": compatModel, "stream": stream, "max_tokens": 16}
					if route == "/v1/responses" {
						request["input"] = "synthetic"
					} else {
						request["messages"] = []any{map[string]any{"role": "user", "content": "synthetic"}}
					}
					status, out := h.send(t, route, request, nil, 0)
					if status != http.StatusOK || !strings.Contains(string(out), "9007199254740993") || !strings.Contains(string(out), "call_synthetic") {
						t.Fatalf("multiline tool output changed: status=%d out=%s", status, out)
					}
					if !stream && !json.Valid(out) {
						t.Fatal("aggregate output is not valid JSON")
					}
					h.key.mu.Lock()
					u := h.key.usage
					h.key.mu.Unlock()
					if h.calls.Load() != 1 || u.Requests != 1 || u.Total != 3 || u.Unknown != 0 {
						t.Fatalf("framing changed billing: calls=%d usage=%+v", h.calls.Load(), u)
					}
				})
			}
		}
	}
}
