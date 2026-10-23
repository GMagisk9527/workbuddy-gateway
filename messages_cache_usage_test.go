package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 2026-10-23 实测：WorkBuddy 上游 prompt_cache_hit_tokens 才是真实命中量，
// cache_read_input_tokens / cached_tokens 恒为 0；写入字段存在但恒为 0。
func TestAnthropicUsageCacheMapping(t *testing.T) {
	for _, tc := range []struct {
		name        string
		usage       map[string]any
		wantInput   int
		wantRead    int
		wantCreated int
		noCreated   bool
	}{
		{
			name: "observed-hit-zero-write",
			usage: map[string]any{
				"prompt_tokens": 519.0, "completion_tokens": 1.0,
				"prompt_cache_hit_tokens": 384.0, "prompt_cache_miss_tokens": 135.0,
				"prompt_cache_write_tokens": 0.0, "cache_creation_input_tokens": 0.0,
				"cache_read_input_tokens": 0.0, "cached_tokens": 0.0,
			},
			wantInput: 135, wantRead: 384, wantCreated: 0,
		},
		{
			name: "vendor-write-field",
			usage: map[string]any{
				"prompt_tokens": 300.0, "completion_tokens": 1.0,
				"prompt_cache_hit_tokens": 100.0, "prompt_cache_miss_tokens": 200.0,
				"prompt_cache_write_tokens": 50.0,
			},
			wantInput: 150, wantRead: 100, wantCreated: 50,
		},
		{
			name: "anthropic-write-preferred",
			usage: map[string]any{
				"prompt_tokens": 300.0, "completion_tokens": 1.0,
				"prompt_cache_hit_tokens":     100.0,
				"cache_creation_input_tokens": 50.0, "prompt_cache_write_tokens": 40.0,
			},
			wantInput: 150, wantRead: 100, wantCreated: 50,
		},
		{
			name: "write-missing-is-omitted",
			usage: map[string]any{
				"prompt_tokens": 200.0, "completion_tokens": 1.0,
				"prompt_cache_hit_tokens": 120.0,
			},
			wantInput: 80, wantRead: 120, noCreated: true,
		},
		{
			name: "negative-write-ignored",
			usage: map[string]any{
				"prompt_tokens": 200.0, "completion_tokens": 1.0,
				"prompt_cache_hit_tokens": 100.0, "cache_creation_input_tokens": -5.0,
			},
			wantInput: 100, wantRead: 100, noCreated: true,
		},
		{
			name: "hit-exceeds-prompt",
			usage: map[string]any{
				"prompt_tokens": 10.0, "completion_tokens": 1.0,
				"prompt_cache_hit_tokens": 100.0,
			},
			wantInput: 0, wantRead: 10, noCreated: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &anthropicStreamState{usage: tc.usage}
			got := state.anthropicUsage()
			if got["input_tokens"] != tc.wantInput || got["cache_read_input_tokens"] != tc.wantRead {
				t.Fatalf("usage=%v want input=%d read=%d", got, tc.wantInput, tc.wantRead)
			}
			if _, exists := got["cache_creation_input_tokens"]; exists == tc.noCreated {
				t.Fatalf("cache_creation presence=%t want absent=%t usage=%v", exists, tc.noCreated, got)
			}
			if !tc.noCreated && got["cache_creation_input_tokens"] != tc.wantCreated {
				t.Fatalf("cache_creation=%v want %d", got["cache_creation_input_tokens"], tc.wantCreated)
			}
			if got["output_tokens"] != 1 {
				t.Fatalf("output_tokens=%v", got["output_tokens"])
			}
		})
	}
}

func cacheUsageUpstream(usage string) string {
	return "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":" + usage + "}\n\n" +
		"data: [DONE]\n\n"
}

func streamMessagesUsage(t *testing.T, upstream string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
	streamMessagesResponse(rec, req, resp, "offline", 1, &Account{Path: "offline.json"}, &profileCN, time.Now(), false)
	for _, event := range splitSSE(t, rec.Body.String()) {
		if event.name != "message_delta" {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(event.data), &data); err != nil {
			t.Fatal(err)
		}
		usage, _ := data["usage"].(map[string]any)
		return usage
	}
	t.Fatalf("missing message_delta: %s", rec.Body.String())
	return nil
}

func aggregateMessagesUsage(t *testing.T, upstream string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(upstream))}
	writeMessagesAggregate(rec, req, resp, "offline", 1, &Account{Path: "offline.json"}, &profileCN, time.Now(), false)
	var message map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
		t.Fatal(err)
	}
	usage, _ := message["usage"].(map[string]any)
	return usage
}

func TestMessagesCacheUsageStreamAndAggregateAgree(t *testing.T) {
	for _, tc := range []struct {
		name     string
		usage    string
		wantIn   float64
		wantRead float64
		wantNew  float64
		absent   bool
	}{
		{
			name:   "observed",
			usage:  `{"prompt_tokens":519,"completion_tokens":1,"prompt_cache_hit_tokens":384,"prompt_cache_miss_tokens":135,"prompt_cache_write_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"cached_tokens":0}`,
			wantIn: 135, wantRead: 384, wantNew: 0,
		},
		{
			name:   "vendor-write",
			usage:  `{"prompt_tokens":300,"completion_tokens":1,"prompt_cache_hit_tokens":100,"prompt_cache_miss_tokens":200,"prompt_cache_write_tokens":50}`,
			wantIn: 150, wantRead: 100, wantNew: 50,
		},
		{
			name:   "write-missing",
			usage:  `{"prompt_tokens":200,"completion_tokens":1,"prompt_cache_hit_tokens":120}`,
			wantIn: 80, wantRead: 120, absent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := cacheUsageUpstream(tc.usage)
			stream := streamMessagesUsage(t, upstream)
			aggregate := aggregateMessagesUsage(t, upstream)
			for label, usage := range map[string]map[string]any{"stream": stream, "aggregate": aggregate} {
				if usage["input_tokens"] != tc.wantIn || usage["cache_read_input_tokens"] != tc.wantRead {
					t.Fatalf("%s usage=%v want input=%v read=%v", label, usage, tc.wantIn, tc.wantRead)
				}
				if _, exists := usage["cache_creation_input_tokens"]; exists == tc.absent {
					t.Fatalf("%s cache_creation presence=%t want absent=%t: %v", label, exists, tc.absent, usage)
				}
				if !tc.absent && usage["cache_creation_input_tokens"] != tc.wantNew {
					t.Fatalf("%s cache_creation=%v want %v", label, usage["cache_creation_input_tokens"], tc.wantNew)
				}
			}
			if fmt.Sprint(stream["input_tokens"]) != fmt.Sprint(aggregate["input_tokens"]) ||
				fmt.Sprint(stream["cache_read_input_tokens"]) != fmt.Sprint(aggregate["cache_read_input_tokens"]) {
				t.Fatalf("stream and aggregate disagree: stream=%v aggregate=%v", stream, aggregate)
			}
		})
	}
}

func TestStatsCacheWriteValue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		usage     map[string]any
		wantValue int64
		wantSrc   string
		wantOK    bool
	}{
		{"anthropic", map[string]any{"cache_creation_input_tokens": 50.0}, 50, "cache_creation_input_tokens", true},
		{"vendor", map[string]any{"prompt_cache_write_tokens": 40.0}, 40, "prompt_cache_write_tokens", true},
		{"positive-vendor-wins-over-zero", map[string]any{"cache_creation_input_tokens": 0.0, "prompt_cache_write_tokens": 40.0}, 40, "prompt_cache_write_tokens", true},
		{"explicit-zero", map[string]any{"cache_creation_input_tokens": 0.0}, 0, "cache_creation_input_tokens", true},
		{"nested", map[string]any{"prompt_tokens_details": map[string]any{"cache_creation_tokens": 30.0}}, 30, "prompt_tokens_details.cache_creation_tokens", true},
		{"missing", map[string]any{}, 0, "", false},
		{"negative-only", map[string]any{"prompt_cache_write_tokens": -1.0}, 0, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, source, ok := statsCacheWriteValue(tc.usage)
			if value != tc.wantValue || source != tc.wantSrc || ok != tc.wantOK {
				t.Fatalf("got (%d,%q,%t) want (%d,%q,%t)", value, source, ok, tc.wantValue, tc.wantSrc, tc.wantOK)
			}
		})
	}
}

func TestStatsRowRecordsCacheWrite(t *testing.T) {
	resetStatsState(t)
	requestStats.Lock()
	requestStats.enabled = true
	requestStats.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	attempt := &statsAttempt{
		id: "account-id", attemptID: "attempt-1", model: "test-model",
		started: time.Now(), complete: true,
		usage: map[string]any{"prompt_tokens": 300.0, "completion_tokens": 1.0, "prompt_cache_write_tokens": 50.0},
	}
	req = req.WithContext(context.WithValue(req.Context(), statsRequestContextKey{}, attempt))
	statsFinishAttempt(req, http.StatusOK)

	requestStats.Lock()
	rows := append([]statsRequest(nil), requestStats.document.Requests...)
	requestStats.Unlock()
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	row := rows[0]
	if row.CacheWriteTokens == nil || *row.CacheWriteTokens != 50 || row.CacheWriteSource != "prompt_cache_write_tokens" {
		t.Fatalf("cache write not recorded: %+v", row)
	}
}
