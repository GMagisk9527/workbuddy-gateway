package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func BenchmarkRedactionText(b *testing.B) {
	for _, count := range []int{0, 10, 100, 1000} {
		b.Run(fmt.Sprintf("secrets_%d", count), func(b *testing.B) {
			isolateSecrets(b)
			values := make([]string, count)
			for i := range values {
				sum := sha256.Sum256([]byte(fmt.Sprintf("redaction-benchmark-%d", i)))
				values[i] = fmt.Sprintf("synthetic-%x", sum)
			}
			registerSecrets(values...)
			const text = "trace=synthetic-trace model=synthetic-model result=ok duration=10ms"
			_ = redactSensitiveText(text)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = redactSensitiveText(text)
			}
		})
	}
}

func BenchmarkRedactionAdminStats(b *testing.B) {
	quiet()
	for _, count := range []int{1000, statsMaxRequests} {
		b.Run(fmt.Sprintf("rows_%d", count), func(b *testing.B) {
			isolateSecrets(b)
			doc := syntheticPersistenceStats(count)
			for i := range doc.Requests {
				doc.Requests[i].Model = fmt.Sprintf("synthetic-model-%d", i%8)
			}
			requestStats.Lock()
			old := requestStats.document
			requestStats.document = doc
			requestStats.Unlock()
			b.Cleanup(func() { requestStats.Lock(); requestStats.document = old; requestStats.Unlock() })
			date := time.UnixMilli(doc.StartedAt).In(statsZone).Format("2006-01-02")
			r := httptest.NewRequest(http.MethodGet, "/admin/api/stats?period=day&start="+date+"&end="+date, nil)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				handleWebUIStats(&benchNullWriter{h: make(http.Header)}, r)
			}
		})
	}
}
