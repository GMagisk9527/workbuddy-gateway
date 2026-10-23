package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// streamWriteGuard 串行化下游 ResponseWriter 的心跳和数据写入。
// net/http 不保证 ResponseWriter 并发安全，心跳 goroutine 与主流读取
// 必须共用同一把锁。
type streamWriteGuard struct {
	mu       sync.Mutex
	w        http.ResponseWriter
	flusher  http.Flusher
	dataSeen bool
	writeErr error
	body     io.Closer
	cancel   context.CancelFunc
}

func (g *streamWriteGuard) setBody(body io.Closer) {
	g.mu.Lock()
	g.body = body
	err := g.writeErr
	g.mu.Unlock()
	if err != nil && body != nil {
		_ = body.Close()
	}
}

// A failed heartbeat must also unblock an upstream read. Keep the body reference
// behind the same lock so a replay never races with the heartbeat goroutine.
func (g *streamWriteGuard) write(frame []byte, data, heartbeat bool) error {
	g.mu.Lock()
	if g.writeErr != nil || (heartbeat && g.dataSeen) {
		err := g.writeErr
		g.mu.Unlock()
		return err
	}
	n, err := g.w.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	var closeBody io.Closer
	if err != nil {
		g.writeErr, closeBody = err, g.body
	} else {
		g.flusher.Flush()
		g.dataSeen = g.dataSeen || data
	}
	g.mu.Unlock()
	if err != nil && g.cancel != nil {
		g.cancel()
	}
	if closeBody != nil {
		_ = closeBody.Close()
	}
	return err
}

func (g *streamWriteGuard) writeFrame(frame []byte) error {
	return g.write(frame, true, false)
}

func (g *streamWriteGuard) writeData(payload string) error {
	return g.writeFrame([]byte(fmt.Sprintf("data: %s\n\n", payload)))
}

// writeUpstreamComment 转发上游已有的 SSE 注释，不把注释算作有效数据。
func (g *streamWriteGuard) writeUpstreamComment(line string) error {
	return g.write([]byte(line+"\n\n"), false, false)
}

func (g *streamWriteGuard) writeHeartbeat() error {
	return g.write([]byte(": keepalive\n\n"), false, true)
}

// markEffectiveData 标记已收到有效上游数据，心跳随即停止。
// Responses 会在读取上游前先写 response.created，这类协议事件不算有效数据。
func (g *streamWriteGuard) markEffectiveData() {
	g.mu.Lock()
	g.dataSeen = true
	g.mu.Unlock()
}

// controlWriter 复用 guard 的写锁输出协议事件，但不把它当作有效模型数据。
// 用于 Responses 的 response.created / in_progress 等前置事件。
type controlWriter struct{ g *streamWriteGuard }

func (w controlWriter) Write(p []byte) (int, error) {
	if err := w.g.write(p, false, false); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (controlWriter) Flush() {}

func (g *streamWriteGuard) err() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.writeErr
}

func (g *streamWriteGuard) hasData() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dataSeen
}

type streamHeartbeat struct {
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// startStreamHeartbeat 在首个有效 data 前周期性下发 SSE 注释；Stop 会等待
// goroutine 退出，避免 handler 返回后仍写 ResponseWriter。
func startStreamHeartbeat(g *streamWriteGuard, interval time.Duration) *streamHeartbeat {
	if interval <= 0 {
		return nil
	}
	h := &streamHeartbeat{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-ticker.C:
				if g.writeHeartbeat() != nil {
					return
				}
			}
		}
	}()
	return h
}

func (h *streamHeartbeat) Stop() {
	if h == nil {
		return
	}
	h.once.Do(func() { close(h.stop) })
	<-h.done
}

// firstByteReplayPath 只覆盖没有协议级重放的入口。Chat 由
// streamChatResponseWithReplay 处理，两套机制不能叠加。
func firstByteReplayPath(path string) bool {
	switch path {
	case "/v1/messages", "/v1/messages/count_tokens", "/v1/responses", "/responses":
		return true
	}
	return false
}

// firstByteReplayBody 在读取层实现「首个字节前重放」：
// 只有读取方尚未拿到任何上游字节时才重发一次，且沿用已选定的账号。
// 它位于 accountStreamBody 内层，重放期间账号锁不会释放，不会触发重新选号，
// 也不会让同一账号并发请求；一旦读出过数据、客户端取消或错误不是瞬时网络
// 故障，就按原错误返回，绝不重放。
type firstByteReplayBody struct {
	reader    io.ReadCloser
	replay    func() (*http.Response, error)
	ctx       context.Context
	r         *http.Request
	reqID     uint64
	model     string
	budget    int
	replays   int
	delivered bool
}

func newFirstByteReplayBody(reader io.ReadCloser, ctx context.Context, r *http.Request, reqID uint64, model string, budget int, replay func() (*http.Response, error)) *firstByteReplayBody {
	return &firstByteReplayBody{reader: reader, ctx: ctx, r: r, reqID: reqID, model: model, budget: budget, replay: replay}
}

func (b *firstByteReplayBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	if n > 0 {
		b.delivered = true
		return n, err
	}
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	if !canReplayStream(err, b.delivered, b.replays < b.budget, b.ctx.Err() != nil) {
		return n, err
	}
	b.replays++
	resp, replayErr := b.replay()
	if replayErr != nil || resp == nil || resp.Body == nil {
		debugEvent(b.r, "warn", "first_byte_replay_failed", map[string]any{
			"attempt": b.replays, "model": b.model,
			"business_impact": "首个有效字节前重放未能建立连接，按原读取错误返回",
		})
		log.Printf("[异常] traceId=%s requestId=%d 发生阶段=首字节前重放 模型=%s 结果=重放请求失败 业务影响=本次响应不完整 异常=%s",
			debugTraceID(b.r), b.reqID, b.model, safeDebugError(replayErr))
		return n, err
	}
	if resp.StatusCode >= 400 {
		// 重放拿到的是业务错误而不是 SSE：读完有限错误体后按原错误语义返回，
		// 不把 JSON 错误体交给 SSE 解析器。
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		if rejection := detectUpstreamStreamRejection(body); rejection != nil {
			return n, rejection
		}
		return n, fmt.Errorf("首字节前重放失败：上游返回 HTTP %d", resp.StatusCode)
	}
	_ = b.reader.Close()
	b.reader = resp.Body
	debugEvent(b.r, "warn", "stream_first_byte_replay", map[string]any{
		"attempt": b.replays, "model": b.model,
		"business_impact": "首个有效字节前重放一次；原调用可能额外消耗上游额度",
	})
	log.Printf("[网络重放] traceId=%s requestId=%d 阶段=首字节前 模型=%s 结果=已重放 说明=下游尚未收到任何上游字节，账号锁未释放，未重新选号",
		debugTraceID(b.r), b.reqID, b.model)
	return b.Read(p)
}

func (b *firstByteReplayBody) Close() error {
	return b.reader.Close()
}

// canReplayStream 只允许在客户端没有收到任何有效数据、预算未用尽、
// 且错误属于瞬时网络故障或上游空闲超时时重放。
// clientCanceled 由调用方根据 r.Context().Err() 判断；内部 idle reader 超时
// 也会表现为 context.Canceled，因此必须与客户端取消区分。
func canReplayStream(scanErr error, sawFirstData, budgetLeft, clientCanceled bool) bool {
	if scanErr == nil || sawFirstData || !budgetLeft || clientCanceled {
		return false
	}
	if errors.Is(scanErr, context.Canceled) || errors.Is(scanErr, context.DeadlineExceeded) {
		return true
	}
	return isTransientNetworkError(scanErr)
}

// replayResponseWriter 捕获重放时 upstreamChat 的错误响应，避免心跳已经
// 把 200 响应头发出后再次 WriteHeader，或把 JSON 错误混进 SSE 流。
type replayResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newReplayResponseWriter(source http.Header) *replayResponseWriter {
	return &replayResponseWriter{header: source.Clone()}
}

func (w *replayResponseWriter) Header() http.Header { return w.header }

func (w *replayResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *replayResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func replayUpstreamChat(w http.ResponseWriter, r *http.Request, reqID uint64, modelName string, upstreamBytes []byte, startTime time.Time, guard *streamWriteGuard) (*http.Response, *Account, *upstreamProfile, bool) {
	captured := newReplayResponseWriter(w.Header())
	resp, acc, prof, ok := upstreamChat(captured, r, reqID, modelName, upstreamBytes, startTime)
	if ok {
		return resp, acc, prof, true
	}
	payload := bytes.TrimSpace(captured.body.Bytes())
	if len(payload) == 0 || !json.Valid(payload) {
		status := captured.status
		if status == 0 {
			status = http.StatusBadGateway
		}
		payload, _ = json.Marshal(map[string]any{
			"error": map[string]any{
				"message": "首字节前重放失败，本次响应不完整",
				"type":    "upstream_stream_interrupted",
				"code":    "stream_replay_failed",
				"status":  status,
			},
		})
	}
	guard.writeData(string(payload))
	log.Printf("[异常] traceId=%s requestId=%d 发生阶段=首字节前重放 模型=%s 结果=重放未取得可用上游响应 业务影响=已通过SSE错误事件通知客户端，不伪造[DONE]",
		debugTraceID(r), reqID, modelName)
	debugEvent(r, "error", "stream_first_byte_replay_failed", map[string]any{
		"status_code":     http.StatusBadGateway,
		"business_impact": "客户端已开始接收SSE，无法再改写HTTP状态，改为下发流内错误",
	})
	return nil, nil, nil, false
}
