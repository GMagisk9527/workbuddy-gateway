package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// -----------------------------------------------------------------------------
// OpenAI Responses API (/v1/responses) -> 上游 Chat Completions 转译
//
// 网关只与上游 /v2/chat/completions 通信，此处负责双向协议转换：
//   - 请求：input / instructions / tools(扁平) / tool_choice / max_output_tokens /
//     reasoning.effort  ->  chat messages / tools(嵌套 function) / max_tokens / reasoning_effort
//   - 非流式响应：chat.completion -> response{object:"response", output:[...]}
//   - 流式响应：上游 SSE 增量 -> Responses 语义事件（response.output_text.delta、
//     response.function_call_arguments.delta、response.completed ...）
// -----------------------------------------------------------------------------

// handleResponses 处理 POST /v1/responses。
func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "仅支持 POST 请求")
		return
	}

	reqID := requestIDFor(r)
	startTime := requestStartFor(r)

	readStarted := debugBodyReadStarted(r)
	bodyBytes, err := io.ReadAll(r.Body)
	readDuration := debugElapsedSince(readStarted)
	if err != nil {
		debugBodyReadFailed(r, bodyBytes, readStarted, err)
		if writeRequestBodyLimitError(w, r, err) {
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "read_error", "读取请求体失败")
		return
	}
	defer r.Body.Close()

	var respReq map[string]any
	decodeStarted := time.Now()
	decodeErr := json.Unmarshal(bodyBytes, &respReq)
	decodeDuration := time.Since(decodeStarted)
	if decodeErr != nil {
		debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, false, decodeErr)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
		return
	}

	modelName, _ := respReq["model"].(string)
	if modelName == "" {
		modelName = "hy4-preview"
	}
	isStream, _ := respReq["stream"].(bool)
	debugSetModelAndStream(r, modelName, isStream)
	debugBodyReadCompleted(r, bodyBytes, readDuration, decodeDuration, true, nil)
	prepareConversationContext(r, respReq)

	// 模型黑白名单拦截：命中即拒绝，不消耗任何上游账号额度。
	if disabled, reason := modelDisabled(modelName); disabled {
		log.Printf("[请求被拒绝] traceId=%s requestId=%d 拦截层=模型黑白名单 模型=%s 结果=拒绝 原因=%s 返回状态码=403 业务影响=请求未进入上游调用",
			w.Header().Get("X-Trace-ID"), reqID, modelName, reason)
		debugEvent(r, "warn", "model_blocked_by_config", map[string]any{
			"status_code":     http.StatusForbidden,
			"reason":          reason,
			"business_impact": "模型被网关配置禁用，请求未进入上游调用",
		})
		writeOpenAIError(w, http.StatusForbidden, "model_disabled", reason)
		return
	}

	r, respReq, err = prepareResponsesReasoning(r, respReq, modelName)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chatReq, toolMap, err := responsesToChatRequestDetailed(respReq, modelName)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	chatReq["stream"] = true // 上游强制流式，非流式由网关本地聚合
	recordCompatibilityRule(r, "responses_to_chat")
	// 下游 Responses 恢复原始 namespace/custom 形状；Chat 上游只认识扁平的 function 名。
	r = withResponsesToolMap(r, toolMap)
	r = withResponsesMetadata(r, respReq["metadata"])

	prepareUpstreamChatRequest(chatReq, r, reqID, modelName)
	logResponsesToolOutputConversion(r, reqID, respReq)

	upstreamBytes, err := json.Marshal(chatReq)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "encode_error", "序列化请求失败")
		return
	}

	log.Printf("[#%d] POST /v1/responses -> Upstream [Model: %s, Stream: %v]", reqID, modelName, isStream)

	// 前缀摘要用已解析的请求对象计算：只涉及首条消息与 tools，不扫描历史。
	if !keyModelAllowed(w, r, modelName) {
		return
	}
	r = withAffinityDigestIfEnabled(r, chatReq, modelName)
	resp, acc, prof, ok := upstreamChat(w, r, reqID, modelName, upstreamBytes, startTime)
	if !ok {
		return
	}
	if isStream {
		streamResponsesResponse(w, r, resp, modelName, reqID, acc, prof, startTime)
	} else {
		writeResponsesAggregate(w, r, resp, modelName, reqID, acc, prof, startTime)
	}
}

// responsesToChatRequest 保留旧的测试/调用接口；主链路使用带工具映射的详细版本。
func responsesToChatRequest(respReq map[string]any, modelName string) (map[string]any, error) {
	chat, _, err := responsesToChatRequestDetailed(respReq, modelName)
	return chat, err
}

// responsesToChatRequestDetailed 将 Responses 请求体转换为上游 Chat Completions
// 请求体，并返回上游 function 名到客户端工具身份的反向映射。
func responsesToChatRequestDetailed(respReq map[string]any, modelName string) (map[string]any, map[string]responsesToolMeta, error) {
	if err := validateResponsesReplay(respReq); err != nil {
		return nil, nil, err
	}
	chat := map[string]any{"model": modelName}

	messages := []any{}
	if instructions, ok := respReq["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	switch input := respReq["input"].(type) {
	case string:
		if strings.TrimSpace(input) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": input})
		}
	case []any:
		// DSH 将一次模型回复拆成 reasoning、message、function_call 等独立项；
		// Chat 上游却要求同一次回复是一条 assistant 消息。连续的助手项在
		// user/tool 结果处结束，遇到新的 reasoning 项也代表下一次助手回复。
		// 即使一次输出没有 reasoning，也必须将正文与工具调用合在一起。
		var pendingReasoning string
		var pendingAssistant map[string]any
		flushAssistant := func() {
			if pendingAssistant != nil {
				messages = append(messages, pendingAssistant)
				pendingAssistant = nil
			}
			pendingReasoning = ""
		}
		for _, itemAny := range input {
			switch item := itemAny.(type) {
			case string:
				flushAssistant()
				messages = append(messages, map[string]any{"role": "user", "content": item})
			case map[string]any:
				typ, _ := item["type"].(string)
				if typ == "reasoning" {
					if pendingAssistant != nil {
						flushAssistant() // 新 reasoning 项代表下一次助手回复
					}
					if text := reasoningReplayText(item); text != "" {
						if pendingReasoning != "" {
							pendingReasoning += "\n\n"
						}
						pendingReasoning += text
					}
					continue
				}
				if typ == "web_search_call" {
					continue // 不把历史搜索项误转成会打断工具调用的消息
				}
				for _, msgAny := range convertResponsesInputItem(item) {
					msg, ok := msgAny.(map[string]any)
					if !ok {
						continue
					}
					if role, _ := msg["role"].(string); role == "assistant" {
						if pendingAssistant == nil {
							pendingAssistant = msg
							if pendingReasoning != "" {
								if _, explicit := pendingAssistant["reasoning_content"]; !explicit {
									pendingAssistant["reasoning_content"] = pendingReasoning
								}
							}
						} else {
							mergeResponsesAssistantItem(pendingAssistant, msg)
						}
						continue
					}
					flushAssistant() // user/tool 边界，不把旧推理带到下一轮
					messages = append(messages, msg)
				}
			}
		}
		flushAssistant()
	}

	if len(messages) == 0 {
		return nil, nil, fmt.Errorf("input 为空：Responses 请求必须提供 input 或 instructions")
	}
	chat["messages"] = messages

	if v, ok := respReq["temperature"]; ok && v != nil {
		chat["temperature"] = v
	}
	if v, ok := respReq["top_p"]; ok && v != nil {
		chat["top_p"] = v
	}
	if v, ok := respReq["parallel_tool_calls"].(bool); ok {
		chat["parallel_tool_calls"] = v
	}
	if text, ok := respReq["text"].(map[string]any); ok {
		if verbosity, ok := text["verbosity"].(string); ok && verbosity != "" {
			chat["verbosity"] = verbosity
		}
	}
	if v, ok := respReq["max_output_tokens"]; ok && v != nil {
		chat["max_tokens"] = v
	}
	for _, key := range []string{"parallel_tool_calls", "service_tier", "prompt_cache_key", "prompt_cache_retention", "safety_identifier", "user"} {
		if v, ok := respReq[key]; ok && v != nil {
			chat[key] = v
		}
	}
	if text, ok := respReq["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			if format["type"] == "json_schema" {
				schema := map[string]any{}
				for _, key := range []string{"name", "description", "schema", "strict"} {
					if v, ok := format[key]; ok {
						schema[key] = v
					}
				}
				chat["response_format"] = map[string]any{"type": "json_schema", "json_schema": schema}
			} else {
				chat["response_format"] = format
			}
		}
		if v, ok := text["verbosity"]; ok && v != nil {
			chat["verbosity"] = v
		}
	}
	tools, toolMap, err := convertResponsesToolsChecked(respReq["tools"])
	if err != nil {
		return nil, nil, err
	}
	if len(tools) > 0 {
		chat["tools"] = tools
	}
	if tc, selected := convertResponsesToolChoiceWithMap(respReq["tool_choice"], toolMap); tc != nil {
		chat["tool_choice"] = tc
		if selected != "" {
			allTools, _ := chat["tools"].([]any)
			filtered := make([]any, 0, 1)
			for _, raw := range allTools {
				item, _ := raw.(map[string]any)
				fn, _ := item["function"].(map[string]any)
				if strOf(fn["name"]) == selected {
					filtered = append(filtered, item)
				}
			}
			if len(filtered) == 0 {
				return nil, nil, fmt.Errorf("tool_choice 指定的工具未在 tools 中定义")
			}
			chat["tools"] = filtered
		}
	}
	if reasoning, ok := respReq["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && effort != "" && effort != "none" {
			chat["reasoning_effort"] = effort
		}
		if summary, ok := reasoning["summary"].(string); ok && summary != "" {
			chat["reasoning_summary"] = summary
		}
	}
	return chat, toolMap, nil
}

// reasoningReplayText 取出 DSH 回放的 reasoning 项里的推理正文。
// 网关自己发出去的项把全文放在 summary；官方 Responses 也可能把全文放在 content。
// 有 content 时用 content，否则用 summary。encrypted_content 不是明文，不能当成 reasoning_content。
func reasoningReplayText(item map[string]any) string {
	if text := joinReasoningParts(item["content"]); text != "" {
		return text
	}
	return joinReasoningParts(item["summary"])
}

func joinReasoningParts(raw any) string {
	parts, ok := raw.([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, partAny := range parts {
		part, ok := partAny.(map[string]any)
		if !ok {
			continue
		}
		text, _ := part["text"].(string)
		if text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(text)
	}
	return b.String()
}

// mergeResponsesAssistantItem 将一个 Responses 回复的正文与工具调用合成同一条 Chat assistant。
// reasoning_content 已在第一项上，后续项只补充正文和工具列表，不复制推理。
func mergeResponsesAssistantItem(dst, item map[string]any) {
	if assistantMessageHasPayload(item) {
		if !assistantMessageHasPayload(dst) {
			dst["content"] = item["content"]
		} else {
			dst["content"] = append(responsesChatContentParts(dst["content"]), responsesChatContentParts(item["content"])...)
		}
	}
	if calls, ok := item["tool_calls"].([]any); ok && len(calls) > 0 {
		if existing, ok := dst["tool_calls"].([]any); ok {
			dst["tool_calls"] = append(existing, calls...)
		} else {
			dst["tool_calls"] = calls
		}
	}
}

func responsesChatContentParts(content any) []any {
	switch value := content.(type) {
	case []any:
		return value
	case string:
		if value != "" {
			return []any{map[string]any{"type": "text", "text": value}}
		}
	}
	return nil
}

func reasoningPassthroughStats(chat map[string]any) (attached int, chars int, missing int) {
	list, _ := chat["messages"].([]any)
	for _, msgAny := range list {
		msg, ok := msgAny.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		text, _ := msg["reasoning_content"].(string)
		if text == "" {
			missing++
			continue
		}
		attached++
		chars += utf8.RuneCountInString(text)
	}
	return attached, chars, missing
}

// convertResponsesInputItem 将单个 Responses input item 转换为 0..1 条 chat 消息。
func convertResponsesInputItem(item map[string]any) []any {
	switch typ, _ := item["type"].(string); typ {
	case "function_call":
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		name, _ := item["name"].(string)
		if namespace, _ := item["namespace"].(string); namespace != "" {
			name = flattenNamespaceToolName(namespace, name)
		}
		args, _ := item["arguments"].(string)
		return []any{map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{map[string]any{
				"id":       ifEmpty(callID, "call_"+compactUUID()),
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": args},
			}},
		}}
	case "custom_tool_call":
		callID, _ := item["call_id"].(string)
		if callID == "" {
			callID, _ = item["id"].(string)
		}
		name, _ := item["name"].(string)
		if namespace, _ := item["namespace"].(string); namespace != "" {
			name = flattenNamespaceToolName(namespace, name)
		}
		input, _ := item["input"].(string)
		return []any{map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{map[string]any{
				"id":       ifEmpty(callID, "call_"+compactUUID()),
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": customToolArguments(input)},
			}},
		}}
	case "function_call_output", "custom_tool_call_output":
		callID, _ := item["call_id"].(string)
		text, images := convertResponsesToolOutput(item["output"])
		messages := []any{map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      text,
		}}
		if len(images) > 0 {
			// 与 Anthropic tool_result 的图片处理一致：tool 保留文本，
			// 图片提升成 user 多模态内容，不能把 Base64 当普通文本回放。
			// handleResponses 随后的工具序列修复会将并行批次的图片消息
			// 移至全部 tool 结果之后，避免打断调用/结果配对（11148）。
			messages = append(messages, map[string]any{"role": "user", "content": images})
		}
		return messages
	case "reasoning", "web_search_call":
		// reasoning 的正文在 responsesToChatRequest 里挂到助手消息的 reasoning_content。
		// web_search_call 不能转成消息：插在并行 function_call 与 output 之间会触发 11148。
		return nil
	}

	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}
	message := map[string]any{"role": role, "content": convertResponsesContent(item["content"])}
	if role == "assistant" {
		// 有些客户端直接在助手历史消息上携带 Chat 风格的推理字段；
		// 不要在 Responses 转 Chat 的过程中再次丢弃它。
		if reasoning, ok := item["reasoning_content"].(string); ok {
			message["reasoning_content"] = reasoning
		}
	}
	return []any{message}
}

// convertResponsesContent 将 Responses content（string 或 parts 数组）转换为 chat content。
func convertResponsesContent(content any) any {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		parts := make([]any, 0, len(c))
		for _, pAny := range c {
			p, ok := pAny.(map[string]any)
			if !ok {
				if s, ok := pAny.(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": s})
				}
				continue
			}
			switch typ, _ := p["type"].(string); typ {
			case "input_text", "output_text", "text":
				if t, ok := p["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "refusal":
				if t, ok := p["refusal"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": t})
				}
			case "input_image":
				url, _ := p["image_url"].(string)
				if url != "" {
					image := map[string]any{"url": url}
					if detail, ok := p["detail"]; ok {
						image["detail"] = detail
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": image})
				}
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return parts
	default:
		return fmt.Sprintf("%v", c)
	}
}

// convertResponsesToolOutput 处理 Responses 的字符串或多模态工具结果。
// 普通 JSON 对象/数组维持旧序列化行为；明确的文本块提取原文，图片块作为
// 多模态内容返回。未知块仍作为 JSON 文本保留，不静默丢弃工具数据。
func convertResponsesToolOutput(output any) (string, []any) {
	blocks, ok := output.([]any)
	if !ok {
		return stringifyToolOutput(output), nil
	}
	typed := false
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringField(block, "type") {
		case "input_text", "output_text", "text", "input_image", "image_url":
			typed = true
		}
	}
	if !typed {
		return stringifyToolOutput(output), nil
	}
	var texts []string
	var images []any
	for _, raw := range blocks {
		if block, ok := raw.(map[string]any); ok {
			switch stringField(block, "type") {
			case "input_text", "output_text", "text":
				if text, ok := block["text"].(string); ok {
					texts = append(texts, text)
					continue
				}
			case "input_image", "image_url":
				url, _ := block["image_url"].(string)
				detail, _ := block["detail"].(string)
				if nested, ok := block["image_url"].(map[string]any); ok {
					url, _ = nested["url"].(string)
					detail, _ = nested["detail"].(string)
				}
				if url != "" {
					image := map[string]any{"url": url}
					if detail != "" {
						image["detail"] = detail
					}
					images = append(images, map[string]any{"type": "image_url", "image_url": image})
					continue
				}
			}
		}
		texts = append(texts, stringifyToolOutput(raw))
	}
	text := strings.Join(texts, "\n")
	if text == "" && len(images) > 0 {
		text = "[图片]"
	}
	return text, images
}

// logResponsesToolOutputConversion 只记录块数、位置与长度；图片 URL、
// Base64、工具正文及提示词不进入日志，沿用请求的 TraceID 和文件日志。
func logResponsesToolOutputConversion(r *http.Request, reqID uint64, request map[string]any) {
	input, _ := request["input"].([]any)
	outputs, textChars, imageCount := 0, 0, 0
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || item["type"] != "function_call_output" {
			continue
		}
		text, images := convertResponsesToolOutput(item["output"])
		outputs++
		textChars += utf8.RuneCountInString(text)
		imageCount += len(images)
	}
	log.Printf("[Responses工具结果转换] traceId=%s requestId=%d 工具结果=%d 文本字符数=%d 图片块=%d 图片位置=工具结果之后的user多模态消息 结果=图片不再作为Base64文本计入上下文，原工具文本保留且不记录正文",
		debugTraceID(r), reqID, outputs, textChars, imageCount)
	debugEvent(r, "debug", "responses_tool_output_converted", map[string]any{
		"tool_outputs": outputs, "text_chars": textChars, "image_blocks": imageCount,
		"image_position":  "user_after_tool_results",
		"business_impact": "保留工具文本与图片；避免Base64被当文本计费或撑满上下文；不记录正文",
	})
}

// stringifyToolOutput 将 function_call_output 的 output 统一转为字符串。
func stringifyToolOutput(output any) string {
	switch o := output.(type) {
	case nil:
		return ""
	case string:
		return o
	default:
		if b, err := json.Marshal(o); err == nil {
			return string(b)
		}
		return fmt.Sprintf("%v", o)
	}
}

// convertResponsesToolChoice 将 Responses tool_choice 转换为 chat tool_choice。
func convertResponsesToolChoice(tc any) any {
	choice, _ := convertResponsesToolChoiceWithMap(tc, nil)
	return choice
}

// convertResponsesToolChoiceWithMap 只返回上游接受的字符串枚举；命名工具
// 同时返回扁平工具名，调用方会据此裁剪 tools。腾讯上游不接受 OpenAI 的
// function 对象形式。
func convertResponsesToolChoiceWithMap(tc any, tools map[string]responsesToolMeta) (any, string) {
	switch v := tc.(type) {
	case string:
		if v == "auto" || v == "none" || v == "required" {
			return v, ""
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		if typ == "function" || typ == "custom" {
			name, _ := v["name"].(string)
			if name == "" {
				if fn, ok := v["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			namespace, _ := v["namespace"].(string)
			name = flattenNamespaceToolName(namespace, name)
			if name != "" {
				if tools != nil {
					if meta, ok := tools[name]; ok {
						return "required", meta.UpstreamName
					}
				}
				return "required", name
			}
		}
	}
	return nil, ""
}

// writeResponsesAggregate 聚合上游 SSE 后转换为 Responses 非流式响应。
func writeResponsesAggregate(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	body := newTTFTReader(resp.Body, startTime)
	defer finishFirstToken(r, modelName, reqID, body)
	completionJSON, err := aggregateCompletion(body, modelName)
	if err != nil {
		if writeAggregateBusinessRejection(w, r, reqID, modelName, prof, err) {
			return
		}
		debugEvent(r, "error", "aggregate_response_failed", map[string]any{
			"error_type": debugErrorType(err),
			"error":      safeDebugError(err),
		})
		logAggregateFailure(r, reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "aggregate_error", "聚合上游流式响应失败: "+err.Error())
		return
	}
	usage := usageFromCompletion(completionJSON)
	statsObserveUsage(r, usage, true)
	observeModelCredit(acc, modelName, usage, reqID)
	recordModelTokens(modelName, usage, reqID)
	recordModelLatency(modelName, time.Since(startTime))
	out, err := chatCompletionToResponsesForRequest(r, completionJSON, modelName, responsesToolMapFor(r))
	if err != nil {
		debugEvent(r, "error", "response_translation_failed", map[string]any{
			"error_type": debugErrorType(err),
			"error":      safeDebugError(err),
		})
		log.Printf("[#%d] Responses 转换失败: %v", reqID, err)
		writeOpenAIError(w, http.StatusInternalServerError, "translate_error", "转换 Responses 响应失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
	debugEvent(r, "info", "aggregate_response_completed", map[string]any{
		"status_code":    http.StatusOK,
		"response_bytes": len(out),
	})
	log.Printf("[#%d] Responses 非流式响应完成 (账号 %s [%s], 耗时 %v)", reqID, acc.Path, prof.Label, time.Since(startTime))
}

// chatCompletionToResponses 保留旧接口；主链路使用带工具映射的版本。
func chatCompletionToResponses(chatJSON []byte, modelName string) ([]byte, error) {
	return chatCompletionToResponsesWithTools(chatJSON, modelName, nil)
}

// chatCompletionToResponsesWithTools 将 chat.completion JSON 转换为 Responses 响应对象。
func chatCompletionToResponsesWithTools(chatJSON []byte, modelName string, toolMap map[string]responsesToolMeta) ([]byte, error) {
	return chatCompletionToResponsesForRequest(nil, chatJSON, modelName, toolMap)
}

func chatCompletionToResponsesForRequest(r *http.Request, chatJSON []byte, modelName string, toolMap map[string]responsesToolMeta) ([]byte, error) {
	var chat map[string]any
	if err := json.Unmarshal(chatJSON, &chat); err != nil {
		return nil, err
	}

	var content, refusal, reasoning, finishReason string
	var toolCalls []any
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			finishReason, _ = choice["finish_reason"].(string)
			if msg, ok := choice["message"].(map[string]any); ok {
				content, _ = msg["content"].(string)
				refusal, _ = msg["refusal"].(string)
				reasoning, _ = msg["reasoning_content"].(string)
				toolCalls, _ = msg["tool_calls"].([]any)
			}
		}
	}
	status, incompleteDetails := responsesCompletionState(finishReason)

	output := []any{}
	if reasoning != "" {
		item := map[string]any{
			"id":      "rs_" + compactUUID(),
			"type":    "reasoning",
			"status":  status,
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
		}
		output = append(output, decorateResponsesReasoningItem(r, item))
	}
	if content != "" || refusal != "" || len(toolCalls) == 0 {
		parts := []any{}
		if content != "" || refusal == "" {
			parts = append(parts, map[string]any{"type": "output_text", "text": content, "annotations": []any{}})
		}
		if refusal != "" {
			parts = append(parts, map[string]any{"type": "refusal", "refusal": refusal})
		}
		output = append(output, map[string]any{
			"id":      "msg_" + compactUUID(),
			"type":    "message",
			"status":  status,
			"role":    "assistant",
			"content": parts,
		})
	}
	for _, tcAny := range toolCalls {
		tc, ok := tcAny.(map[string]any)
		if !ok {
			continue
		}
		callID, _ := tc["id"].(string)
		name, args := "", ""
		if fn, ok := tc["function"].(map[string]any); ok {
			name, _ = fn["name"].(string)
			args, _ = fn["arguments"].(string)
		}
		item := responsesToolCallItem(callID, name, args, toolMap)
		item["status"] = status
		output = append(output, item)
	}

	now := time.Now().Unix()
	created := now
	if v, ok := chat["created"].(float64); ok && v > 0 && int64(v) <= now {
		created = int64(v)
	}
	respID, _ := chat["id"].(string)
	if respID == "" {
		respID = "resp_" + compactUUID()
	} else {
		respID = "resp_" + strings.TrimPrefix(respID, "chatcmpl-")
	}

	result := buildResponsesEnvelopeForRequest(r, respID, modelName, created)
	result["status"] = status
	result["incomplete_details"] = incompleteDetails
	result["output"] = output
	if status == "completed" {
		result["completed_at"] = now
	}
	if usage, ok := chat["usage"].(map[string]any); ok {
		result["usage"] = toResponsesUsage(usage)
	}
	return json.Marshal(result)
}

// A clean upstream EOF can still carry truncated output. Keep that distinction
// in both Responses exits, including tool calls whose arguments may be partial.
func responsesCompletionState(finishReason string) (string, any) {
	switch finishReason {
	case "length":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	default:
		return "completed", nil
	}
}

// buildResponsesEnvelope 构造 Responses 响应骨架（含规范要求的常见字段）。
func buildResponsesEnvelope(id, model string, createdAt int64) map[string]any {
	return map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           createdAt,
		"completed_at":         nil,
		"status":               "in_progress",
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"max_output_tokens":    nil,
		"model":                model,
		"output":               []any{},
		"parallel_tool_calls":  true,
		"previous_response_id": nil,
		"reasoning":            map[string]any{"effort": nil, "summary": nil},
		"store":                false, // 网关不保存可由 previous_response_id 查询的响应。
		"temperature":          1.0,
		"text":                 map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice":          "auto",
		"tools":                []any{},
		"top_p":                1.0,
		"truncation":           "disabled",
		"usage":                nil,
		"metadata":             map[string]any{},
	}
}

// toResponsesUsage 将 chat usage 转换为 Responses usage 结构。
func toResponsesUsage(u map[string]any) map[string]any {
	in := numOr0(u["prompt_tokens"])
	out := numOr0(u["completion_tokens"])
	total := numOr0(u["total_tokens"])
	if total == 0 {
		total = in + out
	}
	cached, reasoningTokens := 0.0, int64(0)
	// 统一走 statsCachedValue：兼容 prompt_cache_hit_tokens / cache_read_input_tokens /
	// cached_tokens / *_details.cached_tokens 多种表示，只取一个、不相加。
	if v, _, ok := statsCachedValue(u); ok {
		cached = float64(v)
	}
	var validReasoning bool
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		reasoningTokens, validReasoning = responsesReasoningTokens(d["reasoning_tokens"])
	}
	if !validReasoning {
		reasoningTokens, _ = responsesReasoningTokens(u["completion_thinking_tokens"])
	}
	return map[string]any{
		"input_tokens":          int64(in),
		"input_tokens_details":  map[string]any{"cached_tokens": int64(cached)},
		"output_tokens":         int64(out),
		"output_tokens_details": map[string]any{"reasoning_tokens": int64(reasoningTokens)},
		"total_tokens":          int64(total),
	}
}

// Prefer the standard detail even when it explicitly reports zero. The upstream
// thinking counter is an alternative representation, never an additional cost.
func responsesReasoningTokens(value any) (int64, bool) {
	var number float64
	switch value := value.(type) {
	case float64:
		number = value
	case json.Number:
		var err error
		number, err = value.Float64()
		if err != nil {
			return 0, false
		}
	case int:
		if value >= 0 {
			return int64(value), true
		}
		return 0, false
	case int64:
		if value >= 0 {
			return value, true
		}
		return 0, false
	default:
		return 0, false
	}
	if number < 0 || number >= math.Exp2(63) || math.IsNaN(number) || math.Trunc(number) != number {
		return 0, false
	}
	return int64(number), true
}

func numOr0(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// streamResponsesResponse 将上游 Chat Completions SSE 实时转译为 Responses 语义事件流。
func streamResponsesResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, modelName string, reqID uint64, acc *Account, prof *upstreamProfile, startTime time.Time) {
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "streaming_unsupported", "服务器不支持流式响应 Flush")
		return
	}
	// 上游首字节可能很慢：先下发 SSE 注释心跳，避免边缘节点静默期重置连接。
	// 所有协议事件都经同一把写锁写出，心跳不会与事件帧交错。
	streamContext, cancelStream := context.WithCancel(r.Context())
	defer cancelStream()
	r = r.WithContext(streamContext)
	guard := &streamWriteGuard{w: w, flusher: flusher, cancel: cancelStream}
	guard.setBody(resp.Body)
	heartbeat := startStreamHeartbeat(guard, currentStreamKeepalive())
	defer heartbeat.Stop()
	// response.created / in_progress 不是模型有效数据，不能停掉心跳。
	out := controlWriter{guard}

	seq := 0
	var eventBuf []byte
	var sseWriteErr error
	writeFailed := func(err error) {
		sseWriteErr = err
		_ = resp.Body.Close()
	}
	emit := func(eventType string, data map[string]any) {
		if sseWriteErr != nil {
			return
		}
		data["type"] = eventType
		data["sequence_number"] = seq
		seq++
		b, err := json.Marshal(data)
		if err != nil {
			return
		}
		eventBuf = append(eventBuf[:0], "event: "...)
		eventBuf = append(eventBuf, eventType...)
		eventBuf = append(eventBuf, "\ndata: "...)
		eventBuf = append(eventBuf, b...)
		eventBuf = append(eventBuf, '\n', '\n')
		if err := writeSSE(out, out, eventBuf); err != nil {
			writeFailed(err)
		}
	}

	created := time.Now().Unix()
	respID := "resp_" + compactUUID()
	envelope := buildResponsesEnvelopeForRequest(r, respID, modelName, created)
	emit("response.created", map[string]any{"response": envelope})
	emit("response.in_progress", map[string]any{"response": envelope})

	nextIndex := 0
	idxToItem := map[int]map[string]any{}

	// reasoning item 状态
	reasoningOpen := false
	reasoningIndex := -1
	reasoningItemID := ""

	// message item 状态
	msgOpen := false
	msgIndex := -1
	msgItemID := ""
	textPartIndex, refusalPartIndex := -1, -1
	var messagePartTypes []string

	// function_call item 状态
	toolCalls := map[int]*mergedToolCall{}
	var toolOrder []int
	tcItemID := map[int]string{}
	tcIndex := map[int]int{}
	tcMeta := map[int]responsesToolMeta{}

	openReasoning := func() {
		if reasoningOpen {
			return
		}
		reasoningOpen = true
		reasoningIndex = nextIndex
		nextIndex++
		reasoningItemID = "rs_" + compactUUID()
		emit("response.output_item.added", map[string]any{
			"output_index": reasoningIndex,
			"item":         map[string]any{"id": reasoningItemID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
		})
		emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}

	openMessage := func() {
		if msgOpen {
			return
		}
		msgOpen = true
		msgIndex = nextIndex
		nextIndex++
		msgItemID = "msg_" + compactUUID()
		emit("response.output_item.added", map[string]any{
			"output_index": msgIndex,
			"item":         map[string]any{"id": msgItemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
		})
	}
	openMessagePart := func(refusal bool) {
		openMessage()
		partIndex := &textPartIndex
		if refusal {
			partIndex = &refusalPartIndex
		}
		if *partIndex >= 0 {
			return
		}
		part := map[string]any{"type": "output_text", "annotations": []any{}, "text": ""}
		if refusal {
			part = map[string]any{"type": "refusal", "refusal": ""}
		}
		*partIndex = len(messagePartTypes)
		messagePartTypes = append(messagePartTypes, part["type"].(string))
		emit("response.content_part.added", map[string]any{
			"item_id": msgItemID, "output_index": msgIndex, "content_index": *partIndex, "part": part,
		})
	}

	var usage map[string]any
	finishReason := ""
	sawDone := false
	streamComplete := false

	body := newTTFTReader(resp.Body, startTime)
	defer finishFirstToken(r, modelName, reqID, body)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	mode := fastPathMode.Load()
	var view respChunkView
	var usageRaw []byte
	defer func() {
		// The fast path defers decoding usage until the stream exits, including
		// exits caused by a failed downstream write. Missing credit stays unknown.
		if len(usageRaw) > 0 {
			var u map[string]any
			if json.Unmarshal(usageRaw, &u) == nil {
				usage = u
			}
		}
		statsObserveUsage(r, usage, streamComplete && sseWriteErr == nil)
		observeModelCredit(acc, modelName, usage, reqID)
		recordModelTokens(modelName, usage, reqID)
		recordModelLatency(modelName, time.Since(startTime))
	}()
	var msgRaw, refusalRaw, reasoningRaw []byte
	var textPrefix, refusalPrefix, reasoningPrefix []byte
	toolPrefix := map[int][]byte{}
	deltaBuf := make([]byte, 0, 512)
	var streamRejection *upstreamStreamRejection
	for sseWriteErr == nil && scanner.Scan() {
		data := stripDataPrefixBytes(scanner.Bytes())
		if len(data) == 0 {
			continue
		}
		// 上游已经有数据流动，停止心跳；心跳只覆盖首字节前的静默期。
		guard.markEffectiveData()
		if bytes.Equal(data, doneMarker) {
			sawDone = true
			continue
		}
		if streamRejection = detectUpstreamStreamRejection(data); streamRejection != nil {
			break
		}
		if mode == int32(fastPathOn) && parseResponsesChunk(data, &view) {
			// 快路径：结构扫描 + 直接拼装事件（见 responsesfast.go）
			if view.firstTok {
				observeFirstTokenFlag(body)
			}
			if len(view.usage) > 0 {
				usageRaw = append(usageRaw[:0], view.usage...)
			}
			if view.finish != "" && finishReason == "" {
				finishReason = view.finish
			}
			for c := 0; c < view.nChoices; c++ {
				if sseWriteErr != nil {
					break
				}
				if raw := view.reasoning[c]; len(raw) > 0 {
					openReasoning()
					if sseWriteErr != nil {
						break
					}
					if reasoningPrefix == nil {
						reasoningPrefix = respReasoningDeltaPrefix(make([]byte, 0, 192), reasoningItemID, reasoningIndex)
					}
					reasoningRaw = append(reasoningRaw, raw...)
					if err := emitRawDelta(out, out, &deltaBuf, reasoningPrefix, raw, &seq); err != nil {
						writeFailed(err)
						break
					}
				}
				if raw := view.content[c]; len(raw) > 0 {
					openMessagePart(false)
					if sseWriteErr != nil {
						break
					}
					if textPrefix == nil {
						textPrefix = respContentDeltaPrefix(make([]byte, 0, 192), "response.output_text.delta", msgItemID, msgIndex, textPartIndex)
					}
					msgRaw = append(msgRaw, raw...)
					if err := emitRawDelta(out, out, &deltaBuf, textPrefix, raw, &seq); err != nil {
						writeFailed(err)
						break
					}
				}
				if raw := view.refusal[c]; len(raw) > 0 {
					openMessagePart(true)
					if sseWriteErr != nil {
						break
					}
					if refusalPrefix == nil {
						refusalPrefix = respContentDeltaPrefix(make([]byte, 0, 192), "response.refusal.delta", msgItemID, msgIndex, refusalPartIndex)
					}
					refusalRaw = append(refusalRaw, raw...)
					if err := emitRawDelta(out, out, &deltaBuf, refusalPrefix, raw, &seq); err != nil {
						writeFailed(err)
						break
					}
				}
				if n := view.toolCnt[c]; n > 0 {
					before := len(toolOrder)
					for t := 0; t < n; t++ {
						applyToolCallView(toolCalls, &toolOrder, &view.tools[view.toolOff[c]+t])
					}
					for i := before; i < len(toolOrder); i++ {
						idx := toolOrder[i]
						st := toolCalls[idx]
						if st.ID == "" {
							st.ID = "call_" + compactUUID()
						}
						meta, _ := responsesToolMetaFor(r, st.Name)
						tcMeta[idx] = meta
						item := responsesToolCallItem(st.ID, st.Name, "", responsesToolMapFor(r))
						item["status"] = "in_progress"
						tcItemID[idx] = item["id"].(string)
						tcIndex[idx] = nextIndex
						nextIndex++
						emit("response.output_item.added", map[string]any{
							"output_index": tcIndex[idx],
							"item":         item,
						})
					}
					for t := 0; t < n; t++ {
						if sseWriteErr != nil {
							break
						}
						tv := &view.tools[view.toolOff[c]+t]
						if len(tv.args) == 0 || tcMeta[tv.index].Kind == responsesToolCustom {
							continue
						}
						p := toolPrefix[tv.index]
						if p == nil {
							p = respToolDeltaPrefix(make([]byte, 0, 192), tcItemID[tv.index], tcIndex[tv.index])
							toolPrefix[tv.index] = p
						}
						if err := emitRawDelta(out, out, &deltaBuf, p, tv.args, &seq); err != nil {
							writeFailed(err)
							break
						}
					}
				}
			}
			continue
		}
		var chunk map[string]any
		if json.Unmarshal(data, &chunk) != nil {
			continue
		}
		observeFirstToken(body, chunk)
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
			usageRaw = usageRaw[:0]
		}
		finishReason = noteFinishReason(finishReason, chunk)
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			choice, _ := c.(map[string]any)
			if choice == nil {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
				openReasoning()
				// 统一累计到 raw 缓冲区：快慢路径可能逐片交替，只有单一累计器才能保证
				// 收尾事件（*.done / response.completed）包含全部输出。
				reasoningRaw = append(reasoningRaw, jsonEscapeBody(rc)...)
				emit("response.reasoning_summary_text.delta", map[string]any{
					"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "delta": rc,
				})
			}
			if ct, ok := delta["content"].(string); ok && ct != "" {
				openMessagePart(false)
				msgRaw = append(msgRaw, jsonEscapeBody(ct)...)
				emit("response.output_text.delta", map[string]any{
					"item_id": msgItemID, "output_index": msgIndex, "content_index": textPartIndex, "delta": ct,
				})
			}
			if refusal, ok := delta["refusal"].(string); ok && refusal != "" {
				openMessagePart(true)
				refusalRaw = append(refusalRaw, jsonEscapeBody(refusal)...)
				emit("response.refusal.delta", map[string]any{
					"item_id": msgItemID, "output_index": msgIndex, "content_index": refusalPartIndex, "delta": refusal,
				})
			}
			if tcs, ok := delta["tool_calls"].([]any); ok && len(tcs) > 0 {
				// 先提取本次参数增量，供合并后补发 delta 事件
				type argDelta struct {
					idx int
					arg string
				}
				var argDeltas []argDelta
				for _, tcAny := range tcs {
					tc, ok := tcAny.(map[string]any)
					if !ok {
						continue
					}
					idx := 0
					if v, ok := tc["index"].(float64); ok {
						idx = int(v)
					}
					if fn, ok := tc["function"].(map[string]any); ok {
						if a, ok := fn["arguments"].(string); ok && a != "" {
							argDeltas = append(argDeltas, argDelta{idx: idx, arg: a})
						}
					}
				}
				before := len(toolOrder)
				applyToolCallDelta(toolCalls, &toolOrder, tcs)
				for i := before; i < len(toolOrder); i++ {
					idx := toolOrder[i]
					st := toolCalls[idx]
					if st.ID == "" {
						st.ID = "call_" + compactUUID()
					}
					meta, _ := responsesToolMetaFor(r, st.Name)
					tcMeta[idx] = meta
					tcIndex[idx] = nextIndex
					nextIndex++
					if meta.Kind == responsesToolCustom {
						tcItemID[idx] = "ctc_" + compactUUID()
						item := responsesToolCallItem(st.ID, st.Name, "", responsesToolMapFor(r))
						item["id"], item["status"] = tcItemID[idx], "in_progress"
						emit("response.output_item.added", map[string]any{
							"output_index": tcIndex[idx],
							"item":         item,
						})
						continue
					}
					tcItemID[idx] = "fc_" + compactUUID()
					itemName := st.Name
					item := map[string]any{
						"id": tcItemID[idx], "type": "function_call", "status": "in_progress",
						"call_id": st.ID, "name": itemName, "arguments": "",
					}
					if meta.Namespace != "" {
						item["name"] = meta.Name
						item["namespace"] = meta.Namespace
					}
					emit("response.output_item.added", map[string]any{"output_index": tcIndex[idx], "item": item})
				}
				for _, ad := range argDeltas {
					if tcMeta[ad.idx].Kind == responsesToolCustom {
						continue // 自定义工具的 JSON 参数在收尾时还原为原始 input
					}
					emit("response.function_call_arguments.delta", map[string]any{
						"item_id": tcItemID[ad.idx], "output_index": tcIndex[ad.idx], "delta": ad.arg,
					})
				}
			}
		}
		if sseWriteErr != nil {
			_ = resp.Body.Close()
			break
		}
	}
	if sseWriteErr != nil || guard.err() != nil {
		// 客户端已断开：关闭响应体会取消上游请求，停止继续消耗额度。
		_ = resp.Body.Close()
		log.Printf("[#%d] 下游写入失败，已终止本次流 (账号 %s)", reqID, acc.Path)
		return
	}
	scanErr := scanner.Err()
	if streamRejection == nil {
		// 读取层重放可能直接返回带业务码的错误（例如第二次请求收到 11128），
		// 扫描循环没有对应的 data 行，这里补做一次识别，保留业务码。
		streamRejection = reportUpstreamStreamRejection(r, reqID, modelName, prof, scanErr)
	} else {
		scanErr = streamRejection
		reportUpstreamStreamRejection(r, reqID, modelName, prof, scanErr)
	}
	streamComplete = scanErr == nil && finishReason != ""
	completeTokenStream(body, streamComplete)
	if len(usageRaw) > 0 {
		var u map[string]any
		if json.Unmarshal(usageRaw, &u) == nil {
			usage = u
		}
		usageRaw = nil
	}
	messageText := decodeJSONBody(msgRaw)
	refusalText := decodeJSONBody(refusalRaw)
	reasoningText := decodeJSONBody(reasoningRaw)
	messageChars, reasoningChars := len(messageText), len(reasoningText)
	if scanErr != nil {
		debugEvent(r, "error", "stream_response_failed", map[string]any{
			"error_type": debugErrorType(scanErr),
			"error":      safeDebugError(scanErr),
		})
		// 上游流中断时不能伪造 response.completed 与 [DONE]：那会让下游把残缺输出
		// 当成完整结果。改为下发一个 failed 事件并直接返回，明确告知本次响应不完整。
		reason := "上游流式响应中断，本次回复不完整"
		code := "stream_interrupted"
		if streamRejection != nil {
			reason, code = streamRejection.clientMessage(), streamRejection.info.Code
		}
		if errors.Is(scanErr, context.DeadlineExceeded) {
			reason = fmt.Sprintf("上游超过 %v 无数据，判定连接卡死并中断，本次回复不完整", upstreamIdleTimeout)
		}
		failed := buildResponsesEnvelopeForRequest(r, respID, modelName, created)
		failed["status"] = "failed"
		failed["error"] = map[string]any{"code": code, "message": reason}
		emit("response.failed", map[string]any{"response": failed})
		log.Printf("[异常] traceId=%s requestId=%d 发生阶段=上游流式读取 账号=%s 异常=%v 业务影响=本次响应不完整，已下发 response.failed 而非伪造完成", debugTraceID(r), reqID, acc.Path, scanErr)
		return
	}
	if finishReason == "" {
		// 干净 EOF：读错误是空的，但没有非空 finish_reason。
		// 不能把已经收到的推理再塞进 output_item.done / response.completed，
		// 那会让客户端把残缺输出当成完整结果；只发一个小的 response.failed。
		debugEvent(r, "error", "stream_closed_without_finish", map[string]any{
			"error_type":      "stream_closed_without_finish",
			"finish_reason":   "",
			"saw_done":        sawDone,
			"reasoning_chars": reasoningChars,
			"message_chars":   messageChars,
			"tool_call_count": len(toolOrder),
			"business_impact": "上游干净结束但没有 finish_reason，已拒绝 response.completed，改为小的 response.failed",
		})
		failed := buildResponsesEnvelopeForRequest(r, respID, modelName, created)
		failed["status"] = "failed"
		failed["error"] = map[string]any{
			"code":    "stream_closed_without_finish",
			"message": errStreamClosedWithoutFinish.Error(),
		}
		emit("response.failed", map[string]any{"response": failed})
		log.Printf("[异常] traceId=%s requestId=%d 发生阶段=Responses收尾 账号=%s 结果=拒绝当成成功 原因=上游干净结束但没有 finish_reason 是否看到DONE=%t 已收到推理字符数=%d 已收到正文字符数=%d 工具调用数=%d 业务影响=不下发 output_item.done 和 response.completed，客户端收到 response.failed 是否已处理=是",
			debugTraceID(r), reqID, acc.Path, sawDone, reasoningChars, messageChars, len(toolOrder))
		return
	}

	// 若无任何输出，补一个空 message，保证 output 非空且事件序列完整
	if !reasoningOpen && !msgOpen && len(toolOrder) == 0 {
		openMessagePart(false)
	}
	status, incompleteDetails := responsesCompletionState(finishReason)

	// 收尾：reasoning
	if reasoningOpen {
		text := reasoningText
		emit("response.reasoning_summary_text.done", map[string]any{"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "text": text})
		emit("response.reasoning_summary_part.done", map[string]any{"item_id": reasoningItemID, "output_index": reasoningIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text}})
		item := map[string]any{"id": reasoningItemID, "type": "reasoning", "status": status, "summary": []any{map[string]any{"type": "summary_text", "text": text}}}
		item = decorateResponsesReasoningItem(r, item)
		emit("response.output_item.done", map[string]any{"output_index": reasoningIndex, "item": item})
		idxToItem[reasoningIndex] = item
	}

	// 收尾：message
	if msgOpen {
		parts := make([]any, 0, len(messagePartTypes))
		for index, typ := range messagePartTypes {
			part := map[string]any{"type": "output_text", "annotations": []any{}, "text": messageText}
			if typ == "refusal" {
				part = map[string]any{"type": "refusal", "refusal": refusalText}
				emit("response.refusal.done", map[string]any{"item_id": msgItemID, "output_index": msgIndex, "content_index": index, "refusal": refusalText})
			} else {
				emit("response.output_text.done", map[string]any{"item_id": msgItemID, "output_index": msgIndex, "content_index": index, "text": messageText})
			}
			emit("response.content_part.done", map[string]any{"item_id": msgItemID, "output_index": msgIndex, "content_index": index, "part": part})
			parts = append(parts, part)
		}
		item := map[string]any{"id": msgItemID, "type": "message", "status": status, "role": "assistant", "content": parts}
		emit("response.output_item.done", map[string]any{"output_index": msgIndex, "item": item})
		idxToItem[msgIndex] = item
	}

	// 收尾：function calls
	for _, idx := range toolOrder {
		st := toolCalls[idx]
		args := st.Args.String()
		meta := tcMeta[idx]
		if meta.Kind == responsesToolCustom {
			input := customToolInputFromArguments(args)
			emit("response.custom_tool_call_input.delta", map[string]any{
				"item_id": tcItemID[idx], "output_index": tcIndex[idx], "delta": input,
			})
			emit("response.custom_tool_call_input.done", map[string]any{
				"item_id": tcItemID[idx], "output_index": tcIndex[idx], "input": input,
			})
			item := map[string]any{
				"id": tcItemID[idx], "type": "custom_tool_call", "status": status,
				"call_id": st.ID, "name": meta.Name, "input": input,
			}
			if meta.Namespace != "" {
				item["namespace"] = meta.Namespace
			}
			emit("response.output_item.done", map[string]any{"output_index": tcIndex[idx], "item": item})
			idxToItem[tcIndex[idx]] = item
			continue
		}
		eventName := st.Name
		if meta.Namespace != "" {
			eventName = meta.Name
		}
		emit("response.function_call_arguments.done", map[string]any{"item_id": tcItemID[idx], "output_index": tcIndex[idx], "name": eventName, "arguments": args})
		item := map[string]any{"id": tcItemID[idx], "type": "function_call", "status": status, "call_id": st.ID, "name": st.Name, "arguments": args}
		if meta.Namespace != "" {
			item["name"] = meta.Name
			item["namespace"] = meta.Namespace
		}
		emit("response.output_item.done", map[string]any{"output_index": tcIndex[idx], "item": item})
		idxToItem[tcIndex[idx]] = item
	}

	output := make([]any, 0, len(idxToItem))
	for i := 0; i < nextIndex; i++ {
		if item, ok := idxToItem[i]; ok {
			output = append(output, item)
		}
	}

	final := buildResponsesEnvelopeForRequest(r, respID, modelName, created)
	final["status"] = status
	final["incomplete_details"] = incompleteDetails
	final["output"] = output
	if status == "completed" {
		final["completed_at"] = time.Now().Unix()
	}
	if usage != nil {
		final["usage"] = toResponsesUsage(usage)
	}
	emit("response."+status, map[string]any{"response": final})
	if sseWriteErr != nil {
		return
	}

	if err := writeSSE(w, flusher, []byte("data: [DONE]\n\n")); err != nil {
		writeFailed(err)
		return
	}
	if scanErr == nil {
		debugEvent(r, "info", "stream_response_completed", map[string]any{"status_code": http.StatusOK})
	}
	log.Printf("[#%d] Responses 流式输出完成 (账号 %s [%s], 耗时 %v, 首Token(v2) %s)", reqID, acc.Path, prof.Label, time.Since(startTime), body.text())
}

// compactUUID 返回去掉连字符的随机 UUID。
func compactUUID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}
