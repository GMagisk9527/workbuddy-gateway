package main

import (
	"log"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"
)

// promptSettings 是配置文件内的可选提示词。零值完全保留旧行为。
// 日志仅记录来源和长度，绝不记录配置或客户端的提示词正文。
type promptSettings struct {
	fallback string
	force    string
}

var (
	promptSettingsMu sync.RWMutex
	currentPrompts   promptSettings
)

func setSystemPromptConfig(fallback, force string) {
	promptSettingsMu.Lock()
	currentPrompts = promptSettings{fallback: fallback, force: force}
	promptSettingsMu.Unlock()
}

func configuredSystemPrompts() promptSettings {
	promptSettingsMu.RLock()
	settings := currentPrompts
	promptSettingsMu.RUnlock()
	return settings
}

func configuredFallbackSystemPrompt() string {
	fallback := configuredSystemPrompts().fallback
	if strings.TrimSpace(fallback) == "" {
		return defaultSystemPrompt
	}
	return fallback
}

// Fast path for raw force and the legacy DSH identity edits. Configurable name
// edits are checked in their complete message context below.
func systemPromptForceSuffix(content, force string) (int, bool) {
	tail, source := content, force
	neutralOpening := dshGUIContextNeutral + dshGUIContextOpening[len(dshGUIContextSentence):]
	for {
		pos := strings.LastIndex(source, dshGUIContextOpening)
		if pos < 0 {
			break
		}
		literal := source[pos+len(dshGUIContextOpening):]
		if !strings.HasSuffix(tail, literal) {
			return 0, false
		}
		tail = tail[:len(tail)-len(literal)]
		switch {
		case strings.HasSuffix(tail, dshGUIContextOpening):
			tail = tail[:len(tail)-len(dshGUIContextOpening)]
		case strings.HasSuffix(tail, neutralOpening):
			tail = tail[:len(tail)-len(neutralOpening)]
		default:
			return 0, false
		}
		source = source[:pos]
	}
	if strings.HasSuffix(tail, source) {
		return len(tail) - len(source), true
	}
	if identity, changed := sanitizeSystemPromptPrefix(source); changed && strings.HasSuffix(tail, identity) {
		return len(tail) - len(identity), true
	}
	return 0, false
}

// Restore raw force at the same position in the message's parsing view, then
// verify the exact wire text. Parsing force alone would mistake a closing fence
// from the client for an opening fence. Configuration itself is never changed.
func systemPromptForceWireMatches(existing, force, context string, start int, identityAllowed bool) bool {
	if existing == force {
		return true
	}
	code := dshMarkdownCodeRanges(context)
	wire, _ := normalizeDSHGUIContextPart(force, start, code)
	// The shared compatibility pass reparses Markdown after the GUI edit, then
	// applies all configured name rules. Use that same order and message scope.
	code = dshMarkdownCodeRanges(context[:start] + wire)
	wire, _ = normalizeIdentityTextWithPrefix(wire, code, start, identityAllowed)
	return existing == wire
}

func systemPromptHasForce(content, force string) bool {
	start, ok := systemPromptForceSuffix(content, force)
	if ok && (start == 0 || strings.HasSuffix(content[:start], "\n\n")) &&
		systemPromptForceWireMatches(content[start:], force, content[:start]+force, start, start == 0) {
		return true
	}
	settings := identitySnapshot()
	if !settings.enabled || len(settings.replacements) == 0 {
		return false
	}
	// Name edits may affect only part of force when it closes client code.
	// Restore raw force at valid paragraph boundaries and check its exact wire
	// representation in that context. Bound the search by its maximum expansion
	// so a short force does not scan every paragraph of a long client prompt.
	limit := len(force)
	ratio := 1
	for _, replacement := range settings.replacements {
		ratio = max(ratio, (len(replacement.New)+len(replacement.Old)-1)/len(replacement.Old))
	}
	if limit > len(content)/ratio {
		limit = len(content)
	} else {
		limit *= ratio
		extra := max(0, len(settings.prefix)-len(blockedSystemPromptPrefix))
		limit += min(extra, len(content)-limit)
	}
	first := len(content) - limit
	if first == 0 && systemPromptForceWireMatches(content, force, force, 0, true) {
		return true
	}
	for pos := max(0, first-2); pos < len(content); {
		next := strings.Index(content[pos:], "\n\n")
		if next < 0 {
			break
		}
		start := pos + next + 2
		if start >= first && systemPromptForceWireMatches(content[start:], force, content[:start]+force, start, false) {
			return true
		}
		pos = start - 1 // Consecutive newlines have overlapping boundaries.
	}
	return false
}

func systemPromptBlockHasForce(message map[string]any, force string) bool {
	text, parts := dshMessageText(message, false)
	blocks, _ := message["content"].([]any)
	if len(blocks) > 0 {
		last, _ := blocks[len(blocks)-1].(map[string]any)
		if last["type"] == "text" && last["text"] == "" {
			// A deletion rule can leave an empty force block. The parsing view
			// skips empty text, so restore its original separator as well.
			if len(parts) > 0 {
				text += "\n"
			}
			return systemPromptForceWireMatches("", force, text+force, len(text), true)
		}
	}
	if len(parts) == 0 {
		return false
	}
	last := parts[len(parts)-1]
	return systemPromptForceWireMatches(last.text, force, text[:last.start]+force, last.start, true)
}

// prepareSystemPromptForUpstream 在 Chat、Responses、Messages 三个入口统一执行。
// 先保证首条为 system，再把实验性的全局强制文本追加到该 system 内容末尾（后置），
// 使其成为整段提示词中位置最靠后的指令、紧贴其后的用户消息；
// 不删除、不覆盖客户端原有 system。content 数组保留原来的内容块顺序，
// 强制文本作为最后一个内容块追加在数组末尾。
func prepareSystemPromptForUpstream(obj map[string]any, r *http.Request, requestID uint64, traceID string) {
	before, _ := obj["messages"].([]any)
	leadingRole := ""
	if len(before) > 0 {
		leadingRole = roleOfMessage(before[0])
	}
	injected := ensureLeadingSystemMessage(obj)
	if injected {
		recordCompatibilityRule(r, "leading_system_injected")
	} else if leadingRole != "system" {
		recordCompatibilityRule(r, "leading_system_normalized")
	}
	settings := configuredSystemPrompts()
	force := settings.force
	forced := strings.TrimSpace(force) != ""
	kind := "未修改"
	messages, _ := obj["messages"].([]any)
	if forced && len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok {
			old := first["content"]
			switch content := old.(type) {
			case string:
				kind = "字符串"
				if content == "" {
					first["content"] = force
				} else if !systemPromptHasForce(content, force) {
					first["content"] = content + "\n\n" + force
				}
			case []any:
				kind = "内容块数组"
				if len(content) > 0 {
					if last, ok := content[len(content)-1].(map[string]any); ok && last["type"] == "text" {
						if _, ok := last["text"].(string); ok && systemPromptBlockHasForce(first, force) {
							break // 兼容规范化前后均不重复追加同一配置指令。
						}
					}
				}
				blocks := make([]any, 0, len(content)+1)
				blocks = append(blocks, content...)
				blocks = append(blocks, map[string]any{"type": "text", "text": force})
				first["content"] = blocks
			case nil:
				kind = "空内容"
				first["content"] = force
			default:
				// 非法 system 内容仍由上游参数校验；不静默丢弃客户端数据。
				forced = false
				kind = "未支持的内容类型，已跳过强制后置"
			}
		}
	}
	if forced {
		recordCompatibilityRule(r, "configured_system_appended")
	}
	fallbackSource := "未使用（客户端提供system）"
	if injected {
		fallbackSource = "内置保底"
		if strings.TrimSpace(settings.fallback) != "" {
			fallbackSource = "配置保底"
		}
	}
	if traceID == "" {
		traceID = debugTraceID(r)
	}
	if traceID == "" {
		traceID = r.Header.Get("X-Trace-ID")
	}
	log.Printf("[系统提示词规则] traceId=%s requestId=%d 阶段=上游请求序列化前 保底来源=%s 强制全局已应用=%t 强制位置=后置(紧贴用户消息) 强制内容类型=%s 强制字符数=%d 结果=保留客户端原有system且未记录提示词正文",
		traceID, requestID, fallbackSource, forced, kind, utf8.RuneCountInString(force))
	debugEvent(r, "debug", "system_prompt_policy_applied", map[string]any{
		"fallback_source": fallbackSource, "forced_applied": forced,
		"forced_position": "append_after_client_system", "forced_content_kind": kind,
		"forced_chars":    utf8.RuneCountInString(force),
		"business_impact": "按配置在请求出站前处理system；强制文本后置于system末尾且不记录提示词正文",
	})
}
