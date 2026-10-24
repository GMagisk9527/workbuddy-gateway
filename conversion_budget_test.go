package main

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestConversionJSONSizeMatchesMarshalAndStringEscaping(t *testing.T) {
	type stringsAlias []string
	for name, value := range map[string]any{
		"strings":          map[string]any{"special": "\"\\\b\f\n\r\t\x00<>&\u2028\u2029中文\xff", "empty": ""},
		"numbers":          []any{json.Number("9007199254740993"), json.Number("-1.2e+30"), json.Number(""), 1, int64(-17), uint64(math.MaxUint64), 1.5, 1e-9, float32(1e20)},
		"typed":            map[string][]string{"names": {"one", "<two>"}},
		"alias":            stringsAlias{"one", "two"},
		"bytes":            []byte("synthetic"),
		"null":             []any{nil, []any(nil), map[string]any(nil), []byte(nil)},
		"raw":              json.RawMessage(" { \"escaped\": \"<>&\\\"\\n\\u2028\", \"unicode\": \"\u2028中文\" } "),
		"raw-invalid-utf8": json.RawMessage{'"', 0xff, '"'},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			quoted, _ := json.Marshal(string(encoded))
			size, err := measureConversionJSON(value, len(encoded))
			if err != nil || size.plain != len(encoded) || size.quoted != len(quoted)-2 {
				full, fullErr := measureConversionJSON(value, 1<<20)
				t.Fatalf("size=%+v, full=%+v/%v actual=%d/%d encoded=%s error=%v", size, full, fullErr, len(encoded), len(quoted)-2, encoded, err)
			}
			_, err = measureConversionJSON(value, len(encoded)-1)
			var tooLarge *conversionSizeError
			if !errors.As(err, &tooLarge) {
				t.Fatalf("size limit did not reject the value: %v", err)
			}
		})
	}
}

func TestConversionJSONSizeRejectsInvalidNumbersAndUnsupportedValues(t *testing.T) {
	for _, value := range []any{json.Number("01"), json.Number("1."), json.Number("1e"), json.Number("-"), math.Inf(1), math.NaN(), json.RawMessage(`{"invalid":`), make(chan bool)} {
		if _, err := measureConversionJSON(value, 1024); err == nil {
			t.Fatalf("invalid value accepted: %T", value)
		}
	}
}

func TestMarshalConvertedRequestRejectsSizeBeforeAllocation(t *testing.T) {
	// Reuse a small string so the object graph remains small while its eventual
	// JSON representation exceeds the shared conversion budget.
	part := strings.Repeat("a", 16<<10)
	parts := make([]any, requestDecodedLimit/len(part)+1)
	for i := range parts {
		parts[i] = part
	}
	encoded, err := marshalConvertedRequest(map[string]any{"messages": parts})
	var tooLarge *conversionSizeError
	if !errors.As(err, &tooLarge) || encoded != nil {
		t.Fatalf("oversized conversion was serialized: encoded=%d error=%v", len(encoded), err)
	}
}

func TestMessagesToolReferenceDefinitionIsDeduplicatedAcrossResults(t *testing.T) {
	body := syntheticToolReferenceRequest()
	first := body["messages"].([]any)[1].(map[string]any)["content"].([]any)[0]
	body["messages"] = append(body["messages"].([]any), map[string]any{"role": "user", "content": []any{first}})
	normalized, err := normalizeAnthropicToolReferences(body)
	if err != nil {
		t.Fatal(err)
	}
	messages := normalized["messages"].([]any)
	getReferenceText := func(index int) string {
		return messages[index].(map[string]any)["content"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)["text"].(string)
	}
	if !strings.Contains(getReferenceText(1), `"parameters"`) || strings.Contains(getReferenceText(2), `"parameters"`) || !strings.Contains(getReferenceText(2), "Inspect") {
		t.Fatal("full definitions were repeated across tool results or a later reference lost its identity")
	}
}
