package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// upstreamSSEReader owns framing and transport termination for all three exits.
// Data points at the scanner buffer until Next; adapters retain only what their
// protocol state needs. A terminal marker stops reading even if HTTP stays open.
type upstreamSSEReader struct {
	scanner            *bufio.Scanner
	data               []byte
	comment            bool
	done               bool
	err                error
	comments, metadata int
}

func newUpstreamSSEReader(body io.Reader) *upstreamSSEReader {
	s := bufio.NewScanner(body)
	s.Buffer(make([]byte, 64*1024), 4*1024*1024)
	return &upstreamSSEReader{scanner: s}
}
func (s *upstreamSSEReader) Next() bool {
	if s.done || s.err != nil {
		return false
	}
	for s.scanner.Scan() {
		line := bytes.TrimSpace(s.scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		s.comment = false
		if line[0] == ':' {
			s.comments++
			s.comment = true
			s.data = line
			return true
		}
		if !bytes.HasPrefix(line, []byte("data:")) && line[0] != '{' && !bytes.Equal(line, doneMarker) {
			s.metadata++
			continue
		}
		data := stripDataPrefixBytes(line)
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, doneMarker) {
			s.done = true
			return false
		}
		if rejection := detectUpstreamStreamRejection(data); rejection != nil {
			s.err = rejection
			return false
		}
		if !json.Valid(data) {
			s.err = errInvalidUpstreamChunk
			return false
		}
		s.data = data
		return true
	}
	s.err = s.scanner.Err()
	return false
}
func (s *upstreamSSEReader) Err() error { return s.err }

// A common write/cancellation/heartbeat lifetime; adapters supply protocol
// encoding and error envelopes. Chat may replace guard's body during replay.
type modelStreamSession struct {
	request   *http.Request
	guard     *streamWriteGuard
	heartbeat *streamHeartbeat
	cancel    context.CancelFunc
}

func startModelStream(w http.ResponseWriter, r *http.Request, body io.ReadCloser) (*modelStreamSession, bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	guard := &streamWriteGuard{w: w, flusher: flusher, cancel: cancel}
	performanceFor(r).setLiveStream(true)
	guard.setBody(body)
	return &modelStreamSession{request: r, guard: guard, heartbeat: startStreamHeartbeat(guard, currentStreamKeepalive()), cancel: cancel}, true
}
func (s *modelStreamSession) Close() {
	s.heartbeat.Stop()
	s.cancel()
	performanceFor(s.request).setLiveStream(false)
}
