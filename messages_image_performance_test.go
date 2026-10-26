package main

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func syntheticImageRequest(data string, toolResult bool) map[string]any {
	image := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": data}}
	messages := []any{map[string]any{"role": "user", "content": []any{image}}}
	if toolResult {
		messages = []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "synthetic-image-call", "name": "read_image", "input": map[string]any{}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "synthetic-image-call", "content": []any{image}}}},
		}
	}
	return map[string]any{"model": "synthetic-model", "max_tokens": float64(32), "messages": messages}
}

func TestMessagesImageValidationAvoidsPayloadCopy(t *testing.T) {
	for _, toolResult := range []bool{false, true} {
		t.Run(fmt.Sprintf("tool_result=%t", toolResult), func(t *testing.T) {
			body := syntheticImageRequest(strings.Repeat("A", 4<<20), toolResult)
			if err := validateAnthropicChatInput(body); err != nil {
				t.Fatal(err)
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for i := 0; i < 4; i++ {
				if err := validateAnthropicChatInput(body); err != nil {
					t.Fatal(err)
				}
			}
			runtime.ReadMemStats(&after)
			// Allow ample bookkeeping noise while rejecting any full 4 MiB copy.
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
				t.Fatalf("validation copied image payloads: allocated %d bytes", allocated)
			}
			runtime.KeepAlive(body)
		})
	}
}

func TestAnthropicImageSourceValidationMatchesWireConversion(t *testing.T) {
	for name, tc := range map[string]struct {
		source any
		want   string
	}{
		"base64":      {map[string]any{"type": "base64", "media_type": "image/png", "data": "AAAA"}, "data:image/png;base64,AAAA"},
		"url":         {map[string]any{"type": "url", "url": "https://example.invalid/synthetic.png"}, "https://example.invalid/synthetic.png"},
		"opaque-data": {map[string]any{"type": "base64", "media_type": "custom/type", "data": "unchanged data"}, "data:custom/type;base64,unchanged data"},
		"empty-data":  {map[string]any{"type": "base64", "media_type": "image/png", "data": ""}, ""},
		"empty-media": {map[string]any{"type": "base64", "media_type": "", "data": "AAAA"}, ""},
		"wrong-data":  {map[string]any{"type": "base64", "media_type": "image/png", "data": 7}, ""},
		"empty-url":   {map[string]any{"type": "url", "url": ""}, ""},
		"wrong-url":   {map[string]any{"type": "url", "url": 7}, ""},
		"unsupported": {map[string]any{"type": "file", "file_id": "synthetic"}, ""},
		"not-object":  {"invalid", ""},
		"missing":     {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			image := map[string]any{"type": "image", "source": tc.source}
			if got := anthropicImageURL(image); got != tc.want {
				t.Fatalf("image URL = %q, want %q", got, tc.want)
			}
			for _, role := range []string{"user", "tool", "assistant", "system"} {
				err := validateAnthropicChatContent([]any{image}, role, "synthetic")
				valid := tc.want != "" && (role == "user" || role == "tool")
				if (err == nil) != valid {
					t.Fatalf("role=%s validity changed: %v", role, err)
				}
			}
		})
	}
}

var imageBenchmarkBytes []byte

func BenchmarkMessagesImagePreparation(b *testing.B) {
	for _, toolResult := range []bool{false, true} {
		for _, size := range []int{1 << 20, 4 << 20} {
			body := syntheticImageRequest(strings.Repeat("A", size), toolResult)
			name := fmt.Sprintf("tool_result_%t/%dMiB", toolResult, size>>20)
			b.Run(name+"/validate", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if err := validateAnthropicChatInput(body); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(name+"/convert", func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					chat, err := anthropicToChatRequest(body, "synthetic-model")
					if err != nil {
						b.Fatal(err)
					}
					imageBenchmarkBytes, err = marshalConvertedRequest(chat)
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
