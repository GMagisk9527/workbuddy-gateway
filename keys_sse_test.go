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
