package main

import (
	"context"
	"crypto/x509"
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

// Exercise the production transport, trusting only the synthetic TLS server.
func initTestUpstreamHTTPClient(t *testing.T, server *httptest.Server) {
	t.Helper()
	isolatePolicies(t)
	initHTTPClient()
	t.Cleanup(cfg.HttpClient.CloseIdleConnections)
	if server.TLS != nil {
		roots := x509.NewCertPool()
		roots.AddCert(server.Certificate())
		cfg.HttpClient.Transport.(*hotConfigTransport).transport.TLSClientConfig.RootCAs = roots
	}
}

func sendTestUpstreamHTTP(t *testing.T, server *httptest.Server, payload string, trace *httptrace.ClientTrace) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if trace != nil {
		ctx = httptrace.WithClientTrace(ctx, trace)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	prof := profileCN
	prof.Base = server.URL
	acc := &Account{Path: "synthetic-http.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "synthetic-http-token"}}}
	resp, upstreamCancel, err := doUpstreamRequest(r, acc, &prof, []byte(payload), 1, "synthetic-http")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { upstreamCancel(); resp.Body.Close() })
	return resp, upstreamCancel
}

func TestUpstreamHTTP2ReusesConnectionAfterSSETerminal(t *testing.T) {
	var connections, calls atomic.Int32
	cancelled := make(chan struct{}, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != `{"model":"synthetic"}` {
			t.Errorf("upstream POST body = %q, err = %v", body, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		w.(http.Flusher).Flush()
		// The logical SSE terminal does not require the upstream to send EOF.
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	initTestUpstreamHTTPClient(t, server)
	for i := 0; i < 2; i++ {
		gotConn := make(chan bool, 1)
		resp, cancel := sendTestUpstreamHTTP(t, server, `{"model":"synthetic"}`, &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { gotConn <- info.Reused },
		})
		if resp.ProtoMajor != 2 {
			cancel()
			resp.Body.Close()
			t.Fatalf("upstream negotiated %s, want HTTP/2", resp.Proto)
		}
		body := newIdleReadCloser(resp.Body, 5*time.Second, cancel)
		reader := newUpstreamSSEReader(body)
		if !reader.Next() || !strings.Contains(string(reader.data), "ok") || reader.Next() || reader.Err() != nil || !reader.done {
			t.Fatalf("SSE terminal was not retained: err=%v done=%t", reader.Err(), reader.done)
		}
		body.Close()
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("SSE completion did not cancel the upstream stream")
		}
		if reused := <-gotConn; reused != (i > 0) {
			t.Fatalf("request %d connection reused=%t", i, reused)
		}
	}
	if connections.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("connections=%d upstream calls=%d, want one connection and two calls", connections.Load(), calls.Load())
	}
	// A timeout hot reload must retain HTTP/2 in the cloned transport.
	upstreamConfigMu.Lock()
	upstreamHeaderTimeout += time.Second
	upstreamConfigMu.Unlock()
	resp, _ := sendTestUpstreamHTTP(t, server, `{"model":"synthetic"}`, nil)
	if resp.ProtoMajor != 2 {
		t.Fatalf("timeout hot reload negotiated %s, want HTTP/2", resp.Proto)
	}
}

func TestUpstreamHTTP1Fallback(t *testing.T) {
	for _, tlsEnabled := range []bool{false, true} {
		name := "http"
		if tlsEnabled {
			name = "https"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.ProtoMajor != 1 || string(body) != `{"model":"synthetic"}` {
					t.Errorf("fallback request: protocol=%s body=%q", r.Proto, body)
				}
				io.WriteString(w, "fallback-ok")
			}))
			if tlsEnabled {
				server.StartTLS()
			} else {
				server.Start()
			}
			t.Cleanup(server.Close)
			initTestUpstreamHTTPClient(t, server)
			resp, _ := sendTestUpstreamHTTP(t, server, `{"model":"synthetic"}`, nil)
			body, err := io.ReadAll(resp.Body)
			if resp.ProtoMajor != 1 || err != nil || string(body) != "fallback-ok" {
				t.Fatalf("fallback: protocol=%s body=%q err=%v", resp.Proto, body, err)
			}
		})
	}
}

func TestUpstreamHTTP2CancellationIsolated(t *testing.T) {
	firstCancelled := make(chan struct{})
	releaseSecond := make(chan struct{})
	var calls, connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"start\"}}]}\n\n")
		w.(http.Flusher).Flush()
		if call == 1 {
			<-r.Context().Done()
			close(firstCancelled)
			return
		}
		select {
		case <-releaseSecond:
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"end\"},\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
		case <-r.Context().Done():
		}
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	initTestUpstreamHTTPClient(t, server)
	first, cancelFirst := sendTestUpstreamHTTP(t, server, `{"model":"synthetic"}`, nil)
	second, cancelSecond := sendTestUpstreamHTTP(t, server, `{"model":"synthetic"}`, nil)
	defer cancelSecond()
	first.Body.Close()
	cancelFirst()
	select {
	case <-firstCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("first stream was not cancelled")
	}
	close(releaseSecond)
	reader := newUpstreamSSEReader(second.Body)
	var output strings.Builder
	for reader.Next() {
		output.Write(reader.data)
	}
	if second.ProtoMajor != 2 || reader.Err() != nil || !reader.done || !strings.Contains(output.String(), `"completion_tokens":2`) || !strings.Contains(output.String(), "end") {
		t.Fatalf("second stream lost completion or usage: protocol=%s err=%v output=%q", second.Proto, reader.Err(), output.String())
	}
	if connections.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("connections=%d upstream calls=%d, want multiplexed streams", connections.Load(), calls.Load())
	}
}

func TestUpstreamHTTP2SentPOSTNotReplayed(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		if r.ProtoMajor != 2 {
			t.Errorf("aborted POST negotiated %s, want HTTP/2", r.Proto)
		}
		// Abort after receiving the POST, before returning response headers.
		panic(http.ErrAbortHandler)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	initTestUpstreamHTTPClient(t, server)
	upstreamConfigMu.Lock()
	upstreamTransientRetries = 2
	upstreamConfigMu.Unlock()
	prof := profileCN
	prof.Base = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	acc := &Account{Path: "synthetic-http.json", Auth: &StoredAuth{Edition: "cn"}}
	resp, upstreamCancel, err := doUpstreamRequest(r, acc, &prof, []byte(`{"model":"synthetic"}`), 1, "synthetic-http")
	if upstreamCancel != nil {
		defer upstreamCancel()
	}
	if resp != nil {
		defer resp.Body.Close()
	}
	if err == nil || calls.Load() != 1 {
		t.Fatalf("sent POST was replayed or accepted: calls=%d err=%v", calls.Load(), err)
	}
}
