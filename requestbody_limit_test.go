package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// paddingBody generates JSON followed by whitespace without retaining a large
// input allocation. readBytes includes only bytes actually requested by HTTP.
type paddingBody struct {
	prefix    string
	remaining int64
	readBytes int64
}

func (b *paddingBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > b.remaining {
		n = int(b.remaining)
	}
	i := copy(p[:n], b.prefix)
	b.prefix = b.prefix[i:]
	if i < n {
		p[i] = ' '
		for filled := i + 1; filled < n; {
			filled += copy(p[filled:n], p[i:filled])
		}
	}
	b.remaining -= int64(n)
	b.readBytes += int64(n)
	return n, nil
}

func (*paddingBody) Close() error { return nil }

func bodyLimitAPIHandler(t *testing.T) http.Handler {
	t.Helper()
	isolatedGatewayConfig(t)
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: true, APIKey: "body-limit-key"}); err != nil {
		t.Fatal(err)
	}
	accountMu.Lock()
	oldAccounts := accounts
	accounts = nil
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts = oldAccounts
		accountMu.Unlock()
	})
	return newAPIHandler()
}

var modelBodyLimitCases = []struct {
	path string
	body string
}{
	{"/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
	{"/v1/responses", `{"model":"m","input":"hi"}`},
	{"/v1/messages", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`},
	{"/v1/messages/count_tokens", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
}

func assertBodyLimitError(t *testing.T, rec *httptest.ResponseRecorder, path string) {
	t.Helper()
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%s: status=%d, want 413; body=%s", path, rec.Code, rec.Body.String())
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid error JSON: %v", err)
	}
	if strings.HasPrefix(path, "/v1/messages") {
		if body.Type != "error" || body.Error.Type != "request_too_large" {
			t.Fatalf("Anthropic error shape=%s", rec.Body.String())
		}
	} else if body.Error.Type != "request_body_too_large" {
		t.Fatalf("OpenAI error shape=%s", rec.Body.String())
	}
}

func TestModelRequestBodyKnownLengthLimitBeforeRead(t *testing.T) {
	h := bodyLimitAPIHandler(t)
	for _, tc := range modelBodyLimitCases {
		for _, encoding := range []string{"", "identity"} {
			t.Run(tc.path+"/"+encoding, func(t *testing.T) {
				body := &paddingBody{prefix: tc.body, remaining: requestDecodedLimit + 1}
				req := httptest.NewRequest(http.MethodPost, tc.path, nil)
				req.Body = body
				req.ContentLength = requestDecodedLimit + 1
				req.Header.Set("Authorization", "Bearer body-limit-key")
				req.Header.Set("Content-Encoding", encoding)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				assertBodyLimitError(t, rec, tc.path)
				if body.readBytes != 0 {
					t.Fatalf("declared oversized body was read: %d bytes", body.readBytes)
				}
			})
		}
	}
}

func TestModelRequestBodyKnownLengthNeverEntersHandler(t *testing.T) {
	for _, tc := range modelBodyLimitCases {
		for _, encoding := range []string{"", "identity"} {
			t.Run(tc.path+"/"+encoding, func(t *testing.T) {
				called := false
				h := withRequestBodyDecode(func(w http.ResponseWriter, r *http.Request) { called = true })
				req := httptest.NewRequest(http.MethodPost, tc.path, nil)
				req.ContentLength = requestDecodedLimit + 1
				req.Header.Set("Content-Encoding", encoding)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				assertBodyLimitError(t, rec, tc.path)
				if called {
					t.Fatal("oversized request entered protocol handler")
				}
			})
		}
	}
}

func TestModelRequestBodyUnknownLengthLimit(t *testing.T) {
	h := bodyLimitAPIHandler(t)
	for _, tc := range modelBodyLimitCases {
		for _, encoding := range []string{"", "identity"} {
			t.Run(tc.path+"/"+encoding, func(t *testing.T) {
				body := &paddingBody{prefix: tc.body, remaining: requestDecodedLimit + 1}
				req := httptest.NewRequest(http.MethodPost, tc.path, nil)
				req.Body = body
				req.ContentLength = -1
				req.Header.Set("Authorization", "Bearer body-limit-key")
				req.Header.Set("Content-Encoding", encoding)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				assertBodyLimitError(t, rec, tc.path)
				if body.readBytes != requestDecodedLimit+1 {
					t.Fatalf("read=%d, want exactly limit+1=%d", body.readBytes, requestDecodedLimit+1)
				}
			})
		}
	}
}

func TestModelRequestBodyBoundaryAllowed(t *testing.T) {
	h := bodyLimitAPIHandler(t)
	const path = "/v1/messages/count_tokens"
	const input = `{"messages":[{"role":"user","content":"hi"}]}`
	for _, size := range []int64{int64(len(input)), requestDecodedLimit} {
		for _, known := range []bool{true, false} {
			body := &paddingBody{prefix: input, remaining: size}
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req.Body = body
			req.ContentLength = size
			if !known {
				req.ContentLength = -1
			}
			req.Header.Set("Authorization", "Bearer body-limit-key")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || body.readBytes != size {
				t.Fatalf("size=%d known=%v: status=%d read=%d", size, known, rec.Code, body.readBytes)
			}
		}
	}
}

func TestModelRequestBodyLimitKeepsAuthenticationFirst(t *testing.T) {
	h := bodyLimitAPIHandler(t)
	for _, tc := range modelBodyLimitCases {
		for _, length := range []int64{-1, requestDecodedLimit + 1} {
			body := &paddingBody{prefix: tc.body, remaining: requestDecodedLimit + 1}
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Body = body
			req.ContentLength = length
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized || body.readBytes != 0 {
				t.Fatalf("%s length=%d: status=%d read=%d, want 401 before reads", tc.path, length, rec.Code, body.readBytes)
			}
		}
	}
}
