package main

import (
	"encoding/json"
	"fmt"
)

// Chat has no native tool_reference blocks. Keep deferred tools registered as
// ordinary functions, and turn a search result's reference into the referenced
// function definition. Later references retain the name without repeating the
// schema. Merely discarding the block would erase the tool search result. Only
// tool_result.content may contain a reference.
// All replacement containers are copied so direct converter callers retain
// their original Messages history, including exact json.Number values.
func normalizeAnthropicToolReferences(body map[string]any) (map[string]any, error) {
	type definition struct {
		function map[string]any
		count    int
		size     conversionJSONSize
		seen     bool
		emitted  bool
	}
	var definitions map[string]*definition
	messages, _ := body["messages"].([]any)
	const fullPrefix = "\n[Referenced tool definition]\n"
	const shortPrefix = "\n[Referenced tool]\n"
	fullBase, _ := measureConversionJSON(map[string]any{"type": "text", "text": fullPrefix + "\n"}, requestDecodedLimit)
	shortBase, _ := measureConversionJSON(map[string]any{"type": "text", "text": shortPrefix + "\n"}, requestDecodedLimit)
	total := 0
	// First reserve the complete normalized size. No definition is serialized or
	// inserted until all unique and repeated references fit the shared budget.
	for i, raw := range messages {
		message, _ := raw.(map[string]any)
		if strOf(message["role"]) != "user" {
			continue
		}
		blocks, _ := message["content"].([]any)
		for j, raw := range blocks {
			result, _ := raw.(map[string]any)
			if strOf(result["type"]) != "tool_result" {
				continue
			}
			content, _ := result["content"].([]any)
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
					original, err := measureConversionJSON(body, requestDecodedLimit)
					if err != nil {
						return nil, err
					}
					total = original.plain
					definitions = make(map[string]*definition)
					tools, _ := body["tools"].([]any)
					for _, raw := range convertAnthropicTools(tools) {
						tool, _ := raw.(map[string]any)
						function, _ := tool["function"].(map[string]any)
						toolName := strOf(function["name"])
						if toolName != "" {
							entry := definitions[toolName]
							if entry == nil {
								entry = &definition{function: function}
								definitions[toolName] = entry
							}
							entry.count++
						}
					}
				}
				entry := definitions[name]
				if entry == nil || entry.count != 1 {
					return nil, fmt.Errorf("%s.tool_name 必须引用 tools 中唯一声明的工具", path)
				}
				var replacement int
				if !entry.seen {
					var err error
					entry.size, err = measureConversionJSON(entry.function, requestDecodedLimit)
					if err != nil {
						return nil, err
					}
					entry.seen = true
					replacement = fullBase.plain + entry.size.quoted
				} else {
					nameSize, err := measureConversionJSON(name, requestDecodedLimit)
					if err != nil {
						return nil, err
					}
					replacement = shortBase.plain + nameSize.plain - 2
				}
				original, err := measureConversionJSON(reference, requestDecodedLimit)
				if err != nil {
					return nil, err
				}
				total -= original.plain
				if replacement > requestDecodedLimit-total {
					return nil, &conversionSizeError{limit: requestDecodedLimit}
				}
				total += replacement
			}
		}
	}
	if definitions == nil {
		return body, nil
	}

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
				name := reference["tool_name"].(string)
				entry := definitions[name]
				var text string
				if !entry.emitted {
					encoded, err := json.Marshal(entry.function)
					if err != nil {
						return nil, fmt.Errorf("messages[%d].content[%d].content[%d] 引用的工具定义无法序列化", i, j, k)
					}
					text = fullPrefix + string(encoded) + "\n"
					entry.emitted = true
				} else {
					text = shortPrefix + name + "\n"
				}
				if copiedContent == nil {
					copiedContent = append([]any(nil), content...)
				}
				copiedContent[k] = map[string]any{"type": "text", "text": text}
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
