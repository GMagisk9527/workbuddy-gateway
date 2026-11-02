package main

import (
	"log"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
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
	Old      string
	New      string
	Wildcard bool
	Prefix   string
	Suffix   string
}

type identitySettings struct {
	enabled          bool
	prefix           string
	replacements     []identityReplacement
	replacer         *strings.Replacer
	invalidRules     int
	suppressWildcard bool
	custom           bool
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
	invalid := 0
	replacements := make([]identityReplacement, 0, len(c.Replacements))
	if len(c.Replacements) > 0 {
		for old, newValue := range c.Replacements {
			rule, ok := compileIdentityReplacement(old, newValue)
			if !ok {
				invalid++
				continue
			}
			replacements = append(replacements, rule)
		}
		if replacement, ok := c.Replacements["DeepSeek Harness"]; ok && strings.TrimSpace(replacement) != "" {
			prefixTarget = replacement
		}
	} else {
		for old, newValue := range defaultIdentityReplacements(target) {
			replacements = append(replacements, identityReplacement{Old: old, New: newValue})
		}
	}
	// Prefer the most fixed text, with exact literals winning ties.
	sort.SliceStable(replacements, func(i, j int) bool {
		a, b := replacements[i], replacements[j]
		an, bn := len(a.Old), len(b.Old)
		if a.Wildcard {
			an -= len(identityWildcardToken)
		}
		if b.Wildcard {
			bn -= len(identityWildcardToken)
		}
		if an != bn {
			return an > bn
		}
		if a.Wildcard != b.Wildcard {
			return !a.Wildcard
		}
		return a.Old < b.Old
	})
	var replacer *strings.Replacer
	if enabled && len(replacements) > 0 {
		pairs := make([]string, 0, len(replacements)*2)
		for _, replacement := range replacements {
			if !replacement.Wildcard {
				pairs = append(pairs, replacement.Old, replacement.New)
			}
		}
		replacer = strings.NewReplacer(pairs...)
		replacer.Replace("") // Build the immutable matcher before publishing it.
	}
	identityConfigMu.Lock()
	currentIdentity = identitySettings{
		enabled: enabled, prefix: "You are " + prefixTarget, replacements: replacements, replacer: replacer, invalidRules: invalid, custom: len(c.Replacements) > 0,
	}
	identityConfigMu.Unlock()
	if invalid > 0 {
		log.Printf("[替换规则告警] invalid_rules=%d prompt_body_logged=false", invalid)
	}
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
	if len(settings.replacements) == 0 && !settings.custom {
		if prefix := claudeCodeIdentityPrefix(text); prefix != "" {
			return prefix, neutralClaudeCodeIdentity
		}
	}
	if (!settings.custom || hasDSHNameRule(settings)) && len(text) >= len(blockedSystemPromptPrefix) && strings.EqualFold(text[:len(blockedSystemPromptPrefix)], blockedSystemPromptPrefix) {
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
		if replacement.Wildcard {
			if allowPrefix && !settings.suppressWildcard && strings.HasPrefix(text, replacement.Prefix) {
				return true
			}
			continue
		}
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
	if allowPrefix && !settings.suppressWildcard {
		if rewritten, consumed, matched := settings.replaceWildcard(text, code, offset); matched {
			// Apply literals only to the remaining original text, never the new prefix.
			out.WriteString(rewritten[:len(rewritten)-(len(text)-consumed)])
			pos = consumed
			oldPrefix = ""
		}
	}
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

const (
	identityWildcardToken    = "(.*)"
	identityWildcardMaxRunes = 256
)

func hasDSHNameRule(settings identitySettings) bool {
	for _, rule := range settings.replacements {
		if rule.Old == "DeepSeek Harness" {
			return true
		}
	}
	return false
}
func compileIdentityReplacement(old, newValue string) (identityReplacement, bool) {
	rule := identityReplacement{Old: old, New: newValue}
	if strings.TrimSpace(old) == "" {
		return rule, false
	}
	switch strings.Count(old, identityWildcardToken) {
	case 0:
		return rule, true
	case 1:
		rule.Prefix, rule.Suffix, _ = strings.Cut(old, identityWildcardToken)
		if strings.TrimSpace(rule.Prefix) == "" || strings.TrimSpace(rule.Suffix) == "" ||
			strings.ContainsAny(old, "\r\n") {
			return rule, false
		}
		rule.Wildcard = true
		return rule, true
	default:
		return rule, false
	}
}

func (rule identityReplacement) replace(text string) (string, int) {
	if !rule.Wildcard {
		count := strings.Count(text, rule.Old)
		if count == 0 {
			return text, 0
		}
		return strings.ReplaceAll(text, rule.Old, rule.New), count
	}
	if !strings.HasPrefix(text, rule.Prefix) {
		return text, 0
	}
	remainder := text[len(rule.Prefix):]
	limit := 4*identityWildcardMaxRunes + len(rule.Suffix)
	if len(remainder) > limit {
		remainder = remainder[:limit]
	}
	if newline := strings.IndexAny(remainder, "\r\n"); newline >= 0 {
		remainder = remainder[:newline]
	}
	end := strings.Index(remainder, rule.Suffix)
	if end < 0 || utf8.RuneCountInString(remainder[:end]) > identityWildcardMaxRunes {
		return text, 0
	}
	if rule.New == rule.Old {
		// Mapping an automatically completed default to itself disables that
		// rule without reinserting the literal wildcard token into the prompt.
		return text, 1
	}
	return rule.New + text[len(rule.Prefix)+end+len(rule.Suffix):], 1
}

// Match once against the original block and reject any overlap with code.
func (settings identitySettings) replaceWildcard(text string, code []dshTextRange, offset int) (string, int, bool) {
	for _, rule := range settings.replacements {
		if !rule.Wildcard {
			if strings.HasPrefix(text, rule.Old) {
				break
			}
			continue
		}
		rewritten, n := rule.replace(text)
		if n == 0 {
			continue
		}
		if rule.New == rule.Old {
			return text, 0, true
		}
		consumed := len(text) - len(rewritten) + len(rule.New)
		for _, span := range code {
			if span.start < offset+consumed && span.end > offset {
				return text, 0, false
			}
		}
		return rewritten, consumed, true
	}
	return text, 0, false
}

// Keep configured force preparation cheap when no wildcard can start here.
func (settings identitySettings) wildcardMayChange(text string) bool {
	if !settings.enabled || settings.suppressWildcard {
		return false
	}
	for _, rule := range settings.replacements {
		if rule.Wildcard && strings.HasPrefix(text, rule.Prefix) {
			return true
		}
	}
	return false
}
