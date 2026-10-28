package main

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type performancePhase uint8

const (
	phaseAdmission performancePhase = iota
	phaseRead
	phaseDecompress
	phaseDecode
	phasePrepare
	phaseAccount
	phaseCredential
	phaseEncode
	phaseConnection
	phaseDNS
	phaseTCP
	phaseTLS
	phaseUpload
	phaseResponseWait
	phaseUpstreamRead
	phaseFirstToken
	phaseDownstream
	phaseSettlement
	phaseTotal
	performancePhaseCount
)

var performancePhaseNames = [...]string{"admission_wait", "request_read", "request_decompress", "json_decode", "request_prepare", "account_wait", "credential_ready", "upstream_encode", "connection_acquire", "dns", "tcp_connect", "tls_handshake", "request_upload", "response_wait", "upstream_read", "first_token", "downstream_write", "settlement", "total"}
var performancePhaseLabels = [...]string{"准入等待", "请求体读取", "请求体解压", "JSON 解码", "请求准备与转换", "账号调度", "凭据就绪", "出站编码", "连接获取", "DNS", "TCP 建连", "TLS 握手", "请求上传", "上传后等待首响应字节", "上游响应体读取", "首个有效增量", "下游写入与刷新", "结算与记录", "总耗时"}
var performanceProtocols = [...]string{"chat_completions", "responses", "messages"}

// Fixed dimensions and five one-minute slots, never model/Key/Trace-ID labels.
const performanceWindowMinutes = 5

var performanceBounds = [...]time.Duration{50 * time.Microsecond, 100 * time.Microsecond, 250 * time.Microsecond, 500 * time.Microsecond, time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute, 30 * time.Minute, time.Hour}

type performanceCell struct {
	duration     time.Duration
	seen, failed bool
}

type performanceCounters struct {
	Requests           uint64 `json:"requests"`
	Failed             uint64 `json:"failed"`
	Canceled           uint64 `json:"canceled"`
	Incomplete         uint64 `json:"incompleteStreams"`
	DownstreamErrors   uint64 `json:"downstreamErrors"`
	Attempts           uint64 `json:"upstreamAttempts"`
	TransportRetries   uint64 `json:"transportRetries"`
	Connections        uint64 `json:"connections"`
	ReusedConnections  uint64 `json:"reusedConnections"`
	DroppedTraceEvents uint64 `json:"droppedTraceEvents"`
}

type requestPerformance struct {
	mu                                           sync.Mutex
	traceID                                      string
	protocol                                     int
	start, preparing                             time.Time
	closed                                       bool
	streamSeen, streamComplete, downstreamFailed bool
	cells                                        [performancePhaseCount]performanceCell
	counters                                     performanceCounters
	live                                         *livePerformance
	livePrev, liveNext                           *requestPerformance
	liveRegistered, liveStream, liveFirstToken   bool
	livePhase                                    livePhase
	liveSince                                    time.Time
	liveWriteSince                               atomic.Int64
}

type requestPerformanceKey struct{}

func performanceProtocol(r *http.Request) int {
	if r.Method != http.MethodPost {
		return -1
	}
	switch r.URL.Path {
	case "/v1/chat/completions", "/chat/completions":
		return 0
	case "/v1/responses", "/responses":
		return 1
	case "/v1/messages":
		return 2
	}
	return -1
}

func withRequestPerformance(r *http.Request, start time.Time) (*http.Request, *requestPerformance) {
	protocol := performanceProtocol(r)
	if protocol < 0 {
		return r, nil
	}
	p := &requestPerformance{protocol: protocol, start: start}
	if trace := debugTraceID(r); validDiagnosticTrace(trace) {
		p.traceID = trace
	}
	phaseMetrics.live.begin(p)
	return r.WithContext(context.WithValue(r.Context(), requestPerformanceKey{}, p)), p
}

func performanceFor(r *http.Request) *requestPerformance {
	if r == nil {
		return nil
	}
	p, _ := r.Context().Value(requestPerformanceKey{}).(*requestPerformance)
	return p
}

func performanceStarted(r *http.Request) time.Time {
	if performanceFor(r) == nil {
		return time.Time{}
	}
	return time.Now()
}

func recordPerformance(r *http.Request, phase performancePhase, start time.Time, failed bool) {
	if !start.IsZero() {
		performanceFor(r).observe(phase, time.Since(start), failed)
	}
}

func (p *requestPerformance) observe(phase performancePhase, duration time.Duration, failed bool) {
	if p == nil || phase >= performancePhaseCount || duration < 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	c := &p.cells[phase]
	// First effective increment is a logical-request latency, never summed on replay.
	if phase == phaseFirstToken && c.seen {
		return
	}
	c.duration += duration
	c.seen, c.failed = true, c.failed || failed
	if phase == phaseFirstToken {
		p.firstLiveToken()
	}
}

func (p *requestPerformance) beginPrepare() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed && p.preparing.IsZero() {
		p.preparing = time.Now()
	}
}

func (p *requestPerformance) endPrepare(failed bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	start := p.preparing
	p.preparing = time.Time{}
	p.mu.Unlock()
	if !start.IsZero() {
		p.observe(phasePrepare, time.Since(start), failed)
	}
}

func (p *requestPerformance) streamCompletion(complete bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.streamSeen, p.streamComplete = true, complete
	}
}

func (p *requestPerformance) downstreamError() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.downstreamFailed = true
	}
}

func (p *requestPerformance) networkAttempt(retry bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed {
		p.counters.Attempts++
		if retry {
			p.counters.TransportRetries++
		}
	}
}

type performanceHistogram struct {
	buckets       [len(performanceBounds) + 1]uint64
	count, failed uint64
	sum, max      time.Duration
}

func (h *performanceHistogram) add(c performanceCell) {
	if !c.seen {
		return
	}
	i := 0
	for i < len(performanceBounds) && c.duration > performanceBounds[i] {
		i++
	}
	h.buckets[i]++
	h.count++
	h.sum += c.duration
	if c.failed {
		h.failed++
	}
	h.max = max(h.max, c.duration)
}

func (h *performanceHistogram) merge(other performanceHistogram) {
	for i, n := range other.buckets {
		h.buckets[i] += n
	}
	h.count += other.count
	h.failed += other.failed
	h.sum += other.sum
	h.max = max(h.max, other.max)
}

func (h performanceHistogram) percentile(percent uint64) *float64 {
	if h.count == 0 {
		return nil
	}
	target, seen := (h.count*percent+99)/100, uint64(0)
	for i, n := range h.buckets {
		seen += n
		if seen >= target {
			d := h.max
			if i < len(performanceBounds) {
				d = min(d, performanceBounds[i])
			}
			v := float64(d) / float64(time.Millisecond)
			return &v
		}
	}
	return nil
}

type performanceAggregate struct {
	counters performanceCounters
	phases   [performancePhaseCount]performanceHistogram
}

func (a *performanceAggregate) merge(b performanceAggregate) {
	a.counters.merge(b.counters)
	for i := range a.phases {
		a.phases[i].merge(b.phases[i])
	}
}

func (a *performanceCounters) merge(b performanceCounters) {
	a.Requests += b.Requests
	a.Failed += b.Failed
	a.Canceled += b.Canceled
	a.Incomplete += b.Incomplete
	a.DownstreamErrors += b.DownstreamErrors
	a.Attempts += b.Attempts
	a.TransportRetries += b.TransportRetries
	a.Connections += b.Connections
	a.ReusedConnections += b.ReusedConnections
	a.DroppedTraceEvents += b.DroppedTraceEvents
}

type performanceMinute struct {
	minute    int64
	protocols [len(performanceProtocols)]performanceAggregate
}

type performanceStore struct {
	mu      sync.Mutex
	started time.Time
	minutes [performanceWindowMinutes]performanceMinute
	live    livePerformance
	recent  requestDiagnosticRing
}

var phaseMetrics = &performanceStore{started: time.Now()}

// Only this final merge touches global state. Streaming chunks update their
// own request mutex; network callbacks arriving after completion are ignored.
func (p *requestPerformance) finish(store *performanceStore, status int, canceled bool, now time.Time) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.cells[phaseTotal] = performanceCell{duration: max(now.Sub(p.start), 0), seen: true}
	p.counters.Requests = 1
	// A confirmed complete response must not become a failure merely because
	// the client disconnects while final bookkeeping is finishing.
	if p.streamSeen && p.streamComplete && !p.downstreamFailed && status < 400 {
		canceled = false
	}
	if canceled {
		p.counters.Canceled = 1
	}
	if p.streamSeen && !p.streamComplete {
		p.counters.Incomplete = 1
	}
	if p.downstreamFailed {
		p.counters.DownstreamErrors = 1
	}
	if status >= 400 || canceled || p.counters.Incomplete != 0 || p.downstreamFailed {
		p.counters.Failed = 1
		p.cells[phaseTotal].failed = true
	}
	counters, cells, protocol := p.counters, p.cells, p.protocol
	p.mu.Unlock()
	p.finishLive()
	minute := now.Unix() / 60
	store.mu.Lock()
	defer store.mu.Unlock()
	store.rememberRequestLocked(p.traceID, protocol, status, now, counters, cells)
	b := &store.minutes[(minute%performanceWindowMinutes+performanceWindowMinutes)%performanceWindowMinutes]
	if b.minute > minute {
		return
	} // A delayed old observation cannot evict a newer minute.
	if b.minute != minute {
		*b = performanceMinute{minute: minute}
	}
	a := &b.protocols[protocol]
	a.counters.merge(counters)
	for i, c := range cells {
		a.phases[i].add(c)
	}
}

type performancePhaseSnapshot struct {
	Name      string   `json:"name"`
	Label     string   `json:"label"`
	Samples   uint64   `json:"samples"`
	Failed    uint64   `json:"failed"`
	AverageMS *float64 `json:"averageMs"`
	P95MS     *float64 `json:"p95Ms"`
	P99MS     *float64 `json:"p99Ms"`
}

type performanceProtocolSnapshot struct {
	Protocol string `json:"protocol"`
	performanceCounters
	ReuseRate *float64                   `json:"connectionReuseRate"`
	Phases    []performancePhaseSnapshot `json:"phases"`
	InFlight  liveProtocolSnapshot       `json:"inFlight"`
}

type performanceSnapshot struct {
	StartedAt   int64                         `json:"startedAt"`
	WindowStart int64                         `json:"windowStart"`
	AsOf        int64                         `json:"asOf"`
	Protocols   []performanceProtocolSnapshot `json:"protocols"`
}

func (s *performanceStore) snapshot(now time.Time) performanceSnapshot {
	live := s.live.snapshot(now)
	minute := now.Unix() / 60
	var merged [len(performanceProtocols) + 1]performanceAggregate
	s.mu.Lock()
	for _, b := range s.minutes {
		if b.minute < minute-performanceWindowMinutes+1 || b.minute > minute {
			continue
		}
		for i, a := range b.protocols {
			merged[i+1].merge(a)
			merged[0].merge(a)
		}
	}
	started := s.started
	s.mu.Unlock()
	out := performanceSnapshot{StartedAt: started.UnixMilli(), WindowStart: time.Unix((minute-performanceWindowMinutes+1)*60, 0).UnixMilli(), AsOf: now.UnixMilli()}
	for i, a := range merged {
		name := "all"
		if i > 0 {
			name = performanceProtocols[i-1]
		}
		p := performanceProtocolSnapshot{Protocol: name, performanceCounters: a.counters}
		p.InFlight = live[i]
		if a.counters.Connections > 0 {
			v := float64(a.counters.ReusedConnections) / float64(a.counters.Connections)
			p.ReuseRate = &v
		}
		for j, h := range a.phases {
			v := performancePhaseSnapshot{Name: performancePhaseNames[j], Label: performancePhaseLabels[j], Samples: h.count, Failed: h.failed, P95MS: h.percentile(95), P99MS: h.percentile(99)}
			if h.count > 0 {
				avg := float64(h.sum) / float64(h.count) / float64(time.Millisecond)
				v.AverageMS = &avg
			}
			p.Phases = append(p.Phases, v)
		}
		out.Protocols = append(out.Protocols, p)
	}
	return out
}

func handleWebUIPerformance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "性能指标只支持 GET")
		return
	}
	if trace := r.URL.Query().Get("trace"); trace != "" {
		if !validDiagnosticTrace(trace) {
			writeJSONError(w, http.StatusBadRequest, "Trace ID 格式无效")
			return
		}
		entry, ok := phaseMetrics.requestDiagnostic(trace, time.Now())
		if !ok {
			writeJSONError(w, http.StatusNotFound, "近期未找到该请求；可能仍在执行、已过期或被有界记录淘汰")
			return
		}
		writeAdminData(w, r, entry)
		return
	}
	writeAdminData(w, r, struct {
		performanceSnapshot
		Runtime liveRuntimeSnapshot `json:"runtime"`
	}{phaseMetrics.snapshot(time.Now()), snapshotLiveRuntime(r)})
}

func (p *requestPerformance) debugFields() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	phases := map[string]float64{}
	for i, c := range p.cells {
		if c.seen {
			phases[performancePhaseNames[i]] = float64(c.duration) / float64(time.Millisecond)
		}
	}
	return map[string]any{"protocol": performanceProtocols[p.protocol], "phase_durations_ms": phases, "upstream_attempts": p.counters.Attempts, "transport_retries": p.counters.TransportRetries}
}
