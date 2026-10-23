package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMessagesUpstreamRefusalIsPreserved(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "aggregate", true: "stream"}[stream], func(t *testing.T) {
			upstream := respChunk(`{"refusal":"Synthetic refusal."}`, "stop") + "data: [DONE]\n\n"
			response := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			account := &Account{Path: "offline.json"}
			if stream {
				streamMessagesResponse(recorder, request, response, "offline", 1, account, &profileCN, time.Now(), false)
			} else {
				writeMessagesAggregate(recorder, request, response, "offline", 1, account, &profileCN, time.Now(), false)
			}
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "Synthetic refusal.") {
				t.Fatalf("refusal silently lost: HTTP %d: %s", recorder.Code, recorder.Body.String())
			}
			if stream {
				if !strings.Contains(recorder.Body.String(), `"stop_reason":"refusal"`) {
					t.Fatal("refusal stop reason missing")
				}
			} else {
				var message map[string]any
				if err := json.Unmarshal(recorder.Body.Bytes(), &message); err != nil {
					t.Fatal(err)
				}
				if message["stop_reason"] != "refusal" {
					t.Fatal("refusal stop reason missing")
				}
			}
		})
	}
}
