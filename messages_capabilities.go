package main

import "fmt"

// Preserve harmless client hints, but reject requested hosted capabilities
// before billing instead of silently reporting an ordinary Chat completion.
func validateAnthropicCapabilities(body map[string]any) error {
	if value := body["context_management"]; value != nil {
		management, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("context_management 必须是对象")
		}
		for key := range management {
			if key != "edits" {
				return fmt.Errorf("当前 Chat 网关仅支持 context_management 中保留全部 thinking 的无操作配置")
			}
		}
		if value := management["edits"]; value != nil {
			edits, ok := value.([]any)
			if !ok {
				return fmt.Errorf("context_management.edits 必须是数组")
			}
			for _, raw := range edits {
				edit, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("context_management.edits 必须包含编辑对象")
				}
				// Claude Code currently asks to retain every thinking block.
				// This is a no-op and must not break otherwise valid requests.
				if strOf(edit["type"]) != "clear_thinking_20251015" || edit["keep"] != "all" {
					return fmt.Errorf("当前 Chat 网关不支持 context_management 的历史裁剪；仅支持 clear_thinking_20251015 且 keep=all，请由客户端管理历史")
				}
				for key := range edit {
					if key != "type" && key != "keep" {
						return fmt.Errorf("当前 Chat 网关不支持此 context_management 编辑选项")
					}
				}
			}
		}
	}
	if value := body["mcp_servers"]; value != nil {
		servers, ok := value.([]any)
		if !ok {
			return fmt.Errorf("mcp_servers 必须是数组")
		}
		if len(servers) != 0 {
			return fmt.Errorf("当前 Chat 网关不支持托管 mcp_servers；请由客户端执行工具")
		}
	}
	if value := body["container"]; value != nil {
		if id, ok := value.(string); !ok || id != "" {
			return fmt.Errorf("当前 Chat 网关不支持托管 container；请由客户端执行工具")
		}
	}
	if value := body["service_tier"]; value != nil {
		tier, ok := value.(string)
		if !ok {
			return fmt.Errorf("service_tier 必须是字符串")
		}
		if tier != "" && tier != "auto" {
			return fmt.Errorf("当前 Chat 网关无法保证 Anthropic service_tier；请使用 auto")
		}
	}
	return nil
}
