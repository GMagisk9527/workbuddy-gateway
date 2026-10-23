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

func responseJSONMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestResponsesOutputRefusalAggregate(t *testing.T) {
	body := respChunk(`{"role":"assistant","refusal":"Cannot "}`, "") +
		respChunk(`{"refusal":"help.\\n"}`, "") + respChunk(`{}`, "stop")
	agg, err := aggregateCompletion(strings.NewReader(body), "test-model")
	if err != nil {
		t.Fatal(err)
	}
	chat := responseJSONMap(t, agg)
	message := chat["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["refusal"] != "Cannot help.\\n" {
		t.Errorf("aggregated refusal=%v", message["refusal"])
	}
	out, err := chatCompletionToResponses(agg, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	response := responseJSONMap(t, out)
	parts := response["output"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["type"] != "refusal" || parts[0].(map[string]any)["refusal"] != "Cannot help.\\n" {
		t.Errorf("refusal content=%#v", parts)
	}
}

func TestResponsesOutputRefusalStreaming(t *testing.T) {
	for _, first := range []string{"text", "refusal"} {
		for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
			t.Run(fmt.Sprintf("%s/%d", first, mode), func(t *testing.T) {
				text, refusal := `{"content":"Answer "}`, `{"refusal":"Cannot "}`
				opening, second := text, refusal
				textIndex, refusalIndex := 0, 1
				if first == "refusal" {
					opening, second = refusal, text
					textIndex, refusalIndex = 1, 0
				}
				body := respChunk(`{"reasoning_content":"consider"}`, "") + respChunk(opening, "") +
					respChunk(`{"tool_calls":[{"index":0,"id":"call_test","type":"function","function":{"name":"Read","arguments":"{}"}}]}`, "") +
					respChunk(second, "") +
					// An escaped field name forces the normal map path between fast chunks.
					respChunk(`{"\u0072efusal":"help.\\n","content":"done."}`, "") + respChunk(`{}`, "stop")
				stream := captureResponsesStream(t, body, mode)
				events := splitSSE(t, stream)
				refusalText, outputText := "", ""
				var final map[string]any
				refusalDone, partDone := false, false
				lastSequence := -1
				for _, event := range events {
					if event.data == "[DONE]" || event.data == "" {
						continue
					}
					data := responseJSONMap(t, []byte(event.data))
					sequence := int(data["sequence_number"].(float64))
					if sequence != lastSequence+1 {
						t.Fatalf("sequence gap: %d after %d", sequence, lastSequence)
					}
					lastSequence = sequence
					switch event.name {
					case "response.refusal.delta", "response.refusal.done":
						if data["output_index"] != float64(1) || data["content_index"] != float64(refusalIndex) {
							t.Errorf("refusal indices=%v", data)
						}
						if event.name == "response.refusal.delta" {
							refusalText += data["delta"].(string)
						} else {
							refusalDone = data["refusal"] == "Cannot help.\\n"
						}
					case "response.output_text.delta":
						if data["output_index"] != float64(1) || data["content_index"] != float64(textIndex) {
							t.Errorf("text indices=%v", data)
						}
						outputText += data["delta"].(string)
					case "response.content_part.done":
						part := data["part"].(map[string]any)
						if part["type"] == "refusal" {
							partDone = part["refusal"] == "Cannot help.\\n"
						}
					case "response.completed":
						final = data["response"].(map[string]any)
					}
				}
				if refusalText != "Cannot help.\\n" || outputText != "Answer done." || !refusalDone || !partDone {
					t.Fatalf("missing refusal events: refusal=%q text=%q done=%v part=%v", refusalText, outputText, refusalDone, partDone)
				}
				if final == nil {
					t.Fatal("missing completed response")
				}
				output := final["output"].([]any)
				if len(output) != 3 || output[2].(map[string]any)["type"] != "function_call" {
					t.Fatalf("output order=%v", output)
				}
				parts := output[1].(map[string]any)["content"].([]any)
				if len(parts) != 2 || parts[refusalIndex].(map[string]any)["refusal"] != refusalText || parts[textIndex].(map[string]any)["text"] != outputText {
					t.Errorf("final content parts=%v", parts)
				}
			})
		}
	}
}

func TestResponsesOutputRefusalOnlyAndNull(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		body := respChunk(`{"refusal":null}`, "") + respChunk(`{"refusal":""}`, "") + respChunk(`{"refusal":"No."}`, "") + respChunk(`{}`, "stop")
		stream := captureResponsesStream(t, body, mode)
		if strings.Contains(stream, "response.output_text.") || strings.Count(stream, "event: response.refusal.delta") != 1 || !strings.Contains(stream, `"refusal":"No."`) {
			t.Errorf("mode=%d refusal-only stream=%s", mode, stream)
		}
	}
}

func TestResponsesOutputThinkingUsageFallback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		detail   any
		include  bool
		fallback any
		want     int64
	}{
		{"missing", nil, false, float64(7), 7}, {"standard", float64(3), true, float64(7), 3},
		{"zero", float64(0), true, float64(7), 0}, {"null", nil, true, float64(7), 7},
		{"invalid", "bad", true, float64(7), 7}, {"negative", float64(-1), true, float64(7), 7},
		{"invalid fallback", nil, false, float64(-4), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := map[string]any{"prompt_tokens": float64(5), "completion_tokens": float64(9), "total_tokens": float64(14), "credit": float64(0.5), "completion_thinking_tokens": tc.fallback}
			if tc.include {
				usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": tc.detail}
			}
			out := toResponsesUsage(usage)
			if got := out["output_tokens_details"].(map[string]any)["reasoning_tokens"]; got != tc.want {
				t.Errorf("reasoning=%v want=%d", got, tc.want)
			}
			if out["output_tokens"] != int64(9) || out["total_tokens"] != int64(14) || usage["credit"] != float64(0.5) {
				t.Errorf("usage totals changed: %v", out)
			}
		})
	}
}

func TestResponsesOutputRefusalTokenClassification(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"refusal":"No."}`, true}, {`{"refusal":" "}`, true},
		{`{"refusal":""}`, false}, {`{"refusal":null}`, false}, {`{"refusal":27}`, false},
	} {
		if got := hasFirstTokenDelta(firstTokenChunk(t, tc.raw)); got != tc.want {
			t.Errorf("map refusal=%s got=%v", tc.raw, got)
		}
		var meta chunkMeta
		_, ok := cleanChunkInto(nil, []byte(strings.TrimSpace(strings.TrimPrefix(respChunk(tc.raw, ""), "data: "))), &meta)
		if !ok || meta.firstToken != tc.want {
			t.Errorf("chat fast refusal=%s ok=%v first=%v", tc.raw, ok, meta.firstToken)
		}
		var view respChunkView
		if !parseResponsesChunk([]byte(strings.TrimSpace(strings.TrimPrefix(respChunk(tc.raw, ""), "data: "))), &view) || view.firstTok != tc.want {
			t.Errorf("responses fast refusal=%s first=%v", tc.raw, view.firstTok)
		}
	}
}

func TestResponsesOutputMetadataEnvelopes(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		for _, finish := range []string{"stop", "length", ""} {
			t.Run(fmt.Sprintf("%d/%s", mode, finish), func(t *testing.T) {
				old := fastPathMode.Load()
				fastPathMode.Store(int32(mode))
				defer fastPathMode.Store(old)
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				metadata := map[string]any{"tag": "synthetic"}
				req = withResponsesMetadata(req, metadata)
				metadata["tag"] = "mutated"
				body := respChunk(`{"content":"answer"}`, "")
				if finish != "" {
					body += respChunk(`{}`, finish)
				}
				writer := httptest.NewRecorder()
				acc := &Account{Path: "offline-test.json"}
				streamResponsesResponse(writer, req, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, "test-model", 1, acc, &profileCN, time.Now())
				count := 0
				for _, event := range splitSSE(t, writer.Body.String()) {
					if event.data == "" || event.data == "[DONE]" {
						continue
					}
					data := responseJSONMap(t, []byte(event.data))
					if envelope, ok := data["response"].(map[string]any); ok {
						count++
						if envelope["metadata"].(map[string]any)["tag"] != "synthetic" {
							t.Errorf("%s metadata=%v", event.name, envelope["metadata"])
						}
					}
				}
				if count != 3 {
					t.Errorf("response envelope count=%d", count)
				}
			})
		}
	}
	request := withResponsesMetadata(httptest.NewRequest(http.MethodPost, "/v1/responses", nil), map[string]any{"tag": "aggregate"})
	writer := httptest.NewRecorder()
	body := respChunk(`{"content":"answer"}`, "") + respChunk(`{}`, "stop")
	writeResponsesAggregate(writer, request, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, "test-model", 1, &Account{Path: "offline-test.json"}, &profileCN, time.Now())
	if metadata := responseJSONMap(t, writer.Body.Bytes())["metadata"].(map[string]any); metadata["tag"] != "aggregate" {
		t.Errorf("aggregate metadata=%v", metadata)
	}
	chat, err := responsesToChatRequest(map[string]any{"input": "hello", "metadata": map[string]any{"tag": "synthetic"}}, "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := chat["metadata"]; exists {
		t.Error("metadata leaked upstream")
	}
}
