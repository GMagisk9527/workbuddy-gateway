package main

// 真正经过网关协议链的 SSE 集成测试（httptest 模拟上游，不消耗真实积分）。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type sseCase struct {
	name        string
	sendUsage   bool
	sendCredit  float64
	abortMiddle bool
}

func TestKeySettlementThroughRealSSEChain(t *testing.T) {
	isolatedGatewayConfig(t)
	resetModelFilter()
	t.Cleanup(resetModelFilter)
	oldCN := profileCN
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accountMu.Unlock()
	t.Cleanup(func() {
		profileCN = oldCN
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: false}); err != nil {
		t.Fatal(err)
	}

	// 每个场景由 caseIdx 决定上游返回什么
	var caseIdx atomic.Int32
	const model = "sse-test-model"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 不校验具体路径：这里只验证结算链路，上游 URL 由 profile 决定。
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"c1\",\"model\":%q,\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n", model)
		if flusher != nil {
			flusher.Flush()
		}
		switch caseIdx.Load() {
		case 0: // 正常完成且带 credit
			io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"usage\":{\"credit\":1.5,\"prompt_tokens\":100,\"completion_tokens\":10,\"total_tokens\":110}}\n\ndata: [DONE]\n\n")
		case 1: // 正常完成但没有 usage
			io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}]}\n\ndata: [DONE]\n\n")
		case 2: // 中途断开，没有任何终止帧
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"part\"}}]}\n\n")
		}
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer up.Close()
	profileCN.Base, profileCN.Origin = up.URL, up.URL
	cfg.HttpClient = up.Client()

	acc := &Account{Path: "sse.json", Auth: &StoredAuth{
		Edition: "cn", Account: StoredAccount{UID: "u-sse"},
		Auth: StoredTokens{AccessToken: "synthetic", ExpiresAt: time.Now().Add(time.Hour).Unix(), Domain: "synthetic.example"},
	}, QuotaKnown: true, QuotaRemaining: 1000,
		ModelStates: map[string]*modelRuntimeState{model: {CostClass: modelCostFree}}}
	accountMu.Lock()
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()

	// 受管 Key
	resetKeyWorld(t)
	if err := ledgerLoad(keyDataDir); err != nil {
		t.Fatal(err)
	}
	plain, k := mustCreateKey(t, keyCreateOptions{Name: "sse", MaxConcurrent: 4})

	srv := httptest.NewServer(newAPIHandler())
	defer srv.Close()

	call := func(ctx context.Context) int {
		body := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+plain)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	// 场景 1：正常完成、带 credit
	caseIdx.Store(0)
	if code := call(context.Background()); code != 200 {
		t.Fatalf("正常流应 200，实际 %d", code)
	}
	k.mu.Lock()
	total1, req1 := k.usage.Total, k.usage.Requests
	k.mu.Unlock()
	if total1 != 1.5 || req1 != 1 {
		t.Fatalf("应记 1.5 分 / 1 次请求，实际 %v / %d", total1, req1)
	}

	// 场景 2：正常完成但没有 usage → 未知，不是免费
	caseIdx.Store(1)
	if code := call(context.Background()); code != 200 {
		t.Fatalf("无 usage 的流也应 200，实际 %d", code)
	}
	k.mu.Lock()
	unknown2 := k.usage.Unknown
	req2 := k.usage.Requests
	k.mu.Unlock()
	if unknown2 != 1 || req2 != 2 {
		t.Fatalf("无 usage 应记 1 次未知 / 2 次请求，实际 %d / %d", unknown2, req2)
	}

	// 场景 3：客户端中途取消
	caseIdx.Store(2)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_ = call(ctx)
	time.Sleep(80 * time.Millisecond) // 让服务端收尾

	k.mu.Lock()
	req3, unknown3, total3 := k.usage.Requests, k.usage.Unknown, k.usage.Total
	k.mu.Unlock()
	if total3 != 1.5 {
		t.Fatalf("取消的请求不得凭空扣费，实际 total=%v", total3)
	}
	if req3 != 3 {
		t.Fatalf("取消的请求也应计 1 次请求（共 3 次），实际 %d", req3)
	}
	if unknown3 < 1 {
		t.Fatalf("取消且无 usage 应计未知，实际 unknown=%d", unknown3)
	}

	// 结果必须落盘且幂等：重放后与内存一致
	memTotal, memReq, memUnknown := total3, req3, unknown3
	reloadKeysAndLedger(t, keyDataDir)
	got := keyByID(k.ID)
	if got == nil {
		t.Fatal("重放后 Key 应仍存在")
	}
	got.mu.Lock()
	dTotal, dReq, dUnknown := got.usage.Total, got.usage.Requests, got.usage.Unknown
	got.mu.Unlock()
	if dTotal != memTotal || dReq != memReq || dUnknown != memUnknown {
		t.Fatalf("重放与内存不一致: 内存=%v/%d/%d 重放=%v/%d/%d",
			memTotal, memReq, memUnknown, dTotal, dReq, dUnknown)
	}
}
// 真实链路：上游阻塞在中途时删除 Key，在途请求的 3 分必须进匿名归档。
func TestInFlightDeleteThroughRealHTTPChain(t *testing.T) {
	isolatedGatewayConfig(t)
	resetModelFilter()
	t.Cleanup(resetModelFilter)
	oldCN := profileCN
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accountMu.Unlock()
	t.Cleanup(func() {
		profileCN = oldCN
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: false}); err != nil {
		t.Fatal(err)
	}

	const model = "inflight-model"
	release := make(chan struct{})
	firstFrameSent := make(chan struct{})
	upstreamExited := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamExited)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"c\",\"model\":%q,\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"part\"}}]}\n\n", model)
		if flusher != nil {
			flusher.Flush()
		}
		close(firstFrameSent)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"usage\":{\"credit\":3,\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer up.Close()
	profileCN.Base, profileCN.Origin = up.URL, up.URL
	cfg.HttpClient = up.Client()

	acc := &Account{Path: "inflight.json", Auth: &StoredAuth{
		Edition: "cn", Account: StoredAccount{UID: "u-inflight"},
		Auth: StoredTokens{AccessToken: "synthetic", ExpiresAt: time.Now().Add(time.Hour).Unix(), Domain: "synthetic.example"},
	}, QuotaKnown: true, QuotaRemaining: 1000,
		ModelStates: map[string]*modelRuntimeState{model: {CostClass: modelCostFree}}}
	accountMu.Lock()
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()

	resetKeyWorld(t)
	if err := ledgerLoad(keyDataDir); err != nil {
		t.Fatal(err)
	}
	plain, k := mustCreateKey(t, keyCreateOptions{Name: "inflight", MaxConcurrent: 2})
	keyID := k.ID

	srv := httptest.NewServer(newAPIHandler())
	defer srv.Close()

	body := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 等第一帧真正到达客户端，证明请求已经进入上游流式中途
	buf := make([]byte, 256)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("首帧读取失败: %v", err)
	}
	<-firstFrameSent

	// 在途时删除 Key
	if err := keyDelete(keyID); err != nil {
		t.Fatal(err)
	}
	if keyByID(keyID) != nil {
		t.Fatal("删除后索引不应再有该 Key")
	}

	close(release) // 上游继续给出真实 usage.credit=3
	_, _ = io.Copy(io.Discard, resp.Body)
	select {
	case <-upstreamExited:
	case <-time.After(2 * time.Second):
		t.Fatal("上游未在预期时间内退出")
	}

	// 3 分必须进入匿名归档
	archived := archivedUsage(keyID)
	if archived == nil || archived.Total != 3 {
		t.Fatalf("在途请求的 3 分应进入归档，实际 %+v", archived)
	}

	// 压缩 + 重启后仍在，且不重复
	ledgerCompact()
	reloadKeysAndLedger(t, keyDataDir)
	if a := archivedUsage(keyID); a == nil || a.Total != 3 {
		t.Fatalf("重启后归档应仍为 3，实际 %+v", a)
	}

	// 已删除的 Key 不能再发起新请求
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+plain)
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("已删除 Key 的新请求应 401，实际 %d", resp2.StatusCode)
	}
}

// 客户端中途取消：用 barrier 控制上游，证明取消确实发生在流式中途。
func TestClientCancelMidStreamSettlesOnce(t *testing.T) {
	isolatedGatewayConfig(t)
	resetModelFilter()
	t.Cleanup(resetModelFilter)
	oldCN := profileCN
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accountMu.Unlock()
	t.Cleanup(func() {
		profileCN = oldCN
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
	if err := applyGatewayConfig(gatewayFileConfig{APIKeyEnabled: false}); err != nil {
		t.Fatal(err)
	}

	const model = "cancel-model"
	release := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	var upstreamCreditSent atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"id\":\"c\",\"model\":%q,\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"first\"}}]}\n\n", model)
		if flusher != nil {
			flusher.Flush()
		}
		// 阻塞等待：要么被放行（正常收尾），要么网关关闭上游连接（客户端取消）
		select {
		case <-release:
		case <-r.Context().Done():
			close(upstreamCanceled)
			return
		}
		upstreamCreditSent.Store(true)
		io.WriteString(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{}}],\"usage\":{\"credit\":2,\"prompt_tokens\":10,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer up.Close()
	profileCN.Base, profileCN.Origin = up.URL, up.URL
	cfg.HttpClient = up.Client()

	acc := &Account{Path: "cancel.json", Auth: &StoredAuth{
		Edition: "cn", Account: StoredAccount{UID: "u-cancel"},
		Auth: StoredTokens{AccessToken: "synthetic", ExpiresAt: time.Now().Add(time.Hour).Unix(), Domain: "synthetic.example"},
	}, QuotaKnown: true, QuotaRemaining: 1000,
		ModelStates: map[string]*modelRuntimeState{model: {CostClass: modelCostFree}}}
	accountMu.Lock()
	accounts, rrIndex = []*Account{acc}, 0
	accountMu.Unlock()

	resetKeyWorld(t)
	if err := ledgerLoad(keyDataDir); err != nil {
		t.Fatal(err)
	}
	plain, k := mustCreateKey(t, keyCreateOptions{Name: "cancel", MaxConcurrent: 2})

	srv := httptest.NewServer(newAPIHandler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	body := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 确认首帧已到达（证明已进入流式中途）
	buf := make([]byte, 256)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("首帧读取失败: %v", err)
	}
	// 明确取消，而不是靠 sleep
	cancel()
	// 服务端观察到取消：网关会关闭上游响应体
	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("服务端没有观察到客户端取消（上游连接未被关闭）")
	}
	if upstreamCreditSent.Load() {
		t.Fatal("取消发生在上游返回 credit 之前，不应有 credit")
	}

	// 等待结算完成（最多 3 秒，用轮询条件而不是固定 sleep）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		k.mu.Lock()
		reqs := k.usage.Requests
		k.mu.Unlock()
		if reqs >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	k.mu.Lock()
	reqs, unknown, total, zero := k.usage.Requests, k.usage.Unknown, k.usage.Total, k.usage.ZeroCost
	k.mu.Unlock()
	if reqs != 1 {
		t.Fatalf("取消的请求应恰好结算 1 次，实际 %d", reqs)
	}
	if total != 0 {
		t.Fatalf("没有 credit 时不得凭空扣费，实际 total=%v", total)
	}
	if zero != 0 {
		t.Fatalf("缺失 credit 不能记成零费用，实际 zero=%d", zero)
	}
	if unknown != 1 {
		t.Fatalf("缺失 credit 应恰好记 1 次未知，实际 %d", unknown)
	}

	// 重放后不重复
	reloadKeysAndLedger(t, keyDataDir)
	got := keyByID(k.ID)
	got.mu.Lock()
	r2, u2 := got.usage.Requests, got.usage.Unknown
	got.mu.Unlock()
	if r2 != 1 || u2 != 1 {
		t.Fatalf("重放后应仍为 1 次请求 / 1 次未知，实际 %d / %d", r2, u2)
	}
}