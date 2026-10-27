package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type requestTimeoutBody struct{ err error }

func (b requestTimeoutBody) Read([]byte) (int, error) { return 0, b.err }
func (requestTimeoutBody) Close() error               { return nil }

type requestTimeoutError struct{}

func (requestTimeoutError) Error() string   { return "i/o timeout" }
func (requestTimeoutError) Timeout() bool   { return true }
func (requestTimeoutError) Temporary() bool { return true }

func TestRequestReadTimeoutReturnsProtocolShape(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(route, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, route, nil)
			req.Body = requestTimeoutBody{err: requestTimeoutError{}}
			rec := httptest.NewRecorder()
			switch route {
			case "/v1/chat/completions":
				handleChatCompletions(rec, req)
			case "/v1/responses":
				handleResponses(rec, req)
			case "/v1/messages":
				handleMessages(rec, req)
			default:
				handleCountTokens(rec, req)
			}
			body := rec.Body.String()
			if rec.Code != http.StatusRequestTimeout || !strings.Contains(body, "读取请求体超时") {
				t.Fatalf("timeout shape=%d %s", rec.Code, body)
			}
			if route == "/v1/messages" || route == "/v1/messages/count_tokens" {
				if !strings.Contains(body, "invalid_request_error") {
					t.Fatalf("anthropic timeout shape=%s", body)
				}
			} else if !strings.Contains(body, "request_body_timeout") {
				t.Fatalf("openai timeout code missing: %s", body)
			}
		})
	}
}

func TestRequestReadFailureRemainsBadRequest(t *testing.T) {
	for _, err := range []error{errors.New("connection reset by peer"), context.Canceled, net.ErrClosed} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		req.Body = requestTimeoutBody{err: err}
		handleResponses(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "read_error") {
			t.Fatalf("non-timeout read failure=%d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestAPIReadTimeoutUsesConfiguredValue(t *testing.T) {
	old := serviceTimeouts.Load()
	timeouts := serviceTimeoutConfig{write: 30 * time.Minute, read: 42 * time.Minute, grace: 2 * time.Minute, cleanup: 15 * time.Second}
	serviceTimeouts.Store(&timeouts)
	t.Cleanup(func() { serviceTimeouts.Store(old) })
	slot := newGatewayHTTPSlot("127.0.0.1:0", false, nil, 42*time.Minute)
	if slot.server.ReadTimeout != 42*time.Minute || slot.server.ReadHeaderTimeout != apiReadHeaderTimeout {
		t.Fatalf("API timeouts not applied: read=%v header=%v", slot.server.ReadTimeout, slot.server.ReadHeaderTimeout)
	}
}

func TestCompressedRequestBodyTimeoutReturns408(t *testing.T) {
	for _, route := range []string{"/v1/responses", "/v1/messages"} {
		t.Run(route, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, route, nil)
			req.Header.Set("Content-Encoding", "gzip")
			req.Body = requestTimeoutBody{err: requestTimeoutError{}}
			rec := httptest.NewRecorder()
			decodeRequestBodyMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("compressed timeout reached handler")
			})).ServeHTTP(rec, req)
			if rec.Code != http.StatusRequestTimeout || !strings.Contains(rec.Body.String(), "读取请求体超时") {
				t.Fatalf("compressed timeout=%d %s", rec.Code, rec.Body.String())
			}
			if route == "/v1/messages" && !strings.Contains(rec.Body.String(), "invalid_request_error") {
				t.Fatalf("compressed anthropic timeout shape=%s", rec.Body.String())
			}
		})
	}
}
