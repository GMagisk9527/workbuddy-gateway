package main

// /admin/probe 的安全回归：只允许管理密钥 + 回环来源，且必须限制探测规模。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	probeTestAdmin  = "probe-test-admin-key"
	probeTestLegacy = "probe-test-legacy-key"
)

func setupProbeTest(t *testing.T, adminKey string) {
	t.Helper()
	isolatedGatewayConfig(t)
	resetModelFilter()
	t.Cleanup(resetModelFilter)
	accountMu.Lock()
	oldAccounts, oldRR := accounts, rrIndex
	accounts, rrIndex = nil, 0 // 默认空账号池：拒绝路径不会触达上游
	accountMu.Unlock()
	t.Cleanup(func() {
		accountMu.Lock()
		accounts, rrIndex = oldAccounts, oldRR
		accountMu.Unlock()
	})
	cfgFile := gatewayFileConfig{APIKeyEnabled: true, APIKey: probeTestLegacy}
	if adminKey != "" {
		cfgFile.AdminKey = adminKey
	}
	if err := applyGatewayConfig(cfgFile); err != nil {
		t.Fatal(err)
	}
	resetKeyWorld(t)
}

// probeCall 发一次探测请求；remote 为空时用默认（非回环）来源。
func probeCall(t *testing.T, h http.Handler, token, remote, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/probe", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if remote != "" {
		req.RemoteAddr = remote
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestProbeAuthMatrix(t *testing.T) {
	setupProbeTest(t, probeTestAdmin)
	plain, _ := mustCreateKey(t, keyCreateOptions{Name: "probe-client"})
	h := newAPIHandler()
	const loop = "127.0.0.1:5555"

	cases := []struct {
		name  string
		token string
		wan   string
		want  int
	}{
		{"匿名", "", loop, http.StatusUnauthorized},
		{"普通受管 Key", plain, loop, http.StatusUnauthorized},
		{"旧 gateway.apiKey", probeTestLegacy, loop, http.StatusUnauthorized},
		{"管理密钥（回环）", probeTestAdmin, loop, http.StatusNotFound}, // 通过鉴权后因无账号返回 404
		{"管理密钥（非回环）", probeTestAdmin, "203.0.113.9:5555", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := probeCall(t, h, tc.token, tc.wan, `{"limit":1}`)
			if rec.Code != tc.want {
				t.Fatalf("状态码 = %d，期望 %d，响应=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// 经 Nginx/Caddy 转发时 RemoteAddr 也是回环，普通 Key 仍然不能探测。
func TestProbeRejectsManagedKeyFromProxiedLoopback(t *testing.T) {
	setupProbeTest(t, probeTestAdmin)
	plain, _ := mustCreateKey(t, keyCreateOptions{Name: "behind-proxy"})
	h := newAPIHandler()

	req := httptest.NewRequest(http.MethodPost, "/admin/probe", strings.NewReader(`{"limit":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plain)
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("X-Real-IP", "127.0.0.1")
	req.RemoteAddr = "127.0.0.1:6000" // 模拟本机反代
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("代理转发场景下普通 Key 必须被拒，实际 %d", rec.Code)
	}
}

// 没有配置管理密钥时，探测接口必须关闭（不回退到普通 Key，也不允许匿名）。
func TestProbeRejectsWhenAdminKeyMissing(t *testing.T) {
	setupProbeTest(t, "")
	plain, _ := mustCreateKey(t, keyCreateOptions{Name: "no-admin"})
	h := newAPIHandler()
	const loop = "127.0.0.1:5555"

	for _, token := range []string{"", plain, probeTestLegacy} {
		rec := probeCall(t, h, token, loop, `{"limit":1}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("未配置 adminKey 时必须 503，token=%q 实际 %d", token, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "admin_key_not_configured") {
			t.Fatalf("响应应说明原因，实际 %s", rec.Body.String())
		}
	}
}

// 普通模型 API 的鉴权与额度不受影响。
func TestNormalAPIAuthUnaffectedByProbeFix(t *testing.T) {
	setupProbeTest(t, probeTestAdmin)
	h := newAPIHandler()

	get := func(token string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := get(""); code != http.StatusUnauthorized {
		t.Fatalf("匿名访问 /v1/models 应 401，实际 %d", code)
	}
	if code := get(probeTestLegacy); code != http.StatusOK {
		t.Fatalf("旧 Key 访问 /v1/models 应 200，实际 %d", code)
	}
}

// 显式传大量模型必须被拒，且不产生任何上游调用。
func TestProbeRejectsTooManyModels(t *testing.T) {
	setupProbeTest(t, probeTestAdmin)
	h := newAPIHandler()

	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer up.Close()
	oldBase := profileCN.Base
	profileCN.Base = up.URL
	t.Cleanup(func() { profileCN.Base = oldBase })

	accountMu.Lock()
	accounts = []*Account{{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}}}}
	accountMu.Unlock()

	body := `{"models":["m1","m2","m3","m4","m5","m6"]}`
	rec := probeCall(t, h, probeTestAdmin, "127.0.0.1:5555", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("6 个模型应被拒绝（400），实际 %d: %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("被拒绝的请求不得调用上游，实际 %d 次", calls.Load())
	}
}

// 去重后不超限：重复模型不能绕过上限。
func TestProbeDeduplicatesModels(t *testing.T) {
	setupProbeTest(t, probeTestAdmin)
	h := newAPIHandler()
	// 账号池留空：去重通过后会在"没有匹配账号"处返回 404，
	// 这样既能证明没被数量校验拦下，又完全不触达上游。
	accountMu.Lock()
	accounts = nil
	accountMu.Unlock()

	// 5 个不同模型 + 2 个重复 → 去重后 5 个，应当通过数量校验
	body := `{"models":["m1","m2","m3","m4","m5","m1","m1"]}`
	rec := probeCall(t, h, probeTestAdmin, "127.0.0.1:5555", body)
	if rec.Code == http.StatusBadRequest {
		t.Fatalf("去重后不应超限，实际被拒: %s", rec.Body.String())
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("空账号池应返回 404 no_account，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 账号数 × 模型数 超过 10 次必须拒绝，且零上游调用。
func TestProbeRejectsTooManyUpstreamCalls(t *testing.T) {
	setupProbeTest(t, probeTestAdmin)
	h := newAPIHandler()

	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer up.Close()
	oldBase := profileCN.Base
	profileCN.Base = up.URL
	t.Cleanup(func() { profileCN.Base = oldBase })

	accountMu.Lock()
	accounts = []*Account{
		{Path: "a.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}}},
		{Path: "b.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}}},
		{Path: "c.json", Auth: &StoredAuth{Edition: "cn", Auth: StoredTokens{AccessToken: "t", ExpiresAt: time.Now().Add(time.Hour).Unix()}}},
	}
	accountMu.Unlock()

	// 3 账号 × 5 模型 = 15 > 10
	body := `{"models":["m1","m2","m3","m4","m5"]}`
	rec := probeCall(t, h, probeTestAdmin, "127.0.0.1:5555", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("账号×模型超限应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("超限请求不得产生上游调用，实际 %d", calls.Load())
	}
}

// 超大的 limit 参数不能绕过上限。
func TestProbeRejectsOversizedLimit(t *testing.T) {
	setupProbeTest(t, probeTestAdmin)
	h := newAPIHandler()
	for _, limit := range []string{"50", "1000", "0", "-3"} {
		rec := probeCall(t, h, probeTestAdmin, "127.0.0.1:5555", `{"limit":`+limit+`}`)
		switch limit {
		case "0", "-3":
			// 非正数回落默认值，属于合法输入；只要不是 500 即可
			if rec.Code >= 500 {
				t.Fatalf("limit=%s 不应造成服务端错误: %d", limit, rec.Code)
			}
		default:
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("limit=%s 应 400，实际 %d", limit, rec.Code)
			}
		}
	}
}
