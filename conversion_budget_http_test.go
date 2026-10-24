package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestMessagesReferenceConversionLimitStopsBeforeUpstream(t *testing.T) {
	h := newCompatHarness(t, "cn")
	body := syntheticToolReferenceRequest()
	body["model"] = compatModel
	tools, refs := []any{}, []any{}
	description := strings.Repeat("<", 64<<10)
	for i := 0; i < 48; i++ {
		name := fmt.Sprintf("Inspect%d", i)
		tools = append(tools, map[string]any{"name": name, "description": description, "input_schema": map[string]any{"type": "object"}})
		refs = append(refs, map[string]any{"type": "tool_reference", "tool_name": name})
	}
	body["tools"] = tools
	body["messages"].([]any)[1].(map[string]any)["content"].([]any)[0].(map[string]any)["content"] = refs
	status, raw := h.send(t, "/v1/messages", body, nil, 0)
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(string(raw), `"type":"request_too_large"`) || h.calls.Load() != 0 {
		t.Fatalf("oversized conversion must be rejected before upstream: HTTP %d calls=%d response=%s", status, h.calls.Load(), raw)
	}
}

func TestMessagesPreparedRequestLimitStopsBeforeSerialization(t *testing.T) {
	h := newCompatHarness(t, "cn")
	// A configured force prompt can make the final request exceed the wire
	// budget even when the original client request is tiny. Keep the live
	// object small; HTML escaping accounts for the large serialized form.
	withSystemPrompts(t, "", strings.Repeat("<", 6<<20))
	body := map[string]any{"model": compatModel, "messages": []any{map[string]any{"role": "user", "content": "Synthetic request."}}}
	status, raw := h.send(t, "/v1/messages", body, nil, 0)
	if status != http.StatusRequestEntityTooLarge || !strings.Contains(string(raw), `"type":"request_too_large"`) || h.calls.Load() != 0 {
		t.Fatalf("oversized prepared request must be rejected before upstream: HTTP %d calls=%d response=%s", status, h.calls.Load(), raw)
	}
}
