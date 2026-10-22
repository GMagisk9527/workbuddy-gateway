package main

import "strings"

const neutralClaudeCodeIdentity = "You are an AI coding assistant."

// Complete client self-descriptions, not arbitrary brand mentions. Keep the
// following instructions unchanged without claiming official client status.
var claudeCodeIdentityPrefixes = []string{
	"You are Claude Code, Anthropic's official CLI tool for Claude.",
	"You are Claude Code, Anthropic's official CLI for Claude.",
	"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
	"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
	"You are Claude Code, an AI assistant that orchestrates software engineering tasks across multiple workers.",
	"You are Claude Code.",
}

func claudeCodeIdentityPrefix(text string) string {
	for _, prefix := range claudeCodeIdentityPrefixes {
		if !strings.HasPrefix(text, prefix) {
			continue
		}
		if len(text) == len(prefix) || strings.ContainsRune(" \t\r\n", rune(text[len(prefix)])) {
			return prefix
		}
	}
	return ""
}
