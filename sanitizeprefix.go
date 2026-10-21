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
		switch content := msg["content"].(type) {
		case string:
			if sanitized, changed := sanitizeSystemPromptPrefix(content); changed {
				msg["content"] = sanitized
				modified++
			}
		case []any:
			for _, block := range content {
				part, ok := block.(map[string]any)
				if !ok {
					continue
				}
				if part["type"] != "text" {
					continue
				}
				text, ok := part["text"].(string)
				if !ok {
					continue
				}
				if sanitized, changed := sanitizeSystemPromptPrefix(text); changed {
					part["text"] = sanitized
					modified++
				}
			}
		}
	}
	return modified
}

func logSystemPromptPrefixCompatibility(r *http.Request, obj map[string]any) {
	if sanitizeSystemPromptPrefixes(obj) > 0 {
		recordCompatibilityRule(r, "dsh_system_identity_prefix")
	}
	if sanitizeMessageIdentityPrefixes(obj, "assistant") > 0 {
		recordCompatibilityRule(r, "dsh_assistant_identity_prefix")
	}
	if normalizeDSHGUIContexts(obj) > 0 {
		recordCompatibilityRule(r, "dsh_gui_context_identity")
	}
}
