package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const restrictedReplayDelayDefault = 2 * time.Minute

// Each request snapshots this value. Tests shorten the delay on synthetic HTTP
// servers; there is no background replay queue or persisted request body.
var restrictedReplayDelay = restrictedReplayDelayDefault

type restrictedReplayKey struct{}

type restrictedFailure struct {
	status  int
	info    upstreamRejection
	message string
}

func (f *restrictedFailure) write(w http.ResponseWriter, r *http.Request) {
	if f.info.Code == "" {
		writeUpstreamError(w, r, f.status, "upstream_error", f.message)
		return
	}
	writeUpstreamBusinessRejection(w, r, f.status, f.info, f.message)
}

// This state belongs to one incoming request, including its existing network
// replay paths. Only the initial upstream selection may delay a restriction;
// after an upstream response is accepted, no restriction can trigger a replay.
type restrictedReplay struct {
	r         *http.Request
	w         http.ResponseWriter
	model     string
	stream    bool
	allowed   bool
	used      bool
	delay     time.Duration
	failure   *restrictedFailure
	cancel    context.CancelFunc
	guard     *streamWriteGuard
	heartbeat *streamHeartbeat
}

func withRestrictedReplay(w http.ResponseWriter, r *http.Request, model string, stream bool) (*http.Request, *restrictedReplay) {
	ctx, cancel := context.WithCancel(r.Context())
	s := &restrictedReplay{w: w, model: model, stream: stream, allowed: true, delay: restrictedReplayDelay, cancel: cancel}
	r = r.WithContext(context.WithValue(ctx, restrictedReplayKey{}, s))
	s.r = r
	return r, s
}

func restrictedReplayFor(r *http.Request) *restrictedReplay {
	s, _ := r.Context().Value(restrictedReplayKey{}).(*restrictedReplay)
	return s
}

func (s *restrictedReplay) Close() {
	s.stopHeartbeat()
	s.cancel()
}

func (s *restrictedReplay) stopHeartbeat() {
	if s.heartbeat != nil {
		s.heartbeat.Stop()
		performanceFor(s.r).setLiveStream(false)
	}
}

func (s *restrictedReplay) canWait(model string, attempted map[*Account]bool) bool {
	if s == nil || !s.allowed || s.used || s.r.Context().Err() != nil {
		return false
	}
	// Do not hold a retry permit or an account reservation during the delay.
	// The normal scheduler and retry budget check eligibility again at send time.
	if scope := retryScopeFor(s.r); scope != nil {
		scope.mu.Lock()
		available := !scope.closed && scope.extras < scope.config.extra
		scope.mu.Unlock()
		if !available {
			return false
		}
	}
	_, _, err := pickAccountForModel(model, attempted, "", false, false, nil)
	return err == nil
}

func (s *restrictedReplay) wait() error {
	s.used = true
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	performanceFor(s.r).setLivePhase(liveAccount)
	log.Printf("[受限请求等待] traceId=%s business_code=11140 delay=%v max_fallbacks=1 说明=账号名额已释放；只在原请求仍连接时尝试其他账号，不恢复受限账号",
		debugTraceID(s.r), s.delay)
	if s.stream {
		flusher, ok := s.w.(http.Flusher)
		if !ok {
			return http.ErrNotSupported
		}
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("Connection", "keep-alive")
		s.w.Header().Set("X-Accel-Buffering", "no")
		s.guard = &streamWriteGuard{w: s.w, flusher: flusher, cancel: s.cancel}
		// Flush immediately, then keep the connection alive through both the
		// cooldown and the following account's response-header wait.
		if err := s.guard.writeUpstreamComment(": keepalive"); err != nil {
			return err
		}
		performanceFor(s.r).setLiveStream(true)
		interval := currentStreamKeepalive()
		if interval <= 0 {
			interval = streamKeepaliveDefault
		}
		s.heartbeat = startStreamHeartbeat(s.guard, interval)
	}
	select {
	case <-s.r.Context().Done():
		return s.r.Context().Err()
	case <-timer.C:
		return s.r.Context().Err()
	}
}

// Errors remain HTTP/JSON unless a cooldown heartbeat has committed SSE/200.
// Capture the existing error writers so that this case gets a protocol error
// event, never a second HTTP status or a JSON document inserted into SSE.
func (s *restrictedReplay) upstreamChat(reqID uint64, upstreamBytes []byte, start time.Time) (*http.Response, *Account, *upstreamProfile, bool) {
	captured := newReplayResponseWriter(s.w.Header())
	resp, acc, prof, ok := upstreamChat(captured, s.r, reqID, s.model, upstreamBytes, start)
	s.allowed = false
	s.stopHeartbeat()
	if s.r.Context().Err() != nil && firstTokenFor(s.r).err() == nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, nil, nil, false
	}
	if ok {
		return resp, acc, prof, true
	}
	if s.guard == nil {
		for name, values := range captured.header {
			s.w.Header()[name] = append([]string(nil), values...)
		}
		status := captured.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		s.w.WriteHeader(status)
		_, _ = s.w.Write(captured.body.Bytes())
		return nil, nil, nil, false
	}
	// Keep settlement status independent of the SSE transport's committed 200.
	statsFinishAttempt(s.r, captured.status)
	var payload map[string]any
	if json.Unmarshal(captured.body.Bytes(), &payload) != nil {
		payload = map[string]any{"error": map[string]any{"type": "upstream_error", "code": "restricted_replay_failed", "message": "等待后未取得可用上游响应"}}
	}
	event := ""
	switch {
	case strings.HasSuffix(s.r.URL.Path, "/messages"):
		event = "error"
		payload["type"] = event
	case strings.HasSuffix(s.r.URL.Path, "/responses"):
		event = "response.failed"
		detail, _ := payload["error"].(map[string]any)
		failed := buildResponsesEnvelopeForRequest(s.r, "resp_"+uuid.NewString(), s.model, time.Now().Unix())
		failed["status"] = "failed"
		failed["error"] = map[string]any{"code": fmt.Sprint(detail["code"]), "message": detail["message"]}
		payload = map[string]any{"type": event, "sequence_number": 0, "response": failed}
	}
	data, _ := json.Marshal(payload)
	frame := []byte("data: " + string(data) + "\n\n")
	if event != "" {
		frame = append([]byte("event: "+event+"\n"), frame...)
	}
	_ = s.guard.writeFrame(frame)
	return nil, nil, nil, false
}
