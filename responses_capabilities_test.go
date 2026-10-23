package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestResponsesUnsupportedCapabilitiesStopBeforeUpstream(t *testing.T) {
	cases := map[string]map[string]any{
		"background":           {"background": true},
		"prompt-template":      {"prompt": map[string]any{"id": "private-template-sentinel"}},
		"automatic-truncation": {"truncation": "auto"},
		"logprobs":             {"include": []any{"message.output_text.logprobs"}},
		"json-schema":          {"text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "answer", "schema": map[string]any{"type": "object"}, "strict": true}}},
		"json-object":          {"text": map[string]any{"format": map[string]any{"type": "json_object"}}},
	}
	for name, options := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", name, stream), func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				body := map[string]any{"model": compatModel, "input": "Synthetic input.", "stream": stream}
				for key, value := range options {
					body[key] = value
				}
				status, out := h.send(t, "/v1/responses", body, nil, 0)
				if status != 400 || h.calls.Load() != 0 || !strings.Contains(string(out), `"type":"invalid_request"`) {
					t.Fatalf("unsupported semantics reached upstream: status=%d calls=%d output=%s", status, h.calls.Load(), out)
				}
				if strings.Contains(string(out), "private-template-sentinel") || strings.Contains(h.logs.String(), "private-template-sentinel") {
					t.Fatal("template identifier leaked")
				}
			})
		}
	}
}

func TestResponsesHarmlessCapabilityDefaults(t *testing.T) {
	for _, options := range []map[string]any{
		{},
		{"background": false, "prompt": nil, "truncation": "disabled", "include": []any{}, "metadata": map[string]any{}},
		{"background": nil, "prompt": map[string]any{}, "truncation": "", "include": nil, "text": map[string]any{"format": map[string]any{"type": "text"}}},
		{"text": map[string]any{}, "metadata": map[string]any{strings.Repeat("界", 64): strings.Repeat("值", 512)}},
		{"include": []any{"reasoning.encrypted_content"}},
	} {
		options["input"] = "Synthetic input."
		if _, err := responsesToChatRequest(options, compatModel); err != nil {
			t.Fatalf("harmless defaults rejected: %v", err)
		}
	}
}

func TestResponsesMetadataValidation(t *testing.T) {
	tooMany := map[string]any{}
	for i := range 17 {
		tooMany[fmt.Sprint(i)] = "value"
	}
	for name, metadata := range map[string]any{
		"scalar":           "private-metadata-sentinel",
		"non-string-value": map[string]any{"key": map[string]any{"private-metadata-sentinel": true}},
		"too-many":         tooMany,
		"long-key":         map[string]any{strings.Repeat("界", 65): "value"},
		"long-value":       map[string]any{"key": strings.Repeat("值", 513)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := responsesToChatRequest(map[string]any{"input": "Synthetic input.", "metadata": metadata}, compatModel)
			if err == nil || !strings.Contains(err.Error(), "metadata") || strings.Contains(err.Error(), "private-metadata-sentinel") {
				t.Fatalf("invalid metadata accepted or exposed: %v", err)
			}
		})
	}
}

func TestResponsesCapabilityOptionsRequireValidShapes(t *testing.T) {
	for name, options := range map[string]map[string]any{
		"background":  {"background": "false"},
		"prompt":      {"prompt": "private-template-sentinel"},
		"truncation":  {"truncation": 1},
		"include":     {"include": "reasoning.encrypted_content"},
		"text":        {"text": "private-format-sentinel"},
		"format":      {"text": map[string]any{"format": "text"}},
		"format-type": {"text": map[string]any{"format": map[string]any{"type": 1}}},
	} {
		t.Run(name, func(t *testing.T) {
			options["input"] = "Synthetic input."
			if _, err := responsesToChatRequest(options, compatModel); err == nil {
				t.Fatal("invalid option accepted")
			}
		})
	}
}
