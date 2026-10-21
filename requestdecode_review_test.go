package main

// PR #2 评审回归：匿名/免鉴权路径不得读取并解压请求体；Anthropic 压缩错误要有正确错误类别。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// readCounter 记录请求体被读取的次数与字节数。
type readCounter struct {
	body  io.ReadCloser
	reads atomic.Int64
	bytes atomic.Int64
}

func (c *readCounter) Read(p []byte) (int, error) {
	n, err := c.body.Read(p)
	if n > 0 {
		c.reads.Add(1)
		c.bytes.Add(int64(n))
	}
	return n, err
}

func (c *readCounter) Close() error { return c.body.Close() }

func newProbeRequestWithCounter(t *testing.T, method, path string, payload []byte) (*http.Request, *readCounter) {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	counter := &readCounter{body: io.NopCloser(bytes.NewReader(payload))}
	req.Body = counter
	return req, counter
}

// 免鉴权与"鉴权在 mux 内部"的路径，都不得在鉴权前读取请求体。
func TestAuditAnonymousPathsNeverReadBody(t *testing.T) {
	isolatedGatewayConfig(t)
	// 配置管理密钥：探测接口此时对"没有管理密钥"返回 401，而不是"未配置"的 503。
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: true, APIKey: "audit-key", AdminKey: "audit-admin"}); err != nil {
		t.Fatal(err)
	}
	h := newAPIHandler()

	bomb := gzipBytes(t, make([]byte, 4096))
	cases := []struct {
		name     string
		method   string
		path     string
		remote   string
		wantCode int
	}{
		{"health", http.MethodGet, "/health", "203.0.113.5:1234", http.StatusOK},
		{"ping", http.MethodGet, "/ping", "203.0.113.5:1234", http.StatusOK},
		{"root", http.MethodGet, "/", "203.0.113.5:1234", http.StatusOK},
		{"probe-匿名", http.MethodPost, "/admin/probe", "203.0.113.5:1234", http.StatusUnauthorized},
		{"probe-匿名回环", http.MethodPost, "/admin/probe", "127.0.0.1:5555", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, counter := newProbeRequestWithCounter(t, tc.method, tc.path, bomb)
			req.Header.Set("Content-Encoding", "gzip")
			req.Header.Set("Content-Type", "application/json")
			req.RemoteAddr = tc.remote
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("状态码 %d，期望 %d", rec.Code, tc.wantCode)
			}
			if n := counter.reads.Load(); n != 0 {
				t.Fatalf("该路径不得读取请求体，实际读了 %d 次 / %d 字节", n, counter.bytes.Load())
			}
		})
	}
}

// 匿名探测带压缩炸弹：必须先在鉴权处被拒（401），而不是被解压层拒（413）。
func TestAuditAnonymousProbeDoesNotDecompressBomb(t *testing.T) {
	isolatedGatewayConfig(t)
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: true, APIKey: "audit-key", AdminKey: "audit-admin"}); err != nil {
		t.Fatal(err)
	}
	h := newAPIHandler()

	bomb := gzipBytes(t, make([]byte, requestDecodedLimit+1024))
	if len(bomb) > requestCompressedLimit {
		t.Skipf("压缩后仍超过上限，测试数据不合适: %d", len(bomb))
	}
	req, counter := newProbeRequestWithCounter(t, http.MethodPost, "/admin/probe", bomb)
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.5:1234" // 非回环匿名来源
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名探测应先 401，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if n := counter.reads.Load(); n != 0 {
		t.Fatalf("鉴权前不得读取请求体，实际读了 %d 次", n)
	}
}

// Anthropic 客户端必须拿到正确的 error.type。
func TestAuditAnthropicEncodingErrorTypes(t *testing.T) {
	isolatedGatewayConfig(t)
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: true, APIKey: "audit-key"}); err != nil {
		t.Fatal(err)
	}
	h := newAPIHandler()

	bomb := gzipBytes(t, make([]byte, requestDecodedLimit+1024))
	if len(bomb) > requestCompressedLimit {
		t.Skipf("压缩后仍超过上限: %d", len(bomb))
	}
	cases := []struct {
		name     string
		encoding string
		payload  []byte
		wantCode int
		wantType string
	}{
		{"损坏 gzip", "gzip", []byte("not-gzip"), http.StatusBadRequest, "invalid_request_error"},
		{"不支持的编码", "br", []byte("whatever"), http.StatusUnsupportedMediaType, "invalid_request_error"},
		{"解压超限", "gzip", bomb, http.StatusRequestEntityTooLarge, "request_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
				req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(tc.payload))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer audit-key")
				req.Header.Set("Content-Encoding", tc.encoding)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)

				if rec.Code != tc.wantCode {
					t.Fatalf("%s %s: 状态码 %d，期望 %d", path, tc.name, rec.Code, tc.wantCode)
				}
				var body struct {
					Type  string `json:"type"`
					Error struct {
						Type string `json:"type"`
					} `json:"error"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("%s %s: 响应不是合法 JSON: %s", path, tc.name, rec.Body.String())
				}
				if body.Type != "error" {
					t.Fatalf("%s %s: 外层 type 应为 error，实际 %q", path, tc.name, body.Type)
				}
				if body.Error.Type != tc.wantType {
					t.Fatalf("%s %s: error.type = %q，期望 %q", path, tc.name, body.Error.Type, tc.wantType)
				}
			}
		})
	}
}
