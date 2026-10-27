package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
)

// Only the decompression middleware constructs this body, after checking both
// the decoded size and the shared input budget.
type decodedRequestBody struct {
	data   []byte
	reader *bytes.Reader
}

func newDecodedRequestBody(data []byte) *decodedRequestBody {
	return &decodedRequestBody{data: data, reader: bytes.NewReader(data)}
}

func (b *decodedRequestBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (*decodedRequestBody) Close() error                 { return nil }

func readModelRequestBody(r *http.Request) (data []byte, readErr error) {
	started := performanceStarted(r)
	defer func() { recordPerformance(r, phaseRead, started, readErr != nil) }()
	if body, ok := r.Body.(*decodedRequestBody); ok {
		remaining := body.data[len(body.data)-body.reader.Len():]
		_, err := body.reader.Seek(0, io.SeekEnd)
		return remaining, err
	}
	// A known, bounded length needs one allocation. The extra byte lets an
	// exact-length body report EOF without growing its buffer just to probe.
	// Every read still goes through Body, including MaxBytesReader and leases.
	if r.ContentLength > 0 && r.ContentLength <= requestDecodedLimit {
		buf := make([]byte, int(r.ContentLength)+1)
		used := 0
		for used < len(buf) {
			n, err := r.Body.Read(buf[used:])
			used += n
			if err != nil {
				if err == io.EOF {
					err = nil
				}
				return buf[:used], err
			}
		}
		// Preserve ReadAll behavior if a custom body exceeds its declared size.
		tail, err := io.ReadAll(r.Body)
		return append(buf, tail...), err
	}
	return io.ReadAll(r.Body)
}

// requestBodyReadTimedOut distinguishes a server read deadline/client upload
// timeout from malformed or disconnected request bodies.
func requestBodyReadTimedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// writeModelRequestBodyReadError returns a protocol-shaped failure without
// recording body contents. A read deadline is reported as 408 so clients can
// distinguish a slow upload from invalid JSON.
func writeModelRequestBodyReadError(w http.ResponseWriter, r *http.Request, body []byte, err error) {
	status, code, message := http.StatusBadRequest, "read_error", "读取请求体失败"
	if requestBodyReadTimedOut(err) {
		status, code, message = http.StatusRequestTimeout, "request_body_timeout", "读取请求体超时，请检查上行带宽或减小请求体"
	}
	log.Printf("[请求体读取失败] traceId=%s 路径=%s 状态码=%d 错误类型=%s 声明字节=%d 已读字节=%d 说明=不记录正文或密钥",
		debugTraceID(r), r.URL.Path, status, debugErrorType(err), r.ContentLength, len(body))
	writeUpstreamError(w, r, status, code, message)
}
