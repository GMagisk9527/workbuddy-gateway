package main

import (
	"strings"

	"net/http"
)

const (
	blockedSystemPromptPrefix     = "You are an AI agent powered by DeepSeek Harness"
	replacementSystemPromptPrefix = "You are an AI agent powered by a large language model"
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
	anthropic := r != nil && r.URL != nil && r.URL.Path == "/v1/messages"
	if sanitizeMessageIdentityPrefixesForProtocol(obj, "system", anthropic) > 0 {
		recordCompatibilityRule(r, "dsh_system_identity_prefix")
	}
	if sanitizeMessageIdentityPrefixesForProtocol(obj, "assistant", anthropic) > 0 {
		recordCompatibilityRule(r, "dsh_assistant_identity_prefix")
	}
	if normalizeDSHGUIContextsForProtocol(obj, anthropic) > 0 {
		recordCompatibilityRule(r, "dsh_gui_context_identity")
	}
}
