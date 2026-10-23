package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Keep numbers inside tool arguments and schemas exact. Only the known scalar
// generation options use the converter's existing float64 representation.
func decodeAnthropicMessageBody(raw []byte, body *map[string]any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(body); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("请求体必须是单个 JSON 对象")
	}
	for _, key := range []string{"max_tokens", "temperature", "top_p", "top_k"} {
		if n, ok := (*body)[key].(json.Number); ok {
			value, err := n.Float64()
			if err != nil {
				return fmt.Errorf("%s 数值超出支持范围", key)
			}
			(*body)[key] = value
		}
	}
	if thinking, ok := (*body)["thinking"].(map[string]any); ok {
		if n, ok := thinking["budget_tokens"].(json.Number); ok {
			value, err := n.Float64()
			if err != nil {
				return fmt.Errorf("thinking.budget_tokens 数值超出支持范围")
			}
			thinking["budget_tokens"] = value
		}
	}
	return nil
}

func validateAnthropicChatInput(body map[string]any) error {
	messages, ok := body["messages"].([]any)
	if !ok || len(messages) == 0 {
		return fmt.Errorf("messages 字段必须是非空数组")
	}
	if system, exists := body["system"]; exists && system != nil {
		if err := validateAnthropicChatContent(system, "system", "system"); err != nil {
			return err
		}
	}
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("messages[%d] 必须是包含 role 的消息对象", i)
		}
		role := strOf(message["role"])
		if role == "" {
			return fmt.Errorf("messages[%d] 必须是包含 role 的消息对象", i)
		}
		// Retain the gateway's instruction-role compatibility. Some client
		// launchers include system/developer entries inside messages; the shared
		// Chat preparation preserves system and normalizes developer to system.
		if role != "user" && role != "assistant" && role != "system" && role != "developer" {
			return fmt.Errorf("messages[%d].role 必须是 user、assistant、system 或 developer", i)
		}
		if err := validateAnthropicChatContent(message["content"], role, fmt.Sprintf("messages[%d].content", i)); err != nil {
			return err
		}
	}
	if raw, exists := body["tools"]; exists && raw != nil {
		tools, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("tools 必须是数组")
		}
		for i, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("tools[%d] 必须是工具对象", i)
			}
			if fn, exists := tool["function"]; exists {
				function, ok := fn.(map[string]any)
				if !ok || strOf(function["name"]) == "" {
					return fmt.Errorf("tools[%d].function 缺少有效 name", i)
				}
				continue // Retain the existing Chat-form tool compatibility.
			}
			kind := strOf(tool["type"])
			if kind != "" && kind != "custom" {
				return fmt.Errorf("tools[%d] 的工具类型无法转为 Chat 自定义函数；当前不支持 Anthropic 托管或预定义工具", i)
			}
			if strOf(tool["name"]) == "" {
				return fmt.Errorf("tools[%d] 缺少有效 name", i)
			}
			if strict, exists := tool["strict"]; exists {
				if _, ok := strict.(bool); !ok {
					return fmt.Errorf("tools[%d].strict 必须是布尔值", i)
				}
			}
		}
	}
	return nil
}

func validateAnthropicChatContent(content any, role, path string) error {
	if _, ok := content.(string); ok {
		return nil
	}
	blocks, ok := content.([]any)
	if !ok || (role != "system" && len(blocks) == 0) {
		return fmt.Errorf("%s 必须是文本或非空内容块数组", path)
	}
	for i, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s[%d] 必须是内容块对象", path, i)
		}
		blockPath := fmt.Sprintf("%s[%d]", path, i)
		switch strOf(block["type"]) {
		case "text":
			if _, ok := block["text"].(string); ok {
				continue
			}
		case "image":
			if (role == "user" || role == "tool") && anthropicImageURL(block) != "" {
				continue
			}
		case "thinking", "redacted_thinking":
			if role == "assistant" {
				continue // Signatures/redacted bytes are intentionally not replayed.
			}
		case "tool_use":
			if role == "assistant" && strOf(block["id"]) != "" && strOf(block["name"]) != "" {
				if input, ok := block["input"].(map[string]any); !ok || input == nil {
					return fmt.Errorf("%s.input 必须是 JSON 对象", blockPath)
				}
				continue
			}
		case "tool_result":
			if role != "user" || strOf(block["tool_use_id"]) == "" {
				break
			}
			if flag, exists := block["is_error"]; exists {
				if _, ok := flag.(bool); !ok {
					return fmt.Errorf("%s.is_error 必须是布尔值", blockPath)
				}
			}
			if nested, exists := block["content"]; exists && nested != nil {
				if list, ok := nested.([]any); ok && len(list) == 0 {
					continue // Empty tool results are valid.
				}
				if err := validateAnthropicChatContent(nested, "tool", blockPath+".content"); err != nil {
					return err
				}
			}
			continue
		}
		return fmt.Errorf("%s 的内容块类型或字段无法转换到 Chat；支持文本、图片及客户端工具调用/结果，不支持文档、搜索结果或托管工具块", blockPath)
	}
	return nil
}

func validateAnthropicOutputFormat(body map[string]any) error {
	raw, exists := body["output_config"]
	if !exists || raw == nil {
		return nil
	}
	output, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("output_config 必须是对象")
	}
	if format, exists := output["format"]; exists && format != nil {
		// A live probe showed WorkBuddy accepting response_format=json_schema
		// without applying the constraint. Do not report that as supported.
		return fmt.Errorf("当前 WorkBuddy Chat 上游不支持保证 JSON Schema 约束的 output_config.format；请移除此选项或使用原生支持结构化输出的上游")
	}
	return nil
}
