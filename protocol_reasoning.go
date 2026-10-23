package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

const gatewayReasoningPrefix = "wbg-r1."

// Opaque reasoning is carried by the client, not retained in a gateway cache.
// The key is scoped to its API credential and the authenticated data separates
// models and protocols. A stable credential permits replay across restarts.
type protocolReasoningCodec struct {
	aead cipher.AEAD
	aad  []byte
}

var anonymousReasoningKey = struct {
	once sync.Once
	key  [32]byte
}{}

func newProtocolReasoningCodec(r *http.Request, model, protocol string) (*protocolReasoningCodec, error) {
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if token == "" {
		token = currentAdminKey()
	}
	var key [32]byte
	if token != "" {
		key = sha256.Sum256([]byte("workbuddy-gateway/reasoning/key/v1\x00" + token))
	} else {
		// An entirely unconfigured, unauthenticated gateway has no durable
		// secret. Its opaque histories are deliberately process-local.
		anonymousReasoningKey.once.Do(func() { _, _ = rand.Read(anonymousReasoningKey.key[:]) })
		key = anonymousReasoningKey.key
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &protocolReasoningCodec{aead: aead, aad: []byte("workbuddy-gateway/reasoning/v1\x00" + protocol + "\x00" + model)}, nil
}

func (c *protocolReasoningCodec) seal(text string) string {
	return gatewayReasoningPrefix + base64.RawStdEncoding.EncodeToString(c.aead.Seal(nil, nil, []byte(text), c.aad))
}

func (c *protocolReasoningCodec) open(encoded string) (string, error) {
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(encoded, gatewayReasoningPrefix))
	if err != nil {
		return "", fmt.Errorf("无法恢复网关推理状态：签名无效，请使用原 API Key 和模型回放原始历史")
	}
	text, err := c.aead.Open(nil, nil, raw, c.aad)
	if err != nil {
		return "", fmt.Errorf("无法恢复网关推理状态：签名无效，请使用原 API Key 和模型回放原始历史")
	}
	return string(text), nil
}

func cloneProtocolObject(source map[string]any) map[string]any {
	copy := make(map[string]any, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

type anthropicThinkingPolicy struct {
	codec *protocolReasoningCodec
	hide  bool
}
type anthropicThinkingPolicyKey struct{}

func prepareAnthropicThinking(r *http.Request, body map[string]any, model string) (*http.Request, map[string]any, error) {
	thinking, _ := body["thinking"].(map[string]any)
	display := strOf(thinking["display"])
	if value := thinking["display"]; value != nil {
		if _, ok := value.(string); !ok || display != "" && display != "summarized" && display != "omitted" && display != "updates" {
			return r, body, fmt.Errorf("thinking.display 必须是 summarized、omitted 或 updates")
		}
	}
	hide := anthropicThinkingOn(body) && (display == "omitted" || display == "updates")
	messages, _ := body["messages"].([]any)
	needsCodec := hide
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		blocks, _ := message["content"].([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			if block["type"] == "thinking" && strings.HasPrefix(strOf(block["signature"]), gatewayReasoningPrefix) {
				needsCodec = true
			}
		}
	}
	if !needsCodec {
		return r, body, nil
	}
	codec, err := newProtocolReasoningCodec(r, model, "messages")
	if err != nil {
		return r, body, fmt.Errorf("初始化网关推理状态失败")
	}
	restored := append([]any(nil), messages...)
	changed := false
	for i, raw := range messages {
		message, _ := raw.(map[string]any)
		blocks, _ := message["content"].([]any)
		var updated []any
		for j, raw := range blocks {
			block, _ := raw.(map[string]any)
			if block["type"] != "thinking" || !strings.HasPrefix(strOf(block["signature"]), gatewayReasoningPrefix) {
				continue
			}
			if message["role"] != "assistant" {
				return r, body, fmt.Errorf("网关推理状态仅可用于 assistant thinking 内容块")
			}
			text, err := codec.open(strOf(block["signature"]))
			if err != nil {
				return r, body, err
			}
			if updated == nil {
				updated = append([]any(nil), blocks...)
			}
			copy := cloneProtocolObject(block)
			copy["thinking"] = text
			updated[j] = copy
		}
		if updated != nil {
			copy := cloneProtocolObject(message)
			copy["content"] = updated
			restored[i] = copy
			changed = true
		}
	}
	if changed {
		body = cloneProtocolObject(body)
		body["messages"] = restored
	}
	return r.WithContext(context.WithValue(r.Context(), anthropicThinkingPolicyKey{}, anthropicThinkingPolicy{codec: codec, hide: hide})), body, nil
}

func applyAnthropicThinkingPolicy(state *anthropicStreamState, r *http.Request) {
	if policy, ok := r.Context().Value(anthropicThinkingPolicyKey{}).(anthropicThinkingPolicy); ok && policy.hide {
		state.hiddenThinking = policy.codec
	}
}

type responsesReasoningPolicyKey struct{}

func prepareResponsesReasoning(r *http.Request, body map[string]any, model string) (*http.Request, map[string]any, error) {
	requested := false
	if include, ok := body["include"].([]any); ok {
		for _, value := range include {
			requested = requested || value == "reasoning.encrypted_content"
		}
	}
	items, _ := body["input"].([]any)
	needsCodec := requested
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] == "reasoning" && strings.HasPrefix(strOf(item["encrypted_content"]), gatewayReasoningPrefix) {
			needsCodec = true
		}
	}
	if !needsCodec {
		return r, body, nil
	}
	codec, err := newProtocolReasoningCodec(r, model, "responses")
	if err != nil {
		return r, body, fmt.Errorf("初始化网关推理状态失败")
	}
	var updated []any
	for i, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] != "reasoning" || !strings.HasPrefix(strOf(item["encrypted_content"]), gatewayReasoningPrefix) {
			continue
		}
		text, err := codec.open(strOf(item["encrypted_content"]))
		if err != nil {
			return r, body, err
		}
		if updated == nil {
			updated = append([]any(nil), items...)
		}
		copy := cloneProtocolObject(item)
		copy["content"] = []any{map[string]any{"type": "reasoning_text", "text": text}}
		updated[i] = copy
	}
	if updated != nil {
		body = cloneProtocolObject(body)
		body["input"] = updated
	}
	if requested {
		r = r.WithContext(context.WithValue(r.Context(), responsesReasoningPolicyKey{}, codec))
	}
	return r, body, nil
}

func decorateResponsesReasoningItem(r *http.Request, item map[string]any) map[string]any {
	if r == nil || item["type"] != "reasoning" || strOf(item["encrypted_content"]) != "" {
		return item
	}
	codec, _ := r.Context().Value(responsesReasoningPolicyKey{}).(*protocolReasoningCodec)
	if codec == nil {
		return item
	}
	if text := reasoningReplayText(item); text != "" {
		copy := cloneProtocolObject(item)
		copy["encrypted_content"] = codec.seal(text)
		return copy
	}
	return item
}
