//go:build networkfaults

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This suite uses a real, separately installed Toxiproxy, never a production
// endpoint or credential. The build tag keeps the tool out of normal builds.
type faultProxy struct {
	t           *testing.T
	base, token string
	client      *http.Client
}

func freeFaultAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	_ = l.Close()
	return address
}

func faultChild(t *testing.T, path string, args []string, extraEnv ...string) {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(t.TempDir(), "child.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(absolute, args...)
	cmd.Dir = t.TempDir()
	// An isolated working directory and explicit arguments keep installed proxy
	// settings and credentials out of these subprocesses.
	for _, item := range os.Environ() {
		name := strings.ToUpper(strings.SplitN(item, "=", 2)[0])
		if !strings.HasPrefix(name, "CC_COMPRESS_") && name != "TOXIPROXY_AUTH_TOKEN" && name != "LOG_LEVEL" {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = log.Close() })
}

func startFaultProxy(t *testing.T) *faultProxy {
	t.Helper()
	path := os.Getenv("WORKBUDDY_TOXIPROXY")
	if path == "" {
		t.Fatal("WORKBUDDY_TOXIPROXY must point to the Toxiproxy v2.13.0 server; see tests/README.md")
	}
	address := freeFaultAddress(t)
	_, port, _ := net.SplitHostPort(address)
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		t.Fatal(err)
	}
	f := &faultProxy{t: t, base: "http://" + address, token: hex.EncodeToString(token[:]), client: &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil}}}
	faultChild(t, path, []string{"-host", "127.0.0.1", "-port", port, "-seed", "1"}, "TOXIPROXY_AUTH_TOKEN="+f.token, "LOG_LEVEL=error")
	waitForTestCondition(t, 5*time.Second, func() bool {
		r, _ := http.NewRequest("GET", f.base+"/version", nil)
		r.Header.Set("Authorization", "Bearer "+f.token)
		resp, err := f.client.Do(r)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return resp.StatusCode == 200
	})
	t.Cleanup(f.client.CloseIdleConnections)
	return f
}

func (f *faultProxy) api(method, path string, body any, out any) {
	f.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	r, err := http.NewRequest(method, f.base+path, bytes.NewReader(encoded))
	if err != nil {
		f.t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+f.token)
	r.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(r)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		f.t.Fatal(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		f.t.Fatalf("Toxiproxy %s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		f.t.Fatal("invalid Toxiproxy response")
	}
}

func (f *faultProxy) proxy(name, upstream string) string {
	f.t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		f.t.Fatal(err)
	}
	if host, _, err := net.SplitHostPort(u.Host); err != nil || !net.ParseIP(host).IsLoopback() {
		f.t.Fatal("fault upstream must be loopback")
	}
	var result struct {
		Listen string `json:"listen"`
	}
	f.api("POST", "/proxies", map[string]any{"name": name, "listen": "127.0.0.1:0", "upstream": u.Host, "enabled": true}, &result)
	return u.Scheme + "://" + result.Listen
}

func (f *faultProxy) toxic(name, kind, direction string, attributes map[string]any) {
	f.api("POST", "/proxies/"+name+"/toxics", map[string]any{"name": "fault", "type": kind, "stream": direction, "toxicity": 1, "attributes": attributes}, nil)
}

const faultPartial = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"synthetic-partial\"}}],\"usage\":{\"credit\":3}}\n\n"
const faultTerminal = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"credit\":3}}\n\ndata: [DONE]\n\n"

type faultGateway struct {
	t          *testing.T
	url, plain string
	key        *apiKey
	client     *http.Client
	store      *performanceStore
}

func startFaultGateway(t *testing.T, upstream string) *faultGateway {
	t.Helper()
	withStreamTestAccount(t)
	dir := resetKeyWorld(t)
	if err := ledgerLoad(dir); err != nil {
		t.Fatal(err)
	}
	plain, key := mustCreateKey(t, keyCreateOptions{Name: "synthetic-network-fault", QuotaTotal: 30})
	store := isolatedPerformance(t)
	oldBase, oldClient := profileCN.Base, cfg.HttpClient
	profileCN.Base = upstream
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, ResponseHeaderTimeout: 5 * time.Second}
	cfg.HttpClient = &http.Client{Transport: transport}
	t.Cleanup(func() { transport.CloseIdleConnections(); profileCN.Base, cfg.HttpClient = oldBase, oldClient })
	server := httptest.NewServer(newAPIHandler())
	t.Cleanup(server.Close)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	return &faultGateway{t, server.URL, plain, key, client, store}
}

func (g *faultGateway) request(ctx context.Context, path, padding, trace string) *http.Response {
	g.t.Helper()
	body := map[string]any{"model": "synthetic-network-fault", "stream": true, "max_tokens": 32}
	if path == "/v1/responses" {
		body["input"] = "hi" + padding
	} else {
		body["messages"] = []any{map[string]any{"role": "user", "content": "hi" + padding}}
	}
	encoded, _ := json.Marshal(body)
	r, err := http.NewRequestWithContext(ctx, "POST", g.url+path, bytes.NewReader(encoded))
	if err != nil {
		g.t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+g.plain)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Trace-ID", trace)
	resp, err := g.client.Do(r)
	if err != nil {
		g.t.Fatal(err)
	}
	g.t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		g.t.Fatalf("gateway HTTP %d: %s", resp.StatusCode, data)
	}
	return resp
}

func (g *faultGateway) diagnostic(trace string) requestDiagnosticView {
	g.t.Helper()
	var d requestDiagnosticView
	waitForTestCondition(g.t, 5*time.Second, func() bool { var ok bool; d, ok = g.store.requestDiagnostic(trace, time.Now()); return ok })
	waitForTestCondition(g.t, 5*time.Second, func() bool {
		r := snapshotLiveRuntime(httptest.NewRequest("GET", "/health", nil))
		_, slots, _ := modelConcurrency.snapshot("")
		return slots == 0 && r.AdmissionActive == 0 && r.AdmissionWaiting == 0 && r.PreparingActive == 0 && r.BodyBytes == 0 && r.OutputBytes == 0 && r.RetryActive == 0
	})
	return d
}

func (g *faultGateway) billed(want float64) {
	g.t.Helper()
	g.key.mu.Lock()
	defer g.key.mu.Unlock()
	if u := g.key.usage; u.Total != want || u.Unknown != 0 || u.Requests != 1 {
		g.t.Fatalf("fault changed settlement: %+v want credit=%v exactly once", u, want)
	}
}

func faultReadUntil(t *testing.T, body io.Reader, marker string) {
	t.Helper()
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), marker) {
			return
		}
	}
	t.Fatalf("did not receive %s: %v", marker, scanner.Err())
}

func TestNetworkFaultSlowUpload(t *testing.T) {
	f := startFaultProxy(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				data, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Contains(data, []byte(strings.Repeat("p", 32<<10))) {
					http.Error(w, "invalid synthetic upload", 400)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, faultPartial+faultTerminal)
			}))
			defer upstream.Close()
			g := startFaultGateway(t, f.proxy("slow-"+strings.ReplaceAll(path, "/", "-"), upstream.URL))
			f.toxic("slow-"+strings.ReplaceAll(path, "/", "-"), "bandwidth", "upstream", map[string]any{"rate": 64})
			resp := g.request(context.Background(), path, strings.Repeat("p", 64<<10), "synthetic-slow-upload")
			data, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || !bytes.Contains(data, []byte("synthetic-partial")) {
				t.Fatalf("slow upload failed: %v %s", err, data)
			}
			d := g.diagnostic("synthetic-slow-upload")
			if calls.Load() != 1 || d.Counters.Failed != 0 || len(d.AttemptTrail) != 1 || d.AttemptTrail[0].Outcome != "complete" {
				t.Fatalf("slow upload replayed or incomplete: calls=%d %+v", calls.Load(), d)
			}
			g.billed(3)
		})
	}
}

func TestNetworkFaultPartialStreamNeverReplays(t *testing.T) {
	f := startFaultProxy(t)
	for _, kind := range []string{"reset_peer", "limit_data"} {
		for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
			t.Run(kind+path, func(t *testing.T) {
				var calls atomic.Int32
				release := make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, faultPartial)
					w.(http.Flusher).Flush()
					select {
					case <-release:
						_, _ = io.WriteString(w, faultTerminal)
					case <-r.Context().Done():
					}
				}))
				t.Cleanup(upstream.Close)
				name := kind + strings.ReplaceAll(path, "/", "-")
				g := startFaultGateway(t, f.proxy(name, upstream.URL))
				resp := g.request(context.Background(), path, "", "synthetic-broken-stream")
				faultReadUntil(t, resp.Body, "synthetic-partial")
				attributes := map[string]any{"timeout": 0}
				if kind == "limit_data" {
					attributes = map[string]any{"bytes": 1}
				}
				f.toxic(name, kind, "downstream", attributes)
				unblock()
				data, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				for _, terminal := range []string{"data: [DONE]", "event: message_stop", "event: response.completed"} {
					if bytes.Contains(data, []byte(terminal)) {
						t.Fatalf("broken stream reported success: %s", data)
					}
				}
				d := g.diagnostic("synthetic-broken-stream")
				if calls.Load() != 1 || d.Counters.Incomplete != 1 || d.Counters.Failed != 1 || len(d.AttemptTrail) != 1 || d.AttemptTrail[0].Outcome == "complete" {
					t.Fatalf("partial stream replayed or hidden: calls=%d %+v", calls.Load(), d)
				}
				g.billed(3)
			})
		}
	}
}

func TestNetworkFaultClientCancelReleasesResources(t *testing.T) {
	f := startFaultProxy(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			stopped := make(chan struct{})
			var once sync.Once
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, faultPartial)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				once.Do(func() { close(stopped) })
			}))
			t.Cleanup(upstream.Close)
			g := startFaultGateway(t, f.proxy("cancel"+strings.ReplaceAll(path, "/", "-"), upstream.URL))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			resp := g.request(ctx, path, "", "synthetic-client-cancel")
			faultReadUntil(t, resp.Body, "synthetic-partial")
			cancel()
			_ = resp.Body.Close()
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("cancel did not reach upstream")
			}
			d := g.diagnostic("synthetic-client-cancel")
			if calls.Load() != 1 || d.Counters.Canceled != 1 || d.Counters.Failed != 1 {
				t.Fatalf("cancel was replayed or unobserved: %+v", d)
			}
			g.billed(3)
		})
	}
}

func TestNetworkFaultHTTP2ResetAfterOutput(t *testing.T) {
	f := startFaultProxy(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			var calls, protocol atomic.Int32
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				protocol.Store(int32(r.ProtoMajor))
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, faultPartial)
				w.(http.Flusher).Flush()
				select {
				case <-release:
					_, _ = io.WriteString(w, faultTerminal)
				case <-r.Context().Done():
				}
			}))
			upstream.EnableHTTP2 = true
			upstream.StartTLS()
			t.Cleanup(upstream.Close)
			name := "h2" + strings.ReplaceAll(path, "/", "-")
			g := startFaultGateway(t, f.proxy(name, upstream.URL))
			// Retain certificate verification using httptest's synthetic trust root.
			transport := upstream.Client().Transport.(*http.Transport).Clone()
			transport.Proxy = nil
			transport.ForceAttemptHTTP2 = true
			cfg.HttpClient = &http.Client{Transport: transport}
			t.Cleanup(transport.CloseIdleConnections)
			resp := g.request(context.Background(), path, "", "synthetic-h2-reset")
			faultReadUntil(t, resp.Body, "synthetic-partial")
			f.toxic(name, "reset_peer", "downstream", map[string]any{"timeout": 0})
			unblock()
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			d := g.diagnostic("synthetic-h2-reset")
			if protocol.Load() != 2 || calls.Load() != 1 || d.Counters.Incomplete != 1 || d.Counters.Failed != 1 {
				t.Fatalf("HTTP/2 reset not exercised or replayed: protocol=%d calls=%d diagnostic=%+v", protocol.Load(), calls.Load(), d)
			}
			g.billed(3)
		})
	}
}

func TestNetworkFaultCompressedProxyChain(t *testing.T) {
	compress := os.Getenv("WORKBUDDY_COMPRESS_BIN")
	if compress == "" {
		t.Skip("optional native compression proxy: set WORKBUDDY_COMPRESS_BIN; see tests/README.md")
	}
	f := startFaultProxy(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, faultPartial+faultTerminal)
			}))
			defer upstream.Close()
			g := startFaultGateway(t, upstream.URL)
			var compressed atomic.Bool
			gatewayURL := g.url
			u, _ := url.Parse(gatewayURL)
			bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Content-Encoding") == "zstd" {
					compressed.Store(true)
				}
				r.URL.Scheme, r.URL.Host = "http", u.Host
				r.RequestURI = ""
				res, err := g.client.Do(r)
				if err != nil {
					http.Error(w, err.Error(), 502)
					return
				}
				defer res.Body.Close()
				for k, values := range res.Header {
					for _, value := range values {
						w.Header().Add(k, value)
					}
				}
				w.WriteHeader(res.StatusCode)
				_, _ = io.Copy(w, res.Body)
			}))
			defer bridge.Close()
			link := f.proxy("compression"+strings.ReplaceAll(path, "/", "-"), bridge.URL)
			name := "compression" + strings.ReplaceAll(path, "/", "-")
			f.toxic(name, "slicer", "downstream", map[string]any{"average_size": 13, "size_variation": 0, "delay": 100})
			listen := freeFaultAddress(t)
			faultChild(t, compress, []string{"--listen", listen, "--upstream", link, "--no-proxy", "--algorithm", "zstd", "--level", "3", "--min-bytes", "256"})
			g.url = "http://" + listen
			waitForTestCondition(t, 5*time.Second, func() bool {
				r, err := g.client.Get(g.url + "/health")
				if err != nil {
					return false
				}
				defer r.Body.Close()
				_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 4096))
				return r.StatusCode == 200
			})
			resp := g.request(context.Background(), path, strings.Repeat("compressible synthetic data ", 4096), "synthetic-compression-chain")
			data, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			d := g.diagnostic("synthetic-compression-chain")
			if err != nil || !compressed.Load() || calls.Load() != 1 || d.Counters.Failed != 0 || !bytes.Contains(data, []byte("synthetic-partial")) {
				t.Fatalf("compressed segmented chain failed: compressed=%t calls=%d error=%v diagnostic=%+v", compressed.Load(), calls.Load(), err, d)
			}
			g.billed(3)
		})
	}
}
