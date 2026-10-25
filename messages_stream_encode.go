package main

import (
	"encoding/json"
	"slices"
)

// Append directly to the caller's output, avoiding a temporary frame string
// and another copy when several events belong to one upstream chunk.
func appendAnthropicFrame(out []byte, eventType string, payload []byte) []byte {
	out = slices.Grow(out, len(eventType)+len(payload)+16)
	out = append(out, "event: "...)
	out = append(out, eventType...)
	out = append(out, "\ndata: "...)
	out = append(out, payload...)
	return append(out, '\n', '\n')
}

// Field order matches the generic map encoder, including its JSON escaping.
type anthropicTextDeltaEvent struct {
	Delta struct {
		Text string `json:"text"`
		Type string `json:"type"`
	} `json:"delta"`
	Index int    `json:"index"`
	Type  string `json:"type"`
}

func (s *anthropicStreamState) appendTextDelta(out []byte, index int, text string) []byte {
	if s.pendingEventErr != nil {
		return out
	}
	if s.pendingToolIndex >= 0 && index > s.pendingToolIndex {
		// Preserve the pre-encoding size check, shared pending budget and owned
		// frame storage while an earlier tool's identity is incomplete.
		return append(out, s.emit("content_block_delta", map[string]any{
			"index": index,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})...)
	}
	event := anthropicTextDeltaEvent{Index: index, Type: "content_block_delta"}
	event.Delta.Text, event.Delta.Type = text, "text_delta"
	payload, err := json.Marshal(event)
	if err != nil {
		return out
	}
	return appendAnthropicFrame(out, "content_block_delta", payload)
}
