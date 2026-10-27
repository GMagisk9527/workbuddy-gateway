package main

// 请求体压缩解码的回归测试：解压正确性、错误分类与体积上限。

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
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

func zstdSingleSegmentBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	zw, err := zstd.NewWriter(nil, zstd.WithSingleSegment(true))
	if err != nil {
		t.Fatal(err)
	}
	defer zw.Close()
	return zw.EncodeAll(raw, nil)
}

func zstdBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
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

func TestDecodeCompressedBodies(t *testing.T) {
	plain := []byte(`{"model":"m","messages":[{"role":"user","content":"你好"}]}`)
	h := decodeRequestBodyMiddleware(echoHandler())

	for name, body := range map[string][]byte{
		"gzip":    gzipBytes(t, plain),
		"deflate": zlibBytes(t, plain),
		"zstd":    zstdBytes(t, plain),
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
	for _, enc := range []string{"br", "compress"} {
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

func TestDecodeRejectsCorruptCompressedBody(t *testing.T) {
	h := decodeRequestBodyMiddleware(echoHandler())
	for _, enc := range []string{"gzip", "zstd"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("not-gzip-at-all"))
		req.Header.Set("Content-Encoding", enc)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("损坏的 %s 应 400，实际 %d", enc, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "invalid_request_encoding") {
			t.Fatalf("%s 错误码应可识别，实际 %s", enc, rec.Body.String())
		}
	}
}

// 压缩炸弹：解压后超过上限必须拒绝，而不是把内存吃光。
func TestDecodeRejectsDecompressionBomb(t *testing.T) {
	// 40MB 的零字节压成 gzip 只有几十 KB
	huge := make([]byte, requestDecodedLimit+1024)
	for name, body := range map[string][]byte{
		"gzip": gzipBytes(t, huge),
		"zstd": zstdBytes(t, huge),
	} {
		if len(body) > requestCompressedLimit {
			t.Fatalf("%s 测试数据压缩后过大: %d", name, len(body))
		}
		h := decodeRequestBodyMiddleware(echoHandler())
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Encoding", name)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s 解压超过上限应 413，实际 %d", name, rec.Code)
		}
	}
}

// 真实中间件链：压缩请求必须与非压缩请求得到同样的业务结果。
func TestCompressedRequestThroughRealChain(t *testing.T) {
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
	for name, payload := range map[string][]byte{
		"gzip": gzipBytes(t, body),
		"zstd": zstdBytes(t, body),
	} {
		if code := post(payload, name); code != plainCode {
			t.Fatalf("压缩请求与明文请求结果应一致：明文 %d，%s %d", plainCode, name, code)
		}
		if badCode := post([]byte("broken"), name); badCode != http.StatusBadRequest {
			t.Fatalf("损坏的 %s 必须 400，实际 %d", name, badCode)
		}
	}
}

func TestDecodeZstdSingleSegmentBoundaries(t *testing.T) {
	h := decodeRequestBodyMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprint(w, n)
	}))
	cases := []struct {
		name   string
		size   int
		status int
	}{
		{"17MiB", 17 << 20, http.StatusOK},
		{"32MiB", requestDecodedLimit, http.StatusOK},
		{"32MiB+1KiB", requestDecodedLimit + 1024, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := bytes.Repeat([]byte("x"), tc.size)
			encoded := zstdSingleSegmentBytes(t, raw)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded))
			req.Header.Set("Content-Encoding", "zstd")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status == http.StatusOK && rec.Body.String() != fmt.Sprint(tc.size) {
				t.Fatalf("decoded size=%s want=%d", rec.Body.String(), tc.size)
			}
			if tc.status == http.StatusRequestEntityTooLarge && !strings.Contains(rec.Body.String(), "request_body_too_large") {
				t.Fatalf("size error not mapped: %s", rec.Body.String())
			}
		})
	}
}

func TestDecodeZstdCorruptSizeMetadata(t *testing.T) {
	// Frame content size is 1, but the raw block contains the two bytes {}.
	badFrameSize := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x20, 1, 0x11, 0, 0, '{', '}'}
	// A raw 1025-byte block exceeds its declared 1024-byte frame window.
	badBlockSize := append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0, 0x09, 0x20, 0}, bytes.Repeat([]byte("x"), 1025)...)
	for _, tc := range []struct {
		name string
		body []byte
		err  error
	}{
		{"frame_content_size", badFrameSize, zstd.ErrFrameSizeExceeded},
		{"block_window_size", badBlockSize, zstd.ErrWindowSizeExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			zr, err := zstd.NewReader(bytes.NewReader(tc.body), zstd.WithDecoderConcurrency(1))
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, zr)
			zr.Close()
			if !errors.Is(err, tc.err) {
				t.Fatalf("fixture error=%v want=%v", err, tc.err)
			}
			for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens"} {
				t.Run(path, func(t *testing.T) {
					h := decodeRequestBodyMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
						t.Fatal("corrupt body reached handler")
					}))
					req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(tc.body))
					req.Header.Set("Content-Encoding", "zstd")
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if rec.Code != http.StatusBadRequest {
						t.Fatalf("status=%d want=400 body=%s", rec.Code, rec.Body.String())
					}
				})
			}
		})
	}
}

func TestDecodeZstdOversizedWindow(t *testing.T) {
	prefixes := map[string][]byte{
		"first_frame":             nil,
		"after_checksummed_frame": zstdBytes(t, []byte("{}")),
		"after_skippable_frame":   {0x50, 0x2a, 0x4d, 0x18, 1, 0, 0, 0, 'x'},
		"after_rle_frame":         {0x28, 0xb5, 0x2f, 0xfd, 0, 0, 0x53, 0, 0, 'x'},
	}
	for name, prefix := range prefixes {
		for _, descriptor := range []byte{0x80, 0xff} { // 64 MiB and the format's largest window.
			t.Run(fmt.Sprintf("%s/window_%02x", name, descriptor), func(t *testing.T) {
				body := append(append([]byte{}, prefix...), []byte{0x28, 0xb5, 0x2f, 0xfd, 0, descriptor, 0x11, 0, 0, '{', '}'}...)
				h := decodeRequestBodyMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					t.Fatal("oversized window reached handler")
				}))
				req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
				req.Header.Set("Content-Encoding", "zstd")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusRequestEntityTooLarge {
					t.Fatalf("status=%d want=413 body=%s", rec.Code, rec.Body.String())
				}
			})
		}
	}
}
