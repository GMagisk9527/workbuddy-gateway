package main

// 请求体大小限制与压缩解码。
//
// 客户端插件（dsh-plugin-model-request-accelerator 等）会把请求体压成 gzip/br
// 再上传，把 3.8MB 的上下文缩到 1.5MB 左右，直接省掉上行时间。网关这里只负责
// **接收侧解码**：声明了 Content-Encoding 就把 body 还原成原始 JSON 交给现有
// handler，其它请求一个字节都不动。
//
// 取舍：
//   - 支持 gzip / deflate / zstd；zstd 使用 klauspost/compress，brotli 仍返回 415，
//     让客户端按自己的协商逻辑回退。
//   - 压缩数据和解压后的输入共用在途字节预算，并保留单请求大小上限。
//     准入层另按内存水位限制新请求，给 JSON 解析、协议转换和长流留出余量。

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	// 压缩后最多收 16MiB；反向代理可另设更低的外层上限。
	requestCompressedLimit = 16 << 20
	// 明文或解压后的 JSON 最多 32MiB。
	requestDecodedLimit = 32 << 20
)

var errRequestBodyTooLarge = errors.New("request body too large")

// decodeRequestBodyMiddleware 在鉴权/准入之后、进入 handler 之前还原压缩的请求体。
func decodeRequestBodyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
		switch encoding {
		case "", "identity":
			if r.ContentLength > requestDecodedLimit {
				writeUpstreamError(w, r, http.StatusRequestEntityTooLarge, "request_body_too_large", "请求体超过网关上限")
				return
			}
			if err := wrapMemoryBody(r); err != nil {
				writeRequestMemoryError(w, r, err)
				return
			}
			// A missing Content-Length (chunked upload) must have the same bound.
			// Limit while reading instead of buffering another copy in middleware.
			r.Body = http.MaxBytesReader(w, r.Body, requestDecodedLimit)
			next.ServeHTTP(w, r)
			return
		case "gzip", "x-gzip", "deflate", "zstd":
			if r.ContentLength > requestCompressedLimit {
				writeUpstreamError(w, r, http.StatusRequestEntityTooLarge, "request_body_too_large", "压缩请求体超过网关上限")
				return
			}
		default:
			// 交给客户端自行回退（插件收到 415 会改用 gzip 并记住该 endpoint）。
			log.Printf("[请求解码] traceId=%s 结果=拒绝 编码=%s 原因=不支持该压缩算法", w.Header().Get("X-Trace-ID"), encoding)
			writeUpstreamError(w, r, http.StatusUnsupportedMediaType, "unsupported_content_encoding",
				"不支持的请求体压缩算法: "+encoding+"（仅支持 gzip / deflate / zstd）")
			return
		}
		if err := wrapMemoryBody(r); err != nil {
			writeRequestMemoryError(w, r, err)
			return
		}

		readStarted := performanceStarted(r)
		raw, err := io.ReadAll(io.LimitReader(r.Body, requestCompressedLimit+1))
		recordPerformance(r, phaseRead, readStarted, err != nil || len(raw) > requestCompressedLimit)
		if err != nil {
			if writeRequestMemoryError(w, r, err) {
				return
			}
			writeCompressedRequestBodyReadError(w, r, raw, err)
			return
		}
		if len(raw) > requestCompressedLimit {
			writeUpstreamError(w, r, http.StatusRequestEntityTooLarge, "request_body_too_large", "压缩请求体超过网关上限")
			return
		}
		start := time.Now()
		decoded, err := decodeCompressedBodyWithMemory(encoding, raw, memoryLeaseFor(r))
		recordPerformance(r, phaseDecompress, start, err != nil)
		if err != nil {
			if writeRequestMemoryError(w, r, err) {
				return
			}
			if errors.Is(err, errRequestBodyTooLarge) {
				writeUpstreamError(w, r, http.StatusRequestEntityTooLarge, "request_body_too_large", "请求体超过网关上限")
				return
			}
			log.Printf("[请求解码] traceId=%s 结果=失败 编码=%s 原因=%v", w.Header().Get("X-Trace-ID"), encoding, err)
			writeUpstreamError(w, r, http.StatusBadRequest, "invalid_request_encoding", "请求体不是有效的 "+encoding+" 数据")
			return
		}

		r.Body = newDecodedRequestBody(decoded)
		// 请求体已还原成明文：这两个头必须去掉，否则下游按压缩数据理解。
		r.Header.Del("Content-Encoding")
		r.Header.Del("Content-Length")
		r.ContentLength = int64(len(decoded))
		log.Printf("[请求解码] traceId=%s 编码=%s 压缩后=%d 解压后=%d 耗时ms=%.2f 说明=已还原为明文JSON",
			w.Header().Get("X-Trace-ID"), encoding, len(raw), len(decoded), float64(time.Since(start).Microseconds())/1000)
		next.ServeHTTP(w, r)
	})
}

// The protocol handlers keep their existing errors for ordinary read failures,
// but the shared size boundary must be a 413 in each protocol's error shape.
func writeRequestBodyLimitError(w http.ResponseWriter, r *http.Request, err error) bool {
	if writeRequestMemoryError(w, r, err) {
		return true
	}
	var limitErr *http.MaxBytesError
	if !errors.As(err, &limitErr) {
		return false
	}
	writeUpstreamError(w, r, http.StatusRequestEntityTooLarge, "request_body_too_large", "请求体超过网关上限")
	return true
}

// decodeCompressedBody 解压并限制解压后大小。
func decodeCompressedBody(encoding string, raw []byte) ([]byte, error) {
	return decodeCompressedBodyWithMemory(encoding, raw, nil)
}

func decodeCompressedBodyWithMemory(encoding string, raw []byte, lease *requestMemoryLease) ([]byte, error) {
	var reader io.Reader
	switch encoding {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		reader = zr
	case "deflate":
		// HTTP 的 deflate 规范上是 zlib 包装，但历史上也有裸 deflate 的客户端。
		if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			defer zr.Close()
			reader = zr
		} else {
			reader = flate.NewReader(bytes.NewReader(raw))
		}
	case "zstd":
		// Bound decoder window/memory independently of the compressed input.
		zr, err := zstd.NewReader(bytes.NewReader(raw),
			zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(uint64(requestDecodedLimit)),
			zstd.WithDecoderMaxWindow(uint64(requestDecodedLimit)),
		)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		reader = zr
	default:
		return nil, errors.New("unsupported encoding")
	}
	if lease != nil {
		var err error
		reader, err = lease.reader(reader, -1)
		if err != nil {
			return nil, err
		}
	}
	out, err := io.ReadAll(io.LimitReader(reader, requestDecodedLimit+1))
	if err != nil {
		if errors.Is(err, zstd.ErrDecoderSizeExceeded) ||
			(errors.Is(err, zstd.ErrWindowSizeExceeded) && zstdWindowExceedsLimit(raw)) {
			return nil, errRequestBodyTooLarge
		}
		return nil, err
	}
	if len(out) > requestDecodedLimit {
		return nil, errRequestBodyTooLarge
	}
	return out, nil
}

// ErrWindowSizeExceeded also describes corrupt blocks. Inspect frame boundaries
// only on that error, so a malformed small frame is not mistaken for our limit.
func zstdWindowExceedsLimit(raw []byte) bool {
	for len(raw) > 0 {
		var h zstd.Header
		if h.Decode(raw) != nil {
			return false
		}
		n := h.HeaderSize
		if h.Skippable {
			if uint64(h.SkippableSize) > uint64(len(raw)-n) {
				return false
			}
			raw = raw[n+int(h.SkippableSize):]
			continue
		}
		window := h.WindowSize
		if h.SingleSegment {
			window = max(h.FrameContentSize, 1024)
		}
		if window > uint64(requestDecodedLimit) {
			return true
		}
		for {
			if len(raw)-n < 3 {
				return false
			}
			block := uint32(raw[n]) | uint32(raw[n+1])<<8 | uint32(raw[n+2])<<16
			n += 3
			kind, size := (block>>1)&3, int(block>>3)
			if kind == 3 || size > 128<<10 || uint64(size) > window {
				return false
			}
			if kind == 1 { // RLE stores a single byte, regardless of output size.
				size = 1
			}
			if size > len(raw)-n {
				return false
			}
			n += size
			if block&1 != 0 {
				break
			}
		}
		if h.HasCheckSum {
			n += 4
		}
		if n > len(raw) {
			return false
		}
		raw = raw[n:]
	}
	return false
}

// withRequestBodyDecode 把解码中间件套在单个协议处理函数上：
// 只有这些路由会读取并解压请求体，免鉴权路径与探测接口的鉴权前阶段都不碰 body。
func withRequestBodyDecode(h http.HandlerFunc) http.HandlerFunc {
	decoded := decodeRequestBodyMiddleware(http.HandlerFunc(h))
	admitted := requestAdmissionMiddleware(decoded)
	return func(w http.ResponseWriter, r *http.Request) { admitted.ServeHTTP(w, r) }
}
