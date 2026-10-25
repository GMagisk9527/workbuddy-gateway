package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func largeContextFixture(b *testing.B, size int) []byte {
	b.Helper()
	text := strings.Repeat("synthetic tool output line\n", size/len("synthetic tool output line\n"))
	body := map[string]any{"model": "synthetic-model", "max_tokens": 256, "stream": true,
		"system": "You are a helpful coding assistant.",
		"messages": []any{
			map[string]any{"role": "user", "content": "Read the synthetic source."},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "synthetic-tool", "name": "read_file", "input": map[string]any{"path": "synthetic.go", "wide": json.Number("9007199254740993")}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "synthetic-tool", "content": text}}},
		},
		"tools": []any{map[string]any{"name": "read_file", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		b.Fatal(err)
	}
	return raw
}

func BenchmarkMessagesRequestConversion(b *testing.B) {
	for _, size := range []int{1 << 10, 128 << 10, 1 << 20, 4 << 20} {
		b.Run(fmt.Sprintf("input_%dKiB", size>>10), func(b *testing.B) {
			raw := largeContextFixture(b, size)
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var body map[string]any
				if err := decodeAnthropicMessageBody(raw, &body); err != nil {
					b.Fatal(err)
				}
				chat, err := anthropicToChatRequest(body, "synthetic-model")
				if err != nil {
					b.Fatal(err)
				}
				encoded, err := marshalConvertedRequest(chat)
				if err != nil || len(encoded) == 0 {
					b.Fatalf("conversion failed: %v", err)
				}
			}
		})
	}
}

func BenchmarkConversionJSONString(b *testing.B) {
	for name, text := range map[string]string{"ascii": strings.Repeat("synthetic line ", 65536), "escaped": strings.Repeat("a\n\"\\<>&", 65536), "unicode": strings.Repeat("中文🙂", 65536)} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := measureConversionJSON(text, requestDecodedLimit); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkReadDecodedRequestBody(b *testing.B) {
	data := []byte(strings.Repeat("synthetic input ", 262144))
	// This measures buffer handoff, not scanning/copying byte throughput.
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := &http.Request{Body: newDecodedRequestBody(data)}
		got, err := readModelRequestBody(r)
		if err != nil || len(got) != len(data) {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadKnownRequestBody(b *testing.B) {
	data := []byte(strings.Repeat("synthetic input ", 262144))
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := &http.Request{Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data))}
		got, err := readModelRequestBody(r)
		if err != nil || len(got) != len(data) {
			b.Fatal(err)
		}
	}
}
