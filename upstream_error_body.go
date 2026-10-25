package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

// Fixed budget for error bodies; tests shorten it without changing SSE limits.
var upstreamErrorBodyReadTimeout = 10 * time.Second

var errUpstreamErrorBodyTimeout = errors.New("上游错误响应读取超时")

// Owns cancellation and closure for this error response. Unlike a stream idle
// timer, the fixed deadline cannot be extended by trickling error-body bytes.
func readUpstreamErrorBody(body io.ReadCloser, cancel context.CancelFunc, limit int64) ([]byte, error) {
	var once sync.Once
	closeBody := func() { once.Do(func() { _ = body.Close() }) }
	finished := make(chan struct{})
	timer := time.AfterFunc(upstreamErrorBodyReadTimeout, func() {
		if cancel != nil {
			cancel()
		}
		closeBody()
		close(finished)
	})
	data, err := io.ReadAll(io.LimitReader(body, limit))
	if !timer.Stop() {
		// Join the callback before releasing account/attempt state. It must not
		// outlive this response or cancel a later attempt.
		<-finished
		err = errUpstreamErrorBodyTimeout
	}
	if cancel != nil {
		cancel()
	}
	closeBody()
	return data, err
}
