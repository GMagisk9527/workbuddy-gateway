package main

import (
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"
)

type performanceTraceSpan struct {
	starts, ends     uint64
	startSum, endSum time.Duration
	failed           bool
}

// End offsets minus start offsets account for parallel dials in any completion
// order. Storage is fixed; no hosts, addresses or error strings are kept.
type performanceTrace struct {
	mu                    sync.Mutex
	p                     *requestPerformance
	start                 time.Time
	spans                 [performancePhaseCount]performanceTraceSpan
	closed, firstResponse bool
}

func (t *performanceTrace) begin(phase performancePhase) {
	s := &t.spans[phase]
	s.starts++
	s.startSum += time.Since(t.start)
}
func (t *performanceTrace) end(phase performancePhase, failed bool) {
	s := &t.spans[phase]
	if s.ends >= s.starts {
		return
	}
	s.ends++
	s.endSum += time.Since(t.start)
	s.failed = s.failed || failed
}
func (t *performanceTrace) startPhase(phase performancePhase) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.begin(phase)
	}
}
func (t *performanceTrace) stopPhase(phase performancePhase, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.end(phase, failed)
	}
}
func (t *performanceTrace) finish(failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	elapsed := time.Since(t.start)
	for phase, s := range t.spans {
		if s.starts == 0 {
			continue
		}
		if s.ends != s.starts {
			if !failed {
				// Omit incomplete callbacks on early responses, not a fake zero.
				t.p.mu.Lock()
				if !t.p.closed {
					t.p.counters.DroppedTraceEvents += s.starts - s.ends
				}
				t.p.mu.Unlock()
				continue
			}
			s.endSum += time.Duration(s.starts-s.ends) * elapsed
			s.failed = true
		}
		t.p.observe(performancePhase(phase), max(s.endSum-s.startSum, 0), s.failed)
	}
}
func newPerformanceTrace(p *requestPerformance) (*performanceTrace, *httptrace.ClientTrace) {
	t := &performanceTrace{p: p, start: time.Now()}
	hooks := &httptrace.ClientTrace{
		GetConn: func(string) { t.startPhase(phaseConnection) },
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.closed {
				return
			}
			t.end(phaseConnection, false)
			t.firstResponse = false // A redirect can acquire another connection.
			t.begin(phaseUpload)
			p.mu.Lock()
			if !p.closed {
				p.counters.Connections++
				if info.Reused {
					p.counters.ReusedConnections++
				}
			}
			p.mu.Unlock()
		},
		DNSStart:          func(httptrace.DNSStartInfo) { t.startPhase(phaseDNS) },
		DNSDone:           func(info httptrace.DNSDoneInfo) { t.stopPhase(phaseDNS, info.Err != nil) },
		ConnectStart:      func(string, string) { t.startPhase(phaseTCP) },
		ConnectDone:       func(_, _ string, err error) { t.stopPhase(phaseTCP, err != nil) },
		TLSHandshakeStart: func() { t.startPhase(phaseTLS) },
		TLSHandshakeDone:  func(_ tls.ConnectionState, err error) { t.stopPhase(phaseTLS, err != nil) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.closed {
				return
			}
			t.end(phaseUpload, info.Err != nil)
			if info.Err == nil {
				if t.firstResponse {
					t.spans[phaseResponseWait].starts++
					t.spans[phaseResponseWait].ends++
				} else {
					t.begin(phaseResponseWait)
				}
			}
		},
		GotFirstResponseByte: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.closed {
				t.firstResponse = true
				t.end(phaseResponseWait, false)
			}
		},
	}
	return t, hooks
}
