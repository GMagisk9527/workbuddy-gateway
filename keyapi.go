package main

// 多 Key 的 HTTP 层：准入中间件、自助用量查询、管理接口。

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// keyAdmissionMiddleware 在鉴权之后做「启用/过期/额度/并发」准入，并在请求结束时释放并发位。
// 模型白名单不在这一层判断：模型名在请求体里，重复解析大 JSON 代价高，
// 由各协议 handler 在解析完成后用 keyModelAllowed 校验。
func keyAdmissionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := keyIdentityFor(r)
		if id == nil || isAuthExemptPath(r.URL.Path) || r.URL.Path == "/v1/usage" {
			next.ServeHTTP(w, r)
			return
		}
		k := keyByID(id.ID)
		if k == nil {
			writeUpstreamError(w, r, http.StatusUnauthorized, "invalid_api_key", "未提供有效 API 密钥")
			return
		}
		admitErr, release := keyAdmit(k, "")
		if admitErr != nil {
			log.Printf("[APIKey] traceId=%s 结果=拒绝 原因=%s 状态码=%d key=%s", w.Header().Get("X-Trace-ID"), admitErr.Code, admitErr.Status, k.ID)
			writeUpstreamError(w, r, admitErr.Status, admitErr.Code, admitErr.Message)
			return
		}
		defer release()
		next.ServeHTTP(w, r)
	})
}

// keyModelAllowed 校验模型白名单；不通过时已写好错误响应。
func keyModelAllowed(w http.ResponseWriter, r *http.Request, model string) bool {
	id := keyIdentityFor(r)
	if id == nil {
		return true
	}
	k := keyByID(id.ID)
	if k == nil {
		return true
	}
	if k.allowModel(model) {
		return true
	}
	log.Printf("[APIKey] traceId=%s 结果=模型不允许 key=%s 模型=%s", w.Header().Get("X-Trace-ID"), k.ID, normalizeModelName(model))
	writeUpstreamError(w, r, http.StatusForbidden, "model_not_allowed",
		"该 API Key 无权使用模型 "+normalizeModelName(model))
	return false
}

// keyAllowedModelFilter 返回模型是否对该 Key 可见（/v1/models 过滤用）。
func keyAllowedModelFilter(r *http.Request) func(string) bool {
	id := keyIdentityFor(r)
	if id == nil {
		return func(string) bool { return true }
	}
	k := keyByID(id.ID)
	if k == nil || k.legacy || len(k.Models) == 0 {
		return func(string) bool { return true }
	}
	return k.allowModel
}

// ---------------------------------------------------------------------------
// 自助用量：GET /v1/usage
// ---------------------------------------------------------------------------

func handleKeyUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeUpstreamError(w, r, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 GET")
		return
	}
	id := keyIdentityFor(r)
	if id == nil {
		writeUpstreamError(w, r, http.StatusUnauthorized, "invalid_api_key", "未提供有效 API 密钥")
		return
	}
	k := keyByID(id.ID)
	if k == nil {
		writeUpstreamError(w, r, http.StatusUnauthorized, "invalid_api_key", "未提供有效 API 密钥")
		return
	}
	view := keyUsageSnapshot(k)
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "key_usage",
		"id":     k.ID,
		"key":    maskKeyPrefix(k),
		"legacy": k.isLegacy(),
		"usage":  view,
	})
}

func maskKeyPrefix(k *apiKey) string {
	if k == nil {
		return ""
	}
	if k.isLegacy() {
		return "config.apiKey（兼容旧 Key）"
	}
	return k.Prefix + "…"
}

// ---------------------------------------------------------------------------
// 管理接口：/admin/api/keys
// ---------------------------------------------------------------------------

type keyAdminView struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Masked        string       `json:"masked"`
	Status        string       `json:"status"`
	Legacy        bool         `json:"legacy"`
	Enabled       bool         `json:"enabled"`
	CreatedAt     int64        `json:"createdAt"`
	ExpiresAt     int64        `json:"expiresAt,omitempty"`
	LastUsedAt    int64        `json:"lastUsedAt,omitempty"`
	Models        []string     `json:"models,omitempty"`
	MaxConcurrent int          `json:"maxConcurrent"`
	QuotaTotal    float64      `json:"quotaTotal"`
	QuotaDaily    float64      `json:"quotaDaily"`
	QuotaMonthly  float64      `json:"quotaMonthly"`
	Note          string       `json:"note,omitempty"`
	Usage         keyUsageView `json:"usage"`
	LastReject    string       `json:"lastReject,omitempty"`
}

func keyAdminSnapshot(k *apiKey) keyAdminView {
	view := keyUsageSnapshot(k)
	k.mu.Lock()
	reject := k.lastReject
	k.mu.Unlock()
	return keyAdminView{
		ID: k.ID, Name: k.Name, Masked: maskKeyPrefix(k), Status: view.Status, Legacy: k.isLegacy(),
		Enabled: k.Enabled && !k.isLegacy(), CreatedAt: k.CreatedAt, ExpiresAt: k.ExpiresAt,
		LastUsedAt: view.LastUsed, Models: k.Models, MaxConcurrent: k.concurrency(),
		QuotaTotal: k.QuotaTotal, QuotaDaily: k.QuotaDaily, QuotaMonthly: k.QuotaMonthly,
		Note: k.Note, Usage: view, LastReject: reject,
	}
}

type keyAdminRequest struct {
	Action        string   `json:"action"`
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	QuotaTotal    float64  `json:"quotaTotal"`
	QuotaDaily    float64  `json:"quotaDaily"`
	QuotaMonthly  float64  `json:"quotaMonthly"`
	ExpiresAt     int64    `json:"expiresAt"`
	Models        []string `json:"models"`
	MaxConcurrent int      `json:"maxConcurrent"`
	Note          string   `json:"note"`
	Enabled       *bool    `json:"enabled"`
	Confirm       bool     `json:"confirm"`
}

func handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeAdminData(w, r, keyAdminListPayload())
		return
	case http.MethodPost:
	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "仅支持 GET/POST")
		return
	}

	var in keyAdminRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	opts := keyCreateOptions{
		Name: in.Name, QuotaTotal: in.QuotaTotal, QuotaDaily: in.QuotaDaily, QuotaMonthly: in.QuotaMonthly,
		ExpiresAt: in.ExpiresAt, Models: in.Models, MaxConcurrent: in.MaxConcurrent, Note: in.Note,
	}

	switch in.Action {
	case "create":
		plain, k, err := keyCreate(opts)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "创建密钥失败："+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "key": plain, "item": keyAdminSnapshot(k),
			"notice": "完整密钥只显示这一次，关闭后无法再次查看。",
		})
	case "update":
		k, err := keyUpdate(in.ID, opts)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAdminData(w, r, map[string]any{"ok": true, "item": keyAdminSnapshot(k)})
	case "toggle":
		if in.Enabled == nil {
			writeJSONError(w, http.StatusBadRequest, "缺少 enabled")
			return
		}
		if err := keySetEnabled(in.ID, *in.Enabled); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAdminData(w, r, keyAdminListPayload())
	case "rotate":
		if !in.Confirm {
			writeJSONError(w, http.StatusBadRequest, "重新生成会让旧密钥立即失效，请确认后重试")
			return
		}
		plain, k, err := keyRotate(in.ID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "key": plain, "item": keyAdminSnapshot(k),
			"notice": "旧密钥已立即失效；完整密钥只显示这一次。",
		})
	case "delete":
		if !in.Confirm {
			writeJSONError(w, http.StatusBadRequest, "删除不可撤销，请确认后重试")
			return
		}
		if err := keyDelete(in.ID); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAdminData(w, r, keyAdminListPayload())
	case "reset":
		if !in.Confirm {
			writeJSONError(w, http.StatusBadRequest, "重置用量请确认后重试")
			return
		}
		if err := keyResetUsage(in.ID); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAdminData(w, r, keyAdminListPayload())
	default:
		writeJSONError(w, http.StatusBadRequest, "未知操作")
	}
}

// keyAdminListPayload 组装列表 + 概览。
func keyAdminListPayload() map[string]any {
	items := keyList()
	views := make([]keyAdminView, 0, len(items))
	var totalAll, todayAll float64
	enabledCount := 0
	today := keyDayKey(time.Now())
	for _, k := range items {
		v := keyAdminSnapshot(k)
		if strings.HasPrefix(v.Status, "active") {
			enabledCount++
		}
		totalAll += v.Usage.Used
		if v.Usage.DayKey == today {
			todayAll += v.Usage.DailyUsed
		}
		views = append(views, v)
	}
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].Legacy != views[j].Legacy {
			return views[i].Legacy
		}
		return views[i].CreatedAt < views[j].CreatedAt
	})
	return map[string]any{
		"ok": true, "items": views,
		"summary": map[string]any{
			"total": len(views), "enabled": enabledCount,
			"allTimeCredits": totalAll, "todayCredits": todayAll,
			"dayKey": today, "ledger": ledgerStats(),
		},
		"legacyConfigured": currentAPIKey() != "",
	}
}

// errNotFound 便于测试断言。
var errNotFound = errors.New("not found")
