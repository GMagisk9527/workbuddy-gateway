package main

import (
	"fmt"
	"unicode/utf8"
)

// This gateway is a stateless Chat translator, not an OpenAI response store.
// Reject server-side references before billing rather than silently dropping
// conversation history or encrypted state that the Chat upstream cannot replay.
func validateResponsesReplay(body map[string]any) error {
	if err := validateResponsesCapabilities(body); err != nil {
		return err
	}
	for _, key := range []string{"previous_response_id", "conversation"} {
		if value := body[key]; value != nil && value != "" {
			return fmt.Errorf("%s 不受无状态网关支持：请在 input 中回放完整消息及 function_call/function_call_output 历史", key)
		}
	}
	if body["store"] == true {
		return fmt.Errorf("store=true 不受无状态网关支持：请使用 store=false 并由客户端回放历史")
	}
	input, ok := body["input"].([]any)
	if !ok {
		switch body["input"].(type) {
		case nil, string:
			return nil
		default:
			return fmt.Errorf("input 必须是字符串或消息数组")
		}
	}
	for i, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			if _, text := raw.(string); text { // 保留现有文本数组兼容
				continue
			}
			return fmt.Errorf("input[%d] 必须是消息对象", i)
		}
		switch stringField(item, "type") {
		case "", "message":
			if err := validateResponsesMessage(item); err != nil {
				return fmt.Errorf("input[%d]: %w", i, err)
			}
		case "reasoning":
			if item["encrypted_content"] != nil && reasoningReplayText(item) == "" {
				return fmt.Errorf("input[%d]: Chat 上游无法回放仅含 encrypted_content 的 reasoning 项", i)
			}
		case "function_call":
			if stringField(item, "call_id") == "" || stringField(item, "name") == "" {
				return fmt.Errorf("input[%d]: function_call 必须包含 call_id 和 name", i)
			}
			if _, ok := item["arguments"].(string); !ok {
				return fmt.Errorf("input[%d]: function_call.arguments 必须是 JSON 字符串", i)
			}
		case "function_call_output":
			if stringField(item, "call_id") == "" {
				return fmt.Errorf("input[%d]: function_call_output 必须包含 call_id", i)
			}
		case "custom_tool_call":
			if stringField(item, "call_id") == "" || stringField(item, "name") == "" {
				return fmt.Errorf("input[%d]: custom_tool_call 必须包含 call_id 和 name", i)
			}
			if _, ok := item["input"].(string); !ok {
				return fmt.Errorf("input[%d]: custom_tool_call.input 必须是字符串", i)
			}
		case "custom_tool_call_output":
			if stringField(item, "call_id") == "" {
				return fmt.Errorf("input[%d]: custom_tool_call_output 必须包含 call_id", i)
			}
		case "web_search_call": // 保留已有的搜索历史兼容，不把它插入工具结果序列
		default:
			return fmt.Errorf("input[%d]: 当前 Chat 上游不支持此 Responses 项类型；请回放完整文本/函数调用历史", i)
		}
	}
	return nil
}

// Validate options with semantics that a synchronous Chat upstream cannot
// provide. Empty/default options remain compatible; metadata is kept locally
// by the response layer rather than forwarded as an upstream user identifier.
func validateResponsesCapabilities(body map[string]any) error {
	if value := body["background"]; value != nil {
		background, ok := value.(bool)
		if !ok {
			return fmt.Errorf("background 必须是布尔值")
		}
		if background {
			return fmt.Errorf("background=true 不受当前同步 Chat 网关支持；请使用 background=false")
		}
	}
	if value := body["prompt"]; value != nil {
		prompt, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("prompt 必须是对象")
		}
		if len(prompt) != 0 {
			return fmt.Errorf("当前 Chat 网关不支持 prompt 模板；请在 instructions/input 中提供展开后的内容")
		}
	}
	if value := body["truncation"]; value != nil {
		mode, ok := value.(string)
		if !ok {
			return fmt.Errorf("truncation 必须是字符串")
		}
		if mode != "" && mode != "disabled" {
			return fmt.Errorf("当前 Chat 网关不支持自动 truncation；请使用 disabled 并由客户端管理历史长度")
		}
	}
	if value := body["include"]; value != nil {
		includes, ok := value.([]any)
		if !ok {
			return fmt.Errorf("include 必须是字符串数组")
		}
		for _, raw := range includes {
			include, ok := raw.(string)
			if !ok {
				return fmt.Errorf("include 必须是字符串数组")
			}
			// The response layer provides gateway-encrypted reasoning for
			// stateless replay. Other optional native output data is unavailable.
			if include != "reasoning.encrypted_content" {
				return fmt.Errorf("当前 Chat 网关的 include 仅支持 reasoning.encrypted_content")
			}
		}
	}
	if value := body["text"]; value != nil {
		text, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("text 必须是对象")
		}
		if value := text["format"]; value != nil {
			format, ok := value.(map[string]any)
			if !ok {
				return fmt.Errorf("text.format 必须是对象")
			}
			if value := format["type"]; value != nil {
				kind, ok := value.(string)
				if !ok {
					return fmt.Errorf("text.format.type 必须是字符串")
				}
				if kind != "" && kind != "text" {
					// WorkBuddy accepts response_format without guaranteeing its
					// constraints. Match the Messages structured-output boundary.
					return fmt.Errorf("当前 WorkBuddy Chat 上游不支持保证结构化输出约束的 text.format；请使用 type=text 或原生支持结构化输出的上游")
				}
			}
		}
	}
	if value := body["metadata"]; value != nil {
		metadata, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("metadata 必须是字符串键值对象")
		}
		if len(metadata) > 16 {
			return fmt.Errorf("metadata 最多允许 16 个键值对")
		}
		for key, raw := range metadata {
			value, ok := raw.(string)
			if !ok || utf8.RuneCountInString(key) > 64 || utf8.RuneCountInString(value) > 512 {
				return fmt.Errorf("metadata 的键必须不超过 64 个字符，值必须是不超过 512 个字符的字符串")
			}
		}
	}
	return nil
}

func validateResponsesMessage(item map[string]any) error {
	if role := stringField(item, "role"); role != "system" && role != "developer" && role != "user" && role != "assistant" {
		return fmt.Errorf("message.role 必须是 system/developer/user/assistant")
	}
	parts, ok := item["content"].([]any)
	if !ok {
		switch item["content"].(type) {
		case string, nil:
			return nil
		default:
			return fmt.Errorf("message.content 必须是字符串或内容块数组")
		}
	}
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			if _, text := raw.(string); text {
				continue
			}
			return fmt.Errorf("消息内容块结构非法")
		}
		switch stringField(part, "type") {
		case "input_text", "output_text", "text", "refusal":
		case "input_image":
			if stringField(part, "image_url") == "" {
				return fmt.Errorf("Chat 上游要求 input_image.image_url，不支持仅含 file_id 的图片")
			}
		default:
			return fmt.Errorf("Chat 上游不支持此消息内容块类型")
		}
	}
	return nil
}
