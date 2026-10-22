package main

import (
	"regexp"
	"strings"
)

var claudeCodeAttributionVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:\.[A-Za-z0-9_-]+)?$`)
var claudeCodeAttributionToken = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// consumeClaudeCodeAttribution handles the standalone Anthropic attribution
// block before system text is flattened for the WorkBuddy protocol. It is
// transport metadata, not an instruction. Unrecognized shapes stay untouched.
func consumeClaudeCodeAttribution(body map[string]any) bool {
	blocks, ok := body["system"].([]any)
	if !ok || len(blocks) == 0 {
		return false
	}
	block, ok := blocks[0].(map[string]any)
	if !ok || len(block) != 2 || block["type"] != "text" {
		return false
	}
	text, ok := block["text"].(string)
	const prefix = "x-anthropic-billing-header:"
	if !ok || len(text) > 512 || !strings.HasPrefix(text, prefix) || !strings.HasSuffix(text, ";") {
		return false
	}
	for _, c := range text {
		if c < ' ' || c > '~' {
			return false
		}
	}
	fields := strings.Split(strings.TrimPrefix(text, prefix), ";")
	seen := make(map[string]bool, 3)
	for _, field := range fields[:len(fields)-1] {
		key, value, found := strings.Cut(strings.Trim(field, " "), "=")
		if !found || seen[key] {
			return false
		}
		seen[key] = true
		switch key {
		case "cc_version":
			if len(value) > 96 || !claudeCodeAttributionVersion.MatchString(value) {
				return false
			}
		case "cc_entrypoint":
			if value != "cli" && value != "sdk-cli" {
				return false
			}
		case "cch":
			if len(value) > 128 || !claudeCodeAttributionToken.MatchString(value) {
				return false
			}
		default:
			return false
		}
	}
	if !seen["cc_version"] || !seen["cc_entrypoint"] {
		return false
	}
	body["system"] = blocks[1:]
	return true
}
