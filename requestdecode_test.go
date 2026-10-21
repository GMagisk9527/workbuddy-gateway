package main

// 请求体压缩解码的回归测试：解压正确性、错误分类与体积上限。

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zlibBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// echoHandler 把收到的 body 原样回显，用于验证解压结果。
func echoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Saw-Encoding", r.Header.Get("Content-Encoding"))
		_, _ = w.Write(body)
	})
}

func TestDecodeGzipAndDeflate(t *testing.T) {
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"你好"}]}`)
	h := decodeRequestBodyMiddleware(echoHandler())

	for name, body := range map[string][]byte{
		"gzip":    gzipBytes(t, plain),
		"deflate": zlibBytes(t, plain),
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Encoding", name)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: 状态码 %d，期望 200", name, rec.Code)
		}
		if got := rec.Body.Bytes(); !bytes.Equal(got, plain) {
			t.Fatalf("%s: 解压结果不一致，得到 %q", name, string(got))
		}
		if enc := rec.Header().Get("X-Saw-Encoding"); enc != "" {
			t.Fatalf("%s: 解压后必须移除 Content-Encoding，handler 仍看到 %q", name, enc)
		}
	}
}

func TestDecodeLeavesPlainBodyUntouched(t *testing.T) {
	plain := []byte(`{"hello":"world"}`)
	h := decodeRequestBodyMiddleware(echoHandler())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(plain))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), plain) {
		t.Fatalf("未压缩请求不应被改动: %d %q", rec.Code, rec.Body.String())
	}
}

func TestDecodeRejectsUnsupportedEncoding(t *testing.T) {
	h := decodeRequestBodyMiddleware(echoHandler())
	for _, enc := range []string{"br", "zstd", "compress"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("x"))
		req.Header.Set("Content-Encoding", enc)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("%s: 应返回 415（让客户端回退到 gzip），实际 %d", enc, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "unsupported_content_encoding") {
			t.Fatalf("%s: 错误码应可识别，实际 %s", enc, rec.Body.String())
		}
	}
}

func TestDecodeRejectsCorruptGzip(t *testing.T) {
	h := decodeRequestBodyMiddleware(echoHandler())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("not-gzip-at-all"))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("损坏的 gzip 应 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_encoding") {
		t.Fatalf("错误码应可识别，实际 %s", rec.Body.String())
	}
}

// 压缩炸弹：解压后超过上限必须拒绝，而不是把内存吃光。
func TestDecodeRejectsDecompressionBomb(t *testing.T) {
	// 40MB 的零字节压成 gzip 只有几十 KB
	huge := make([]byte, requestDecodedLimit+1024)
	body := gzipBytes(t, huge)
	if len(body) > requestCompressedLimit {
		t.Fatalf("测试数据压缩后过大: %d", len(body))
	}
	h := decodeRequestBodyMiddleware(echoHandler())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("解压超过上限应 413，实际 %d", rec.Code)
	}
}

// 真实中间件链：压缩请求必须与非压缩请求得到同样的业务结果。
func TestGzipRequestThroughRealChain(t *testing.T) {
	isolatedGatewayConfig(t)
	accountMu.Lock()
	oldAccounts := accounts
	accounts = nil // 空账号池：handler 解析完请求体后会返回 401 no_auth
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: true, APIKey: "gz-test-key"}); err != nil {
		t.Fatal(err)
	}
	h := newAPIHandler()

	body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	post := func(payload []byte, encoding string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer gz-test-key")
		if encoding != "" {
			req.Header.Set("Content-Encoding", encoding)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	plainCode := post(body, "")
	if plainCode == http.StatusBadRequest {
		t.Fatalf("前提不成立：明文请求不应 400，实际 %d", plainCode)
	}
	if gzCode := post(gzipBytes(t, body), "gzip"); gzCode != plainCode {
		t.Fatalf("压缩请求与明文请求结果应一致：明文 %d，gzip %d", plainCode, gzCode)
	}
	if badCode := post([]byte("broken"), "gzip"); badCode != http.StatusBadRequest {
		t.Fatalf("损坏的 gzip 必须 400，实际 %d", badCode)
	}
}
