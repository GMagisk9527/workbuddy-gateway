package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestModelRequestPreservesJSONNumbersUpstream(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, kind := range []string{"tool-schema", "tool-output"} {
			t.Run(route+"/"+kind, func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				withRequestProfile(t, requestProfileLegacy)
				upstreamRaw := make(chan []byte, 1)
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var reader io.Reader = r.Body
					if r.Header.Get("Content-Encoding") == "gzip" {
						compressed, err := gzip.NewReader(r.Body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(http.StatusInternalServerError)
							return
						}
						defer compressed.Close()
						reader = compressed
					}
					raw, err := io.ReadAll(reader)
					if err != nil {
						t.Error(err)
					}
					upstreamRaw <- raw
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, conversationFixtureSSE(true))
				}))
				defer up.Close()
				profileCN.Base, profileCN.Origin = up.URL, up.URL
				profileINTL.Base, profileINTL.Origin = up.URL, up.URL
				cfg.HttpClient = up.Client()

				numbers := map[string]any{
					"ids":   []any{json.Number("9007199254740993")},
					"ratio": json.Number("0.12345678901234567890123456789"),
				}
				parameters := map[string]any{"type": "object", "properties": map[string]any{
					"id":    map[string]any{"type": "integer", "enum": numbers["ids"]},
					"ratio": map[string]any{"type": "number", "const": numbers["ratio"]},
				}}
				body := map[string]any{"model": compatModel, "temperature": 0.25, "top_p": 0.9}
				if route == "/v1/chat/completions" {
					body["max_tokens"] = 64
					body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": parameters}}}
					body["messages"] = []any{map[string]any{"role": "user", "content": "Synthetic request."}}
					if kind == "tool-output" {
						body["messages"] = []any{
							map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_precision", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}},
							map[string]any{"role": "tool", "tool_call_id": "call_precision", "content": map[string]any{"nested": numbers}},
						}
					}
				} else {
					body["max_output_tokens"] = 64
					body["tools"] = []any{map[string]any{"type": "function", "name": "lookup", "parameters": parameters}}
					body["input"] = "Synthetic request."
					if kind == "tool-output" {
						body["input"] = []any{
							map[string]any{"type": "function_call", "call_id": "call_precision", "name": "lookup", "arguments": "{}"},
							map[string]any{"type": "function_call_output", "call_id": "call_precision", "output": map[string]any{"nested": numbers}},
						}
					}
				}
				status, out := h.send(t, route, body, nil, 0)
				if status != http.StatusOK {
					t.Fatalf("HTTP %d: %s", status, out)
				}
				var wire map[string]any
				select {
				case raw := <-upstreamRaw:
					decoder := json.NewDecoder(bytes.NewReader(raw))
					decoder.UseNumber()
					if err := decoder.Decode(&wire); err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("no upstream request")
				}
				if wire["max_tokens"] != json.Number("64") || wire["temperature"] != json.Number("0.25") || wire["top_p"] != json.Number("0.9") {
					t.Fatalf("generation options changed: %v", wire)
				}
				if kind == "tool-schema" {
					fn := wire["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
					if !reflect.DeepEqual(fn["parameters"], parameters) {
						t.Fatalf("tool-schema numbers changed: got %v; want %v", fn["parameters"], parameters)
					}
					return
				}
				for _, raw := range wire["messages"].([]any) {
					message := raw.(map[string]any)
					if message["role"] != "tool" {
						continue
					}
					content := message["content"]
					if text, ok := content.(string); ok {
						decoder := json.NewDecoder(bytes.NewBufferString(text))
						decoder.UseNumber()
						if err := decoder.Decode(&content); err != nil {
							t.Fatal(err)
						}
					}
					if !reflect.DeepEqual(content, map[string]any{"nested": numbers}) {
						t.Fatalf("nested tool-output numbers changed: got %v; want %v", content, numbers)
					}
					return
				}
				t.Fatal("tool output missing upstream")
			})
		}
	}
}

func TestDecodeModelRequestPreservesScalarCompatibility(t *testing.T) {
	raw := []byte(`{
		"max_tokens":64,"max_completion_tokens":65,"max_output_tokens":66,
		"temperature":0.25,"top_p":0.9,"top_k":7,
		"frequency_penalty":0.5,"presence_penalty":-0.5,
		"n":1,"top_logprobs":2,"max_tool_calls":3,
		"seed":9007199254740993,"extension":{"count":9007199254740993}
	}`)
	var body map[string]any
	if err := decodeModelRequestBody(raw, &body); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{
		"max_tokens": 64, "max_completion_tokens": 65, "max_output_tokens": 66,
		"temperature": 0.25, "top_p": 0.9, "top_k": 7,
		"frequency_penalty": 0.5, "presence_penalty": -0.5,
		"n": 1, "top_logprobs": 2, "max_tool_calls": 3,
	} {
		if got, ok := body[key].(float64); !ok || got != want {
			t.Fatalf("%s: got %v (%T), want float64(%v)", key, body[key], body[key], want)
		}
	}
	if body["seed"] != json.Number("9007199254740993") || body["extension"].(map[string]any)["count"] != json.Number("9007199254740993") {
		t.Fatalf("numeric identifiers or extension fields lost precision: %v", body)
	}
}

func TestDecodeModelRequestRejectsTrailingJSON(t *testing.T) {
	for _, raw := range []string{`{} {}`, `{} null`, `{} invalid`, `[]`, `{"max_tokens":1e400}`} {
		t.Run(raw, func(t *testing.T) {
			var body map[string]any
			if err := decodeModelRequestBody([]byte(raw), &body); err == nil {
				t.Fatal("invalid model request accepted")
			}
		})
	}
	var body map[string]any
	if err := decodeModelRequestBody([]byte("{} \n\t"), &body); err != nil {
		t.Fatalf("trailing whitespace rejected: %v", err)
	}
}
