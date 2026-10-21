package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const (
	dshGUIContextSentence = "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	dshGUIContextNeutral  = "You are interacting with the user through a Web GUI whose URL the runtime context names."
	// Exact WEB_SURFACE_PROMPT from @deepseek-ai/dsh-harness-dev-context
	// 0.2.1-rc.2. Unknown or customized templates are deliberately unchanged.
	dshGUIContextBytes  = 1006
	dshGUIContextSHA256 = "7496863f7f81a12cad97ff5c643339f8ddf344b6525c191c29ab086bec5a400a"
)

func hasDSHSystemIdentity(text string) bool {
	for _, prefix := range []string{blockedSystemPromptPrefix, replacementSystemPromptPrefix} {
		if len(text) >= len(prefix) && strings.EqualFold(text[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

// normalizeDSHGUIContextText changes only the environment sentence of the exact
// shipped paragraph, outside fenced code. All GUI/build instructions and every
// other byte are retained. This is not a general product-name replacement.
func normalizeDSHGUIContextText(text string) (string, int) {
	if !strings.Contains(text, dshGUIContextSentence) {
		return text, 0
	}
	var out strings.Builder
	copied, changed := 0, 0
	var fence byte
	fenceLength := 0
	previousBlank := true
	for start := 0; start < len(text); {
		end := start + strings.IndexByte(text[start:], '\n')
		if end < start {
			end = len(text)
		}
		line := strings.TrimSuffix(text[start:end], "\r")
		next := end
		if next < len(text) {
			next++
		}
		followingBlank := next == len(text) || text[next] == '\n' || strings.HasPrefix(text[next:], "\r\n")
		trimmed := strings.TrimLeft(line, " ")
		isFence := false
		if len(line)-len(trimmed) <= 3 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			n := 1
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if n >= 3 {
				if fence == 0 {
					fence, fenceLength, isFence = trimmed[0], n, true
				} else if fence == trimmed[0] && n >= fenceLength && strings.TrimSpace(trimmed[n:]) == "" {
					fence, fenceLength, isFence = 0, 0, true
				}
			}
		}
		if !isFence && fence == 0 && previousBlank && followingBlank && len(line) == dshGUIContextBytes && strings.HasPrefix(line, dshGUIContextSentence) {
			digest := sha256.Sum256([]byte(line))
			if hex.EncodeToString(digest[:]) == dshGUIContextSHA256 {
				out.WriteString(text[copied:start])
				out.WriteString(dshGUIContextNeutral)
				out.WriteString(text[start+len(dshGUIContextSentence) : end])
				copied, changed = end, changed+1
			}
		}
		previousBlank = strings.TrimSpace(line) == ""
		start = next
	}
	if changed == 0 {
		return text, 0
	}
	out.WriteString(text[copied:])
	return out.String(), changed
}

func normalizeDSHGUIContexts(obj map[string]any) int {
	messages, _ := obj["messages"].([]any)
	changed := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || roleOfMessage(message) != "system" {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			if hasDSHSystemIdentity(content) {
				normalized, n := normalizeDSHGUIContextText(content)
				if n > 0 {
					message["content"] = normalized
					changed += n
				}
			}
		case []any:
			isDSH := false
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]any)
				if block["type"] != "text" {
					continue
				}
				if text, ok := block["text"].(string); ok {
					isDSH = hasDSHSystemIdentity(text)
					break
				}
			}
			if !isDSH {
				continue
			}
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]any)
				if block["type"] != "text" {
					continue
				}
				if text, ok := block["text"].(string); ok {
					normalized, n := normalizeDSHGUIContextText(text)
					if n > 0 {
						block["text"] = normalized
						changed += n
					}
				}
			}
		}
	}
	return changed
}
