package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 统计明细必须有硬上限：只保留最近的记录，最旧的成批丢弃。
func TestStatsRequestDetailIsBounded(t *testing.T) {
	if statsMaxRequests <= statsPruneBatch {
		t.Fatal("statsMaxRequests must be larger than statsPruneBatch")
	}
	rows := make([]statsRequest, statsMaxRequests)
	if kept, dropped := trimStatsRequests(rows); dropped != 0 || len(kept) != statsMaxRequests {
		t.Fatalf("exact limit trimmed: dropped=%d kept=%d", dropped, len(kept))
	}
	newest := statsRequest{At: 123456789}
	rows = append(rows, newest)
	kept, dropped := trimStatsRequests(rows)
	if dropped != statsPruneBatch {
		t.Fatalf("dropped=%d want a full prune batch %d", dropped, statsPruneBatch)
	}
	if len(kept) > statsMaxRequests || len(kept) != statsMaxRequests-statsPruneBatch+1 {
		t.Fatalf("kept=%d want %d", len(kept), statsMaxRequests-statsPruneBatch+1)
	}
	if kept[len(kept)-1] != newest {
		t.Fatal("newest request record was dropped")
	}
}

// 控制类上游响应同样有上限，异常响应不能无限读入内存。
func TestDoJSONContextLimitsUpstreamResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		chunk := bytes.Repeat([]byte(" "), 1<<20)
		for i := 0; i < 9; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	_, _, err := doJSONContext(context.Background(), srv.Client(), http.MethodGet, srv.URL, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("oversized upstream response was not rejected: %v", err)
	}
}
