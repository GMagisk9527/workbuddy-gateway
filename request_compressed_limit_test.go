package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCompressedBodyAboveOldLimitThroughAPIChain(t *testing.T) {
	isolatedGatewayConfig(t)
	accountMu.Lock()
	previous := accounts
	accounts = nil
	accountMu.Unlock()
	t.Cleanup(func() { accountMu.Lock(); accounts = previous; accountMu.Unlock() })
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: true, APIKey: "synthetic-limit-key"}); err != nil {
		t.Fatal(err)
	}
	// Stored gzip blocks deliberately avoid compression to exercise the wire
	// limit with valid JSON, without any real model or credential traffic.
	plain := append([]byte(`{"model":"synthetic","messages":[{"role":"user","content":"hi"}]}`), bytes.Repeat([]byte(" "), 17<<20)...)
	var wire bytes.Buffer
	zw, err := gzip.NewWriterLevel(&wire, gzip.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if wire.Len() <= 16<<20 || wire.Len() > 24<<20 {
		t.Fatalf("wrong fixture size: %d", wire.Len())
	}
	for _, unknownLength := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(wire.Bytes()))
		req.Header.Set("Authorization", "Bearer synthetic-limit-key")
		req.Header.Set("Content-Encoding", "gzip")
		if unknownLength {
			req.ContentLength = -1
		}
		rec := httptest.NewRecorder()
		newAPIHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "no_auth") {
			t.Fatalf("large body must reach empty-account handler without upstream: unknown=%v status=%d body=%s", unknownLength, rec.Code, rec.Body.String())
		}
	}
}

type compressedLimitReader struct{ remaining, read int64 }

func (b *compressedLimitReader) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.remaining)
	clear(p[:n])
	b.remaining -= n
	b.read += n
	return int(n), nil
}
func (*compressedLimitReader) Close() error { return nil }

func TestCompressedWireLimit24MiB(t *testing.T) {
	if requestCompressedLimit != 24<<20 || requestDecodedLimit != 32<<20 {
		t.Fatal("unexpected request limits")
	}
	for _, encoding := range []string{"gzip", "deflate", "zstd"} {
		for _, declared := range []int64{24 << 20, (24 << 20) + 1, -1} {
			body := &compressedLimitReader{remaining: (24 << 20) + 1}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			req.Body, req.ContentLength = body, declared
			req.Header.Set("Content-Encoding", encoding)
			rec := httptest.NewRecorder()
			decodeRequestBodyMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("oversized body reached handler") })).ServeHTTP(rec, req)
			if rec.Code != 413 || !strings.Contains(rec.Body.String(), "压缩请求体超过网关上限") {
				t.Fatalf("encoding=%s declared=%d status=%d", encoding, declared, rec.Code)
			}
			if declared > 24<<20 && body.read != 0 {
				t.Fatal("declared oversize should reject before reading")
			}
			if body.read > (24<<20)+1 {
				t.Fatal("read beyond bounded probe")
			}
		}
	}
}
