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
	replacer     *strings.Replacer
}

var (
	defaultIdentityTerms = []string{"Claude Code", "Claude", "DeepSeek Harness", "Codex", "ZCode"}
	identityConfigMu     sync.RWMutex
	currentIdentity      = defaultIdentitySettings()
)

const (
	defaultWorkspaceRootPhrase = "workspace root"
	defaultWorkspaceBasePhrase = "workspace base"
	defaultUserPathsPhrase     = "are paths the user"
	defaultUserPathsNeutral    = "are paths that the user"
)

func defaultIdentityReplacements(target string) map[string]string {
	replacements := make(map[string]string, len(defaultIdentityTerms)+2)
	for _, term := range defaultIdentityTerms {
		replacements[term] = target
	}
	replacements[defaultWorkspaceRootPhrase] = defaultWorkspaceBasePhrase
	replacements[defaultUserPathsPhrase] = defaultUserPathsNeutral
	return replacements
}

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
		for old, newValue := range defaultIdentityReplacements(target) {
			replacements = append(replacements, identityReplacement{Old: old, New: newValue})
		}
	}
	// Longest first avoids splitting "Claude Code" into "Assistant Code".
	sort.SliceStable(replacements, func(i, j int) bool {
		if len(replacements[i].Old) != len(replacements[j].Old) {
			return len(replacements[i].Old) > len(replacements[j].Old)
		}
		return replacements[i].Old < replacements[j].Old
	})
	var replacer *strings.Replacer
	if enabled && len(replacements) > 0 {
		pairs := make([]string, 0, len(replacements)*2)
		for _, replacement := range replacements {
			pairs = append(pairs, replacement.Old, replacement.New)
		}
		replacer = strings.NewReplacer(pairs...)
		replacer.Replace("") // Build the immutable matcher before publishing it.
	}
	identityConfigMu.Lock()
	currentIdentity = identitySettings{
		enabled: enabled, prefix: "You are " + prefixTarget, replacements: replacements, replacer: replacer,
	}
	identityConfigMu.Unlock()
}

func identitySnapshot() identitySettings {
	identityConfigMu.RLock()
	snapshot := currentIdentity
	identityConfigMu.RUnlock()
	// Settings and their replacement slice are immutable after publication.
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
	settings := identitySnapshot()
	if !identityTextMayChange(text, settings, true) {
		return text, false
	}
	return normalizeIdentityText(text, dshMarkdownCodeRanges(text), 0, true, settings)
}

// Offsets belong to the original, complete message, including other parts.
func normalizeIdentityTextWithRanges(text string, code []dshTextRange, offset int) (string, bool) {
	return normalizeIdentityTextWithPrefix(text, code, offset, true)
}

func identityPrefixReplacement(text string, settings identitySettings) (string, string) {
	if len(settings.replacements) == 0 {
		if prefix := claudeCodeIdentityPrefix(text); prefix != "" {
			return prefix, neutralClaudeCodeIdentity
		}
	}
	if len(text) >= len(blockedSystemPromptPrefix) && strings.EqualFold(text[:len(blockedSystemPromptPrefix)], blockedSystemPromptPrefix) {
		return text[:len(blockedSystemPromptPrefix)], settings.prefix
	}
	return "", ""
}

func normalizeIdentityTextWithPrefix(text string, code []dshTextRange, offset int, allowPrefix bool) (string, bool) {
	return normalizeIdentityText(text, code, offset, allowPrefix, identitySnapshot())
}

// This is only a candidate check. Matching text must still be parsed in its
// complete Markdown context, including unchanged text parts before it.
func identityTextMayChange(text string, settings identitySettings, allowPrefix bool) bool {
	if !settings.enabled {
		return false
	}
	// Bound the extra scans for large custom rule sets. The compiled matcher
	// below handles those in one pass instead of scanning the text per rule.
	if len(settings.replacements) > 8 {
		return true
	}
	if allowPrefix {
		if old, _ := identityPrefixReplacement(text, settings); old != "" {
			return true
		}
	}
	for _, replacement := range settings.replacements {
		if replacement.Old != replacement.New && strings.Contains(text, replacement.Old) {
			return true
		}
	}
	return false
}

func (s identitySettings) replace(text string) string {
	if s.replacer == nil {
		return text
	}
	return s.replacer.Replace(text)
}

func normalizeIdentityText(text string, code []dshTextRange, offset int, allowPrefix bool, settings identitySettings) (string, bool) {
	if !identityTextMayChange(text, settings, allowPrefix) {
		return text, false
	}
	var out strings.Builder
	pos := 0
	oldPrefix, newPrefix := identityPrefixReplacement(text, settings)
	if allowPrefix && oldPrefix != "" {
		end := len(oldPrefix)
		inCode := false
		for _, span := range code {
			if span.start < offset+end && span.end > offset {
				inCode = true
				break
			}
		}
		if !inCode {
			out.WriteString(newPrefix)
			pos = end
		}
	}
	for _, span := range code {
		start, end := max(pos, span.start-offset), min(len(text), span.end-offset)
		if end <= start {
			continue
		}
		out.WriteString(settings.replace(text[pos:start]))
		out.WriteString(text[start:end])
		pos = end
	}
	out.WriteString(settings.replace(text[pos:]))
	result := out.String()
	return result, result != text
}
