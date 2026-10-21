package main

// 请求体压缩解码。
//
// 客户端插件（dsh-plugin-model-request-accelerator 等）会把请求体压成 gzip/br
// 再上传，把 3.8MB 的上下文缩到 1.5MB 左右，直接省掉上行时间。网关这里只负责
// **接收侧解码**：声明了 Content-Encoding 就把 body 还原成原始 JSON 交给现有
// handler，其它请求一个字节都不动。
//
// 取舍：
//   - 只支持 gzip / deflate（标准库即可），brotli/zstd 返回 415，让客户端按
//     自己的协商逻辑回退到 gzip——插件正是这么设计的（411/415/501 触发回退）。
//   - 解压是"读满再解"：这几个 handler 本来就会把 body 全量读进内存解析，
//     这里不额外增加内存模型；但必须有上限，防止压缩炸弹把 244MB 的机器打爆。

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
)

const (
	// 压缩后最多收 16MB（nginx 侧 client_max_body_size 50m 是外层上限）。
	requestCompressedLimit = 16 << 20
	// 解压后最多 32MB：真实长上下文请求约 4~8MB，留足余量又能挡住压缩炸弹。
	requestDecodedLimit = 32 << 20
)

var errRequestBodyTooLarge = errors.New("request body too large")

// decodeRequestBodyMiddleware 在鉴权/准入之后、进入 handler 之前还原压缩的请求体。
func decodeRequestBodyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
		switch encoding {
		case "", "identity":
			next.ServeHTTP(w, r)
			return
		case "gzip", "x-gzip", "deflate":
			// 支持
		default:
			// 交给客户端自行回退（插件收到 415 会改用 gzip 并记住该 endpoint）。
			log.Printf("[请求解码] traceId=%s 结果=拒绝 编码=%s 原因=不支持该压缩算法", w.Header().Get("X-Trace-ID"), encoding)
			writeUpstreamError(w, r, http.StatusUnsupportedMediaType, "unsupported_content_encoding",
				"不支持的请求体压缩算法: "+encoding+"（仅支持 gzip / deflate）")
			return
		}

		raw, err := io.ReadAll(io.LimitReader(r.Body, requestCompressedLimit+1))
		if err != nil {
			writeUpstreamError(w, r, http.StatusBadRequest, "invalid_request_encoding", "读取压缩请求体失败")
			return
		}
		if len(raw) > requestCompressedLimit {
			writeUpstreamError(w, r, http.StatusRequestEntityTooLarge, "request_body_too_large", "压缩请求体超过网关上限")
			return
		}
		start := time.Now()
		decoded, err := decodeCompressedBody(encoding, raw)
		if err != nil {
			if errors.Is(err, errRequestBodyTooLarge) {
				writeUpstreamError(w, r, http.StatusRequestEntityTooLarge, "request_body_too_large", "解压后的请求体超过网关上限")
				return
			}
			log.Printf("[请求解码] traceId=%s 结果=失败 编码=%s 原因=%v", w.Header().Get("X-Trace-ID"), encoding, err)
			writeUpstreamError(w, r, http.StatusBadRequest, "invalid_request_encoding", "请求体不是有效的 "+encoding+" 数据")
			return
		}

		r.Body = io.NopCloser(bytes.NewReader(decoded))
		// 请求体已还原成明文：这两个头必须去掉，否则下游按压缩数据理解。
		r.Header.Del("Content-Encoding")
		r.Header.Del("Content-Length")
		r.ContentLength = int64(len(decoded))
		log.Printf("[请求解码] traceId=%s 编码=%s 压缩后=%d 解压后=%d 耗时ms=%.2f 说明=已还原为明文JSON",
			w.Header().Get("X-Trace-ID"), encoding, len(raw), len(decoded), float64(time.Since(start).Microseconds())/1000)
		next.ServeHTTP(w, r)
	})
}

// decodeCompressedBody 解压并限制解压后大小。
func decodeCompressedBody(encoding string, raw []byte) ([]byte, error) {
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
	default:
		return nil, errors.New("unsupported encoding")
	}
	out, err := io.ReadAll(io.LimitReader(reader, requestDecodedLimit+1))
	if err != nil {
		return nil, err
	}
	if len(out) > requestDecodedLimit {
		return nil, errRequestBodyTooLarge
	}
	return out, nil
}
