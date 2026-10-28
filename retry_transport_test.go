package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

type retryInjectingTransport struct {
	base    *hotConfigTransport
	fail    atomic.Bool
	flushes atomic.Int32
}

func TestIsolatedRetryPreservesClientAndTransportPolicies(t *testing.T) {
	proxy, _ := url.Parse("http://proxy.synthetic.invalid:3128")
	shared := &http.Transport{Proxy: http.ProxyURL(proxy), TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ResponseHeaderTimeout: 17 * time.Second, ForceAttemptHTTP2: true}
	jar, _ := cookiejar.New(nil)
	redirectCalls := 0
	base := &http.Client{Transport: shared, Jar: jar, Timeout: 23 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { redirectCalls++; return http.ErrUseLastResponse }}
	client, cleanup := isolatedRetryClient(base)
	if cleanup == nil {
		t.Fatal("no isolated transport")
	}
	defer cleanup()
	defer cleanup()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == shared || !transport.DisableKeepAlives {
		t.Fatal("retry did not own its transport")
	}
	if shared.DisableKeepAlives || client.Timeout != base.Timeout || client.Jar != jar {
		t.Fatal("shared client settings changed")
	}
	request := httptest.NewRequest("GET", "https://synthetic.invalid", nil)
	got, _ := transport.Proxy(request)
	if got.String() != proxy.String() || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || transport.ResponseHeaderTimeout != shared.ResponseHeaderTimeout || !transport.ForceAttemptHTTP2 {
		t.Fatal("proxy/TLS/timeout settings lost")
	}
	if err := client.CheckRedirect(request, nil); err != http.ErrUseLastResponse || redirectCalls != 1 {
		t.Fatal("redirect policy lost")
	}
}

func TestIsolatedRetryHTTP2StreamCancelKeepsSharedConnection(t *testing.T) {
	isolatePolicies(t)
	canceled := make(chan struct{})
	peer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Peer", r.RemoteAddr)
		if r.URL.Path == "/v2/chat/completions" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: one\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			close(canceled)
			return
		}
		_, _ = io.WriteString(w, "warm")
	}))
	peer.EnableHTTP2 = true
	peer.StartTLS()
	defer peer.Close()
	roots := x509.NewCertPool()
	roots.AddCert(peer.Certificate())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = true
	transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	transport.ResponseHeaderTimeout, _, _ = upstreamSettings()
	defer transport.CloseIdleConnections()
	injected := &retryInjectingTransport{base: &hotConfigTransport{transport: transport}}
	client := &http.Client{Transport: injected}
	old := cfg.HttpClient
	cfg.HttpClient = client
	defer func() { cfg.HttpClient = old }()
	warm, err := client.Get(peer.URL + "/warm")
	if err != nil {
		t.Fatal(err)
	}
	warmPeer := warm.Header.Get("X-Peer")
	_, _ = io.Copy(io.Discard, warm.Body)
	_ = warm.Body.Close()
	injected.fail.Store(true)
	profile := profileCN
	profile.Base = peer.URL
	profile.Origin = peer.URL
	resp, cancel, err := doUpstreamRequest(httptest.NewRequest("POST", "/v1/messages", nil), mkAccount("synthetic-stream.json"), &profile, []byte(`{"model":"synthetic"}`), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	defer resp.Body.Close()
	if resp.ProtoMajor != 2 || resp.Header.Get("X-Peer") == warmPeer {
		t.Fatal("stream did not use fresh HTTP2 connection")
	}
	initial := make([]byte, len("data: one\n\n"))
	if _, err := io.ReadFull(resp.Body, initial); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = resp.Body.Close()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("retry stream was not canceled")
	}
	after, err := client.Get(peer.URL + "/warm")
	if err != nil {
		t.Fatal(err)
	}
	defer after.Body.Close()
	_, _ = io.Copy(io.Discard, after.Body)
	if after.Header.Get("X-Peer") != warmPeer || injected.flushes.Load() != 0 {
		t.Fatal("cancel closed the shared connection")
	}
}

func (t *retryInjectingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/v2/chat/completions" && t.fail.CompareAndSwap(true, false) {
		return nil, io.EOF
	}
	return t.base.RoundTrip(r)
}
func (t *retryInjectingTransport) freshRetryTransport() *http.Transport {
	return t.base.freshRetryTransport()
}
func (t *retryInjectingTransport) CloseIdleConnections() {
	t.flushes.Add(1)
	t.base.CloseIdleConnections()
}

func TestIsolatedRetryKeepsWarmConnectionsAndUsesFreshHTTP1AndHTTP2(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		name := "http1"
		if h2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			isolatePolicies(t)
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Peer", r.RemoteAddr)
				w.Header().Set("X-Protocol", r.Proto)
				_, _ = io.WriteString(w, "synthetic")
			})
			target := httptest.NewUnstartedServer(handler)
			target.EnableHTTP2 = h2
			target.StartTLS()
			defer target.Close()
			other := httptest.NewUnstartedServer(handler)
			other.EnableHTTP2 = h2
			other.StartTLS()
			defer other.Close()
			roots := x509.NewCertPool()
			roots.AddCert(target.Certificate())
			roots.AddCert(other.Certificate())
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
			transport.ForceAttemptHTTP2 = h2
			transport.ResponseHeaderTimeout, _, _ = upstreamSettings()
			defer transport.CloseIdleConnections()
			injected := &retryInjectingTransport{base: &hotConfigTransport{transport: transport}}
			client := &http.Client{Transport: injected}
			previous := cfg.HttpClient
			cfg.HttpClient = client
			defer func() { cfg.HttpClient = previous }()
			oldBackoff := upstreamRetryBackoff
			upstreamRetryBackoff = time.Millisecond
			defer func() { upstreamRetryBackoff = oldBackoff }()
			get := func(url string) (string, bool) {
				t.Helper()
				reused := false
				r, _ := http.NewRequest("GET", url, nil)
				r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused }}))
				resp, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if h2 && resp.ProtoMajor != 2 {
					t.Fatal("HTTP2 fixture not negotiated")
				}
				return resp.Header.Get("X-Peer"), reused
			}
			warmTarget, _ := get(target.URL + "/warm")
			warmOther, _ := get(other.URL + "/warm")
			injected.fail.Store(true)
			profile := profileCN
			profile.Base = target.URL
			profile.Origin = target.URL
			resp, cancel, err := doUpstreamRequest(httptest.NewRequest("POST", "/v1/messages", nil), mkAccount("synthetic-fresh.json"), &profile, []byte(`{"model":"synthetic"}`), 1, "")
			if err != nil {
				t.Fatal(err)
			}
			if resp.Header.Get("X-Peer") == warmTarget {
				t.Error("retry reused the shared target connection")
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			cancel()
			if injected.flushes.Load() != 0 {
				t.Error("shared idle pool was flushed")
			}
			if peer, reused := get(other.URL + "/warm"); !reused || peer != warmOther {
				t.Fatal("unrelated upstream lost its warm connection")
			}
			if peer, reused := get(target.URL + "/warm"); !reused || peer != warmTarget {
				t.Fatal("original target pool was replaced")
			}
		})
	}
}
