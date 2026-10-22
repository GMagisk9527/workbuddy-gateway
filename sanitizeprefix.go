package main

import (
	"log"
	"net/http"
	"strings"
)

const (
	blockedSystemPromptPrefix     = "You are an AI agent powered by DeepSeek Harness"
	replacementSystemPromptPrefix = "You are an AI agent powered by a large language model"
	// Keep the upstream name for its shared compatibility tests and callers.
	blockedSystemPromptPrefixReplacement = replacementSystemPromptPrefix
)

// The upstream compatibility rule is limited to this exact identity prefix at
// byte zero. It preserves all following developer instructions and user content.
func sanitizeSystemPromptPrefix(text string) (string, bool) {
	if len(text) < len(blockedSystemPromptPrefix) || !strings.EqualFold(text[:len(blockedSystemPromptPrefix)], blockedSystemPromptPrefix) {
		return text, false
	}
	return replacementSystemPromptPrefix + text[len(blockedSystemPromptPrefix):], true
}

func sanitizeSystemPromptPrefixes(obj map[string]any) int {
	return sanitizeMessageIdentityPrefixes(obj, "system")
}

// History replay is scanned as well: process every standard text part of the
// selected role, without touching user/tool data, reasoning, or tool arguments.
func sanitizeMessageIdentityPrefixes(obj map[string]any, role string) int {
	return sanitizeMessageIdentityPrefixesForProtocol(obj, role, false)
}

func normalizeDSHIdentityPrefixes(obj map[string]any, role string) int {
	return sanitizeMessageIdentityPrefixes(obj, role)
}

func sanitizeMessageIdentityPrefixesForProtocol(obj map[string]any, role string, anthropic bool) int {
	messages, ok := obj["messages"].([]any)
	if !ok {
		return 0
	}
	modified := 0
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok || roleOfMessage(msg) != role {
			continue
		}
		text, parts := dshMessageText(msg, anthropic)
		var code []dshTextRange
		parsedCode := false
		for _, part := range parts {
			if sanitized, changed := sanitizeSystemPromptPrefix(part.text); changed {
				if !parsedCode {
					code, parsedCode = dshMarkdownCodeRanges(text), true
				}
				if !dshPositionInCode(code, part.start) {
					part.container[part.key] = sanitized
					modified++
				}
			}
		}
	}
	return modified
}

func logSystemPromptPrefixCompatibility(r *http.Request, obj map[string]any) {
	var requestID uint64
	if r != nil {
		requestID = requestIDFor(r)
	}
	logDSHPromptCompatibility(r, requestID, obj)
}

// The shared outbound chain retains the upstream count-only diagnostic event,
// using the fork's message-level Markdown context before any text is merged.
func logDSHPromptCompatibility(r *http.Request, requestID uint64, obj map[string]any) {
	anthropic := r != nil && r.URL != nil && r.URL.Path == "/v1/messages"
	system := sanitizeMessageIdentityPrefixesForProtocol(obj, "system", anthropic)
	assistant := sanitizeMessageIdentityPrefixesForProtocol(obj, "assistant", anthropic)
	gui := normalizeDSHGUIContextsForProtocol(obj, anthropic)
	if system > 0 {
		recordCompatibilityRule(r, "dsh_system_identity_prefix")
	}
	if assistant > 0 {
		recordCompatibilityRule(r, "dsh_assistant_identity_prefix")
	}
	if gui > 0 {
		recordCompatibilityRule(r, "dsh_gui_context_identity")
	}
	if system+assistant+gui == 0 {
		return
	}
	trace := debugTraceID(r)
	if trace == "" && r != nil {
		trace = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[DSH提示词兼容] traceId=%s requestId=%d system前缀=%d assistant前缀=%d GUI环境句=%d 结果=已改写为中性描述，未记录提示词正文",
		trace, requestID, system, assistant, gui)
	debugEvent(r, "info", "dsh_prompt_compatibility", map[string]any{
		"system_prefix_parts": system, "assistant_prefix_parts": assistant,
		"gui_context_matches": gui, "prompt_body_logged": false,
	})
}
