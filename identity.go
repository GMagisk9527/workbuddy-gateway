package main

import (
	"sort"
	"strings"
	"sync"
)

// runtimeIdentityConfig controls the display names used when normalizing
// system/developer/assistant prose. An absent config keeps the fork's neutral
// prefix compatibility without replacing names throughout instructions.
type runtimeIdentityConfig struct {
	Enabled      *bool             `json:"enabled,omitempty"`
	Target       string            `json:"target,omitempty"`
	Replacements map[string]string `json:"replacements,omitempty"`
}

type identityReplacement struct {
	Old string
	New string
}

type identitySettings struct {
	enabled      bool
	prefix       string
	replacements []identityReplacement
}

var (
	defaultIdentityTerms = []string{"Claude Code", "Claude", "DeepSeek Harness", "Codex", "ZCode"}
	identityConfigMu     sync.RWMutex
	currentIdentity      = defaultIdentitySettings()
)

func defaultIdentitySettings() identitySettings {
	return identitySettings{enabled: true, prefix: replacementSystemPromptPrefix}
}

func defaultIdentityConfig() runtimeIdentityConfig {
	return runtimeIdentityConfig{}
}

func setIdentityConfig(c runtimeIdentityConfig) {
	if c.Enabled == nil && c.Target == "" && len(c.Replacements) == 0 {
		identityConfigMu.Lock()
		currentIdentity = defaultIdentitySettings()
		identityConfigMu.Unlock()
		return
	}
	enabled := true
	if c.Enabled != nil {
		enabled = *c.Enabled
	}
	target := strings.TrimSpace(c.Target)
	if target == "" {
		target = "WorkBuddy"
	}
	prefixTarget := target
	replacements := make([]identityReplacement, 0, len(c.Replacements))
	if len(c.Replacements) > 0 {
		for old, newValue := range c.Replacements {
			if strings.TrimSpace(old) == "" {
				continue
			}
			replacements = append(replacements, identityReplacement{Old: old, New: newValue})
		}
		if replacement, ok := c.Replacements["DeepSeek Harness"]; ok && strings.TrimSpace(replacement) != "" {
			prefixTarget = replacement
		}
	} else {
		for _, term := range defaultIdentityTerms {
			replacements = append(replacements, identityReplacement{Old: term, New: target})
		}
	}
	// Longest first avoids splitting "Claude Code" into "Assistant Code".
	sort.SliceStable(replacements, func(i, j int) bool {
		if len(replacements[i].Old) != len(replacements[j].Old) {
			return len(replacements[i].Old) > len(replacements[j].Old)
		}
		return replacements[i].Old < replacements[j].Old
	})
	identityConfigMu.Lock()
	currentIdentity = identitySettings{
		enabled: enabled, prefix: "You are " + prefixTarget, replacements: replacements,
	}
	identityConfigMu.Unlock()
}

func identitySnapshot() identitySettings {
	identityConfigMu.RLock()
	snapshot := currentIdentity
	snapshot.replacements = append([]identityReplacement(nil), currentIdentity.replacements...)
	identityConfigMu.RUnlock()
	return snapshot
}

func identitySettingsSummary() (bool, string, int) {
	settings := identitySnapshot()
	target := strings.TrimPrefix(settings.prefix, "You are ")
	return settings.enabled, target, len(settings.replacements)
}

// normalizeAssistantIdentityText applies the configurable prefix and
// display-name replacements. It is called only for system/developer/assistant
// text; user/tool content is intentionally outside this function.
func normalizeAssistantIdentityText(text string) (string, bool) {
	return normalizeIdentityTextWithRanges(text, dshMarkdownCodeRanges(text), 0)
}

// Offsets belong to the original, complete message, including other parts.
func normalizeIdentityTextWithRanges(text string, code []dshTextRange, offset int) (string, bool) {
	settings := identitySnapshot()
	if !settings.enabled {
		return text, false
	}
	var pairs []string
	for _, replacement := range settings.replacements {
		pairs = append(pairs, replacement.Old, replacement.New)
	}
	replacer := strings.NewReplacer(pairs...)
	var out strings.Builder
	pos := 0
	if len(text) >= len(blockedSystemPromptPrefix) && strings.EqualFold(text[:len(blockedSystemPromptPrefix)], blockedSystemPromptPrefix) {
		end := len(blockedSystemPromptPrefix)
		inCode := false
		for _, span := range code {
			if span.start < offset+end && span.end > offset {
				inCode = true
				break
			}
		}
		if !inCode {
			out.WriteString(settings.prefix)
			pos = end
		}
	}
	for _, span := range code {
		start, end := max(pos, span.start-offset), min(len(text), span.end-offset)
		if end <= start {
			continue
		}
		out.WriteString(replacer.Replace(text[pos:start]))
		out.WriteString(text[start:end])
		pos = end
	}
	out.WriteString(replacer.Replace(text[pos:]))
	result := out.String()
	return result, result != text
}
