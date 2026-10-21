package main

import "fmt"

// This gateway is a stateless Chat translator, not an OpenAI response store.
// Reject server-side references before billing rather than silently dropping
// conversation history or encrypted state that the Chat upstream cannot replay.
func validateResponsesReplay(body map[string]any) error {
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
		case "web_search_call": // 保留已有的搜索历史兼容，不把它插入工具结果序列
		default:
			return fmt.Errorf("input[%d]: 当前 Chat 上游不支持此 Responses 项类型；请回放完整文本/函数调用历史", i)
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
