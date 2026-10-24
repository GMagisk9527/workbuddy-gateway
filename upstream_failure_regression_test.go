package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAggregateReadFailureRetainsUsage(t *testing.T) {
	upstream := `data: {"choices":[{"index":0,"delta":{"content":"partial"}}],"usage":{"credit":3,"prompt_tokens":5,"completion_tokens":1}}` + "\n\n"
	_, err := aggregateCompletion(&dataThenErrorReader{data: []byte(upstream), err: io.ErrUnexpectedEOF}, "synthetic-audit")
	if err == nil {
		t.Fatal("synthetic read failure must fail the aggregate")
	}
	usage, retained := aggregateFailureUsage(err)
	credit, known := usageCredit(usage)
	if !retained || !known || credit != 3 {
		t.Fatalf("measured credit lost after read error: retained=%t credit=%v known=%t err=%v", retained, credit, known, err)
	}
}

func TestAggregateSettlementControls(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "messages"} {
		for _, known := range []bool{false, true} {
			name := protocol + "/unknown"
			if known {
				name = protocol + "/known"
			}
			t.Run(name, func(t *testing.T) {
				h := newCompatHarness(t, "cn")
				usageField := ""
				if known {
					usageField = `,"usage":{"credit":3,"prompt_tokens":5,"completion_tokens":1}`
				}
				h.reply.Store(&compatReply{status: 200, body: `data: {"choices":[{"index":0,"delta":{"content":"complete"},"finish_reason":"stop"}]` + usageField + "}\n\ndata: [DONE]\n\n"})
				path := "/v1/chat/completions"
				body := map[string]any{"model": compatModel, "stream": false, "messages": []any{map[string]any{"role": "user", "content": "Synthetic control input."}}}
				if protocol == "responses" {
					path = "/v1/responses"
					delete(body, "messages")
					body["input"] = "Synthetic control input."
				} else if protocol == "messages" {
					path = "/v1/messages"
					body["max_tokens"] = 64
				}
				status, _ := h.send(t, path, body, nil, 0)
				if status != 200 || h.calls.Load() != 1 {
					t.Fatalf("control failed: status=%d calls=%d", status, h.calls.Load())
				}
				h.key.mu.Lock()
				usage := h.key.usage
				h.key.mu.Unlock()
				wantTotal, wantUnknown := 0.0, int64(1)
				if known {
					wantTotal, wantUnknown = 3, 0
				}
				if usage.Total != wantTotal || usage.Unknown != wantUnknown || usage.Requests != 1 {
					t.Fatalf("control settlement failed: %+v", usage)
				}
			})
		}
	}
}

func TestUpstreamTLSHandshakeHasDeadline(t *testing.T) {
	isolatePolicies(t)
	config := runtimeFileConfig{}
	config.Upstream.HeaderTimeoutSeconds = 1
	if err := applyRuntimeConfig(config, false, "synthetic-tls-audit"); err != nil {
		t.Fatal(err)
	}
	initHTTPClient()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+listener.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var requestErr error
	go func() {
		defer close(done)
		resp, err := cfg.HttpClient.Do(req)
		requestErr = err
		if resp != nil {
			resp.Body.Close()
		}
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("synthetic TLS socket was not accepted")
	}
	defer func() {
		cancel()
		conn.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("TLS client did not stop after cancellation")
		}
	}()
	select {
	case <-done:
		if requestErr == nil || !strings.Contains(requestErr.Error(), "TLS handshake timeout") {
			t.Fatalf("TLS blackhole did not fail at the handshake deadline: %v", requestErr)
		}
	case <-time.After(12 * time.Second):
		t.Error("TLS handshake stayed blocked for 12s despite 1s response-header timeout; no separate handshake deadline released the request")
	}
}

func TestAggregateMissingFinishSettlesUsage(t *testing.T) {
	for _, protocol := range []string{"chat", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			h.reply.Store(&compatReply{status: 200, body: `data: {"choices":[{"index":0,"delta":{"content":"partial"}}],"usage":{"credit":3,"prompt_tokens":5,"completion_tokens":1}}` + "\n\ndata: [DONE]\n\n"})
			path := "/v1/chat/completions"
			body := map[string]any{"model": compatModel, "stream": false, "messages": []any{map[string]any{"role": "user", "content": "Synthetic audit input."}}}
			if protocol == "responses" {
				path = "/v1/responses"
				delete(body, "messages")
				body["input"] = "Synthetic audit input."
			}
			status, _ := h.send(t, path, body, nil, 0)
			if status < 400 || h.calls.Load() != 1 {
				t.Fatalf("invalid synthetic failure setup: status=%d upstreamCalls=%d", status, h.calls.Load())
			}
			h.key.mu.Lock()
			usage := h.key.usage
			h.key.mu.Unlock()
			if usage.Total != 3 || usage.Requests != 1 {
				t.Fatalf("upstream measured credit must settle once despite missing finish: total=%v requests=%d unknown=%d", usage.Total, usage.Requests, usage.Unknown)
			}
		})
	}
}

func TestMessagesAggregateMissingUsageSettlesUnknown(t *testing.T) {
	for _, failure := range []string{"missing_finish", "invalid_json"} {
		t.Run(failure, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			upstream := `data: {"choices":[{"index":0,"delta":{"content":"partial"}}]}` + "\n\n"
			if failure == "invalid_json" {
				upstream += "data: {bad json}\n\n"
			}
			h.reply.Store(&compatReply{status: 200, body: upstream})
			status, _ := h.send(t, "/v1/messages", map[string]any{"model": compatModel, "stream": false, "max_tokens": 64,
				"messages": []any{map[string]any{"role": "user", "content": "Synthetic audit input."}}}, nil, 0)
			if status != http.StatusBadGateway || h.calls.Load() != 1 {
				t.Fatalf("invalid synthetic failure setup: status=%d upstreamCalls=%d", status, h.calls.Load())
			}
			h.key.mu.Lock()
			usage := h.key.usage
			h.key.mu.Unlock()
			if usage.Unknown != 1 || usage.Requests != 1 {
				t.Fatalf("real upstream attempt without usage must settle unknown: unknown=%d requests=%d", usage.Unknown, usage.Requests)
			}
		})
	}
}

func TestChatMultiChoiceRequiresAllFinishes(t *testing.T) {
	for _, mode := range []fastPathModeT{fastPathOff, fastPathOn} {
		name := "slow"
		if mode == fastPathOn {
			name = "fast"
		}
		t.Run(name, func(t *testing.T) {
			h := newCompatHarness(t, "cn")
			fastPathMode.Store(int32(mode))
			h.reply.Store(&compatReply{status: 200, body: `data: {"choices":[{"index":0,"delta":{"content":"complete"},"finish_reason":"stop"},{"index":1,"delta":{"content":"truncated"},"finish_reason":null}]}` + "\n\ndata: [DONE]\n\n"})
			status, output := h.send(t, "/v1/chat/completions", map[string]any{"model": compatModel, "stream": true, "n": 2,
				"messages": []any{map[string]any{"role": "user", "content": "Synthetic audit input."}}}, nil, 0)
			if status != 200 || h.calls.Load() != 1 {
				t.Fatalf("invalid synthetic streaming setup: status=%d upstreamCalls=%d", status, h.calls.Load())
			}
			if strings.Contains(string(output), "data: [DONE]") || !strings.Contains(string(output), "upstream_stream_incomplete") {
				t.Fatalf("one finished choice incorrectly completed a truncated multi-choice response: %s", output)
			}
		})
	}
}
