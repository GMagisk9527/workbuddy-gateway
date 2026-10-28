package main

import "time"

const requestDiagnosticCapacity = 128
const requestDiagnosticTTL = 5 * time.Minute

// Fixed storage, no Key/model/account/prompt/URL/error-text dimensions.
type requestDiagnostic struct {
	trace            string
	protocol, status int
	at               time.Time
	counters         performanceCounters
	cells            [performancePhaseCount]performanceCell
}
type requestDiagnosticRing struct {
	items       [requestDiagnosticCapacity]requestDiagnostic
	next, count int
}
type requestDiagnosticView struct {
	TraceID  string              `json:"traceId"`
	Protocol string              `json:"protocol"`
	Status   int                 `json:"status"`
	At       int64               `json:"at"`
	Counters performanceCounters `json:"counters"`
	Phases   map[string]float64  `json:"phaseDurationsMs"`
}

func validDiagnosticTrace(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (s *performanceStore) rememberRequestLocked(trace string, protocol, status int, at time.Time, counters performanceCounters, cells [performancePhaseCount]performanceCell) {
	if !validDiagnosticTrace(trace) || protocol < 0 || protocol >= len(performanceProtocols) {
		return
	}
	r := &s.recent
	r.items[r.next] = requestDiagnostic{trace, protocol, status, at, counters, cells}
	r.next = (r.next + 1) % len(r.items)
	r.count = min(r.count+1, len(r.items))
}

func (s *performanceStore) requestDiagnostic(trace string, now time.Time) (requestDiagnosticView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n := 0; n < s.recent.count; n++ {
		d := &s.recent.items[(s.recent.next-1-n+requestDiagnosticCapacity)%requestDiagnosticCapacity]
		if now.Before(d.at) || now.Sub(d.at) > requestDiagnosticTTL {
			continue
		}
		if d.trace != trace {
			continue
		}
		out := requestDiagnosticView{TraceID: d.trace, Protocol: performanceProtocols[d.protocol], Status: d.status, At: d.at.UnixMilli(), Counters: d.counters, Phases: make(map[string]float64)}
		for i, c := range d.cells {
			if c.seen {
				out.Phases[performancePhaseNames[i]] = float64(c.duration) / float64(time.Millisecond)
			}
		}
		return out, true
	}
	return requestDiagnosticView{}, false
}
