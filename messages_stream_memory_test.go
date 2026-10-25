package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func messageTextChunk(text string) map[string]any {
	return map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}}}}
}

func TestMessagesStreamDoesNotRetainOrdinaryText(t *testing.T) {
	const chunks = 1024
	text := strings.Repeat("x", 1024)
	state := newAnthropicStreamState("synthetic", false)
	var received strings.Builder
	for i := 0; i < chunks; i++ {
		for _, payload := range parseSSEPayloads(t, string(state.feed(messageTextChunk(text)))) {
			var event struct {
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				t.Fatal(err)
			}
			received.WriteString(event.Delta.Text)
		}
	}
	if received.String() != strings.Repeat(text, chunks) {
		t.Fatal("streamed text changed")
	}
	for _, block := range state.blocks {
		if block.kind == "text" && block.text.buf.Cap() != 0 {
			t.Fatalf("already streamed ordinary text is still retained: len=%d cap=%d", block.text.buf.Len(), block.text.buf.Cap())
		}
	}
}

func TestMessagesTextDeltaWireCompatibility(t *testing.T) {
	for _, index := range []int{0, 17} {
		for _, text := range []string{
			"plain text", "引号\"与反斜杠\\\n下一行\r\t", "<script>&\u2028\u2029", "emoji:🙂", string([]byte{'x', 0, 0xff, 0xc0, 'y'}),
		} {
			t.Run(fmt.Sprintf("%d/%q", index, text), func(t *testing.T) {
				state := newAnthropicStreamState("synthetic", false)
				state.started = true
				state.text = &anthropicTextSlot{open: true, index: index}
				got := state.feed(messageTextChunk(text))
				payload, err := json.Marshal(map[string]any{
					"type": "content_block_delta", "index": index,
					"delta": map[string]any{"type": "text_delta", "text": text},
				})
				if err != nil {
					t.Fatal(err)
				}
				want := "event: content_block_delta\ndata: " + string(payload) + "\n\n"
				if string(got) != want {
					t.Fatalf("text event changed:\n got: %q\nwant: %q", got, want)
				}
				saved := bytes.Clone(got)
				_ = state.feed(messageTextChunk("another event"))
				if !bytes.Equal(got, saved) {
					t.Fatal("later event overwrote an earlier frame")
				}
			})
		}
	}
}

func TestMessagesTextBehindPendingToolKeepsOrder(t *testing.T) {
	state := newAnthropicStreamState("synthetic", false)
	tool := func(id, name, arguments string) map[string]any {
		return map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{
			"index": float64(0), "id": id, "function": map[string]any{"name": name, "arguments": arguments},
		}}}}}}
	}
	_ = state.feed(tool("", "", `{"path":`))
	const text = "after pending tool <&>\n"
	if got := state.feed(messageTextChunk(text)); len(got) != 0 {
		t.Fatalf("later text escaped before tool identity: %q", got)
	}
	if state.pendingEventBytes == 0 {
		t.Fatal("later text must remain queued until tool identity arrives")
	}
	out := state.feed(tool("call_synthetic", "read_file", `"a"}`))
	state.finish = "tool_calls"
	out = append(out, state.finishEvents()...)
	var kinds []string
	var received strings.Builder
	for _, payload := range parseSSEPayloads(t, string(out)) {
		var event struct {
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatal(err)
		}
		if event.ContentBlock.Type != "" {
			kinds = append(kinds, event.ContentBlock.Type)
		}
		received.WriteString(event.Delta.Text)
	}
	if strings.Join(kinds, ",") != "tool_use,text" || received.String() != text {
		t.Fatalf("pending text changed or reordered: kinds=%v text=%q", kinds, received.String())
	}
	if state.pendingEventBytes != 0 || len(state.pendingEvents) != 0 {
		t.Fatal("released pending events remain retained")
	}
	if err := state.validateToolInputs(); err != nil {
		t.Fatal(err)
	}
}
