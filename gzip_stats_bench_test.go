package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func gzipBenchmarkBody(size int) []byte {
	var s strings.Builder
	s.WriteString(`{"messages":[`)
	for i := 0; s.Len() < size; i++ {
		if i > 0 {
			s.WriteByte(',')
		}
		id := sha256.Sum256([]byte(fmt.Sprintf("synthetic-%d", i)))
		fmt.Fprintf(&s, `{"role":"user","content":"file src/module_%d.go: const value = %d; // record %x"}`, i, i*17, id)
	}
	s.WriteString(`]}`)
	return []byte(s.String())
}

var gzipStatsBenchmarkBytes []byte

func BenchmarkOfficialGzipEncoding(b *testing.B) {
	for _, size := range []int{1024, 128 << 10, 1 << 20} {
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			raw := gzipBenchmarkBody(size)
			if _, err := encodeModelRequest(raw, requestProfileOfficial, &profileCN); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				gzipStatsBenchmarkBytes, err = encodeModelRequest(raw, requestProfileOfficial, &profileCN)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func statsZoneTime() time.Time { return time.UnixMilli(1792886400000) }

func syntheticPersistenceStats(n int) statsDocument {
	in, out, total, cached := int64(10000), int64(1000), int64(11000), int64(4000)
	credit, ttft := 0.25, 1200.5
	doc := statsDocument{Schema: statsSchema, StartedAt: statsZoneTime().UnixMilli(),
		Accounts: map[string]statsAccount{"synthetic-account": {File: "synthetic.json", Edition: "cn",
			ModelStates: map[string]modelStateSnapshot{"synthetic-model": {CostClass: modelCostPaid}}}},
		Requests: make([]statsRequest, n, n+1)}
	for i := range doc.Requests {
		doc.Requests[i] = statsRequest{Account: "synthetic-account", Model: "synthetic-model",
			At: doc.StartedAt + int64(i), Status: 200, Complete: true, TotalTokens: &total, InputTokens: &in,
			OutputTokens: &out, CachedTokens: &cached, CacheSource: "prompt_tokens_details.cached_tokens",
			Affinity: "hit", Credit: &credit, LatencyMS: 30000.5, TTFTMS: &ttft, TTFTMetric: ttftMetric}
	}
	return doc
}

func BenchmarkStatsDocumentEncode(b *testing.B) {
	doc := syntheticPersistenceStats(statsMaxRequests)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		gzipStatsBenchmarkBytes, err = json.Marshal(doc)
		if err != nil {
			b.Fatal(err)
		}
	}
}

var statsBenchmarkSnapshot statsDocument

func BenchmarkStatsSnapshotCriticalSection(b *testing.B) {
	for _, count := range []int{1000, statsMaxRequests} {
		b.Run(fmt.Sprintf("rows_%d", count), func(b *testing.B) {
			requestStats.Lock()
			old := requestStats.document
			requestStats.document = syntheticPersistenceStats(count)
			requestStats.Unlock()
			b.Cleanup(func() {
				requestStats.Lock()
				requestStats.document = old
				requestStats.Unlock()
			})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				requestStats.Lock()
				statsBenchmarkSnapshot = snapshotStatsDocumentLocked()
				requestStats.Unlock()
			}
		})
	}
}
