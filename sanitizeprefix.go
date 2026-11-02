package main

import (
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
	settings := identitySnapshot()
	if !settings.enabled {
		return 0
	}
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
		parts := dshMessageTextParts(msg, anthropic)
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
	applyOriginalIdentityReplacements(obj, r, requestID)
}
