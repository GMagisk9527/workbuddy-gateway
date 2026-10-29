package main

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"time"
)

const requestAttemptCapacity = 16

// Only fixed labels, numbers and relative times survive request completion.
// Account identity is used transiently to recognize rotation, never exported.
type requestAttemptRecord struct {
	started, headers, duration time.Duration
	status                     int
	businessCode               uint32
	kind, outcome, failure     string
}

type requestAttemptView struct {
	Attempt      int      `json:"attempt"`
	Kind         string   `json:"kind"`
	Outcome      string   `json:"outcome"`
	StatusCode   int      `json:"statusCode,omitempty"`
	BusinessCode string   `json:"businessCode,omitempty"`
	FailureClass string   `json:"failureClass,omitempty"`
	StartedMS    float64  `json:"startedMs"`
	HeadersMS    *float64 `json:"headersMs,omitempty"`
	DurationMS   float64  `json:"durationMs"`
}

type attemptObservation struct {
	p    *requestPerformance
	slot int // one-based; zero means recording was disabled or the bound was hit
}

func (p *requestPerformance) beginUpstreamAttempt(acc *Account, transportRetry bool) attemptObservation {
	if p == nil {
		return attemptObservation{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return attemptObservation{}
	}
	if p.currentAttempt != 0 {
		previous := &p.attemptTrail[p.currentAttempt-1]
		if previous.outcome == "response_headers" {
			previous.outcome, previous.failure = "incomplete", "stream_incomplete"
		}
	}
	kind := "initial"
	if transportRetry {
		kind = "transport_retry"
	} else if p.attemptTotal != 0 {
		kind = "stream_replay"
		if acc != p.lastAttemptAccount {
			kind = "account_fallback"
		}
	}
	p.lastAttemptAccount = acc
	p.attemptTotal++
	p.currentAttempt = 0
	if p.attemptCount == requestAttemptCapacity {
		return attemptObservation{}
	}
	p.attemptCount++
	p.currentAttempt = p.attemptCount
	p.attemptTrail[p.attemptCount-1] = requestAttemptRecord{started: max(time.Since(p.start), 0), kind: kind, outcome: "pending"}
	return attemptObservation{p, p.currentAttempt}
}

func (a attemptObservation) update(fn func(*requestAttemptRecord)) {
	if a.p == nil || a.slot == 0 {
		return
	}
	a.p.mu.Lock()
	defer a.p.mu.Unlock()
	if !a.p.closed {
		fn(&a.p.attemptTrail[a.slot-1])
	}
}

func attemptErrorClass(err error, fallback string) string {
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var n net.Error
	if errors.As(err, &n) && n.Timeout() {
		return "timeout"
	}
	return fallback
}

func attemptHTTPFailure(status int) string {
	switch {
	case status == 429:
		return "rate_limited"
	case status == 401 || status == 403:
		return "auth"
	case status >= 500:
		return "upstream_5xx"
	case status >= 400:
		return "upstream_4xx"
	default:
		return ""
	}
}

func (a attemptObservation) response(status int, err error) {
	a.update(func(entry *requestAttemptRecord) {
		entry.duration = max(time.Since(a.p.start)-entry.started, 0)
		if err != nil {
			entry.outcome, entry.failure = "error", attemptErrorClass(err, "network")
			return
		}
		entry.status, entry.headers = status, entry.duration
		entry.outcome, entry.failure = "response_headers", attemptHTTPFailure(status)
		if status >= 400 {
			entry.outcome = "rejected"
		}
	})
}

// Called after the existing error classification; it never changes retry policy.
func (p *requestPerformance) rejectCurrentAttempt(failure, code string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.currentAttempt == 0 {
		return
	}
	entry := &p.attemptTrail[p.currentAttempt-1]
	entry.outcome, entry.failure = "rejected", failure
	number, _ := strconv.ParseUint(code, 10, 32)
	entry.businessCode = uint32(number)
}

type attemptDiagnosticBody struct {
	io.ReadCloser
	observation attemptObservation
}

func (b *attemptDiagnosticBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if err != nil && !errors.Is(err, io.EOF) {
		b.observation.update(func(entry *requestAttemptRecord) {
			entry.outcome, entry.failure = "error", attemptErrorClass(err, "stream_read")
		})
	}
	return n, err
}

func (b *attemptDiagnosticBody) Close() error {
	err := b.ReadCloser.Close()
	b.observation.update(func(entry *requestAttemptRecord) {
		entry.duration = max(time.Since(b.observation.p.start)-entry.started, 0)
	})
	return err
}

// p.mu is held by the caller. Only the last response owns the logical stream's
// completeness; an earlier replay or rejection cannot be made successful later.
func (p *requestPerformance) finishAttemptTrail(canceled bool) {
	if p.currentAttempt == 0 {
		return
	}
	entry := &p.attemptTrail[p.currentAttempt-1]
	if entry.outcome != "response_headers" && entry.failure != "stream_read" {
		return
	}
	if p.streamSeen && p.streamComplete {
		entry.outcome, entry.failure = "complete", ""
	} else if canceled {
		entry.outcome, entry.failure = "error", "canceled"
	} else if p.streamSeen {
		entry.outcome = "incomplete"
		if entry.failure == "" {
			entry.failure = "stream_incomplete"
		}
	}
}

func attemptTrailViews(entries [requestAttemptCapacity]requestAttemptRecord, count int) []requestAttemptView {
	views := make([]requestAttemptView, 0, count)
	for i := 0; i < count; i++ {
		a := entries[i]
		v := requestAttemptView{Attempt: i + 1, Kind: a.kind, Outcome: a.outcome, StatusCode: a.status,
			FailureClass: a.failure, StartedMS: float64(a.started) / float64(time.Millisecond), DurationMS: float64(a.duration) / float64(time.Millisecond)}
		if a.status != 0 {
			h := float64(a.headers) / float64(time.Millisecond)
			v.HeadersMS = &h
		}
		if a.businessCode != 0 {
			v.BusinessCode = strconv.FormatUint(uint64(a.businessCode), 10)
		}
		views = append(views, v)
	}
	return views
}
