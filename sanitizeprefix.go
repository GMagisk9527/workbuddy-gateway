package main

import (
	"log"
	"net/http"
)

const (
	blockedSystemPromptPrefix            = "You are an AI agent powered by DeepSeek Harness"
	replacementSystemPromptPrefix        = "You are an AI agent powered by a large language model"
	blockedSystemPromptPrefixReplacement = replacementSystemPromptPrefix
)

var blockedSystemPromptPrefixes = []string{blockedSystemPromptPrefix}

func sanitizeSystemPromptPrefix(text string) (string, bool) {
	settings := identitySnapshot()
	if !settings.enabled {
		return text, false
	}
	oldPrefix, newPrefix := identityPrefixReplacement(text, settings)
	if oldPrefix == "" {
		return text, false
	}
	rewritten := newPrefix + text[len(oldPrefix):]
	return rewritten, rewritten != text
}

func sanitizeSystemPromptPrefixes(obj map[string]any) int {
	return sanitizeMessageIdentityPrefixes(obj, "system")
}

func sanitizeMessageIdentityPrefixes(obj map[string]any, role string) int {
	return sanitizeMessageIdentityPrefixesForProtocol(obj, role, false)
}

func normalizeDSHIdentityPrefixes(obj map[string]any, role string) int {
	return sanitizeMessageIdentityPrefixes(obj, role)
}

// Markdown scope follows the whole message, including spans across text parts.
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
		code := dshMarkdownCodeRanges(text)
		for _, part := range parts {
			if rewritten, changed := normalizeIdentityTextWithRanges(part.text, code, part.start); changed {
				part.container[part.key] = rewritten
				modified++
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

func logDSHPromptCompatibility(r *http.Request, requestID uint64, obj map[string]any) {
	anthropic := r != nil && r.URL != nil && r.URL.Path == "/v1/messages"
	// Complete GUI phrases must be processed before configurable name rules.
	gui := normalizeDSHGUIContextsForProtocol(obj, anthropic)
	system := sanitizeMessageIdentityPrefixesForProtocol(obj, "system", anthropic)
	assistant := sanitizeMessageIdentityPrefixesForProtocol(obj, "assistant", anthropic)
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
	log.Printf("[DSH提示词兼容] traceId=%s requestId=%d system前缀=%d assistant前缀=%d GUI环境句=%d 结果=已应用兼容规则，未记录提示词正文", trace, requestID, system, assistant, gui)
	debugEvent(r, "info", "dsh_prompt_compatibility", map[string]any{
		"system_prefix_parts": system, "assistant_prefix_parts": assistant,
		"gui_context_matches": gui, "prompt_body_logged": false,
	})
}
