package main

import (
	"encoding/json"
	"fmt"
)

// Chat has no native tool_reference blocks. Keep deferred tools registered as
// ordinary functions, and turn a search result's reference into the referenced
// function definition. Merely accepting and discarding the block would erase
// the tool search result. Only tool_result.content may contain a reference.
// All replacement containers are copied so direct converter callers retain
// their original Messages history, including exact json.Number values.
func normalizeAnthropicToolReferences(body map[string]any) (map[string]any, error) {
	type definition struct {
		function map[string]any
		count    int
	}
	var definitions map[string]definition
	messages, _ := body["messages"].([]any)
	var copiedMessages []any
	for i, raw := range messages {
		message, _ := raw.(map[string]any)
		if strOf(message["role"]) != "user" {
			continue
		}
		blocks, _ := message["content"].([]any)
		var copiedBlocks []any
		for j, raw := range blocks {
			result, _ := raw.(map[string]any)
			if strOf(result["type"]) != "tool_result" {
				continue
			}
			content, _ := result["content"].([]any)
			var copiedContent []any
			for k, raw := range content {
				reference, _ := raw.(map[string]any)
				if strOf(reference["type"]) != "tool_reference" {
					continue
				}
				path := fmt.Sprintf("messages[%d].content[%d].content[%d]", i, j, k)
				name, ok := reference["tool_name"].(string)
				if !ok || name == "" {
					return nil, fmt.Errorf("%s.tool_name 必须是非空字符串", path)
				}
				if definitions == nil {
					definitions = make(map[string]definition)
					tools, _ := body["tools"].([]any)
					for _, raw := range convertAnthropicTools(tools) {
						tool, _ := raw.(map[string]any)
						function, _ := tool["function"].(map[string]any)
						toolName := strOf(function["name"])
						if toolName != "" {
							entry := definitions[toolName]
							entry.function = function
							entry.count++
							definitions[toolName] = entry
						}
					}
				}
				entry := definitions[name]
				if entry.count != 1 {
					return nil, fmt.Errorf("%s.tool_name 必须引用 tools 中唯一声明的工具", path)
				}
				encoded, err := json.Marshal(entry.function)
				if err != nil {
					return nil, fmt.Errorf("%s 引用的工具定义无法序列化", path)
				}
				if copiedContent == nil {
					copiedContent = append([]any(nil), content...)
				}
				copiedContent[k] = map[string]any{"type": "text", "text": "\n[Referenced tool definition]\n" + string(encoded) + "\n"}
			}
			if copiedContent != nil {
				if copiedBlocks == nil {
					copiedBlocks = append([]any(nil), blocks...)
				}
				copiedResult := copyAnthropicReferenceMap(result)
				copiedResult["content"] = copiedContent
				copiedBlocks[j] = copiedResult
			}
		}
		if copiedBlocks != nil {
			if copiedMessages == nil {
				copiedMessages = append([]any(nil), messages...)
			}
			copiedMessage := copyAnthropicReferenceMap(message)
			copiedMessage["content"] = copiedBlocks
			copiedMessages[i] = copiedMessage
		}
	}
	if copiedMessages == nil {
		return body, nil
	}
	copy := copyAnthropicReferenceMap(body)
	copy["messages"] = copiedMessages
	return copy, nil
}

func copyAnthropicReferenceMap(original map[string]any) map[string]any {
	copy := make(map[string]any, len(original))
	for key, value := range original {
		copy[key] = value
	}
	return copy
}
