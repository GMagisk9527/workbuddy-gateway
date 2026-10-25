package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type flushFailureWriter struct {
	header http.Header
	bytes.Buffer
}

type deadlineRecordingWriter struct {
	*httptest.ResponseRecorder
	deadline       time.Time
	armed, cleared int
}

func (w *deadlineRecordingWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	if deadline.IsZero() {
		w.cleared++
	} else {
		w.armed++
	}
	return nil
}
func (w *deadlineRecordingWriter) Write(p []byte) (int, error) {
	if w.deadline.IsZero() || time.Now().After(w.deadline) {
		return 0, context.DeadlineExceeded
	}
	return w.ResponseRecorder.Write(p)
}
func (w *deadlineRecordingWriter) FlushError() error {
	if w.deadline.IsZero() || time.Now().After(w.deadline) {
		return context.DeadlineExceeded
	}
	w.ResponseRecorder.Flush()
	return nil
}
func TestDownstreamDeadlineOnlyCoversActualWrite(t *testing.T) {
	base := &deadlineRecordingWriter{ResponseRecorder: httptest.NewRecorder()}
	writer := &auditResponseWriter{ResponseWriter: &auditResponseWriter{ResponseWriter: base}, writeTimeout: 50 * time.Millisecond}
	for range 2 {
		if _, err := writer.Write([]byte("data: synthetic\n\n")); err != nil {
			t.Fatal(err)
		}
		if err := writer.FlushError(); err != nil {
			t.Fatal(err)
		}
		if !base.deadline.IsZero() {
			t.Fatal("deadline leaked into idle/model thinking period")
		}
		time.Sleep(60 * time.Millisecond) // A total request deadline would kill the next write.
	}
	if base.armed != 4 || base.cleared != 4 {
		t.Fatalf("nested wrappers lost deadlines: %+v", base)
	}
}

func TestSlowDownstreamHTTP1AndHTTP2ReleaseSlots(t *testing.T) {
	for _, mode := range []struct {
		name    string
		tls, h2 bool
	}{
		{name: "http1"}, {name: "http1-tls", tls: true}, {name: "http2", tls: true, h2: true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			isolatePolicies(t)
			s := isolatedPerformance(t)
			timeouts := currentServiceTimeouts()
			timeouts.write = 250 * time.Millisecond
			serviceTimeouts.Store(&timeouts)
			done, exited := make(chan error, 1), make(chan struct{})
			var released atomic.Int32
			h := requestAuditMiddleware(requestAdmissionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				release, _, err := modelConcurrency.tryAcquire(r.Context(), "synthetic")
				if err != nil || release == nil {
					done <- errors.New("test slot unavailable")
					return
				}
				body := newAccountStreamBody(io.NopCloser(strings.NewReader("")), func() { release(); released.Add(1) })
				defer body.Close()
				session, ok := startModelStream(w, r, body)
				if !ok {
					done <- errors.New("test stream unavailable")
					return
				}
				defer session.Close()
				frame := []byte("data: " + strings.Repeat("x", 64<<10) + "\n\n")
				for range 512 {
					if err := session.guard.writeFrame(frame); err != nil {
						if session.request.Context().Err() == nil {
							done <- errors.New("write failure did not cancel")
							return
						}
						done <- nil
						return
					}
				}
				done <- errors.New("slow client was never bounded")
			})))
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(exited); h.ServeHTTP(w, r) }))
			server.EnableHTTP2 = mode.h2
			if mode.tls {
				server.StartTLS()
			} else {
				server.Start()
			}
			defer server.Close()
			client := server.Client()
			req, _ := http.NewRequest("POST", server.URL+"/v1/messages", nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if (resp.ProtoMajor == 2) != mode.h2 {
				t.Fatalf("wrong protocol: %s", resp.Proto)
			}
			// Deliberately leave the response body unread to fill the receive window.
			wait := 5 * time.Second
			if mode.tls && !mode.h2 {
				wait = 8 * time.Second
			} // Go's TLS Close may spend up to 5s sending close_notify after a write failure.
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(wait):
				_ = resp.Body.Close()
				t.Fatal("downstream write did not time out")
			}
			<-exited
			_, active, waiting := ingressAdmission.snapshot()
			metrics := s.snapshot(time.Now()).Protocols[0]
			if released.Load() != 1 || active != 0 || waiting != 0 || metrics.DownstreamErrors != 1 || metrics.InFlight.Active != 0 {
				t.Fatalf("slow client retained resources: releases=%d admission=%d/%d metrics=%+v", released.Load(), active, waiting, metrics.performanceCounters)
			}
		})
	}
}

func (w *flushFailureWriter) Header() http.Header { return w.header }
func (*flushFailureWriter) WriteHeader(int)       {}
func (*flushFailureWriter) Flush()                {}
func (*flushFailureWriter) FlushError() error     { return io.ErrClosedPipe }

func TestStreamFlushFailureCancelsAndClosesBody(t *testing.T) {
	for _, wrapped := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "audit-and-key-wrappers"}[wrapped], func(t *testing.T) {
			base := &flushFailureWriter{header: make(http.Header)}
			var writer http.ResponseWriter = base
			if wrapped {
				writer = &auditResponseWriter{ResponseWriter: &auditResponseWriter{ResponseWriter: base}}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := &resilienceTrackingBody{Reader: bytes.NewReader(nil)}
			guard := &streamWriteGuard{w: writer, flusher: writer.(http.Flusher), cancel: cancel}
			guard.setBody(body)
			if err := guard.writeFrame([]byte("data: synthetic\n\n")); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("flush failure was swallowed: %v", err)
			}
			if ctx.Err() == nil || !body.closed.Load() || guard.err() == nil {
				t.Fatal("flush failure did not cancel and release upstream")
			}
		})
	}
}

func TestHTTP2WriteTimeoutKeepsOtherStreamOnSameConnection(t *testing.T) {
	isolatePolicies(t)
	isolatedPerformance(t)
	timeouts := currentServiceTimeouts()
	timeouts.write = 200 * time.Millisecond
	serviceTimeouts.Store(&timeouts)
	slowDone := make(chan struct{})
	exited := make(chan struct{})
	h := requestAuditMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/responses" {
			<-slowDone
			_, _ = io.WriteString(w, "healthy")
			return
		}
		defer close(exited)
		defer close(slowDone)
		body := io.NopCloser(strings.NewReader(""))
		defer body.Close()
		session, ok := startModelStream(w, r, body)
		if !ok {
			return
		}
		defer session.Close()
		frame := []byte("data: " + strings.Repeat("x", 64<<10) + "\n\n")
		for range 512 {
			if session.guard.writeFrame(frame) != nil {
				return
			}
		}
	}))
	server := httptest.NewUnstartedServer(h)
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	client := server.Client()
	var slowConn, fastConn net.Conn
	slowReq, _ := http.NewRequest("POST", server.URL+"/v1/messages", nil)
	slowReq = slowReq.WithContext(httptrace.WithClientTrace(slowReq.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { slowConn = info.Conn }}))
	slow, err := client.Do(slowReq)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fastReq, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/responses", nil)
	fastReq = fastReq.WithContext(httptrace.WithClientTrace(fastReq.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { fastConn = info.Conn }}))
	fast, err := client.Do(fastReq)
	if err != nil {
		t.Fatal(err)
	}
	defer fast.Body.Close()
	data, err := io.ReadAll(fast.Body)
	if err != nil || string(data) != "healthy" || slowConn != fastConn || fast.ProtoMajor != 2 {
		t.Fatalf("slow stream poisoned shared HTTP2 connection: %v %q same=%t", err, data, slowConn == fastConn)
	}
	<-exited
}
