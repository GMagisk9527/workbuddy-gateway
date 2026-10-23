package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProtocolReasoningStateIsAuthenticatedAndScoped(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/messages", nil)
	request.Header.Set("Authorization", "Bearer synthetic-credential")
	codec, err := newProtocolReasoningCodec(request, "model-a", "messages")
	if err != nil {
		t.Fatal(err)
	}
	const text = "Synthetic hidden reasoning with Unicode: 测试."
	sealed := codec.seal(text)
	if sealed == codec.seal(text) || strings.Contains(sealed, text) {
		t.Fatal("reasoning states need independent nonces and opaque text")
	}
	opened, err := codec.open(sealed)
	if err != nil || opened != text {
		t.Fatalf("round trip: %q, %v", opened, err)
	}
	for _, tc := range []struct{ key, model, protocol string }{
		{"other-credential", "model-a", "messages"},
		{"synthetic-credential", "model-b", "messages"},
		{"synthetic-credential", "model-a", "responses"},
	} {
		other := httptest.NewRequest("POST", "/", nil)
		other.Header.Set("x-api-key", tc.key)
		otherCodec, err := newProtocolReasoningCodec(other, tc.model, tc.protocol)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := otherCodec.open(sealed); err == nil {
			t.Fatalf("state escaped credential/model/protocol boundary: %#v", tc)
		}
	}
	other := httptest.NewRequest("POST", "/", nil)
	other.Header.Set("x-api-key", "synthetic-credential")
	equivalent, err := newProtocolReasoningCodec(other, "model-a", "messages")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := equivalent.open(sealed); err != nil || got != text {
		t.Fatal("equivalent API authentication headers must replay the same state")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(sealed, gatewayReasoningPrefix))
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	for _, invalid := range []string{
		gatewayReasoningPrefix, gatewayReasoningPrefix + "!invalid!", gatewayReasoningPrefix + "AQ",
		gatewayReasoningPrefix + base64.RawStdEncoding.EncodeToString(raw),
	} {
		if _, err := codec.open(invalid); err == nil || strings.Contains(err.Error(), text) {
			t.Fatal("malformed/tampered state accepted or leaked")
		}
	}
}

func TestProtocolReasoningRestoreDoesNotMutateHistory(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/messages", nil)
	request.Header.Set("x-api-key", "synthetic-credential")
	codec, err := newProtocolReasoningCodec(request, "model-a", "messages")
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "", "signature": codec.seal("Synthetic hidden history.")},
			map[string]any{"type": "tool_use", "id": "call_copy", "name": "inspect", "input": map[string]any{"value": json.Number("9007199254740993")}},
		}},
	}}
	before, _ := json.Marshal(body)
	_, restored, err := prepareAnthropicThinking(request, body, "model-a")
	if err != nil {
		t.Fatal(err)
	}
	message := restored["messages"].([]any)[0].(map[string]any)
	if message["content"].([]any)[0].(map[string]any)["thinking"] != "Synthetic hidden history." {
		t.Fatal("history not restored")
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("restoring opaque state mutated the client input")
	}
	if !strings.Contains(string(after), "9007199254740993") {
		t.Fatal("tool number precision changed")
	}
}
