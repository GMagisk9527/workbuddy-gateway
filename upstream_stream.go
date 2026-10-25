package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// upstreamSSEReader owns framing and transport termination for all three exits.
// Single-data events point into the scanner buffer; multiline data is joined in
// a reusable buffer. All returned bytes are valid until Next.
type upstreamSSEReader struct {
	scanner                     *bufio.Scanner
	reader                      io.Reader
	data                        []byte
	comment                     bool
	done                        bool
	err                         error
	comments, metadata          int
	frame, payload, joined      []byte
	fields                      int
	first, bare, unterminated   bool
	splitOffset, splitLineStart int
	skipLF                      bool
}

var errIncompleteUpstreamEvent = errors.New("上游 SSE 事件未以空行结束，本次回复不完整")

func newUpstreamSSEReader(body io.Reader) *upstreamSSEReader {
	s := bufio.NewScanner(body)
	s.Buffer(make([]byte, 64*1024), 4*1024*1024)
	r := &upstreamSSEReader{scanner: s, reader: body, first: true}
	s.Split(r.splitFrame)
	return r
}
func (s *upstreamSSEReader) Next() bool {
	if s.done || s.err != nil {
		return false
	}
	for {
		if err := firstTokenForReader(s.reader).err(); err != nil {
			s.err = err
			return false
		}
		if len(s.frame) > 0 {
			if s.bare {
				data := s.frame
				s.frame = nil
				return s.accept(data)
			}
			line, rest := takeSSELine(s.frame)
			s.frame = rest
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			if line[0] == ':' {
				s.comments++
				s.comment, s.data = true, line
				return true
			}
			var value []byte
			if bytes.HasPrefix(line, []byte("data:")) {
				value = line[len("data:"):]
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
			} else if !bytes.Equal(line, []byte("data")) {
				s.metadata++
				continue
			}
			if s.fields == 0 {
				s.payload = value
			} else {
				if s.fields == 1 {
					s.joined = append(s.joined[:0], s.payload...)
				}
				s.joined = append(s.joined, '\n')
				s.joined = append(s.joined, value...)
				s.payload = s.joined
			}
			s.fields++
			continue
		}
		if s.fields > 0 {
			data := s.payload
			s.fields, s.payload = 0, nil
			if s.unterminated {
				s.err = errIncompleteUpstreamEvent
				return false
			}
			if len(bytes.TrimSpace(data)) > 0 {
				return s.accept(data)
			}
		}
		if !s.scanner.Scan() {
			s.err = s.scanner.Err()
			if s.err == nil {
				s.err = firstTokenForReader(s.reader).finish()
			}
			return false
		}
		s.frame = s.scanner.Bytes()
		if s.first {
			s.frame = bytes.TrimPrefix(s.frame, []byte{0xef, 0xbb, 0xbf})
			s.first = false
		}
	}
}

func (s *upstreamSSEReader) accept(data []byte) bool {
	data = stripDataPrefixBytes(data)
	s.comment = false
	if bytes.Equal(data, doneMarker) {
		if err := firstTokenForReader(s.reader).finish(); err != nil {
			s.err = err
			return false
		}
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

func takeSSELine(data []byte) (line, rest []byte) {
	i := indexSSELineEnd(data)
	if i < 0 {
		return data, nil
	}
	end := i + 1
	if data[i] == '\r' && end < len(data) && data[end] == '\n' {
		end++
	}
	return data[:i], data[end:]
}

// Use the optimized single-byte searches in bounded windows. Searching the
// entire remaining buffer twice would become quadratic for CR-only streams.
func indexSSELineEnd(data []byte) int {
	for offset := 0; offset < len(data); {
		end := min(offset+256, len(data))
		window := data[offset:end]
		lf := bytes.IndexByte(window, '\n')
		beforeLF := window
		if lf >= 0 {
			beforeLF = window[:lf]
		}
		if cr := bytes.IndexByte(beforeLF, '\r'); cr >= 0 {
			return offset + cr
		}
		if lf >= 0 {
			return offset + lf
		}
		offset = end
	}
	return -1
}

// Resume scanning at the previous offset when a frame spans network reads.
// The scanner bounds the entire event, including metadata, to 4 MiB.
func (s *upstreamSSEReader) splitFrame(data []byte, atEOF bool) (int, []byte, error) {
	if s.skipLF && s.splitOffset < len(data) {
		s.skipLF = false
		if data[s.splitOffset] == '\n' {
			if s.splitOffset == 0 && s.splitLineStart == 0 {
				return 1, nil, nil
			}
			s.splitOffset++
			s.splitLineStart++
		}
	}
	for s.splitOffset < len(data) {
		i := indexSSELineEnd(data[s.splitOffset:])
		if i < 0 {
			s.splitOffset = len(data)
			break
		}
		i += s.splitOffset
		end := i + 1
		s.skipLF = data[i] == '\r' && end == len(data) && !atEOF
		if data[i] == '\r' && end < len(data) && data[end] == '\n' {
			end++
		}
		line := data[s.splitLineStart:i]
		if s.first && s.splitLineStart == 0 {
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
		}
		trimmed := bytes.TrimSpace(line)
		bare := s.splitLineStart == 0 && (bytes.HasPrefix(trimmed, []byte("{")) || bytes.Equal(trimmed, doneMarker))
		comment := s.splitLineStart == 0 && bytes.HasPrefix(trimmed, []byte(":"))
		if len(line) == 0 || bare || comment {
			tokenEnd := s.splitLineStart
			if bare || comment {
				tokenEnd = i
			}
			s.splitOffset, s.splitLineStart = 0, 0
			s.bare, s.unterminated = bare, false
			return end, data[:tokenEnd], nil
		}
		s.splitOffset, s.splitLineStart = end, end
	}
	if atEOF && len(data) > 0 {
		line := bytes.TrimSpace(data)
		if s.first {
			line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
		}
		s.bare = s.splitLineStart == 0 && (bytes.HasPrefix(line, []byte("{")) || bytes.Equal(line, doneMarker))
		s.unterminated = !s.bare
		s.splitOffset, s.splitLineStart = 0, 0
		return len(data), data, nil
	}
	return 0, nil, nil
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
