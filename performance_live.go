package main

import (
	"net/http"
	"sync"
	"time"
)

type livePhase uint8

const (
	livePreparing livePhase = iota
	liveAdmission
	liveAccount
	liveCredential
	liveFirstToken
	liveResponding
	liveSettlement
	livePhaseCount
)

var livePhaseNames = [...]string{"preparing", "admission_wait", "account_wait", "credential_wait", "first_token_wait", "responding", "settling"}
var livePhaseLabels = [...]string{"鉴权、读取与准备", "准入获取与排队", "账号调度", "凭据等待", "等待首个有效增量", "处理响应", "结算收尾"}

// Intrusive lists retain only live request timing states, no labels or bodies.
// No extra map entry/allocation per request. Only lifecycle transitions take
// this mutex; writing a chunk only changes its request's atomic timestamp.
type livePerformance struct {
	mu    sync.Mutex
	heads [len(performanceProtocols)]*requestPerformance
}

func (s *livePerformance) begin(p *requestPerformance) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.live, p.liveRegistered, p.liveSince = s, true, p.start
	p.liveNext = s.heads[p.protocol]
	if p.liveNext != nil {
		p.liveNext.livePrev = p
	}
	s.heads[p.protocol] = p
}

func (p *requestPerformance) setLivePhase(phase livePhase) {
	if p == nil || p.live == nil {
		return
	}
	s := p.live
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.liveRegistered && p.livePhase != phase {
		if phase == liveFirstToken && p.liveFirstToken {
			phase = liveResponding
		}
		p.livePhase, p.liveSince = phase, time.Now()
	}
}
func (p *requestPerformance) firstLiveToken() {
	if p == nil || p.live == nil {
		return
	}
	s := p.live
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.liveRegistered {
		p.liveFirstToken = true
		p.livePhase, p.liveSince = liveResponding, time.Now()
	}
}
func (p *requestPerformance) setLiveStream(stream bool) {
	if p == nil || p.live == nil {
		return
	}
	p.live.mu.Lock()
	defer p.live.mu.Unlock()
	if p.liveRegistered {
		p.liveStream = stream
	}
}
func (p *requestPerformance) beginLiveWrite(now time.Time) {
	if p != nil {
		p.liveWriteSince.Store(now.UnixNano())
	}
}
func (p *requestPerformance) endLiveWrite() {
	if p != nil {
		p.liveWriteSince.Store(0)
	}
}
func (p *requestPerformance) finishLive() {
	if p == nil || p.live == nil {
		return
	}
	s := p.live
	s.mu.Lock()
	defer s.mu.Unlock()
	if !p.liveRegistered {
		return
	}
	if p.livePrev != nil {
		p.livePrev.liveNext = p.liveNext
	} else {
		s.heads[p.protocol] = p.liveNext
	}
	if p.liveNext != nil {
		p.liveNext.livePrev = p.livePrev
	}
	p.liveRegistered, p.livePrev, p.liveNext = false, nil, nil
}

type liveStageSnapshot struct {
	Name      string   `json:"name"`
	Label     string   `json:"label"`
	Requests  uint64   `json:"requests"`
	LongestMS *float64 `json:"longestMs"`
}
type liveProtocolSnapshot struct {
	Active            uint64              `json:"active"`
	Streams           uint64              `json:"activeStreams"`
	WaitingFirstToken uint64              `json:"waitingFirstToken"`
	Writing           uint64              `json:"writingDownstream"`
	OldestMS          *float64            `json:"oldestRequestMs"`
	LongestWriteMS    *float64            `json:"longestWriteMs"`
	Stages            []liveStageSnapshot `json:"stages"`
}

func liveMax(dst **float64, duration time.Duration) {
	ms := float64(max(duration, 0)) / float64(time.Millisecond)
	if *dst == nil || ms > **dst {
		*dst = &ms
	}
}
func (s *livePerformance) snapshot(now time.Time) [len(performanceProtocols) + 1]liveProtocolSnapshot {
	var out [len(performanceProtocols) + 1]liveProtocolSnapshot
	for i := range out {
		for j := range livePhaseCount {
			out[i].Stages = append(out[i].Stages, liveStageSnapshot{Name: livePhaseNames[j], Label: livePhaseLabels[j]})
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for protocol, head := range s.heads {
		for p := head; p != nil; p = p.liveNext {
			for _, index := range [2]int{0, protocol + 1} {
				v := &out[index]
				v.Active++
				if p.liveStream {
					v.Streams++
				}
				if p.livePhase == liveFirstToken {
					v.WaitingFirstToken++
				}
				liveMax(&v.OldestMS, now.Sub(p.start))
				stage := &v.Stages[p.livePhase]
				stage.Requests++
				liveMax(&stage.LongestMS, now.Sub(p.liveSince))
				if start := p.liveWriteSince.Load(); start != 0 {
					v.Writing++
					liveMax(&v.LongestWriteMS, now.Sub(time.Unix(0, start)))
				}
			}
		}
	}
	return out
}

type liveRuntimeSnapshot struct {
	AdmissionActive     int    `json:"admissionActive"`
	AdmissionLimit      int    `json:"admissionLimit"`
	AdmissionWaiting    int    `json:"admissionWaiting"`
	AdmissionQueueLimit int    `json:"admissionQueueLimit"`
	BodyBytes           int64  `json:"bodyBytes"`
	BodyBudget          int64  `json:"bodyBudget"`
	MemoryPressure      bool   `json:"memoryPressure"`
	Draining            bool   `json:"draining"`
	OutputBytes         int64  `json:"outputBytes"`
	OutputBudget        int64  `json:"outputBudget"`
	OutputRejected      uint64 `json:"outputRejected"`
	RetryActive         int    `json:"retryActive"`
	RetryLimit          int    `json:"retryLimit"`
	RetryRejected       uint64 `json:"retryRejected"`
	LogSampled          uint64 `json:"logSampled"`
	LogRejected         uint64 `json:"logRejected"`
}

func snapshotLiveRuntime(r *http.Request) liveRuntimeSnapshot {
	l, active, waiting := ingressAdmission.snapshot()
	used, budget, pressure := ingressAdmission.memory.snapshot()
	out := liveRuntimeSnapshot{AdmissionActive: active, AdmissionLimit: l.active, AdmissionWaiting: waiting, AdmissionQueueLimit: l.waiting, BodyBytes: used, BodyBudget: budget, MemoryPressure: pressure}
	sample := retainedOutputs.sample()
	retainedOutputs.mu.Lock()
	_, out.OutputBudget = retainedOutputs.limitsLocked(sample)
	out.OutputBytes, out.OutputRejected = retainedOutputs.used, retainedOutputs.rejected
	retainedOutputs.mu.Unlock()
	upstreamRetries.mu.Lock()
	out.RetryActive, out.RetryLimit, out.RetryRejected = upstreamRetries.active, upstreamRetries.allowanceLocked(), upstreamRetries.denied
	upstreamRetries.mu.Unlock()
	routineLogs.mu.Lock()
	out.LogSampled = routineLogs.skipped
	out.LogRejected = logBudgetRejected.Load()
	routineLogs.mu.Unlock()
	if life := lifecycleFor(r); life != nil {
		out.Draining = life.isDraining()
	}
	return out
}
