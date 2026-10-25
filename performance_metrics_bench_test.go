package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func BenchmarkPerformanceMessagesStream(b *testing.B) {
	quiet()
	body := benchBody(128)
	acc := &Account{Path: "synthetic.json", Auth: &StoredAuth{}}
	for _, sampled := range []bool{false, true} {
		b.Run(map[bool]string{false: "without_context", true: "with_context"}[sampled], func(b *testing.B) {
			r := httptest.NewRequest("POST", "/v1/messages", nil)
			store := &performanceStore{started: time.Now()}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				request := r
				var p *requestPerformance
				if sampled {
					request, p = withRequestPerformance(r, time.Now())
				}
				w := &auditResponseWriter{ResponseWriter: &benchNullWriter{h: make(http.Header)}, performance: p}
				resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
				streamMessagesResponse(w, request, resp, "synthetic-model", 1, acc, &profileCN, time.Now(), false)
				p.finish(store, 200, false, time.Now())
			}
		})
	}
}

func BenchmarkPerformanceObserve(b *testing.B) {
	p := &requestPerformance{start: time.Now()}
	b.ReportAllocs()
	b.ReportMetric(float64(unsafe.Sizeof(performanceStore{})), "store_bytes")
	for i := 0; i < b.N; i++ {
		p.observe(phaseDownstream, time.Microsecond, false)
	}
}
