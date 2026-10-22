package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMessagesAggregatePreservesInterleavedBlocks(t *testing.T) {
	tool := func(index int, id, name, arguments string) string {
		data, err := json.Marshal(map[string]any{"tool_calls": []any{map[string]any{
			"index": index, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": arguments},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		return respChunk(string(data), "")
	}
	for _, tc := range []struct {
		name     string
		upstream string
		thinking bool
		want     []any
	}{
		{
			name: "text-tool-text",
			upstream: respChunk(`{"content":"before-tool"}`, "") + tool(0, "call_first", "inspect", "{}") +
				respChunk(`{"content":"after-tool"}`, ""),
			want: []any{
				map[string]any{"type": "text", "text": "before-tool"},
				map[string]any{"type": "tool_use", "id": "call_first", "name": "inspect", "input": map[string]any{}},
				map[string]any{"type": "text", "text": "after-tool"},
			},
		},
		{
			name: "thinking-text-tools-and-resumed-blocks", thinking: true,
			upstream: respChunk(`{"reasoning_content":"first thought"}`, "") + respChunk(`{"content":"before"}`, "") +
				tool(0, "call_first", "inspect", `{"path":`) + tool(0, "", "", `"a"}`) +
				respChunk(`{"reasoning_content":"second thought"}`, "") + respChunk(`{"content":"between"}`, "") +
				tool(1, "call_second", "check", "{}") + respChunk(`{"content":"after"}`, ""),
			want: []any{
				map[string]any{"type": "thinking", "thinking": "first thought", "signature": anthropicSignature("first thought")},
				map[string]any{"type": "text", "text": "before"},
				map[string]any{"type": "tool_use", "id": "call_first", "name": "inspect", "input": map[string]any{"path": "a"}},
				map[string]any{"type": "thinking", "thinking": "second thought", "signature": anthropicSignature("second thought")},
				map[string]any{"type": "text", "text": "between"},
				map[string]any{"type": "tool_use", "id": "call_second", "name": "check", "input": map[string]any{}},
				map[string]any{"type": "text", "text": "after"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := tc.upstream + respChunk(`{}`, "tool_calls") + "data: [DONE]\n\n"
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
			writeMessagesAggregate(rec, req, resp, "offline", 1, &Account{Path: "offline.json"}, &profileCN, time.Now(), tc.thinking)
			if rec.Code != http.StatusOK {
				t.Fatalf("HTTP status=%d: %s", rec.Code, rec.Body.String())
			}
			var result map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result["content"], tc.want) {
				t.Fatalf("received blocks must retain their contents and order:\n got: %#v\nwant: %#v", result["content"], tc.want)
			}
		})
	}
}
