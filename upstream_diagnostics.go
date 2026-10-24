package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Error() is deliberately safe for existing logging paths. The upstream body is
// retained only for the caller, never interpolated into a diagnostic log.
type upstreamStreamRejection struct {
	info upstreamRejection
	body string
}

func (e *upstreamStreamRejection) Error() string {
	return "upstream SSE rejected the request (code=" + e.info.Code + ")"
}

func (e *upstreamStreamRejection) clientMessage() string {
	// The body is forwarded to the caller, so apply the same identifier masking
	// used for logs and account state.
	return sanitizeUpstreamText("upstream SSE: " + e.body)
}

func detectUpstreamStreamRejection(data []byte) *upstreamStreamRejection {
	// Normal token chunks need no extra JSON decode.
	if !bytes.Contains(data, []byte("11128")) {
		return nil
	}
	info := classifyUpstreamRejection(data)
	if info.Code != "11128" {
		return nil
	}
	return &upstreamStreamRejection{info: info, body: string(data)}
}

func reportUpstreamStreamRejection(r *http.Request, requestID uint64, model string, prof *upstreamProfile, err error) *upstreamStreamRejection {
	var rejection *upstreamStreamRejection
	if !errors.As(err, &rejection) {
		return nil
	}
	logUpstreamRejection(r, requestID, model, prof, http.StatusOK, rejection.info)
	recordModelFailure(model, rejection.info.Code)
	return rejection
}

func writeUpstreamBusinessRejection(w http.ResponseWriter, r *http.Request, status int, info upstreamRejection, message string) {
	errType := "upstream_error"
	anthropic := r.Context().Value(anthropicErrFmtKey{}) == true || r.URL.Path == "/v1/messages"
	if anthropic {
		errType = "api_error"
	}
	payload := map[string]any{"error": map[string]any{
		"type": errType, "code": info.Code, "message": message,
	}}
	if anthropic {
		payload["type"] = "error"
	}
	log.Printf("[请求失败] traceId=%s 状态码=%d 错误类型=%s 业务码=%s 说明=上游详情仅返回调用方",
		w.Header().Get("X-Trace-ID"), status, errType, info.Code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAggregateBusinessRejection(w http.ResponseWriter, r *http.Request, requestID uint64, model string, prof *upstreamProfile, err error) bool {
	if rejection := reportUpstreamStreamRejection(r, requestID, model, prof, err); rejection != nil {
		writeUpstreamBusinessRejection(w, r, http.StatusBadGateway, rejection.info, rejection.clientMessage())
		return true
	}
	return false
}

// Only shape flags and fixed rule names are retained. No prompt, tool arguments,
// credentials, or raw client session identifiers belong in diagnostics.
type requestCompatibility struct {
	protocol     string
	stream       bool
	hasSystem    bool
	hasDeveloper bool
	rules        []string
}

func inspectRequestCompatibility(r *http.Request, body map[string]any) requestCompatibility {
	d := requestCompatibility{protocol: "internal"}
	if r != nil && r.URL != nil {
		switch r.URL.Path {
		case "/v1/responses", "/responses":
			d.protocol = "responses"
		case "/v1/messages":
			d.protocol = "messages"
		case "/v1/chat/completions", "/chat/completions":
			d.protocol = "chat_completions"
		}
	}
	d.stream, _ = body["stream"].(bool)
	if sys, ok := body["system"].(string); ok && sys != "" {
		d.hasSystem = true
	} else if sys, ok := body["system"].([]any); ok && len(sys) > 0 {
		d.hasSystem = true
	}
	if instructions, ok := body["instructions"].(string); ok && instructions != "" {
		d.hasSystem = true
	}
	for _, key := range []string{"messages", "input"} {
		list, _ := body[key].([]any)
		for _, message := range list {
			switch roleOfMessage(message) {
			case "system":
				d.hasSystem = true
			case "developer":
				d.hasDeveloper = true
			}
		}
	}
	return d
}

func recordCompatibilityRule(r *http.Request, rule string) {
	if h := conversationHeadersFor(r); h != nil {
		for _, existing := range h.diagnostic.rules {
			if existing == rule {
				return
			}
		}
		h.diagnostic.rules = append(h.diagnostic.rules, rule)
	}
}

type upstreamRejection struct {
	Code      string
	Category  string
	RequestID string
}

// Code 11128 is overloaded. Prefer a concrete structural reason over a generic
// displayTips about channel verification. A category is evidence, not a claim
// that extra identity headers will satisfy the upstream authorization policy.
func classifyUpstreamRejection(body []byte) upstreamRejection {
	info := upstreamRejection{Category: "upstream_error"}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		return info
	}
	object := root
	if nested, ok := root["error"].(map[string]any); ok {
		if rejectionCode(nested["code"]) == "11128" || root["code"] == nil {
			object = nested
		}
	}
	info.Code = rejectionCode(object["code"])
	for _, obj := range []map[string]any{object, root} {
		for _, key := range []string{"requestId", "request_id"} {
			if value, ok := obj[key].(string); ok && info.RequestID == "" {
				if id, err := uuid.Parse(value); err == nil && id != uuid.Nil {
					info.RequestID = id.String()
				} else if value != "" {
					info.RequestID = "present_redacted"
				}
			}
		}
	}
	if info.Code != "11128" {
		return info
	}
	msg := strings.ToLower(stringField(object, "msg") + " " + stringField(object, "message"))
	tips := strings.ToLower(stringField(object, "displayTips") + " " + stringField(root, "displayTips"))
	compact := strings.NewReplacer("_", " ", "-", " ").Replace(msg)
	switch {
	case strings.Contains(compact, "first message") && strings.Contains(compact, "system"):
		info.Category = "message_structure"
	case strings.Contains(compact, "developer") && (strings.Contains(compact, "role") || strings.Contains(compact, "support")):
		info.Category = "unsupported_role"
	case (strings.Contains(compact, "conversation") || strings.Contains(compact, "session")) &&
		(strings.Contains(compact, "missing") || strings.Contains(compact, "required") || strings.Contains(compact, "empty")):
		info.Category = "conversation_metadata_missing"
	case strings.Contains(msg, "prompt") && (strings.Contains(msg, "blocked") || strings.Contains(msg, "policy")):
		info.Category = "prompt_policy_rejected"
	case strings.Contains(msg, "unapproved channel") || strings.Contains(msg+tips, "channel verification") || strings.Contains(msg+tips, "渠道"):
		info.Category = "channel_verification_failed"
	default:
		info.Category = "invocation_rejected_unknown"
	}
	return info
}

func rejectionCode(raw any) string {
	switch value := raw.(type) {
	case float64:
		if value >= 0 && value <= 999999999 && value == float64(int64(value)) {
			return strconv.FormatInt(int64(value), 10)
		}
	case string:
		if number, err := strconv.ParseUint(value, 10, 32); err == nil {
			return strconv.FormatUint(number, 10)
		}
	}
	return ""
}

func diagnosticAtom(value string) string {
	if value == "" || len(value) > 160 {
		return "redacted"
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("._:/-", r)) {
			return "redacted"
		}
	}
	return value
}

func logUpstreamRejection(r *http.Request, requestID uint64, model string, prof *upstreamProfile, status int, info upstreamRejection) {
	h := conversationHeadersFor(r)
	d := requestCompatibility{protocol: "internal"}
	sessionSupplied := false
	if h != nil {
		d = h.diagnostic
		sessionSupplied = h.Source != "独立生成"
	}
	log.Printf("[上游拒绝诊断] requestId=%d protocol=%s model=%s site=%s http_status=%d business_code=%s category=%s upstream_request_id=%s stream=%t has_system=%t has_developer=%t session_supplied=%t session_ready=%t rules=%s",
		requestID, d.protocol, diagnosticAtom(model), prof.Key, status, info.Code, info.Category,
		info.RequestID, d.stream, d.hasSystem, d.hasDeveloper, sessionSupplied, h != nil, strings.Join(d.rules, ","))
}
