package main

import (
	"context"
	"log"
	"net/http"
)

type identityRequestContextKey struct{}

type identityRequestState struct {
	settings           identitySettings
	originalsProcessed bool
}

func identityStateForRequest(r *http.Request) *identityRequestState {
	if r == nil {
		return &identityRequestState{settings: identitySnapshot()}
	}
	if state, ok := r.Context().Value(identityRequestContextKey{}).(*identityRequestState); ok {
		return state
	}
	state := &identityRequestState{settings: identitySnapshot()}
	*r = *r.WithContext(context.WithValue(r.Context(), identityRequestContextKey{}, state))
	return state
}

// Wildcards see original client blocks only once, before protocol concatenation.
// This also prevents a later suffix in another block being consumed on a second pass.
func applyOriginalIdentityReplacements(obj map[string]any, r *http.Request, requestID uint64) {
	state := identityStateForRequest(r)
	applyIdentityRulePass(obj, r, requestID, !state.originalsProcessed)
	state.originalsProcessed = true
}

// Config-injected fallback/force is a new independent block, not client text.
func applyIdentityReplacements(obj map[string]any, r *http.Request, requestID uint64) {
	applyIdentityRulePass(obj, r, requestID, true)
}

// One immutable snapshot per incoming request, including subsequent preparation passes.
func applyIdentityRulePass(obj map[string]any, r *http.Request, requestID uint64, allowWildcard bool) {
	settings := identityStateForRequest(r).settings
	settings.suppressWildcard = !allowWildcard
	if !settings.enabled {
		return
	}
	anthropic := r != nil && r.URL != nil && r.URL.Path == "/v1/messages"
	// Retain the fork's precise neutral GUI compatibility for legacy/default configs.
	gui := 0
	if !settings.custom {
		gui = normalizeDSHGUIContextsForProtocol(obj, anthropic)
	}
	messages, _ := obj["messages"].([]any)
	system, assistant, wildcard := 0, 0, 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := roleOfMessage(message)
		if role != "system" && role != "developer" && role != "assistant" {
			continue
		}
		parts := dshMessageTextParts(message, anthropic)
		candidate := false
		for _, part := range parts {
			if identityTextMayChange(part.text, settings, true) {
				candidate = true
				break
			}
		}
		if !candidate {
			continue
		}
		code := dshMarkdownCodeRanges(joinDSHMessageText(parts))
		for _, part := range parts {
			if rewritten, changed := normalizeIdentityText(part.text, code, part.start, true, settings); changed {
				part.container[part.key] = rewritten
				if role == "assistant" {
					assistant++
				} else {
					system++
				}
				if allowWildcard {
					if _, _, matched := settings.replaceWildcard(part.text, code, part.start); matched {
						wildcard++
					}
				}
			}
		}
	}
	if system > 0 {
		recordCompatibilityRule(r, "dsh_system_identity_prefix")
	}
	if assistant > 0 {
		recordCompatibilityRule(r, "dsh_assistant_identity_prefix")
	}
	if gui > 0 {
		recordCompatibilityRule(r, "dsh_gui_context_identity")
	}
	if wildcard > 0 {
		recordCompatibilityRule(r, "identity_prefix_wildcard")
	}
	if system+assistant+gui > 0 {
		logIdentityReplacementPass(r, requestID, true, system+assistant+gui, system+assistant-wildcard, wildcard, "applied")
	}
}

func logIdentityReplacementPass(r *http.Request, requestID uint64, enabled bool, changed, literal, wildcard int, result string) {
	trace := debugTraceID(r)
	if trace == "" && r != nil {
		trace = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[配置文字替换] traceId=%s requestId=%d 启用=%t 修改文本块=%d 字面命中=%d 首位通配命中=%d 结果=%s 说明=applied为已替换，no_change为原样保留，identity_disabled为配置关闭；仅处理system/developer/assistant，未记录规则或提示词正文",
		trace, requestID, enabled, changed, literal, wildcard, result)
	event := "dsh_prompt_compatibility"
	if changed == 0 {
		event = "identity_replacements_skipped"
	}
	debugEvent(r, "info", event, map[string]any{
		"modified_text_blocks": changed, "literal_modified_blocks": literal,
		"wildcard_modified_blocks": wildcard, "prompt_body_logged": false, "result": result,
	})
}
