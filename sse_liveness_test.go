package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStreamHeartbeatResumesAfterModelData(t *testing.T) {
	w := newLockedResponseWriter()
	g := &streamWriteGuard{w: w, flusher: w}
	h := startStreamHeartbeat(g, 5*time.Millisecond)
	defer h.Stop()
	if err := g.writeData(`{"choices":[{"delta":{"content":"first"}}]}`); err != nil {
		t.Fatal(err)
	}
	waitForTestCondition(t, 300*time.Millisecond, func() bool { return strings.Contains(w.String(), ": keepalive") })
	if !g.hasData() {
		t.Fatal("quiet-period heartbeat reset replay safety state")
	}
	h.Stop()
	count := strings.Count(w.String(), ": keepalive")
	time.Sleep(20 * time.Millisecond)
	if strings.Count(w.String(), ": keepalive") != count {
		t.Fatal("heartbeat survived Stop")
	}
}

func TestHeartbeatReschedulesFromLastWrite(t *testing.T) {
	last := time.Now()
	g := &streamWriteGuard{lastWrite: last, heartbeatInterval: 15 * time.Second}
	if delay, active := g.nextHeartbeatDelay(last.Add(10 * time.Second)); !active || delay != 5*time.Second {
		t.Fatalf("recent data postponed heartbeat by a whole extra interval: %v", delay)
	}
	g.stopHeartbeats()
	if _, active := g.nextHeartbeatDelay(last); active {
		t.Fatal("terminal state scheduled another heartbeat")
	}
}

type shortSSEReads struct {
	reader io.Reader
	size   int
}

func (r shortSSEReads) Read(p []byte) (int, error) { return r.reader.Read(p[:min(len(p), r.size)]) }

func TestUpstreamSSEFragmentedWireAndCRTerminal(t *testing.T) {
	for _, eol := range []string{"\n", "\r\n", "\r"} {
		wire := "\xef\xbb\xbf: keepalive" + eol + "data: {" + eol + "data: \"value\":1}" + eol + eol + "data: [DONE]" + eol + eol
		for _, size := range []int{1, 2, 3, 7, 64} {
			r := newUpstreamSSEReader(shortSSEReads{strings.NewReader(wire), size})
			n := 0
			for r.Next() {
				if !r.comment {
					n++
				}
			}
			if r.Err() != nil || n != 1 || !r.done {
				t.Fatalf("fragmentation changed SSE: eol=%q size=%d n=%d err=%v", eol, size, n, r.Err())
			}
		}
		terminal := &commonStreamTerminalReader{data: []byte("data: {\"value\":1}" + eol + eol + "data: [DONE]" + eol + eol)}
		r := newUpstreamSSEReader(terminal)
		for r.Next() {
		}
		if r.Err() != nil || terminal.afterDone || !r.done {
			t.Fatalf("read beyond terminal event %q: %v", eol, r.Err())
		}
	}
}

func TestUpstreamSSEWholeEventIsBounded(t *testing.T) {
	chunk := "data: " + strings.Repeat(" ", 128<<10) + "\n"
	r := newUpstreamSSEReader(strings.NewReader("data: {\n" + strings.Repeat(chunk, 33) + "data: }\n\n"))
	if r.Next() || r.Err() == nil {
		t.Fatal("multiline event bypassed the shared 4MiB framing limit")
	}
}

func TestFirstByteReplayStripsOnlyNewStreamsBOM(t *testing.T) {
	first := &resilienceTrackingBody{Reader: &dataThenErrorReader{data: []byte("\xef\xbb\xbfevent: message\n"), err: errors.New("read: connection reset by peer")}}
	second := &resilienceTrackingBody{Reader: strings.NewReader("\xef\xbb\xbfdata: {\"value\":1}\n\ndata: [DONE]\n\n")}
	calls := 0
	body := newFirstByteReplayBody(first, context.Background(), nil, 1, "synthetic", 1, func(context.Context) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: second}, nil
	})
	r := newUpstreamSSEReader(body)
	n := 0
	for r.Next() {
		if !r.comment {
			n++
		}
	}
	_ = body.Close()
	if r.Err() != nil || n != 1 || calls != 1 || !first.closed.Load() || !second.closed.Load() {
		t.Fatalf("metadata/replay BOM damaged events: n=%d calls=%d err=%v", n, calls, r.Err())
	}
}

func FuzzUpstreamSSEFragmentation(f *testing.F) {
	f.Add("synthetic text", uint8(1), uint8(0), true)
	f.Add("引号\"和换行\n", uint8(7), uint8(1), false)
	f.Add("", uint8(2), uint8(2), true)
	f.Fuzz(func(t *testing.T, value string, size, lineEnding uint8, bom bool) {
		if len(value) > 2048 {
			t.Skip()
		}
		encoded, _ := json.Marshal(value)
		var want string
		_ = json.Unmarshal(encoded, &want)
		eol := []string{"\n", "\r\n", "\r"}[int(lineEnding)%3]
		wire := "data: {" + eol + "data: \"content\":" + string(encoded) + eol + "data: }" + eol + eol + "data: [DONE]" + eol + eol
		if bom {
			wire = "\xef\xbb\xbf" + wire
		}
		r := newUpstreamSSEReader(shortSSEReads{strings.NewReader(wire), 1 + int(size)%64})
		n := 0
		for r.Next() {
			if r.comment {
				continue
			}
			var got map[string]string
			if json.Unmarshal(r.data, &got) != nil || got["content"] != want {
				t.Fatal("event payload changed")
			}
			n++
		}
		if r.Err() != nil || n != 1 || !r.done {
			t.Fatalf("fragmented event lost: count=%d err=%v", n, r.Err())
		}
	})
}

func TestUpstreamSSEEventFraming(t *testing.T) {
	for _, tc := range []struct{ name, stream string }{
		{"single", "data: {\"value\":1}\n\ndata: [DONE]\n\n"},
		{"multiline", "event: message\ndata: {\ndata: \"value\":1\ndata: }\n\ndata: [DONE]\n\n"},
		{"comments_in_event", "data: {\n: heartbeat\nid: synthetic\ndata: \"value\":1}\n\ndata: [DONE]\n\n"},
		{"bom", "\xef\xbb\xbfdata: {\"value\":1}\n\ndata: [DONE]\n\n"},
		{"crlf", "data: {\r\ndata: \"value\":1}\r\n\r\ndata: [DONE]\r\n\r\n"},
		{"cr", "data: {\rdata: \"value\":1}\r\rdata: [DONE]\r\r"},
		{"legacy_bare", "{\"value\":1}\n[DONE]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUpstreamSSEReader(strings.NewReader(tc.stream))
			count := 0
			for r.Next() {
				if r.comment {
					continue
				}
				var value struct {
					Value int `json:"value"`
				}
				if err := json.Unmarshal(r.data, &value); err != nil || value.Value != 1 {
					t.Fatalf("event payload changed: %q %v", r.data, err)
				}
				count++
			}
			if r.Err() != nil || count != 1 || !r.done {
				t.Fatalf("valid SSE event lost: count=%d done=%t err=%v", count, r.done, r.Err())
			}
		})
	}
}

func TestUpstreamSSEUnterminatedEventIsNotDispatched(t *testing.T) {
	r := newUpstreamSSEReader(strings.NewReader("data: {\"choices\":[{\"finish_reason\":\"stop\"}]}\n"))
	if r.Next() || r.Err() == nil {
		t.Fatal("unterminated SSE event was treated as complete")
	}
}

func TestFirstByteDetectorBOMAndCRDoNotPermitReplay(t *testing.T) {
	for _, parts := range [][]string{{"\xef", "\xbb", "\xbfdata: {\"value\":1}\n\n"}, {"event: message\r", "data: {\"value\":1}\r\r"}} {
		d := firstByteDataDetector{}
		seen := false
		for _, part := range parts {
			seen = d.feed([]byte(part)) || seen
		}
		if !seen {
			t.Fatal("valid BOM/CR data was invisible to replay safety detector")
		}
	}
}
