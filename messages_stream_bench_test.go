package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Includes upstream SSE decoding, protocol conversion and the response writer.
func BenchmarkMessagesStream(b *testing.B) {
	quiet()
	const events = 128
	body := benchBody(events)
	acc := &Account{Path: "synthetic-benchmark.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "synthetic", ExpiresAt: time.Now().Add(time.Hour).Unix()}}}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := &benchNullWriter{h: make(http.Header)}
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
		streamMessagesResponse(w, r, resp, "synthetic-model", 7, acc, &profileCN, time.Now(), false)
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*events), "ns/event")
}
